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

func TestAMeetingBriefIsAskedOfNornAndHandedToHermodrToHold(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	h, fake := newTest(t, now)
	fired, _ := h.FromNorn(norn("calendar.added", map[string]any{"calendar": "work",
		"event": map[string]any{"id": "x", "subject": "Budget", "start": now.Add(40 * time.Minute).UnixMilli(), "end": now.Add(70 * time.Minute).UnixMilli(), "organizer": "Kim"}}))
	h.Handle(context.Background(), fired)
	got := reactions(t, h)
	if len(got) != 1 || got[0].State != "delivered" || got[0].Text != "Bring the Q3 numbers." {
		t.Fatalf("got %+v", got)
	}
	if len(fake.asked) != 1 || !strings.HasPrefix(fake.asked[0], "/local Brief me for my meeting \"Budget\" at 10:40, organized by Kim") {
		t.Fatalf("asked %q", fake.asked)
	}
	if len(fake.pushed) != 1 || !fake.pushed[0].Hold || fake.pushed[0].Priority != "normal" || fake.pushed[0].Title != "Budget at 10:40" {
		t.Fatalf("handed over %+v", fake.pushed)
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
