package sindri

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"yggdrasil/internal/call"
	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/part"
)

// countingWhy answers with sources and counts how often it is asked.
type countingWhy struct {
	mu      sync.Mutex
	asked   int
	sources []WhySource
}

func (c *countingWhy) Why(context.Context, *Goal) ([]WhySource, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked++
	return c.sources, nil
}

func (c *countingWhy) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.asked
}

// promptOf is what the agent was first told, once it has finished its turn.
func promptOf(t *testing.T, w *Work, agent string) string {
	t.Helper()
	// The fake CLI asks for a tool when its prompt says "ask", as the why does.
	waitFor(t, w.Agents, agent, func(a *Agent) bool { return a.Status == Waiting || a.Status == NeedsApproval })
	w.Agents.mu.Lock()
	defer w.Agents.mu.Unlock()
	for _, entry := range w.Agents.agents[agent].Log {
		if entry.Kind == "prompt" {
			return entry.Text
		}
	}
	return ""
}

func TestAGoalsAgentIsToldWhyTheGoalExists(t *testing.T) {
	w, _ := newWork(t)
	asker := &countingWhy{sources: []WhySource{{Kind: "meeting", At: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC).UnixMilli(),
		Summary: "Person 1 wants monthly totals only in the CSV export.", Ref: "obsidian:Budget - 2026-11-12.md"}}}
	w.Why, w.Zone = asker, time.UTC
	goal, _ := w.Store.AddGoal("Add a CSV export to the budget page", []string{"demo"}, "manual", "", "goal-1")
	started, _, err := w.StartGoal(goal.ID, nil)
	if err != nil || len(started) != 1 {
		t.Fatalf("got %+v %v", started, err)
	}
	prompt := promptOf(t, w, started[0].AgentID)
	if !strings.Contains(prompt, "## Why this goal exists") || !strings.Contains(prompt, "- 2026-11-12, meeting: Person 1 wants monthly totals only") || strings.Contains(prompt, "obsidian:") {
		t.Fatalf("prompt %q", prompt)
	}
	view := must2(w.goalView(goal.ID))
	if view.Why == nil || view.Why.State != "sent" || len(view.Why.Sources) != 1 || view.Why.Sources[0].Ref != "obsidian:Budget - 2026-11-12.md" {
		t.Fatalf("why %+v", view.Why)
	}

	// Within the hour, the same pack again: Norn is not asked twice.
	w.goalWhy(&goal)
	if asker.count() != 1 {
		t.Fatalf("asked %d times", asker.count())
	}
}

func TestNornDownNeverHoldsAGoalUp(t *testing.T) {
	w, _ := newWork(t)
	hang := make(chan struct{})
	norn := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { <-hang }))
	t.Cleanup(func() { close(hang); norn.Close() })
	before := WhyTimeout
	WhyTimeout = 200 * time.Millisecond
	t.Cleanup(func() { WhyTimeout = before })
	w.Why = NornWhy{Norn: call.New("norn", norn.URL, "t", time.Minute)}
	goal, _ := w.Store.AddGoal("Add a CSV export to the budget page", []string{"demo"}, "manual", "", "goal-1")
	at := time.Now()
	started, failed, err := w.StartGoal(goal.ID, nil)
	if err != nil || len(started) != 1 || len(failed) != 0 || time.Since(at) > 5*time.Second {
		t.Fatalf("got %+v %v %v after %s", started, failed, err, time.Since(at))
	}
	if prompt := promptOf(t, w, started[0].AgentID); strings.Contains(prompt, "Why this goal exists") || !strings.Contains(prompt, "Add a CSV export") {
		t.Fatalf("prompt %q", prompt)
	}
	if view := must2(w.goalView(goal.ID)); view.Why == nil || view.Why.State != "unreachable" {
		t.Fatalf("why %+v", view.Why)
	}

	// Norn not there at all: the same.
	w.Why = NornWhy{Norn: call.New("norn", "http://127.0.0.1:1", "t", time.Second)}
	other, _ := w.Store.AddGoal("Leave it as it is", []string{"demo"}, "manual", "", "goal-2")
	started, _, err = w.StartGoal(other.ID, nil)
	if err != nil || len(started) != 1 {
		t.Fatalf("got %+v %v", started, err)
	}
	waitFor(t, w.Agents, started[0].AgentID, func(a *Agent) bool { return a.Status == Waiting })
}

