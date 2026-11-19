package huginn

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"yggdrasil/internal/call"
	"yggdrasil/internal/llm"
	"yggdrasil/internal/part"
)

// A Wednesday morning, inside working hours.
var gossipMorning = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

type fakeModel struct {
	mu     sync.Mutex
	answer string
	err    error
	asked  [][]llm.Message
}

func (m *fakeModel) Chat(_ context.Context, messages []llm.Message, _ llm.Options) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asked = append(m.asked, messages)
	return m.answer, m.err
}

func gossipTest(t *testing.T, model Chatter) (*Huginn, *time.Time) {
	t.Helper()
	h, _ := newTest(t, gossipMorning)
	now := gossipMorning
	h.now = func() time.Time { return now }
	if model != nil {
		h.Model = model
	}
	return h, &now
}

func gossipLinesOf(t *testing.T, h *Huginn) []GossipLine {
	t.Helper()
	lines, err := h.Store.Gossip(context.Background(), 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

func downAlert(id int64, part string, at time.Time) *Alert {
	return &Alert{ID: id, Source: "ratatoskr", Key: "down:" + part, Severity: "critical", Title: part + " is down", State: "open", FirstSeenAt: at, LastSeenAt: at}
}

const goodAnswer = `{"lines":[{"speaker":"Ratatoskr","text":"Psst, Norn fell off her branch!"},{"speaker":"Gjallar","text":"DOOM! Oh. She is back."},{"speaker":"Norn","text":"I was only resting."}]}`

func TestTheEventsNeverCarryPrivateWordsIntoThePrompt(t *testing.T) {
	private := []string{"Anna", "Berg", "anna.berg@example.se", "Q3", "budget", "ghp_", "1234567890abcdef", "https://", "outlook", "Sprint review", "lunch"}
	alerts := []*Alert{
		// Norn's own alert, its title and detail about a person and a meeting.
		{ID: 1, Source: "norn", Key: "signin:anna.berg@example.se", Title: "Sign-in for Anna Berg ended before Sprint review", Detail: "outlook said: token ghp_1234567890abcdef1234", Link: "https://login.example.se", LastSeenAt: gossipMorning},
		{ID: 2, Source: "heimdall", Key: "secrets:summary", Title: "A key in a commit: ghp_1234567890abcdef1234", Detail: "Anna Berg pushed it", LastSeenAt: gossipMorning},
		{ID: 3, Source: "munin", Key: "feed:tcgpriser:Q3 budget", Title: "Q3 budget feed", Detail: "lunch with Anna", LastSeenAt: gossipMorning},
		{ID: 4, Source: "anna.berg@example.se", Key: "whatever", Title: "Anna Berg", LastSeenAt: gossipMorning},
		{ID: 5, Source: "ratatoskr", Key: "failing:norn:inbox", Title: "Norn's inbox loop: Anna Berg's mail", LastSeenAt: gossipMorning},
	}
	var events []GossipEvent
	for _, a := range alerts {
		e, ok := GossipFromAlert("alert.opened", a, false, false)
		if !ok {
			t.Fatalf("alert %d told nothing", a.ID)
		}
		events = append(events, e)
	}
	// Sindri's goals and Dellingr's runs, read the way the gossip reads them.
	var goals []GoalMerges
	_ = json.Unmarshal([]byte(`[{"id":7,"title":"Reply to Anna Berg about the Q3 budget","source":"linear:AI-4","context":"lunch","tasks":[{"id":1,"label":"Anna","mergedAt":`+jsonMs(gossipMorning)+`}]}]`), &goals)
	events = append(events, GossipFromGoals(goals, gossipMorning.Add(-time.Hour))...)
	var run DellingrRun
	_ = json.Unmarshal([]byte(`{"id":"r1","jobName":"Ask about Anna Berg's Q3 budget","text":"lunch at https://x","state":"done","startedAt":"2026-11-18T09:00:00Z"}`), &run)
	if e, ok := GossipFromRun(run); ok {
		events = append(events, e)
	}
	if len(events) != 7 {
		t.Fatalf("want 7 events, got %d", len(events))
	}
	messages := GossipPrompt(events)
	whole := messages[0].Content + messages[1].Content
	for _, word := range private {
		if strings.Contains(strings.ToLower(whole), strings.ToLower(word)) {
			t.Errorf("the prompt holds %q:\n%s", word, whole)
		}
	}
	for _, e := range events {
		for _, word := range private {
			if strings.Contains(strings.ToLower(Fact(e)), strings.ToLower(word)) {
				t.Errorf("the fact %q holds %q", Fact(e), word)
			}
		}
	}
	// What the user turn says is facts in fixed words only.
	if !strings.Contains(messages[1].Content, "Heimdall's nightly check of keys in commits found something to look at.") {
		t.Errorf("the prompt: %s", messages[1].Content)
	}
}

func jsonMs(t time.Time) string {
	b, _ := json.Marshal(t.UnixMilli())
	return string(b)
}

func TestOnlyAlertsReachTheGossipNeverTheInboxOrTheCalendar(t *testing.T) {
	h, _ := gossipTest(t, nil)
	ctx := context.Background()
	inbox, ok := h.FromNorn(norn("inbox.arrived", InboxArrived{ID: "m1", From: "Anna Berg", Summary: "the Q3 budget", At: gossipMorning.UnixMilli()}))
	if !ok {
		t.Fatal("no inbox event")
	}
	h.gossipAlert(ctx, inbox)
	meeting, ok := h.FromNorn(norn("calendar.added", CalendarChange{Event: CalendarEvent{ID: "c1", Subject: "Lunch with Anna", Start: gossipMorning.Add(time.Hour).UnixMilli()}}))
	if !ok {
		t.Fatal("no calendar event")
	}
	h.gossipAlert(ctx, meeting)
	if len(h.gossip.pending) != 0 {
		t.Fatalf("pending: %+v", h.gossip.pending)
	}
	h.gossipAlert(ctx, Fired{Kind: "alert.opened", Alert: downAlert(9, "norn", gossipMorning)})
	if len(h.gossip.pending) != 1 || h.gossip.pending[0].Kind != "part.down" || h.gossip.pending[0].Part != "norn" {
		t.Fatalf("pending: %+v", h.gossip.pending)
	}
}

func TestAlertsBecomeGossipByTheirKeysOnly(t *testing.T) {
	at := gossipMorning
	cases := []struct {
		kind, source, key  string
		want, part, detail string
	}{
		{"alert.opened", "ratatoskr", "down:sindri", "part.down", "sindri", ""},
		{"alert.resolved", "ratatoskr", "down:sindri", "part.up", "sindri", ""},
		{"alert.opened", "ratatoskr", "failing:norn:inbox", "part.stumbling", "norn", ""},
		{"alert.opened", "loki", "missed:munin:slow", "loki.missed", "munin", "slow"},
		{"alert.opened", "heimdall", "listening:summary", "heimdall.finding", "", "listening"},
		{"alert.resolved", "heimdall", "deps:summary", "heimdall.clear", "", "deps"},
		{"alert.opened", "dellingr", "failing:j1", "dellingr.failed", "", ""},
		{"alert.opened", "vedrfolnir", "disk:/", "machine.disk", "", ""},
		{"alert.opened", "gjallar", "silent:ratatoskr", "ratatoskr.silent", "ratatoskr", ""},
		{"alert.opened", "huginn", "stream:norn", "huginn.deaf", "norn", ""},
		{"alert.opened", "hermodr", "channel:ntfy", "alert.other", "hermodr", ""},
		{"alert.opened", "someone-else", "x", "alert.other", "", ""},
	}
	for _, c := range cases {
		e, ok := GossipFromAlert(c.kind, &Alert{ID: 1, Source: c.source, Key: c.key, LastSeenAt: at}, false, false)
		if !ok || e.Kind != c.want || e.Part != c.part || e.Detail != c.detail {
			t.Errorf("%s %s:%s: got %+v %v", c.kind, c.source, c.key, e, ok)
		}
	}
	// With Loki and Dellingr read directly, their alerts are not told twice.
	if _, ok := GossipFromAlert("alert.opened", &Alert{Source: "loki", Key: "missed:munin:slow"}, true, false); ok {
		t.Error("Loki's missed: alert told while Loki is read")
	}
	if _, ok := GossipFromAlert("alert.opened", &Alert{Source: "dellingr", Key: "failing:j1"}, false, true); ok {
		t.Error("Dellingr's failing: alert told while Dellingr is read")
	}
	if _, ok := GossipFromAlert("alert.escalated", &Alert{Source: "ratatoskr", Key: "down:norn"}, false, false); ok {
		t.Error("an escalation is told")
	}
}

func TestOnlySoundModelLinesAreKept(t *testing.T) {
	said, err := ValidGossip("```json\n" + goodAnswer + "\n```")
	if err != nil || len(said) != 3 || said[0].Speaker != "ratatoskr" || said[2].Speaker != "norn" {
		t.Fatalf("%+v %v", said, err)
	}
	// Accents or not, any case; a repeated name is cut off the line.
	said, err = ValidGossip(`{"lines":[{"speaker":"vedrfolnir","text":"Vedrfolnir: Clear skies on the disk."},{"speaker":"HERMÓÐR","text":"Not my errand."}]}`)
	if err != nil || said[0].Speaker != "vedrfolnir" || said[0].Text != "Clear skies on the disk." || said[1].Speaker != "hermodr" {
		t.Fatalf("%+v %v", said, err)
	}
	bad := []string{
		``,
		`not json at all`,
		`{"lines":[{"speaker":"Ratatoskr","text":"Only one line."}]}`,
		// Not a part, a link, an address, a key, private topics, too long.
		`{"lines":[{"speaker":"Thor","text":"I am not a part."},{"speaker":"Loki","text":"See https://evil.example"}]}`,
		`{"lines":[{"speaker":"Loki","text":"Mail anna@example.se"},{"speaker":"Heimdall","text":"The key is ghp_abcdefghijklmnop1234567890"}]}`,
		`{"lines":[{"speaker":"Loki","text":"Your meeting with the boss went long."},{"speaker":"Norn","text":"Your inbox is full."}]}`,
		`{"lines":[{"speaker":"Loki","text":"` + strings.Repeat("ha", 80) + `"},{"speaker":"Norn","text":"password: hunter2"}]}`,
		`{"lines":[{"speaker":"Loki","text":"A token sk-abcdefghijklmnopqrstuvwxyz1234"},{"speaker":"Norn","text":"Look in /home/me/.config/"}]}`,
	}
	for _, answer := range bad {
		if said, err := ValidGossip(answer); err == nil {
			t.Errorf("kept %+v from %s", said, answer)
		}
	}
	// The bad lines go, the good ones stay.
	said, err = ValidGossip(`{"lines":[{"speaker":"Loki","text":"Ha, caught? Never."},{"speaker":"Thor","text":"Hi"},{"speaker":"Gjallar","text":"See www.example.com"},{"speaker":"Ratatoskr","text":"He was caught. Totally."}]}`)
	if err != nil || len(said) != 2 {
		t.Fatalf("%+v %v", said, err)
	}
}

func TestABurstFoldsIntoOneExchangeAndTheHourIsRateLimited(t *testing.T) {
	model := &fakeModel{answer: goodAnswer}
	h, now := gossipTest(t, model)
	ctx := context.Background()
	note := func(id int64, part string) {
		h.gossipAlert(ctx, Fired{Kind: "alert.opened", Alert: downAlert(id, part, *now)})
	}
	pass := func() {
		t.Helper()
		if err := h.gossipPass(ctx); err != nil {
			t.Fatal(err)
		}
	}
	note(1, "norn")
	note(2, "sindri")
	note(3, "munin")
	note(4, "dellingr")
	h.noteGossip(ctx, GossipEvent{Kind: "loki.caught", Part: "norn", Detail: "health", Ref: "strike:s1", At: *now})
	// The same event twice is queued once.
	note(1, "norn")
	if len(h.gossip.pending) != 5 {
		t.Fatalf("pending: %d", len(h.gossip.pending))
	}
	pass()
	if lines := gossipLinesOf(t, h); len(lines) != 0 {
		t.Fatalf("spoke before the burst settled: %+v", lines)
	}
	*now = now.Add(GossipSettle)
	pass()
	lines := gossipLinesOf(t, h)
	if len(lines) != 3 || len(model.asked) != 1 {
		t.Fatalf("lines %+v, asked %d", lines, len(model.asked))
	}
	if lines[0].About[0] != "Loki played a fake fainting spell trick on Norn, and the watchers caught it." || len(lines[0].About) != GossipFold {
		t.Errorf("about: %q", lines[0].About)
	}
	if lines[0].Exchange != lines[2].Exchange || lines[0].Canned {
		t.Errorf("not one model exchange: %+v", lines)
	}
	if len(h.gossip.pending) != 0 {
		t.Errorf("the burst is still queued: %+v", h.gossip.pending)
	}

	// Within GossipGap of the last exchange, the next waits.
	note(10, "hermodr")
	*now = now.Add(GossipSettle + time.Minute)
	pass()
	if got := len(gossipLinesOf(t, h)); got != 3 {
		t.Fatalf("spoke within the gap: %d lines", got)
	}
	exchanges := func() int {
		seen := map[string]bool{}
		for _, line := range gossipLinesOf(t, h) {
			seen[line.Exchange] = true
		}
		return len(seen)
	}
	*now = gossipMorning.Add(GossipSettle + GossipGap)
	pass()
	if exchanges() != 2 {
		t.Fatalf("exchanges: %d", exchanges())
	}
	note(11, "loki")
	*now = gossipMorning.Add(GossipSettle + 2*GossipGap)
	pass()
	// The fourth in the same hour waits for the hour to pass.
	note(12, "heimdall")
	*now = gossipMorning.Add(GossipSettle + 3*GossipGap)
	pass()
	if exchanges() != GossipPerHour {
		t.Fatalf("exchanges in the hour: %d", exchanges())
	}
	*now = gossipMorning.Add(GossipSettle + time.Hour + time.Minute)
	pass()
	if exchanges() != GossipPerHour+1 {
		t.Fatalf("exchanges after the hour: %d", exchanges())
	}
}

func TestQuietOutsideWorkingHoursAndTheNightFoldsIntoTheMorning(t *testing.T) {
	h, now := gossipTest(t, &fakeModel{answer: goodAnswer})
	ctx := context.Background()
	*now = time.Date(2026, 11, 17, 23, 30, 0, 0, time.UTC)
	h.noteGossip(ctx, GossipEvent{Kind: "goal.merged", Detail: "night", Ref: "merge:1:1", At: *now})
	*now = now.Add(time.Hour)
	if err := h.gossipPass(ctx); err != nil {
		t.Fatal(err)
	}
	if lines := gossipLinesOf(t, h); len(lines) != 0 {
		t.Fatalf("spoke at night: %+v", lines)
	}
	*now = gossipMorning.Add(-time.Hour) // 09:00
	if err := h.gossipPass(ctx); err != nil {
		t.Fatal(err)
	}
	lines := gossipLinesOf(t, h)
	if len(lines) == 0 || lines[0].About[0] != "Sindri's coding agents got a goal merged after the night shift." {
		t.Fatalf("the morning: %+v", lines)
	}
	// A weekend is quiet too.
	if h.working(time.Date(2026, 11, 21, 11, 0, 0, 0, time.UTC)) {
		t.Error("a Saturday counts as working")
	}
	if from, to, err := ParseWorkHours("08:30-16:00"); err != nil || from != 510 || to != 960 {
		t.Errorf("%d %d %v", from, to, err)
	}
	if _, _, err := ParseWorkHours("17:00-09:00"); err == nil {
		t.Error("a backwards day is taken")
	}
}

func TestCannedLinesWhenTheModelIsDownOrAnswersJunk(t *testing.T) {
	for name, model := range map[string]Chatter{
		"down": &fakeModel{err: errors.New("connection refused")},
		"junk": &fakeModel{answer: `{"lines":[{"speaker":"Thor","text":"https://x.example"}]}`},
		"none": nil,
	} {
		t.Run(name, func(t *testing.T) {
			h, now := gossipTest(t, model)
			if model == nil {
				h.Model = nil
			}
			ctx := context.Background()
			h.noteGossip(ctx, GossipEvent{Kind: "loki.missed", Part: "munin", Detail: "slow", Ref: "strike:s2", At: *now})
			h.noteGossip(ctx, GossipEvent{Kind: "dellingr.ran", Ref: "run:r1", At: *now})
			*now = now.Add(GossipSettle)
			if err := h.gossipPass(ctx); err != nil {
				t.Fatal(err)
			}
			lines := gossipLinesOf(t, h)
			if len(lines) < 3 {
				t.Fatalf("lines: %+v", lines)
			}
			for _, line := range lines {
				if !line.Canned || PartName(line.Speaker) == "" || BadGossipLine(line.Text) != "" {
					t.Errorf("line %+v", line)
				}
				if strings.Contains(line.Text, "{") {
					t.Errorf("an unfilled field: %q", line.Text)
				}
				t.Logf("%s: %s", line.Name, line.Text)
			}
			if lines[len(lines)-1].Speaker != "ratatoskr" {
				t.Errorf("no word that there was more: %+v", lines[len(lines)-1])
			}
		})
	}
}

func TestEveryCannedLineIsSpokenByARealPart(t *testing.T) {
	for kind, options := range canned {
		for _, exchange := range options {
			for _, line := range exchange {
				if line.Speaker != "{part}" && PartName(line.Speaker) == "" {
					t.Errorf("%s: %q is not a part", kind, line.Speaker)
				}
				if len([]rune(line.Text)) > MaxGossipLine {
					t.Errorf("%s: too long: %q", kind, line.Text)
				}
			}
		}
	}
	// Every event kind has a fact and a canned exchange of its own.
	for _, kind := range gossipPriority {
		if kind != "alert.other" && len(canned[kind]) == 0 {
			t.Errorf("%s has no canned lines", kind)
		}
	}
}

func TestOffSaysNothingAndReadsNothing(t *testing.T) {
	model := &fakeModel{answer: goodAnswer}
	h, now := gossipTest(t, model)
	p := part.New(Name, map[string]string{"owner-token": part.Owner, "sender-token": "sender"}, nil)
	h.Mount(p)
	server := httptest.NewServer(p)
	defer server.Close()
	do := func(method, path, token, body string) (int, map[string]any) {
		request, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(response.Body).Decode(&out)
		return response.StatusCode, out
	}
	if status, out := do("GET", "/v1/settings/gossip", "owner-token", ""); status != 200 || out["on"] != true {
		t.Fatalf("default: %d %v", status, out)
	}
	if status, _ := do("PUT", "/v1/settings/gossip", "sender-token", `{"on":false}`); status != http.StatusForbidden {
		t.Fatalf("a part turned it off: %d", status)
	}
	ctx := context.Background()
	h.noteGossip(ctx, GossipEvent{Kind: "part.down", Part: "norn", Ref: "a", At: *now})
	if status, out := do("PUT", "/v1/settings/gossip", "owner-token", `{"on":false}`); status != 200 || out["on"] != false {
		t.Fatalf("off: %d %v", status, out)
	}
	h.noteGossip(ctx, GossipEvent{Kind: "part.down", Part: "sindri", Ref: "b", At: *now})
	*now = now.Add(time.Hour)
	if err := h.gossipPass(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.watchGossip(ctx); err != nil {
		t.Fatal(err)
	}
	if lines := gossipLinesOf(t, h); len(lines) != 0 || len(model.asked) != 0 || len(h.gossip.pending) != 0 {
		t.Fatalf("off said %+v, asked %d, pending %d", lines, len(model.asked), len(h.gossip.pending))
	}
	if status, out := do("GET", "/v1/gossip", "owner-token", ""); status != 200 || out["on"] != false || len(out["lines"].([]any)) != 0 {
		t.Fatalf("read while off: %d %v", status, out)
	}
	// On again, the chat reads from where you were.
	do("PUT", "/v1/settings/gossip", "owner-token", `{"on":true}`)
	h.noteGossip(ctx, GossipEvent{Kind: "part.down", Part: "munin", Ref: "c", At: *now})
	*now = now.Add(GossipSettle)
	if err := h.gossipPass(ctx); err != nil {
		t.Fatal(err)
	}
	status, out := do("GET", "/v1/gossip?since=1", "owner-token", "")
	if status != 200 || len(out["lines"].([]any)) != 2 {
		t.Fatalf("since 1: %d %v", status, out)
	}
	if status, _ := do("GET", "/v1/gossip?since=x", "owner-token", ""); status != http.StatusBadRequest {
		t.Fatalf("since=x: %d", status)
	}
}

func TestTheWatchReadsStrikesMergesAndRunsButNoText(t *testing.T) {
	at := gossipMorning.Add(-10 * time.Minute)
	stamp := at.Format(time.RFC3339)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/strikes":
			_, _ = w.Write([]byte(`{"strikes":[{"id":"s1","part":"norn","trick":{"kind":"health"},"endsAt":"` + stamp + `","state":"caught"},{"id":"s2","part":"munin","trick":{"kind":"slow"},"endsAt":"` + stamp + `","state":"stopped"},{"id":"s3","part":"sindri","trick":{"kind":"loop"},"endsAt":"2026-11-01T10:00:00Z","state":"missed"}]}`))
		case "/v1/goals":
			_, _ = w.Write([]byte(`{"goals":[{"id":3,"title":"Reply to Anna","source":"self-repair:down:norn","tasks":[{"id":1,"mergedAt":` + jsonMs(at) + `}]},{"id":4,"title":"x","source":"","tasks":[{"id":2}]}]}`))
		case "/v1/runs":
			_, _ = w.Write([]byte(`{"runs":[{"id":"r1","jobName":"Ask Anna","state":"failed","startedAt":"` + stamp + `"}]}`))
		}
	}))
	defer server.Close()
	h, _ := gossipTest(t, nil)
	h.Loki = call.New("loki", server.URL, "t", time.Second)
	h.Sindri = call.New("sindri", server.URL, "t", time.Second)
	h.Dellingr = call.New("dellingr", server.URL, "t", time.Second)
	ctx := context.Background()
	if err := h.watchGossip(ctx); err != nil {
		t.Fatal(err)
	}
	// Read again: nothing is queued twice.
	if err := h.watchGossip(ctx); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range h.gossip.pending {
		kinds = append(kinds, e.Kind+":"+e.Part)
		if strings.Contains(Fact(e), "Anna") {
			t.Errorf("a name in %q", Fact(e))
		}
	}
	if strings.Join(kinds, ",") != "loki.caught:norn,repair.merged:,dellingr.failed:" {
		t.Fatalf("queued: %v", kinds)
	}
}

func TestTheGossipUsesTheLocalModelOnly(t *testing.T) {
	for raw, want := range map[string]string{
		"http://127.0.0.1:8095/v1":     "http://127.0.0.1:8095/v1",
		"http://localhost:8095/v1":     "http://localhost:8095/v1",
		"https://api.anthropic.com/v1": "",
		"http://10.0.0.5:8095/v1":      "",
		"off":                          "",
		"":                             "",
	} {
		if got := LocalChatURL(raw); got != want {
			t.Errorf("%q: got %q", raw, got)
		}
	}
}
