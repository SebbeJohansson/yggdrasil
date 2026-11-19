package norn

// A goal's why: when Sindri starts coding agents on a goal, it asks Norn
// for what lies behind it, so the agent builds what was asked for and not
// only what the one-line goal says. Norn looks on this machine for the mail
// and chats, the meeting notes and the Rewind moments that share the goal's
// words: the inbox (Outlook, Teams, Slack, Gmail), Outlook mail searched
// through Graph, the Obsidian meeting notes, and Rewind's timeline of the
// last 31 days (meetings, your messages to Norn, what it learned, promises,
// reply drafts, notes changed; Sindri's own goals left out).
//
// The local model reads what was found and keeps only what is clearly about
// the goal, each as one line; nothing raw leaves Norn. The lines then have
// people's names swapped for placeholders (pseudo.go), and a line that looks
// like it holds a key or a token is dropped. When nothing matches surely,
// the model cannot read, or names cannot be swapped, the pack is empty:
// better nothing than noise.
//
//	POST /v1/why { goal, context?, repos?, goalId? } → { sources }

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"yggdrasil/internal/llm"
	"yggdrasil/internal/part"
)

const (
	// WhyMaxSources is the most sources a pack holds.
	WhyMaxSources = 5
	// whyCandidates is the most found things the local model reads.
	whyCandidates = 8
	// whySnippet is how much of each found thing the local model reads.
	whySnippet = 700
	// WhySummaryMax is the longest one source's line.
	WhySummaryMax = 220
	// whyMaxWords is the most of the goal's words looked for.
	whyMaxWords = 8
)

// WhyBudget is how long building a pack may take; Sindri waits a little longer.
var WhyBudget = 25 * time.Second

// WhySource is one reason behind a goal, as it may go to a coding agent.
type WhySource struct {
	// Kind is mail (mail and chats), meeting (meetings and their notes) or rewind.
	Kind string `json:"kind"`
	// At is when it happened, epoch ms.
	At int64 `json:"at"`
	// Summary is one line, written by the local model, names swapped for placeholders.
	Summary string `json:"summary"`
	// Ref is where it was found, for the owner: never sent to the agent.
	Ref string `json:"ref"`
}

// WhyPack is what POST /v1/why answers: the sources, none when nothing matched surely.
type WhyPack struct {
	Sources []WhySource `json:"sources"`
}

// whyFound is something found on this machine, before the local model reads
// it. Its text never leaves Norn.
type whyFound struct {
	Kind, Ref, Text string
	At              int64
	score           int
}

// whyStopWords are a goal's words that say nothing about what it is about.
var whyStopWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`add adds added fix fixes fixed make makes build builds show shows let lets use uses
		support new should can could would will need needs want wants get gets set sets put move moves change changes
		update updates remove removes delete keep keeps more less from into onto than then them they their there these
		those also only just like page pages view button buttons feature features thing things goal goals task tasks
		lägg till fixa gör bygg visa ändra ta bort ny nya ska kan vill behöver som från till inte även bara`) {
		whyStopWords[w] = true
	}
}

// WhyWords is what a goal is looked for by: its words, the ones that say
// nothing about what it is about left out, at most whyMaxWords.
func WhyWords(goal string) []string {
	var out []string
	for _, word := range SearchWords(goal) {
		if whyStopWords[word] {
			continue
		}
		out = append(out, word)
		if len(out) == whyMaxWords {
			break
		}
	}
	return out
}

// WhyScore is how many of the words text holds, each counted once, matched
// at the start of a word ("export" finds "exports", not "reexport").
func WhyScore(text string, words []string) int {
	text = strings.ToLower(text)
	score := 0
	for _, word := range words {
		if stemmed := stem(word); stemmed != "" && startsAWord(text, stemmed) {
			score++
		}
	}
	return score
}

