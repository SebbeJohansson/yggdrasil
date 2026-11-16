package norn

// Your Obsidian vault, for meeting notes both ways: a note you write on the
// dashboard after a meeting goes into the vault (into the note you already
// wrote for that meeting that day, or a new one beside your others), and a
// brief for a meeting reads your earlier notes with the same people or on the
// same subject.
//
// The vault holds private notes. What is read from it goes to the local model
// only (a /local question), never to Sindri or Claude, and the routes give
// titles and links, never note text.

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"yggdrasil/internal/part"
)

const (
	// vaultScanEvery is how long a list of the vault's notes is used before it is read again.
	vaultScanEvery = time.Minute
	// noteExcerptChars is how much of an earlier note a brief reads.
	noteExcerptChars = 1200
	// briefNotes is how many earlier notes a brief reads.
	briefNotes = 3
	// afterHeading is where a dashboard note goes in a meeting's note.
	afterHeading = "## Efter mötet"
)

// Vault is an Obsidian vault on disk. A nil or zero Vault is off.
type Vault struct {
	// Dir is the vault's folder; Name is what Obsidian calls it, for links.
	Dir, Name string

	scanned time.Time
	notes   []VaultNote
}

// VaultNote is one note, by its path in the vault.
type VaultNote struct {
	// Path is relative to the vault, with / and the .md.
	Path  string `json:"path"`
	Title string `json:"title"`
	// Date is the YYYY-MM-DD at the end of the title, when there is one.
	Date string `json:"date,omitempty"`
	// Link opens it in Obsidian.
	Link string `json:"link"`
	// Why is what it shares with the meeting: a person's name or a subject word.
	Why []string `json:"why,omitempty"`
}

// OpenVault is the vault at dir, or nil when dir is not one (no .obsidian).
func OpenVault(dir, name string) *Vault {
	if dir == "" {
		return nil
	}
	if info, err := os.Stat(filepath.Join(dir, ".obsidian")); err != nil || !info.IsDir() {
		return nil
	}
	if name == "" {
		name = filepath.Base(dir)
	}
	return &Vault{Dir: dir, Name: name}
}

// On is whether there is a vault.
func (v *Vault) On() bool { return v != nil && v.Dir != "" }

