package huginn

import (
	"context"
	"testing"
	"time"
)

func TestASavePointTellsTheWidgetWhatYouWereOn(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 15, 0, 0, time.UTC)
	h, fake := newTest(t, now)
	h.DashboardURL = "http://localhost:17420/"
	ready := now.UnixMilli()
	event := norn("savepoint.ready", map[string]any{"id": 3, "meeting": "Standup", "title": "Fix the login", "readyAt": ready,
		"link":    "/dashboard/sessions/s2?dir=%2Fp&name=Fix+the+login",
		"session": map[string]any{"id": "s2", "title": "Fix the login", "lastPrompt": "make the button blue", "branch": "feature/login"}})
	fired, ok := h.FromNorn(event)
	if !ok {
		t.Fatal("savepoint.ready was not read")
	}
	h.Handle(context.Background(), fired)

	if len(fake.pushed) != 1 {
		t.Fatalf("sent %v", fake.pushed)
	}
	m := fake.pushed[0]
	if m.Title != "You were on: Fix the login" || m.Text != "Before Standup, your last prompt was: make the button blue" ||
		m.Link != "http://localhost:17420/dashboard/sessions/s2?dir=%2Fp&name=Fix+the+login" {
		t.Errorf("message %+v", m)
	}
	if m.Push == nil || *m.Push || m.Hold {
		t.Errorf("the prompt must stay off the phone and not wait: push %v hold %v", m.Push, m.Hold)
	}
	if got := reactions(t, h); len(got) != 1 || got[0].RuleID != "save-point" || got[0].State != "delivered" {
		t.Errorf("reactions %+v", got)
	}
}

func TestASavePointWithoutAPromptStillSaysWhatYouWereOn(t *testing.T) {
	fields := savePointFields(SavePoint{ID: 1, Title: "Save points"})
	if fields["said"] != "Before the meeting you were on Save points. Pick it up when it suits." {
		t.Errorf("said %q", fields["said"])
	}
	h, _ := newTest(t, time.Now())
	if _, ok := h.FromNorn(norn("savepoint.ready", map[string]any{"id": 0})); ok {
		t.Error("read a save point with nothing in it")
	}
}
