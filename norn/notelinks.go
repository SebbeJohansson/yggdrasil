package norn

// Link suggestions for meeting notes: a note in your vault whose title is
// named in the text ("Northwind" when a note called Northwind exists) can be
// wrapped as [[Northwind]]. Norn only suggests; a click applies one.
//
// Only titles are matched against the text: the text goes nowhere, and no
// note's content is read.

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"yggdrasil/internal/part"
)

const (
	// maxLinkNotes is how many notes in the vault are considered as targets.
	maxLinkNotes = 20000
	// minLinkTitle is the shortest title that is matched: "AI" would be everywhere.
	minLinkTitle = 3
)

// LinkTarget is a note the text can link to.
type LinkTarget struct {
	// Target is what goes inside [[ ]]: the title, or the path when two notes share a title.
	Target string `json:"target"`
	Title  string `json:"-"`
	Link   string `json:"link"`
}

// LinkSuggestion is one note named in a text, not linked yet.
type LinkSuggestion struct {
	LinkTarget
	// Matched is the first match as written in the text.
	Matched string `json:"matched"`
	// Count is how many unlinked matches there are.
	Count int `json:"count"`
	// Wiki is what the first match becomes.
	Wiki string `json:"wiki"`
}

// LinkTargets is every note in the vault a text can link to, longest title
// first. Dated meeting notes are left out, as are hidden folders.
func (v *Vault) LinkTargets(now time.Time) []LinkTarget {
	if now.Sub(v.linksScanned) < vaultScanEvery && v.links != nil {
		return v.links
	}
	byTitle := map[string][]string{}
	count := 0
	_ = filepath.WalkDir(v.Dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != v.Dir && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
			return nil
		}
		if count++; count > maxLinkNotes {
			return filepath.SkipAll
		}
		title := strings.TrimSuffix(name, ".md")
		if titleDate.MatchString(title) || utf8.RuneCountInString(title) < minLinkTitle || strings.IndexFunc(title, unicode.IsLetter) < 0 {
			return nil
		}
		if rel, err := filepath.Rel(v.Dir, path); err == nil {
			byTitle[title] = append(byTitle[title], filepath.ToSlash(rel))
		}
		return nil
	})
	targets := []LinkTarget{}
	for title, paths := range byTitle {
		sort.Strings(paths)
		for _, path := range paths {
			target := title
			if len(paths) > 1 {
				target = strings.TrimSuffix(path, ".md")
			}
			targets = append(targets, LinkTarget{Target: target, Title: title, Link: v.link(path)})
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if li, lj := utf8.RuneCountInString(targets[i].Title), utf8.RuneCountInString(targets[j].Title); li != lj {
			return li > lj
		}
		return targets[i].Target < targets[j].Target
	})
	v.links, v.linksScanned = targets, now
	return targets
}

// protected is what is never linked: front matter, code, existing links,
// URLs and comments.
var protected = regexp.MustCompile("(?s)\\A---\\n.*?\\n---\\n|```.*?```|`[^`\\n]*`|\\[\\[.*?\\]\\]|!?\\[[^\\]\\n]*\\]\\([^)\\n]*\\)|<!--.*?-->|https?://[^\\s)>\\]]+|obsidian://[^\\s)>\\]]+")

type span struct{ from, to int }

func spansOverlap(spans []span, from, to int) bool {
	for _, s := range spans {
		if from < s.to && s.from < to {
			return true
		}
	}
	return false
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }

type linkHit struct {
	span
	target LinkTarget
}

// findLinks is every place in text where a target's title stands as a whole
// word, outside protected text, longest title first and never overlapping.
// With only set, just that target.
func findLinks(text string, targets []LinkTarget, only string) []linkHit {
	var taken []span
	for _, loc := range protected.FindAllStringIndex(text, -1) {
		taken = append(taken, span{loc[0], loc[1]})
	}
	var hits []linkHit
	for _, target := range targets {
		if only != "" && target.Target != only {
			continue
		}
		pattern := regexp.MustCompile("(?i)" + regexp.QuoteMeta(target.Title))
		for _, loc := range pattern.FindAllStringIndex(text, -1) {
			from, to := loc[0], loc[1]
			if before, _ := utf8.DecodeLastRuneInString(text[:from]); from > 0 && isWordRune(before) {
				continue
			}
			if after, _ := utf8.DecodeRuneInString(text[to:]); to < len(text) && isWordRune(after) {
				continue
			}
			if spansOverlap(taken, from, to) {
				continue
			}
			taken = append(taken, span{from, to})
			hits = append(hits, linkHit{span{from, to}, target})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].from < hits[j].from })
	return hits
}

func wikiLink(target, matched string) string {
	if matched == target {
		return "[[" + target + "]]"
	}
	return "[[" + target + "|" + matched + "]]"
}