func TestWhyTurnedOffAsksNornNothing(t *testing.T) {
	w, _ := newWork(t)
	asker := &countingWhy{sources: []WhySource{{Kind: "mail", Summary: "anything"}}}
	w.Why = asker
	p := part.New(Name, contracttest.Tokens(), nil)
	w.mountWhy(p)
	patch := func(token, body string) int {
		request := httptest.NewRequest("PATCH", "/v1/settings/why", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		p.ServeHTTP(recorder, request)
		return recorder.Code
	}
	if !w.WhyOn().On {
		t.Fatal("off before it was turned off")
	}
	if code := patch(contracttest.Sender, `{"on":false}`); code != http.StatusForbidden {
		t.Fatalf("a part turned it off: %d", code)
	}
	if code := patch(contracttest.Owner, `{}`); code != http.StatusBadRequest {
		t.Fatalf("no `on`: %d", code)
	}
	if code := patch(contracttest.Owner, `{"on":false}`); code != http.StatusOK || w.WhyOn().On {
		t.Fatalf("not turned off: %d", code)
	}
	goal, _ := w.Store.AddGoal("Add a CSV export to the budget page", []string{"demo"}, "manual", "", "goal-1")
	started, _, err := w.StartGoal(goal.ID, nil)
	if err != nil || len(started) != 1 {
		t.Fatalf("got %+v %v", started, err)
	}
	if asker.count() != 0 || strings.Contains(promptOf(t, w, started[0].AgentID), "Why this goal exists") {
		t.Fatalf("asked %d times", asker.count())
	}
	if view := must2(w.goalView(goal.ID)); view.Why == nil || view.Why.State != "off" {
		t.Fatalf("why %+v", view.Why)
	}
}

func TestASelfRepairGoalIsNotAskedAbout(t *testing.T) {
	w, _ := newWork(t)
	asker := &countingWhy{}
	w.Why = asker
	goal, _ := w.Store.AddGoal("Self-repair: Norn is down", []string{"demo"}, SelfRepairSource+"alert:ratatoskr:down:norn", "", "goal-1")
	started, _, err := w.StartGoal(goal.ID, nil)
	if err != nil || len(started) != 1 || asker.count() != 0 {
		t.Fatalf("asked %d times: %v", asker.count(), err)
	}
	waitFor(t, w.Agents, started[0].AgentID, func(a *Agent) bool { return a.Status == Waiting })
}

func TestNornWhyAsksNornWithTheGoal(t *testing.T) {
	var got map[string]any
	norn := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/why" || r.Header.Get("Authorization") != "Bearer t" {
			http.Error(rw, "no", http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		part.JSON(rw, 200, map[string]any{"sources": []map[string]any{{"kind": "mail", "at": 5, "summary": "Asked for it.", "ref": "inbox:1"}}})
	}))
	t.Cleanup(norn.Close)
	sources, err := NornWhy{Norn: call.New("norn", norn.URL, "t", time.Second)}.Why(context.Background(), &Goal{ID: 3, Title: "Add it", Context: "bg", Repos: []string{"demo"}})
	if err != nil || len(sources) != 1 || sources[0].Ref != "inbox:1" {
		t.Fatalf("got %+v %v", sources, err)
	}
	if got["goal"] != "Add it" || got["context"] != "bg" || got["goalId"] != float64(3) {
		t.Fatalf("sent %v", got)
	}
}

func TestWhySectionIsEmptyWithoutSources(t *testing.T) {
	if WhySection(nil, time.UTC) != "" {
		t.Fatal("a section for nothing")
	}
	section := WhySection([]WhySource{{Kind: "rewind", Summary: "Decided\nto keep it."}}, time.UTC)
	if !strings.HasSuffix(section, "- undated, rewind: Decided to keep it.") {
		t.Fatalf("section %q", section)
	}
}
