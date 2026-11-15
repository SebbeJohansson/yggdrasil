package norn

// The one-line summary at the top of each inbox section, phrased by a small
// model and checked: a sentence that names someone the inbox doesn't hold is
// thrown away for the plain count. Ported from my-agent's
// tasks/inbox/summary.ts and tasks/inboxSummary.ts.

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"

	"yggdrasil/internal/llm"
)

// SummariseTop is how many items the summary reads.
const SummariseTop = 3

var examplePeople = []string{"Anna Berg", "Bo Lind"}

func summarySystem(scope string) string {
	return strings.Join([]string{
		"You summarise a " + scope + " inbox for a small desktop widget.",
		"Reply with ONE sentence, under 120 characters, plain text, no list, no preamble.",
		"Name the person and what they want. Use only what is in the input:",
		"never invent a time, a name, a deadline or a topic that is not there.",
		"If nothing needs action, say so plainly.",
	}, " ")
}

// SummarySystemPrompt is the system prompt, with the profile as background
// that may set language and tone but never names.
func SummarySystemPrompt(profile, scope string) string {
	if profile == "" {
		return summarySystem(scope)
	}
	return strings.Join([]string{summarySystem(scope), "",
		"About the reader, to choose language, tone and what to lead with.",
		"This is background only: never name a person, project or fact from it",
		"unless the input mentions it too.", "", profile}, "\n")
}

func shortAge(d time.Duration) string {
	minutes := int(math.Max(0, math.Round(d.Minutes())))
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}
	hours := int(math.Round(float64(minutes) / 60))
	if hours < 48 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dd", int(math.Round(float64(hours)/24)))
}

// ItemLine is one item as the model reads it.
func ItemLine(entry Ranked, now time.Time) string {
	marks := append([]string{sourceMark[entry.Item.Source], shortAge(time.Duration(now.UnixMilli()-entry.Item.At) * time.Millisecond)}, entry.Reasons...)
	return fmt.Sprintf("- %s in %s (%s): %s", entry.Item.From, entry.Item.Subject, strings.Join(marks, ", "), entry.Item.Preview)
}

// SummaryMessages is the prompt: one worked example, then the items.
func SummaryMessages(top []Ranked, now time.Time, profile, scope string) []llm.Message {
	lines := make([]string, len(top))
	for i, entry := range top {
		lines[i] = ItemLine(entry, now)
	}
	return []llm.Message{
		{Role: "system", Content: SummarySystemPrompt(profile, scope)},
		{Role: "user", Content: "- Anna Berg in Q3 numbers (ol, 20m, sent to you, asks a question): Can you send these before 14:00?\n- Bo Lind in #release (sl, 2h, mentions you): can you review the tag?"},
		{Role: "assistant", Content: "Anna needs the Q3 numbers before 14:00, and Bo wants you to review the release tag."},
		{Role: "user", Content: strings.Join(lines, "\n")},
	}
}

var (
	preamble    = regexp.MustCompile(`(?i)^(here('| i)s|sure|of course|okay|ok|certainly|summary|in summary|based on)\b`)
	bullet      = regexp.MustCompile(`^[-*•]\s*`)
	wrapped     = regexp.MustCompile(`^"([\s\S]*)"$`)
	summaryWord = regexp.MustCompile(`[\p{L}\x{2019}'-]+`)
	sentenceEnd = regexp.MustCompile(`([.!?])\s+`)
)

// CleanSummary is the model's reply as one sentence, or empty when there is
// nothing usable in it.
func CleanSummary(raw string) string {
	var kept []string
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !preamble.MatchString(line) {
			kept = append(kept, line)
		}
	}
	text := strings.TrimSpace(wrapped.ReplaceAllString(bullet.ReplaceAllString(whitespace.ReplaceAllString(strings.Join(kept, " "), " "), ""), "$1"))
	if text == "" {
		return ""
	}
	if runes := []rune(text); len(runes) > 160 {
		head := string(runes[:160])
		if stop := strings.LastIndex(head, ". "); stop > 40 {
			return head[:stop+1]
		}
		return strings.TrimRight(string(runes[:159]), " ") + "…"
	}
	return text
}

