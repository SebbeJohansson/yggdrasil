package norn

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"yggdrasil/internal/llm"
)

func testVault(t *testing.T, notes map[string]string) *Vault {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{".obsidian", "Notes/Meetings"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, text := range notes {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(path)), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return OpenVault(dir, "Second Brain")
}

func TestOpenVaultNeedsDotObsidian(t *testing.T) {
	if OpenVault(t.TempDir(), "") != nil || OpenVault("", "") != nil {
		t.Fatal("a folder without .obsidian opened")
	}
}

func TestRelatedNotesShareAFirstNameOrASubjectWord(t *testing.T) {
	v := testVault(t, map[string]string{
		"1on1 med johan - 2026-10-07.md":      "",
		"Snack med Linnea om QBR CS - 2026-10-15.md": "",
		"QBR CS sittning 6 - 2026-11-05.md":         "",
		"Notes/Meetings/Retro - 2024-11-08.md":      "",
		"Packlista Japan 2026.md":                   "",
	})
	now := time.Now()
	got := v.Related([]string{"Johan Wikström", "linnea@x.se"}, "QBR CS sittning 7", "", 5, now)
	var titles []string
	for _, note := range got {
		titles = append(titles, note.Title)
	}
	if strings.Join(titles, " | ") != "QBR CS sittning 6 - 2026-11-05 | Snack med Linnea om QBR CS - 2026-10-15 | 1on1 med johan - 2026-10-07" {
		t.Fatalf("got %v", titles)
	}
	if !strings.HasPrefix(got[0].Link, "obsidian://open?vault=Second%20Brain&file=QBR%20CS%20sittning%206") {
		t.Fatalf("link %s", got[0].Link)
	}
}

func TestAMeetingNoteGoesIntoTheOneYouWroteThatDayOrANewOne(t *testing.T) {
	zone, _ := time.LoadLocation("Europe/Stockholm")
	start := time.Date(2026, 11, 17, 10, 0, 0, 0, zone).UnixMilli()
	v := testVault(t, map[string]string{
		"Sprint review - 2026-11-17.md": "# Sprint review\n\n- demo gick bra\n\n## Frågor\n\n- vem äger X?\n",
	})
	now := time.Now()
	note, made, err := v.AddMeetingNote(Meeting{Subject: "Sprint review", Start: start}, "Följ upp med Kim\n- boka nästa", zone, now)
	if err != nil || made || note.Path != "Sprint review - 2026-11-17.md" {
		t.Fatalf("%v %v %+v", err, made, note)
	}
	_, _, _ = v.AddMeetingNote(Meeting{Subject: "Sprint review", Start: start}, "en sak till", zone, now)
	text, _ := os.ReadFile(filepath.Join(v.Dir, note.Path))
	want := "# Sprint review\n\n- demo gick bra\n\n## Frågor\n\n- vem äger X?\n\n## Efter mötet\n\n- Följ upp med Kim\n- boka nästa\n- en sak till\n"
	if string(text) != want {
		t.Fatalf("got:\n%s", text)
	}

	note, made, err = v.AddMeetingNote(Meeting{Subject: "Budget: Q4?", Start: start, Attendees: []string{"Ada", "Bo"}}, "Siffrorna till fredag", zone, now)
	if err != nil || !made || note.Path != "Budget Q4 - 2026-11-17.md" {
		t.Fatalf("%v %v %+v", err, made, note)
	}
	text, _ = os.ReadFile(filepath.Join(v.Dir, note.Path))
	if string(text) != "Deltagare: Ada, Bo\n\n## Efter mötet\n\n- Siffrorna till fredag\n" {
		t.Fatalf("new note:\n%s", text)
	}
}

func TestAppendUnderKeepsWhatFollowsTheSection(t *testing.T) {
	got := appendUnder("# M\n\n## Efter mötet\n\n- a\n\n## Sen\n\ntext\n", afterHeading, "- b")
	if got != "# M\n\n## Efter mötet\n\n- a\n- b\n\n## Sen\n\ntext\n" {
		t.Fatalf("got:\n%s", got)
	}
}

func TestALocalBriefReadsYourNotesAndNeverLeavesForSindri(t *testing.T) {
	var mu sync.Mutex
	var prompts []string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []llm.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		prompts = append(prompts, body.Messages[0].Content)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"answer\": \"Ta med siffrorna.\", \"enough\": false}"}}]}`))
	}))
	defer model.Close()
	handedOver := false
	// Reading the agent list is fine; a POST is a hand-over.
	sindri := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			handedOver = true
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer sindri.Close()

	n, p := newNorn(t)
	n.LLM = &llm.Client{PrimaryURL: model.URL + "/v1"}
	n.Sindri.URL, n.Sindri.Token = sindri.URL, "t"
	n.Vault = testVault(t, map[string]string{"Budget med Kim - 2026-10-01.md": "# Budget\n\n- Kim vill ha Q4-siffror\n"})
	now := time.Now()
	n.Accounts.Work.Set(CalendarSnapshot{From: now.Add(-time.Hour).UnixMilli(), To: now.Add(24 * time.Hour).UnixMilli(), At: now.UnixMilli(),
		Events: []CalendarEvent{{ID: "b", Subject: "Budget", Start: now.Add(20 * time.Minute).UnixMilli(), End: now.Add(time.Hour).UnixMilli(), Organizer: "Kim Berg", Attendees: []string{"Kim Berg"}}}})

	code, body := call(t, p, "POST", "/v1/ask", map[string]string{"question": `/local Brief me for my meeting "Budget" at 10:20: who will be there and what I should bring. Invited: Kim Berg.`, "to": "norn"})
	if code != http.StatusOK || body.(map[string]any)["answer"] != "Ta med siffrorna." {
		t.Fatalf("%d %v", code, body)
	}
	if handedOver {
		t.Fatalf("a /local question went to Sindri: %v", body)
	}
	if len(prompts) == 0 || !strings.Contains(prompts[0], "Kim vill ha Q4-siffror") || !strings.Contains(prompts[0], "Invited: Kim Berg") {
		t.Fatalf("the brief did not read the note: %v", prompts)
	}
}

func TestMeetingNotesRoutes(t *testing.T) {
	n, p := newNorn(t)
	if code, body := call(t, p, "GET", "/v1/notes/meeting?subject=Budget", nil); code != http.StatusOK || body.(map[string]any)["on"] != false {
		t.Fatalf("no vault: %d %v", code, body)
	}
	if code, _ := call(t, p, "POST", "/v1/notes/meeting", map[string]any{"subject": "Budget", "start": 1, "text": "x"}); code != http.StatusServiceUnavailable {
		t.Fatalf("no vault write: %d", code)
	}
	n.Vault = testVault(t, map[string]string{"Budget med Kim - 2026-10-01.md": "secret text"})
	start := time.Now().UnixMilli()
	code, body := call(t, p, "POST", "/v1/notes/meeting", map[string]any{"subject": "Budget", "start": start, "text": "Boka uppföljning"})
	if code != http.StatusCreated || body.(map[string]any)["made"] != true {
		t.Fatalf("%d %v", code, body)
	}
	code, body = call(t, p, "GET", "/v1/notes/meeting?subject=Budget&start="+strings.TrimSpace(jsonNumber(start)), nil)
	answer := body.(map[string]any)
	if code != http.StatusOK || answer["note"] == nil || len(answer["related"].([]any)) != 1 {
		t.Fatalf("%d %v", code, body)
	}
	if raw, _ := json.Marshal(answer); strings.Contains(string(raw), "secret text") {
		t.Fatal("note text left Norn")
	}
}

func jsonNumber(n int64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

func TestOthersThanLeavesYouOut(t *testing.T) {
	got := othersThan([]string{"Alex Morgan Svensson", "Johan Berg", "me@example.com"}, []string{"alex morgan svensson", "ME@example.com", ""})
	if len(got) != 1 || got[0] != "Johan Berg" {
		t.Fatalf("got %v", got)
	}
}

func TestMeetingPeopleLeavesOutYouAsOrganizer(t *testing.T) {
	got := meetingPeople(CalendarEvent{Organizer: "Alex Morgan Svensson", Response: "organizer", Attendees: []string{"Johan Berg"}})
	if len(got) != 1 || got[0] != "Johan Berg" {
		t.Fatalf("got %v", got)
	}
}

func TestWithoutNamesDropsYourNameFromTheSubject(t *testing.T) {
	got := withoutNames("One-to-one Alex & Johan", []string{"Alex Morgan Svensson", "alex.svensson@example.com"})
	if strings.Contains(strings.ToLower(got), "dragon") || !strings.Contains(got, "Johan") {
		t.Fatalf("got %q", got)
	}
	if got := withoutNames("Sprint review", nil); got != "Sprint review" {
		t.Fatalf("got %q", got)
	}
}

func TestASecondNoteForAShortSubjectGoesIntoTheNoteNornMade(t *testing.T) {
	zone, _ := time.LoadLocation("Europe/Stockholm")
	start := time.Date(2026, 11, 17, 10, 0, 0, 0, zone).UnixMilli()
	v := testVault(t, nil)
	first, made, err := v.AddMeetingNote(Meeting{Subject: "1:1 Kim", Start: start}, "a", zone, time.UnixMilli(start))
	if err != nil || !made {
		t.Fatalf("%v %v", made, err)
	}
	second, made, err := v.AddMeetingNote(Meeting{Subject: "1:1 Kim", Start: start}, "b", zone, time.UnixMilli(start))
	if err != nil || made || second.Path != first.Path {
		t.Fatalf("%+v %v %v", second, made, err)
	}
}

func TestNotesTakenDuringAMeetingRewriteOnlyTheirOwnBlock(t *testing.T) {
	zone, _ := time.LoadLocation("Europe/Stockholm")
	start := time.Date(2026, 11, 17, 10, 0, 0, 0, zone).UnixMilli()
	now := time.Date(2026, 11, 17, 10, 5, 0, 0, zone)
	v := testVault(t, nil)
	meeting := Meeting{Subject: "1:1 Kim", Start: start, Attendees: []string{"Kim"}}
	read := func(path string) string {
		raw, _ := os.ReadFile(filepath.Join(v.Dir, path))
		return string(raw)
	}
	note, made, err := v.WriteLiveNote(meeting, "abc123", "- budget", zone, now)
	if err != nil || !made || !strings.HasSuffix(note.Path, "Kim - 2026-11-17.md") {
		t.Fatalf("%+v %v %v", note, made, err)
	}
	// You add a line in Obsidian, then keep typing in the dashboard.
	edited := strings.Replace(read(note.Path), "Deltagare: Kim\n", "Deltagare: Kim\n\nMin egen rad\n", 1)
	_ = os.WriteFile(filepath.Join(v.Dir, note.Path), []byte(edited), 0o644)
	if _, made, err := v.WriteLiveNote(meeting, "abc123", "- budget\n- hiring", zone, now); err != nil || made {
		t.Fatalf("%v %v", made, err)
	}
	got := read(note.Path)
	if !strings.Contains(got, "Min egen rad") || strings.Count(got, "- budget") != 1 || !strings.Contains(got, "- budget\n- hiring\n<!-- /ygg:live abc123 -->") {
		t.Fatalf("got:\n%s", got)
	}
	// Another sitting (another device) gets a block of its own.
	_, _, _ = v.WriteLiveNote(meeting, "zzz999", "- från mobilen", zone, now)
	got = read(note.Path)
	if strings.Count(got, "## Under mötet") != 1 || !strings.Contains(got, "- hiring") || !strings.Contains(got, "- från mobilen") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestTheLiveNoteRouteNeverAnswersNoteText(t *testing.T) {
	n, p := newNorn(t)
	n.Vault = testVault(t, map[string]string{"Budget - 2026-11-17.md": "secret text"})
	start := time.Now().UnixMilli()
	if code, _ := call(t, p, "PUT", "/v1/notes/meeting/live", map[string]any{"subject": "Budget", "start": start, "id": "BAD", "text": "x"}); code != http.StatusBadRequest {
		t.Fatalf("a bad id: %d", code)
	}
	code, body := call(t, p, "PUT", "/v1/notes/meeting/live", map[string]any{"subject": "Budget", "start": start, "id": "sit0001", "text": "- typed"})
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, body)
	}
	if raw, _ := json.Marshal(body); strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "typed") {
		t.Fatalf("note text left Norn: %s", raw)
	}
}

func TestTitleWordsSkipsChatVerbs(t *testing.T) {
	got := titleWords("Snacka AI policy, hur vi kan låta folk prata med persondata, och Figma AI")
	for _, word := range got {
		if word == "snacka" || word == "prata" || word == "folk" || word == "lata" {
			t.Fatalf("titleWords kept filler %q: %v", word, got)
		}
	}
	if got := titleWords("Snacka Jev - 2026-11-17"); len(got) != 0 {
		t.Fatalf("want no words, got %v", got)
	}
}
