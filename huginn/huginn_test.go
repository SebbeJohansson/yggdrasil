package huginn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/part"
	"yggdrasil/internal/sse"
	"yggdrasil/internal/tell"
)

type fakeParts struct {
	mu     sync.Mutex
	asked  []string
	pushed []tell.Message
}

func newTest(t *testing.T, now time.Time) (*Huginn, *fakeParts) {
	t.Helper()
	fake := &fakeParts{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		switch r.URL.Path {
		case "/v1/ask":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			fake.asked = append(fake.asked, body["question"])
			_ = json.NewEncoder(w).Encode(map[string]any{"answer": "Bring the Q3 numbers."})
		case "/v1/agents":
			_ = json.NewEncoder(w).Encode([]Agent{{ID: "a1", Label: "fix login", Status: "working"}, {ID: "a2", Status: "done"}})
		case "/v1/messages":
			var m tell.Message
			_ = json.NewDecoder(r.Body).Decode(&m)
			fake.pushed = append(fake.pushed, m)
			w.WriteHeader(201)
		}
	}))
	t.Cleanup(server.Close)
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h, err := New(context.Background(), store, call.New("norn", server.URL, "t", time.Second), call.New("sindri", server.URL, "t", time.Second),
		alert.NewClient("", ""), tell.NewHermodr(server.URL, "t"), time.UTC, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	h.now = func() time.Time { return now }
	return h, fake
}

func TestMeetsItsContract(t *testing.T) {
	h, _ := newTest(t, time.Now())
	h.Streams = []*sse.Stream{sse.New("norn", "", "", time.Now())}
	p := part.New(Name, contracttest.Tokens(), nil)
	h.Mount(p)
	contracttest.Run(t, p)
}

func norn(kind string, data any) sse.Event {
	payload, _ := json.Marshal(data)
	return sse.Event{ID: "1", Kind: kind, Data: string(payload)}
}