// Pressing is what the summary is about: urgent, or flagged by its sender.
func Pressing(entries []Ranked) []Ranked {
	var pressing []Ranked
	for _, e := range entries {
		if e.Band == "urgent" || e.Item.Important {
			pressing = append(pressing, e)
		}
	}
	return pressing
}

func worthPhrasing(top []Ranked) bool {
	for _, e := range top {
		if e.Band == "urgent" || e.Band == "notable" {
			return true
		}
	}
	return false
}

// PlainSummary is the sentence without a model: who is first, and how many more.
func PlainSummary(top []Ranked, total int) string {
	switch {
	case total == 0:
		return "nothing unread"
	case len(top) == 0:
		return fmt.Sprintf("%d unread, nothing urgent", total)
	}
	who := top[0].Item.From + " in " + top[0].Item.Subject
	if rest := total - 1; rest > 0 {
		return fmt.Sprintf("%s, and %d more", who, rest)
	}
	return who
}

var notAName = map[string]bool{}

func init() {
	for _, word := range strings.Fields(`monday tuesday wednesday thursday friday saturday sunday
		måndag tisdag onsdag torsdag fredag lördag söndag
		january february march april may june july august september october november december
		januari februari mars maj juni juli augusti oktober
		outlook teams slack gmail inbox email today tomorrow`) {
		notAName[word] = true
	}
}

// InventedNames is the capitalised words in a sentence that the items it
// summarises don't hold, and the worked example's names wherever they appear.
func InventedNames(text string, top []Ranked) []string {
	known := map[string]bool{}
	for _, e := range top {
		for _, word := range summaryWord.FindAllString(e.Item.From+" "+e.Item.Subject+" "+e.Item.Preview, -1) {
			known[strings.ToLower(word)] = true
		}
	}
	examples := map[string]bool{}
	for _, person := range examplePeople {
		for _, word := range summaryWord.FindAllString(person, -1) {
			examples[strings.ToLower(word)] = true
		}
	}
	var found []string
	for _, sentence := range strings.Split(sentenceEnd.ReplaceAllString(text, "$1\n"), "\n") {
		for index, word := range summaryWord.FindAllString(sentence, -1) {
			lower := strings.ToLower(word)
			if known[lower] {
				continue
			}
			if examples[lower] {
				found = append(found, word)
				continue
			}
			runes := []rune(word)
			if index > 0 && len(runes) > 1 && unicode.IsUpper(runes[0]) && unicode.IsLower(runes[1]) && !notAName[lower] {
				found = append(found, word)
			}
		}
	}
	return found
}

// Summary is one section's sentence, and whether a model phrased it.
type Summary struct {
	Text      *string `json:"text"`
	FromModel bool    `json:"fromModel"`
}

// Summarise phrases the top items, or falls back to the plain count when
// there is nothing worth phrasing, the model fails, or it names someone the
// inbox doesn't hold.
func (n *Norn) Summarise(ctx context.Context, top []Ranked, total int, now time.Time, scope string) (string, bool) {
	fallback := PlainSummary(top, total)
	if len(top) == 0 || !worthPhrasing(top) {
		return fallback, false
	}
	shown := top[:min(len(top), SummariseTop)]
	raw, err := n.LLM.Chat(ctx, SummaryMessages(shown, now, LoadProfile(n.Config.ProfilePath), scope),
		llm.Options{Purpose: "summary", Model: n.Config.SummaryModel, Temperature: 0.2, TopP: 0.9, MaxTokens: 80})
	if err != nil {
		n.logf("no summary this time; sending the counts instead: %v", err)
		return fallback, false
	}
	text := CleanSummary(raw)
	if text == "" {
		return fallback, false
	}
	if invented := InventedNames(text, shown); len(invented) > 0 {
		n.logf("the model named someone the inbox does not have (%v); sending the counts instead", invented)
		return fallback, false
	}
	return text, true
}
