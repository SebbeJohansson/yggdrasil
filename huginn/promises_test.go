package huginn

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
	"yggdrasil/internal/sse"
	"yggdrasil/internal/tell"
)

func TestAPromiseDueIsToldOnceHeldWithALinkToTheDashboard(t *testing.T) {
	now := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	h, fake := newTest(t, now)
	data, _ := json.Marshal(PromiseDue{ID: 4, What: "Send the Q3 deck", To: "Anna Berg", Due: "2026-11-20", By: "tomorrow", Source: "outlook", Draft: true,
		Link: "http://localhost:17420/dashboard/inbox#promises", At: now.UnixMilli()})
	event := sse.Event{Kind: "promise.due", Data: string(data)}
	for range 2 {
		fired, ok := h.FromNorn(event)
		if !ok {
			t.Fatal("promise.due not read")
		}
		h.Handle(context.Background(), fired)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.pushed) != 1 {
		t.Fatalf("told %d times", len(fake.pushed))
	}
	m := fake.pushed[0]
	if m.Text != "You promised Anna Berg: Send the Q3 deck, by tomorrow. A reply is drafted on the dashboard." || m.Title != "A promise for tomorrow" ||
		m.Link != "http://localhost:17420/dashboard/inbox#promises" || !m.Hold || m.Priority != "normal" {
		t.Fatalf("%+v", m)
	}
}

func TestAPromiseWithNoOneNamedStillReads(t *testing.T) {
	h, _ := newTest(t, time.Now())
	data, _ := json.Marshal(PromiseDue{ID: 5, What: "Kolla med juristen", Due: "2026-11-23", By: "Monday", Dev: true, At: time.Now().UnixMilli()})
	fired, _ := h.FromNorn(sse.Event{Kind: "promise.due", Data: string(data)})
	if got := Fill(promiseRule(nil).Action.Text, fired.Fields); got != "You promised: Kolla med juristen, by Monday. You can make it a goal from the dashboard." {
		t.Fatal(got)
	}
}

func TestEachOpenPromiseIsJudgedOnceAndTheAnswerKeptOnNorn(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	marked := map[string]string{}
	yes := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/v1/promises":
			_ = json.NewEncoder(w).Encode(map[string]any{"promises": []map[string]any{
				{"id": 1, "what": "Fix the login bug in the checkout"},
				{"id": 2, "what": "Send the Q3 deck"},
				{"id": 3, "what": "Already judged", "dev": yes},
			}})
		case r.URL.Path == "/v1/ask":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			asked = append(asked, body["question"])
			answer := "no"
			if strings.Contains(body["question"], "login bug") {
				answer = "Yes."
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"answer": answer})
		case strings.HasSuffix(r.URL.Path, "/dev") && r.Method == http.MethodPut:
			var body map[string]bool
			_ = json.NewDecoder(r.Body).Decode(&body)
			marked[r.URL.Path] = map[bool]string{true: "dev", false: "not"}[body["dev"]]
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h, err := New(context.Background(), store, call.New("norn", server.URL, "t", time.Second), call.New("sindri", server.URL, "t", time.Second),
		alert.NewClient("", ""), tell.NewHermodr(server.URL, "t"), time.UTC, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.watchPromises(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 2 || !strings.HasPrefix(asked[0], "/local Is this promise software development work") {
		t.Fatalf("asked %q", asked)
	}
	if marked["/v1/promises/1/dev"] != "dev" || marked["/v1/promises/2/dev"] != "not" || len(marked) != 2 {
		t.Fatalf("marked %v", marked)
	}
}