var titleDate = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})\s*$`)

// The folders meeting notes are in: the vault's root (where new notes land)
// and Notes/Meetings. Nothing else is listed.
var meetingFolders = []string{".", "Notes/Meetings"}

// Notes is the meeting notes in the vault, newest date first.
func (v *Vault) Notes(now time.Time) []VaultNote {
	if now.Sub(v.scanned) < vaultScanEvery && v.notes != nil {
		return v.notes
	}
	notes := []VaultNote{}
	for _, folder := range meetingFolders {
		entries, err := os.ReadDir(filepath.Join(v.Dir, folder))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
				continue
			}
			path := name
			if folder != "." {
				path = folder + "/" + name
			}
			notes = append(notes, v.note(path))
		}
	}
	sort.SliceStable(notes, func(i, j int) bool { return notes[i].Date > notes[j].Date })
	v.notes, v.scanned = notes, now
	return notes
}

func (v *Vault) note(path string) VaultNote {
	title := strings.TrimSuffix(filepath.Base(path), ".md")
	note := VaultNote{Path: path, Title: title, Link: v.link(path)}
	if match := titleDate.FindStringSubmatch(title); match != nil {
		note.Date = match[1]
	}
	return note
}

// link is obsidian://open for a note.
func (v *Vault) link(path string) string {
	return "obsidian://open?vault=" + url.QueryEscape(v.Name) + "&file=" + url.QueryEscape(strings.TrimSuffix(path, ".md"))
}

var words = regexp.MustCompile(`[\p{L}\p{N}]+`)

// noteStop is words too common in your titles to tie a note to a meeting.
var noteStop = map[string]bool{
	"möte": true, "mötet": true, "meeting": true, "snack": true, "synk": true, "sync": true, "med": true, "with": true,
	"angående": true, "ang": true, "och": true, "and": true, "the": true, "för": true, "for": true, "inför": true, "efter": true,
	"om": true, "about": true, "notes": true, "weekly": true, "daily": true, "avstämning": true, "samtal": true, "kring": true,
}

func fold(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch r {
		case 'å', 'ä':
			r = 'a'
		case 'ö':
			r = 'o'
		case 'é', 'è':
			r = 'e'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// titleWords is a title's words worth matching: folded, four letters or more
// or an acronym (QBR, CS), no filler, no numbers.
func titleWords(text string) []string {
	var out []string
	for _, word := range words.FindAllString(text, -1) {
		lower := strings.ToLower(word)
		acronym := len([]rune(word)) >= 2 && strings.ToUpper(word) == word && strings.IndexFunc(word, unicode.IsLetter) >= 0
		if (len([]rune(word)) < 4 && !acronym) || noteStop[lower] || strings.IndexFunc(word, unicode.IsLetter) < 0 {
			continue
		}
		if folded := fold(lower); !slices.Contains(out, folded) {
			out = append(out, folded)
		}
	}
	return out
}

// firstNames is each person's first name, folded: your titles name people by
// their first name ("1on1 med Johan"). Addresses give their local part.
func firstNames(people []string) []string {
	var out []string
	for _, person := range people {
		if at := strings.IndexByte(person, '@'); at > 0 {
			person = strings.NewReplacer(".", " ", "_", " ").Replace(person[:at])
		}
		if comma := strings.IndexByte(person, ','); comma > 0 {
			person = person[comma+1:]
		}
		first := words.FindString(person)
		if len([]rune(first)) >= 3 {
			if folded := fold(strings.ToLower(first)); !slices.Contains(out, folded) {
				out = append(out, folded)
			}
		}
	}
	return out
}

// Related is the meeting notes naming one of `people` by first name or
// sharing a word with `subject`, newest first, at most `limit`. `except` is
// left out (the meeting's own note).
func (v *Vault) Related(people []string, subject, except string, limit int, now time.Time) []VaultNote {
	names := firstNames(people)
	subjectWords := titleWords(subject)
	found := []VaultNote{}
	for _, note := range v.Notes(now) {
		if note.Path == except {
			continue
		}
		title := titleWords(note.Title)
		var why []string
		for _, name := range names {
			if slices.Contains(title, name) || slices.ContainsFunc(words.FindAllString(fold(strings.ToLower(note.Title)), -1), func(w string) bool { return w == name }) {
				why = append(why, name)
			}
		}
		for _, word := range subjectWords {
			if slices.Contains(title, word) && !slices.Contains(why, word) {
				why = append(why, word)
			}
		}
		if len(why) > 0 {
			note.Why = why
			found = append(found, note)
		}
	}
	// Most shared first, then newest: "QBR CS sittning 6" before "Sittning budgetrevision".
	sort.SliceStable(found, func(i, j int) bool { return len(found[i].Why) > len(found[j].Why) })
	return found[:min(limit, len(found))]
}

// MeetingNote is the note you wrote for a meeting on its day: dated that day,
// sharing a word with its subject. Empty when there is none.
func (v *Vault) MeetingNote(subject string, day string, now time.Time) VaultNote {
	want := titleWords(subject)
	best, bestShared := VaultNote{}, 0
	for _, note := range v.Notes(now) {
		if note.Date != day {
			continue
		}
		shared := 0
		for _, word := range titleWords(note.Title) {
			if slices.Contains(want, word) {
				shared++
			}
		}
		if shared > bestShared {
			best, bestShared = note, shared
		}
	}
	return best
}

// Excerpt is the start of a note, front matter left out.
func (v *Vault) Excerpt(path string, chars int) (string, error) {
	file, err := os.Open(filepath.Join(v.Dir, filepath.FromSlash(path)))
	if err != nil {
		return "", err
	}
	defer file.Close()
	var b strings.Builder
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	front := false
	for line := 0; scanner.Scan() && b.Len() < chars*2; line++ {
		text := scanner.Text()
		if line == 0 && text == "---" {
			front = true
			continue
		}
		if front {
			front = text != "---"
			continue
		}
		if strings.TrimSpace(text) != "" {
			b.WriteString(text)
			b.WriteByte('\n')
		}
	}
	return clipText(strings.TrimSpace(b.String()), chars), scanner.Err()
}

// unsafeName is what a file name in the vault may not hold, on Windows or in Obsidian.
var unsafeName = regexp.MustCompile(`[\\/:*?"<>|#^\[\]]+`)

// NoteName is a new meeting note's file name, as your notes are named:
// "<subject> - 2026-11-17.md".
func NoteName(subject, day string) string {
	name := strings.Join(strings.Fields(unsafeName.ReplaceAllString(subject, " ")), " ")
	if name == "" {
		name = "Möte"
	}
	if len([]rune(name)) > 120 {
		name = string([]rune(name)[:120])
	}
	return name + " - " + day + ".md"
}

// Meeting is what a dashboard note is about.
type Meeting struct {
	Subject   string   `json:"subject"`
	Start     int64    `json:"start"`
	Attendees []string `json:"attendees,omitempty"`
}

// AddMeetingNote writes `text` under "Efter mötet" in the meeting's note for
// its day, making the note when you have not written one. Your own text in
// the note is never changed: the new lines go at the end of that section.
func (v *Vault) AddMeetingNote(meeting Meeting, text string, zone *time.Location, now time.Time) (VaultNote, bool, error) {
	day := time.UnixMilli(meeting.Start).In(zone).Format("2006-01-02")
	note := v.MeetingNote(meeting.Subject, day, now)
	lines := bullets(text)
	if note.Path == "" {
		note = v.note(NoteName(meeting.Subject, day))
		var b strings.Builder
		fmt.Fprintf(&b, "# %s\n\n", meeting.Subject)
		if len(meeting.Attendees) > 0 {
			fmt.Fprintf(&b, "Deltagare: %s\n\n", strings.Join(meeting.Attendees, ", "))
		}
		b.WriteString(afterHeading + "\n\n" + lines + "\n")
		full := filepath.Join(v.Dir, filepath.FromSlash(note.Path))
		file, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return VaultNote{}, false, err
		}
		_, err = file.WriteString(b.String())
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		v.scanned = time.Time{}
		return note, true, err
	}
	full := filepath.Join(v.Dir, filepath.FromSlash(note.Path))
	before, err := os.ReadFile(full)
	if err != nil {
		return VaultNote{}, false, err
	}
	after := appendUnder(string(before), afterHeading, lines)
	return note, false, os.WriteFile(full, []byte(after), 0o644)
}