func startsAWord(text, word string) bool {
	for from := 0; from < len(text); {
		i := strings.Index(text[from:], word)
		if i < 0 {
			return false
		}
		at := from + i
		if at == 0 {
			return true
		}
		before := []rune(text[:at])
		if r := before[len(before)-1]; !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return true
		}
		from = at + len(word)
	}
	return false
}

// WhyNeeded is how many of the goal's words a found thing must hold to be
// read: two, or the one when the goal has only one.
func WhyNeeded(words []string) int { return min(2, len(words)) }

// BestFound is what the local model reads: what holds enough of the words,
// most words first, then newest, each place once, at most whyCandidates.
func BestFound(found []whyFound, words []string) []whyFound {
	need := WhyNeeded(words)
	if need == 0 {
		return nil
	}
	seen := map[string]bool{}
	var kept []whyFound
	for _, f := range found {
		f.score = WhyScore(f.Text, words)
		if f.score < need || seen[f.Ref] || strings.TrimSpace(f.Text) == "" {
			continue
		}
		seen[f.Ref] = true
		kept = append(kept, f)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].score != kept[j].score {
			return kept[i].score > kept[j].score
		}
		return kept[i].At > kept[j].At
	})
	return kept[:min(len(kept), whyCandidates)]
}

// --- where it looks ----------------------------------------------------------------

// whyInbox is what is unread now: mail, Teams, Slack and Gmail.
func (n *Norn) whyInbox(now time.Time) []whyFound {
	if n.Inbox == nil {
		return nil
	}
	items, _, _ := n.Inbox.Current(now)
	var out []whyFound
	for _, ranked := range items {
		item := ranked.Item
		out = append(out, whyFound{Kind: "mail", Ref: "inbox:" + item.ID, At: item.At,
			Text: strings.Join([]string{item.Subject, item.Preview, item.Text, item.Context}, "\n")})
	}
	return out
}

// whyMail is your Outlook mail holding the words, through Graph; nothing
// without the Microsoft sign-in.
func (n *Norn) whyMail(ctx context.Context, words []string) []whyFound {
	if n.Accounts == nil || n.Accounts.Microsoft == nil {
		return nil
	}
	token, err := n.Accounts.Microsoft.Token(ctx)
	if err != nil {
		return nil
	}
	var page struct {
		Value []graphMessage `json:"value"`
	}
	search := url.QueryEscape(`"` + strings.Join(words[:min(len(words), 4)], " ") + `"`)
	if err := graphJSON(ctx, token.AccessToken, "/v1.0/me/messages?$search="+search+"&$top=10&$select=id,subject,receivedDateTime,bodyPreview", &page); err != nil {
		n.logf("why: searching mail: %v", err)
		return nil
	}
	var out []whyFound
	for _, m := range page.Value {
		at, _ := when(m.ReceivedDateTime)
		out = append(out, whyFound{Kind: "mail", Ref: "outlook:" + m.ID, At: at, Text: m.Subject + "\n" + m.BodyPreview})
	}
	return out
}

// whyNotes is the Obsidian meeting notes whose titles share the words, with
// the start of each.
func (n *Norn) whyNotes(words []string, now time.Time) []whyFound {
	if !n.Vault.On() {
		return nil
	}
	n.vaultMu.Lock()
	defer n.vaultMu.Unlock()
	var out []whyFound
	for _, note := range n.Vault.Related(nil, strings.Join(words, " "), "", whyCandidates*2, now) {
		excerpt, err := n.Vault.Excerpt(note.Path, whySnippet*2)
		if err != nil {
			continue
		}
		var at int64
		if day, err := time.ParseInLocation("2006-01-02", note.Date, n.Zone); err == nil {
			at = day.UnixMilli()
		}
		out = append(out, whyFound{Kind: "meeting", Ref: "obsidian:" + note.Path, At: at, Text: note.Title + "\n" + excerpt})
	}
	return out
}