func reactions(t *testing.T, h *Huginn) []Reaction {
	t.Helper()
	list, err := h.Store.Reactions(context.Background(), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestAMessageFromYourManagerIsPushedOnce(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	h, fake := newTest(t, now)
	fired, ok := h.FromNorn(norn("inbox.arrived", map[string]any{"id": "m1", "source": "teams", "from": "Kim", "relation": "manager",
		"band": "urgent", "direct": true, "summary": "Kim needs the numbers by 14:00", "at": now.UnixMilli()}))
	if !ok {
		t.Fatal("not read")
	}
	h.Handle(context.Background(), fired)
	got := reactions(t, h)
	states := map[string]int{}
	for _, r := range got {
		states[r.State]++
	}
	// important-message and urgent-message both match; one tells you.
	if states["delivered"] != 1 || states["skipped"] != 1 || len(fake.pushed) != 1 || fake.pushed[0].Priority != "urgent" || fake.pushed[0].Hold {
		t.Fatalf("states %v, pushed %v", states, fake.pushed)
	}
}

func TestADraftReadyRuleFiltersLikeAMessageAndLinksToTheInbox(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	h, fake := newTest(t, now)
	h.DashboardURL = "http://localhost:17420/"
	rule := Rule{ID: "draft-from-manager", Name: "Draft for my manager", On: Trigger{Event: "draft.ready", Relation: []string{"manager"}},
		Action: Action{Kind: "notify", Text: "A {tone} reply to {from} is ready"}, Deliver: []string{"phone"}, Hold: true, Enabled: true}
	if err := h.Store.PutRule(context.Background(), rule); err != nil {
		t.Fatal(err)
	}
	peer, ok := h.FromNorn(norn("draft.ready", map[string]any{"id": "m2", "source": "teams", "from": "Bo", "relation": "peer", "band": "notable", "direct": true, "at": now.UnixMilli()}))
	if !ok {
		t.Fatal("not read")
	}
	h.Handle(context.Background(), peer)
	if len(fake.pushed) != 0 {
		t.Fatalf("a peer's draft was told: %+v", fake.pushed)
	}
	manager, _ := h.FromNorn(norn("draft.ready", map[string]any{"id": "m1", "source": "teams", "from": "Kim", "relation": "manager", "band": "notable", "direct": true, "tone": "friendly", "at": now.UnixMilli()}))
	h.Handle(context.Background(), manager)
	if len(fake.pushed) != 1 {
		t.Fatalf("pushed %+v", fake.pushed)
	}
	told := fake.pushed[0]
	if told.Text != "A friendly reply to Kim is ready" || told.Title != "Reply drafted to Kim (manager)" || told.Link != "http://localhost:17420/dashboard/inbox" || !told.Hold {
		t.Fatalf("told %+v", told)
	}
}

func TestAMeetingBriefIsAskedOfNornAndHandedToHermodrToHold(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	h, fake := newTest(t, now)
	meeting := map[string]any{"calendar": "work",
		"event": map[string]any{"id": "x", "subject": "Budget", "start": now.Add(25 * time.Minute).UnixMilli(), "end": now.Add(70 * time.Minute).UnixMilli(), "organizer": "Kim"}}
	added, _ := h.FromNorn(norn("calendar.added", meeting))
	h.Handle(context.Background(), added)
	if len(fake.asked) != 0 {
		t.Fatalf("briefed on the add: %q", fake.asked)
	}
	fired, _ := h.FromNorn(norn("calendar.soon", meeting))
	h.Handle(context.Background(), fired)
	got := reactions(t, h)
	if len(got) != 1 || got[0].State != "delivered" || got[0].Text != "Bring the Q3 numbers." {
		t.Fatalf("got %+v", got)
	}
	if len(fake.asked) != 1 || !strings.HasPrefix(fake.asked[0], "/local Brief me for my meeting \"Budget\" at 10:25, organized by Kim") {
		t.Fatalf("asked %q", fake.asked)
	}
	if len(fake.pushed) != 1 || !fake.pushed[0].Hold || fake.pushed[0].Priority != "normal" || fake.pushed[0].Title != "Budget at 10:25" {
		t.Fatalf("handed over %+v", fake.pushed)
	}
}

func TestAMeetingBriefAsksWithWhatYouAddedForItsSeries(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	h, fake := newTest(t, now)
	meeting := map[string]any{"calendar": "work", "prompt": "Focus on the budget.",
		"event": map[string]any{"id": "x", "subject": "Budget", "start": now.Add(25 * time.Minute).UnixMilli(), "end": now.Add(70 * time.Minute).UnixMilli()}}
	fired, _ := h.FromNorn(norn("calendar.soon", meeting))
	h.Handle(context.Background(), fired)
	if len(fake.asked) != 1 || !strings.HasSuffix(fake.asked[0], "and what I should bring. Also: Focus on the budget.") {
		t.Fatalf("asked %q", fake.asked)
	}
}

func TestAMeetingMovedIntoTheHourIsBriefedOnceNotAgainWhenItComesClose(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	h, fake := newTest(t, now)
	meeting := map[string]any{"calendar": "work", "previousStart": now.Add(3 * time.Hour).UnixMilli(),
		"event": map[string]any{"id": "x", "subject": "Budget", "start": now.Add(20 * time.Minute).UnixMilli(), "end": now.Add(70 * time.Minute).UnixMilli()}}
	for _, kind := range []string{"calendar.moved", "calendar.soon"} {
		fired, _ := h.FromNorn(norn(kind, meeting))
		h.Handle(context.Background(), fired)
	}
	if len(fake.asked) != 1 || len(fake.pushed) != 1 {
		t.Fatalf("asked %d times, told %d times", len(fake.asked), len(fake.pushed))
	}
}

func TestACriticalAlertSaysWhatItStops(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	h, _ := newTest(t, now)
	alertEvent := func(state, severity string) sse.Event {
		return sse.Event{Kind: "alert", Data: fmt.Sprintf(`{"id":7,"source":"munin","key":"down:anthropic-status","severity":%q,"title":"Anthropic is down","state":%q,"firstSeenAt":%q,"lastSeenAt":%q}`,
			severity, state, now.Format(time.RFC3339), now.Format(time.RFC3339))}
	}
	fired, ok := h.FromGjallar(alertEvent("open", "critical"))
	if !ok || fired.Kind != "alert.opened" {
		t.Fatalf("got %+v", fired)
	}
	h.Handle(context.Background(), fired)
	got := reactions(t, h)
	if len(got) != 1 || got[0].State != "delivered" || !strings.Contains(got[0].Text, "Claude Code agents") || !strings.Contains(got[0].Text, "1 running: fix login") {
		t.Fatalf("got %+v", got)
	}
	if _, ok := h.FromGjallar(alertEvent("open", "critical")); ok {
		t.Fatal("the same alert again fired")
	}
	if fired, _ := h.FromGjallar(alertEvent("resolved", "critical")); fired.Kind != "alert.resolved" {
		t.Fatalf("got %q", fired.Kind)
	}
}

func TestAnAgentsRuleStartsDisabledAndOnlyYouEnableIt(t *testing.T) {
	h, _ := newTest(t, time.Now())
	p := part.New(Name, contracttest.Tokens(), nil)
	h.Mount(p)
	server := httptest.NewServer(p)
	defer server.Close()
	do := func(token, method, path, body string) (int, Rule) {
		request, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var rule Rule
		_ = json.NewDecoder(response.Body).Decode(&rule)
		return response.StatusCode, rule
	}
	status, rule := do(contracttest.Sender, "POST", "/v1/rules", `{"name":"Slack from Bo","on":{"event":"inbox.arrived"},"action":{"kind":"notify","text":"{from}: {summary}"}}`)
	if status != 201 || rule.Enabled {
		t.Fatalf("%d %+v", status, rule)
	}
	if status, _ := do(contracttest.Sender, "PATCH", "/v1/rules/"+rule.ID, `{"enabled":true}`); status != 403 {
		t.Fatalf("the agent enabled it: %d", status)
	}
	if status, rule := do(contracttest.Owner, "PATCH", "/v1/rules/"+rule.ID, `{"enabled":true}`); status != 200 || !rule.Enabled {
		t.Fatalf("%d %+v", status, rule)
	}
	if status, _ := do(contracttest.Owner, "DELETE", "/v1/rules/alert-impact", ""); status != 403 {
		t.Fatalf("removed a built-in rule: %d", status)
	}
}

func TestYourCopyOfAnInvitedMeetingIsSuggestedForDeleting(t *testing.T) {
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	h, fake := newTest(t, now)
	h.DashboardURL = "http://127.0.0.1:3000"
	start := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC).UnixMilli()
	change := map[string]any{"calendar": "work",
		"event":  map[string]any{"id": "c", "subject": "🍕 Pizza lunch", "start": start, "end": start + 3600000, "response": "organizer"},
		"invite": map[string]any{"id": "i", "subject": "🍕 Pizza lunch ", "organizer": "Karin Lund", "start": start, "end": start + 3600000}}
	fired, ok := h.FromNorn(norn("calendar.duplicate", change))
	if !ok {
		t.Fatal("calendar.duplicate not read")
	}
	h.Handle(context.Background(), fired)
	if len(fake.pushed) != 1 {
		t.Fatalf("told %d times", len(fake.pushed))
	}
	m := fake.pushed[0]
	want := `Karin Lund sent an invite for "🍕 Pizza lunch". Your own "🍕 Pizza lunch" on Thu 26 Nov at 12:00 is a duplicate: delete it?`
	if m.Text != want || m.Link != "http://127.0.0.1:3000/" || !m.Hold || m.Push == nil || *m.Push {
		t.Fatalf("got %+v", m)
	}
}

func TestMarksOutliveARestart(t *testing.T) {
	h, _ := newTest(t, time.Now())
	ctx := context.Background()
	if id, err := h.Store.Mark(ctx, "norn"); err != nil || id != "" {
		t.Fatalf("got %q %v", id, err)
	}
	_ = h.Store.SetMark(ctx, "norn", "41")
	_ = h.Store.SetMark(ctx, "norn", "42")
	if id, _ := h.Store.Mark(ctx, "norn"); id != "42" {
		t.Fatalf("got %q", id)
	}
}

func TestReleaseRuleMatchesOnlyReleaseAlerts(t *testing.T) {
	rule := releaseRule([]string{"phone"})
	opened := func(key string) Fired {
		return Fired{Kind: "alert.opened", Alert: &Alert{Source: "munin", Key: key, Severity: "warning"}}
	}
	now := time.Now()
	if !rule.On.Matches(opened("release:a/b"), now) {
		t.Fatal("a failed release should match")
	}
	if rule.On.Matches(opened("github"), now) {
		t.Fatal("another munin alert should not match")
	}
}