// bullets is the text as list items, one per line; lines that are already
// list items or headings are kept as they are.
func bullets(text string) string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		line = strings.TrimRight(line, " \t")
		switch trimmed := strings.TrimSpace(line); {
		case trimmed == "":
			continue
		case strings.HasPrefix(trimmed, "- "), strings.HasPrefix(trimmed, "* "), strings.HasPrefix(trimmed, "#"):
			out = append(out, line)
		default:
			out = append(out, "- "+trimmed)
		}
	}
	return strings.Join(out, "\n")
}

// headingLevel is a Markdown heading's level, 0 for a line that is not one.
func headingLevel(line string) int {
	level := len(line) - len(strings.TrimLeft(line, "#"))
	if level == 0 || level > 6 || !strings.HasPrefix(line[level:], " ") {
		return 0
	}
	return level
}

// appendUnder adds lines at the end of the `heading` section (up to the next
// heading of its level or higher), or a new section at the end of the note
// when it has none.
func appendUnder(note, heading, lines string) string {
	all := strings.Split(strings.TrimRight(note, "\n"), "\n")
	at := slices.IndexFunc(all, func(line string) bool { return strings.TrimSpace(line) == heading })
	if at < 0 {
		return strings.TrimRight(note, "\n") + "\n\n" + heading + "\n\n" + lines + "\n"
	}
	level, end := headingLevel(heading), len(all)
	for i := at + 1; i < len(all); i++ {
		if l := headingLevel(all[i]); l > 0 && l <= level {
			end = i
			break
		}
	}
	section := slices.Clone(all[:end])
	for len(section) > at+1 && strings.TrimSpace(section[len(section)-1]) == "" {
		section = section[:len(section)-1]
	}
	if len(section) == at+1 {
		section = append(section, "")
	}
	out := append(section, strings.Split(lines, "\n")...)
	if end < len(all) {
		out = append(append(out, ""), all[end:]...)
	}
	return strings.Join(out, "\n") + "\n"
}

// --- briefs ---------------------------------------------------------------------

var briefAsk = regexp.MustCompile(`(?i)^brief me for my meeting "([^"]+)"`)

// briefNoteLines is your earlier notes for the meeting a brief is asked about:
// who is invited (from the calendar), then each related note's start. Nil
// when the question is not a brief or there is no vault.
func (n *Norn) briefNoteLines(question string, now time.Time) []string {
	match := briefAsk.FindStringSubmatch(strings.TrimSpace(question))
	if match == nil || !n.Vault.On() {
		return nil
	}
	n.vaultMu.Lock()
	defer n.vaultMu.Unlock()
	subject := match[1]
	meeting := n.findMeeting(subject, now)
	var lines []string
	if len(meeting.Attendees) > 0 {
		lines = append(lines, "Invited: "+strings.Join(meeting.Attendees, ", "))
	}
	for _, note := range n.Vault.Related(meeting.Attendees, subject, "", briefNotes, now) {
		text, err := n.Vault.Excerpt(note.Path, noteExcerptChars)
		if err != nil || text == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("Note %q:\n%s", note.Title, text))
	}
	return lines
}