// SuggestLinks is the notes named in text that are not linked yet, most
// mentioned first, then in the order they appear.
func SuggestLinks(text string, targets []LinkTarget) []LinkSuggestion {
	var out []LinkSuggestion
	index := map[string]int{}
	for _, hit := range findLinks(text, targets, "") {
		matched := text[hit.from:hit.to]
		at, seen := index[hit.target.Target]
		if !seen {
			at = len(out)
			index[hit.target.Target] = at
			out = append(out, LinkSuggestion{LinkTarget: hit.target, Matched: matched, Wiki: wikiLink(hit.target.Target, matched)})
		}
		out[at].Count++
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

// ApplyLink wraps every unlinked match of target in text.
func ApplyLink(text string, targets []LinkTarget, target string) (string, int) {
	hits := findLinks(text, targets, target)
	var b strings.Builder
	last := 0
	for _, hit := range hits {
		b.WriteString(text[last:hit.from])
		b.WriteString(wikiLink(hit.target.Target, text[hit.from:hit.to]))
		last = hit.to
	}
	b.WriteString(text[last:])
	return b.String(), len(hits)
}

// LinkInMeetingNote wraps the matches of target in the meeting's saved note.
func (v *Vault) LinkInMeetingNote(meeting Meeting, target string, zone *time.Location, now time.Time) (VaultNote, int, bool, error) {
	day := time.UnixMilli(meeting.Start).In(zone).Format("2006-01-02")
	note := v.MeetingNote(meeting.Subject, day, now)
	if note.Path == "" {
		return VaultNote{}, 0, false, nil
	}
	full := filepath.Join(v.Dir, filepath.FromSlash(note.Path))
	before, err := os.ReadFile(full)
	if err != nil {
		return VaultNote{}, 0, false, err
	}
	after, changed := ApplyLink(string(before), v.LinkTargets(now), target)
	if changed == 0 {
		return note, 0, true, nil
	}
	return note, changed, true, os.WriteFile(full, []byte(after), 0o644)
}

// --- routes ---------------------------------------------------------------------

func (n *Norn) mountNoteLinks(p *part.Part) {
	p.Handle("POST /v1/notes/links/suggest", n.suggestNoteLinks)
	p.Handle("POST /v1/notes/links/apply", n.applyNoteLink)
	p.Handle("POST /v1/notes/meeting/link", n.linkMeetingNote)
}

func (n *Norn) suggestNoteLinks(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := part.Decode(r, &body); err != nil || len([]rune(body.Text)) > maxLiveNote {
		part.Error(w, http.StatusBadRequest, "`text` is the note, at most 20000 characters")
		return
	}
	if !n.Vault.On() {
		part.JSON(w, http.StatusOK, map[string]any{"on": false, "suggestions": []LinkSuggestion{}})
		return
	}
	n.vaultMu.Lock()
	targets := n.Vault.LinkTargets(time.Now())
	n.vaultMu.Unlock()
	suggestions := SuggestLinks(body.Text, targets)
	if suggestions == nil {
		suggestions = []LinkSuggestion{}
	}
	part.JSON(w, http.StatusOK, map[string]any{"on": true, "suggestions": suggestions})
}

func (n *Norn) knownTarget(w http.ResponseWriter, target string) ([]LinkTarget, bool) {
	if !n.Vault.On() {
		part.Error(w, http.StatusServiceUnavailable, errNoVault.Error())
		return nil, false
	}
	targets := n.Vault.LinkTargets(time.Now())
	for _, t := range targets {
		if t.Target == target {
			return targets, true
		}
	}
	part.Error(w, http.StatusNotFound, "no note in the vault is called that")
	return nil, false
}

func (n *Norn) applyNoteLink(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text   string `json:"text"`
		Target string `json:"target"`
	}
	if err := part.Decode(r, &body); err != nil || strings.TrimSpace(body.Target) == "" || len([]rune(body.Text)) > maxLiveNote {
		part.Error(w, http.StatusBadRequest, "`text` (at most 20000 characters) and `target` are needed")
		return
	}
	n.vaultMu.Lock()
	targets, ok := n.knownTarget(w, body.Target)
	n.vaultMu.Unlock()
	if !ok {
		return
	}
	text, changed := ApplyLink(body.Text, targets, body.Target)
	part.JSON(w, http.StatusOK, map[string]any{"text": text, "changed": changed})
}

func (n *Norn) linkMeetingNote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Meeting
		Target string `json:"target"`
	}
	if err := part.Decode(r, &body); err != nil || strings.TrimSpace(body.Subject) == "" || body.Start <= 0 || strings.TrimSpace(body.Target) == "" {
		part.Error(w, http.StatusBadRequest, "`subject`, `start` (epoch ms) and `target` are needed")
		return
	}
	n.vaultMu.Lock()
	defer n.vaultMu.Unlock()
	if _, ok := n.knownTarget(w, body.Target); !ok {
		return
	}
	note, changed, found, err := n.Vault.LinkInMeetingNote(body.Meeting, body.Target, n.Zone, time.Now())
	if err != nil {
		n.fail(w, err)
		return
	}
	if !found {
		part.Error(w, http.StatusNotFound, "no note for that meeting yet")
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"note": note, "changed": changed})
}