// whyRewind is Rewind's timeline of the last 31 days, without Sindri's own
// goals (the goal asked about is one of them).
func (n *Norn) whyRewind(ctx context.Context, now time.Time) []whyFound {
	entries, _ := n.timeline(ctx, now.Add(-RewindRangeMax), now, false)
	var out []whyFound
	for _, e := range entries {
		if e.Kind == "break" {
			continue
		}
		kind := "rewind"
		if e.Kind == "meeting" {
			kind = "meeting"
		}
		out = append(out, whyFound{Kind: kind, Ref: fmt.Sprintf("rewind:%s:%d", e.Kind, e.At), At: e.At, Text: e.Title + "\n" + e.Detail})
	}
	return out
}

// --- what the local model makes of it ---------------------------------------------

const whySystem = `You help a coding agent understand why a goal exists. You get the goal and numbered snippets from the owner's own mail, chats, meeting notes and timeline.
Pick only the snippets that are clearly about this goal: the request behind it, a decision, a constraint, or what someone needs from it. Leave out anything only loosely related. Picking none is a good answer.
For each snippet you pick, write one plain sentence of at most 25 words: what was asked, decided or needed. Write it in English.
Never include greetings, links, email addresses, phone numbers, passwords, keys or tokens.
Answer with JSON only: {"picked":[{"n":1,"summary":"…"}]}`

var whyFormat = map[string]any{
	"type":     "object",
	"required": []string{"picked"},
	"properties": map[string]any{
		"picked": map[string]any{"type": "array", "items": map[string]any{
			"type":     "object",
			"required": []string{"n", "summary"},
			"properties": map[string]any{
				"n":       map[string]any{"type": "integer"},
				"summary": map[string]any{"type": "string"},
			},
		}},
	},
}

// WhyMessages is what the local model reads: the goal and what was found.
func WhyMessages(goal, goalContext string, found []whyFound, zone *time.Location) []llm.Message {
	var b strings.Builder
	b.WriteString("Goal: " + goal + "\n")
	if goalContext = strings.TrimSpace(goalContext); goalContext != "" {
		b.WriteString("Background: " + OneLine(goalContext, 600) + "\n")
	}
	b.WriteString("\nSnippets:\n")
	for i, f := range found {
		day := "undated"
		if f.At > 0 {
			day = time.UnixMilli(f.At).In(zone).Format("2006-01-02")
		}
		fmt.Fprintf(&b, "[%d] (%s, %s) %s\n", i+1, f.Kind, day, OneLine(f.Text, whySnippet))
	}
	return []llm.Message{{Role: "system", Content: whySystem}, {Role: "user", Content: b.String()}}
}

var (
	whyLink = regexp.MustCompile(`(?i)\b(https?://|www\.)\S+`)
	// whySecret is a line that may hold a key or a token: a known prefix, a
	// key block, or a long run of letters and digits mixed.
	whySecret = regexp.MustCompile(`(?i)-----BEGIN|\b(gh[pousr]_|github_pat_|xox[abposr]-|sk-|AKIA|ASIA)[A-Za-z0-9_-]{8,}|\b(password|passwd|lösenord|token|secret|api[ _-]?key)\s*[:=]`)
	whyLong   = regexp.MustCompile(`[A-Za-z0-9_+/=-]{24,}`)
)

// looksSecret is whether a line may hold a secret; such a line is dropped.
func looksSecret(line string) bool {
	if whySecret.MatchString(line) {
		return true
	}
	for _, run := range whyLong.FindAllString(line, -1) {
		if strings.IndexFunc(run, unicode.IsDigit) >= 0 && strings.IndexFunc(run, unicode.IsLetter) >= 0 {
			return true
		}
	}
	return false
}