// findMeeting is the next or current meeting with that subject on either
// calendar; only the subject when there is none.
func (n *Norn) findMeeting(subject string, now time.Time) Meeting {
	found := Meeting{Subject: subject}
	best := int64(-1)
	for _, c := range []*Calendar{n.Accounts.Work, n.Accounts.Personal} {
		snapshot := c.Current(now)
		if snapshot == nil {
			continue
		}
		for _, event := range snapshot.Events {
			if !strings.EqualFold(strings.TrimSpace(event.Subject), strings.TrimSpace(subject)) {
				continue
			}
			distance := event.Start - now.UnixMilli()
			if distance < 0 {
				distance = -distance
			}
			if best < 0 || distance < best {
				best = distance
				found = Meeting{Subject: event.Subject, Start: event.Start, Attendees: meetingPeople(event)}
			}
		}
	}
	return found
}

// meetingPeople is the organizer and the attendees, each once.
func meetingPeople(event CalendarEvent) []string {
	var people []string
	for _, name := range append([]string{event.Organizer}, event.Attendees...) {
		if name != "" && !slices.Contains(people, name) {
			people = append(people, name)
		}
	}
	return people
}

// --- routes ---------------------------------------------------------------------

func (n *Norn) mountVault(p *part.Part) {
	p.Handle("GET /v1/notes/meeting", n.meetingNotes)
	p.Handle("POST /v1/notes/meeting", n.addMeetingNote)
}

var errNoVault = errors.New("no Obsidian vault: set OBSIDIAN_VAULT to its folder")

// meetingNotes is the meeting's own note for its day, when you wrote one, and
// the earlier notes that share a person or a word with it. Titles and links only.
func (n *Norn) meetingNotes(w http.ResponseWriter, r *http.Request) {
	if !n.Vault.On() {
		part.JSON(w, http.StatusOK, map[string]any{"on": false, "related": []VaultNote{}})
		return
	}
	query := r.URL.Query()
	subject := strings.TrimSpace(query.Get("subject"))
	if subject == "" || len([]rune(subject)) > 300 {
		part.Error(w, http.StatusBadRequest, "`subject` must be the meeting's subject")
		return
	}
	now := time.Now()
	n.vaultMu.Lock()
	defer n.vaultMu.Unlock()
	meeting := n.findMeeting(subject, now)
	if start := query.Get("start"); start != "" {
		if _, err := fmt.Sscan(start, &meeting.Start); err != nil {
			part.Error(w, http.StatusBadRequest, "`start` must be epoch ms")
			return
		}
	}
	answer := map[string]any{"on": true}
	own := VaultNote{}
	if meeting.Start != 0 {
		own = n.Vault.MeetingNote(meeting.Subject, time.UnixMilli(meeting.Start).In(n.Zone).Format("2006-01-02"), now)
		if own.Path != "" {
			answer["note"] = own
		}
	}
	answer["related"] = n.Vault.Related(meeting.Attendees, meeting.Subject, own.Path, 5, now)
	part.JSON(w, http.StatusOK, answer)
}

func (n *Norn) addMeetingNote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Meeting
		Text string `json:"text"`
	}
	if err := part.Decode(r, &body); err != nil || strings.TrimSpace(body.Subject) == "" || body.Start <= 0 {
		part.Error(w, http.StatusBadRequest, "`subject` and `start` (epoch ms) are the meeting's")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" || len([]rune(text)) > 4000 {
		part.Error(w, http.StatusBadRequest, "`text` must be the note, at most 4000 characters")
		return
	}
	if !n.Vault.On() {
		part.Error(w, http.StatusServiceUnavailable, errNoVault.Error())
		return
	}
	now := time.Now()
	n.vaultMu.Lock()
	defer n.vaultMu.Unlock()
	if len(body.Attendees) == 0 {
		body.Attendees = n.findMeeting(body.Subject, now).Attendees
	}
	note, made, err := n.Vault.AddMeetingNote(body.Meeting, text, n.Zone, now)
	if err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusCreated, map[string]any{"note": note, "made": made})
}
