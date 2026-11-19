package huginn

import (
	"context"
	"testing"
	"time"
)

func TestTheWeeksPlanIsOneCalmWidgetMessage(t *testing.T) {
	now := time.Date(2026, 11, 22, 18, 2, 0, 0, time.UTC)
	h, fake := newTest(t, now)
	h.DashboardURL = "http://localhost:17420/"
	fired, ok := h.FromNorn(norn("weave.ready", map[string]any{"week": "2026-11-23", "words": "The week ahead: focus time on Monday; Tuesday is heavy.",
		"madeAt": now.UnixMilli(), "link": "/dashboard/plan"}))
	if !ok {
		t.Fatal("weave.ready was not read")
	}
	h.Handle(context.Background(), fired)
	if len(fake.pushed) != 1 {
		t.Fatalf("sent %v", fake.pushed)
	}
	m := fake.pushed[0]
	if m.Title != "The week's plan" || m.Text != "The week ahead: focus time on Monday; Tuesday is heavy." || m.Link != "http://localhost:17420/dashboard/plan" {
		t.Errorf("message %+v", m)
	}
	if m.Push == nil || *m.Push || m.Hold {
		t.Errorf("the plan stays off the phone and does not wait: push %v hold %v", m.Push, m.Hold)
	}
	if got := reactions(t, h); len(got) != 1 || got[0].RuleID != "week-plan" || got[0].State != "delivered" {
		t.Errorf("reactions %+v", got)
	}
	if _, ok := h.FromNorn(norn("weave.ready", map[string]any{"words": "no week"})); ok {
		t.Error("read a plan without a week")
	}
}