// ParseWhy reads the local model's picks as sources: each number once, in
// range, its line cut to one sentence without links, at most WhyMaxSources.
// A line that may hold a secret is dropped.
func ParseWhy(raw string, found []whyFound) ([]WhySource, error) {
	var answer struct {
		Picked []struct {
			N       int    `json:"n"`
			Summary string `json:"summary"`
		} `json:"picked"`
	}
	if err := json.Unmarshal([]byte(jsonObject(raw)), &answer); err != nil {
		return nil, err
	}
	used := map[int]bool{}
	out := []WhySource{}
	for _, pick := range answer.Picked {
		if pick.N < 1 || pick.N > len(found) || used[pick.N] {
			continue
		}
		summary := OneLine(whyLink.ReplaceAllString(pick.Summary, ""), WhySummaryMax)
		if summary == "" || looksSecret(summary) {
			continue
		}
		used[pick.N] = true
		f := found[pick.N-1]
		out = append(out, WhySource{Kind: f.Kind, At: f.At, Summary: summary, Ref: f.Ref})
		if len(out) == WhyMaxSources {
			break
		}
	}
	return out, nil
}

// Why is a goal's pack. Every way it can fail gives an empty pack.
func (n *Norn) Why(ctx context.Context, goal, goalContext string, now time.Time) WhyPack {
	empty := WhyPack{Sources: []WhySource{}}
	words := WhyWords(goal)
	if len(words) == 0 {
		return empty
	}
	if n.NamesOff(ctx) {
		// Names would go as they are (PUT /v1/settings/send-names): send nothing instead.
		n.logf("why: names are not being swapped right now, so no pack")
		return empty
	}
	var found []whyFound
	found = append(found, n.whyInbox(now)...)
	found = append(found, n.whyMail(ctx, words)...)
	found = append(found, n.whyNotes(words, now)...)
	found = append(found, n.whyRewind(ctx, now)...)
	best := BestFound(found, words)
	if len(best) == 0 {
		return empty
	}
	think := false
	raw, err := n.LLM.Chat(ctx, WhyMessages(goal, goalContext, best, n.Zone), llm.Options{Purpose: "goal why", Model: n.Config.AskModel,
		Temperature: 0, TopP: 1, MaxTokens: 700, Format: whyFormat, Think: &think, KeepAlive: n.Config.DispatchKeepAlive})
	if err != nil {
		n.logf("why: the local model did not answer: %v", err)
		return empty
	}
	sources, err := ParseWhy(raw, best)
	if err != nil {
		n.logf("why: the local model's answer was not JSON: %v", err)
		return empty
	}
	if len(sources) == 0 {
		return empty
	}
	// One text, one look for names: every line is one line.
	lines := make([]string, len(sources))
	for i, s := range sources {
		lines[i] = s.Summary
	}
	swapped, _, err := n.pseudonymize(ctx, "", nil, strings.Join(lines, "\n"))
	if err != nil {
		n.logf("why: names could not be swapped, so no pack: %v", err)
		return empty
	}
	out := strings.Split(swapped[0], "\n")
	if len(out) != len(sources) {
		return empty
	}
	for i := range sources {
		sources[i].Summary = strings.TrimSpace(out[i])
	}
	sources = slices.DeleteFunc(sources, func(s WhySource) bool { return s.Summary == "" })
	return WhyPack{Sources: sources}
}

// --- the route ---------------------------------------------------------------------

func (n *Norn) mountWhy(p *part.Part) {
	p.Handle("POST /v1/why", n.why)
}

func (n *Norn) why(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Goal    string   `json:"goal"`
		Context string   `json:"context"`
		Repos   []string `json:"repos"`
		GoalID  int64    `json:"goalId"`
	}
	if err := part.Decode(r, &body); err != nil || strings.TrimSpace(body.Goal) == "" || len([]rune(body.Goal)) > 300 || len(body.Context) > 20_000 {
		part.Error(w, http.StatusBadRequest, "`goal` is the goal's title, at most 300 characters; `context` at most 20000")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), WhyBudget)
	defer cancel()
	pack := n.Why(ctx, strings.TrimSpace(body.Goal), body.Context, time.Now())
	n.logf("why: goal %d: %d sources", body.GoalID, len(pack.Sources))
	part.JSON(w, http.StatusOK, pack)
}
