package sindri

import (
	"context"
	"strings"
	"testing"
	"time"

	"yggdrasil/internal/tell"
)

type told struct{ messages []tell.Message }

func (t *told) On() bool { return true }
func (t *told) Tell(_ context.Context, m tell.Message) error {
	t.messages = append(t.messages, m)
	return nil
}

func TestNightOf(t *testing.T) {
	shift := NightShift{Start: "22:00", Report: "07:00"}
	at := func(clock string) time.Time { return must2(time.Parse("2006-01-02 15:04", "2026-11-17 "+clock)) }
	for clock, want := range map[string]string{"21:59": "", "22:00": "2026-11-17", "23:30": "2026-11-17", "03:00": "2026-11-16", "07:00": "", "12:00": ""} {
		if got := nightOf(at(clock), shift); got != want {
			t.Errorf("%s: got %q, want %q", clock, got, want)
		}
	}
	if got := nightOf(at("13:00"), NightShift{Start: "12:00", Report: "14:00"}); got != "2026-11-17" {
		t.Errorf("a day shift: got %q", got)
	}
}

func TestTheNightShiftStartsMarkedDraftsAndReportsInTheMorning(t *testing.T) {
	w, _ := newWork(t)
	hermodr := &told{}
	w.Hermodr, w.Zone = hermodr, time.UTC
	now := must2(time.Parse(time.DateTime, "2026-11-17 18:00:00"))
	w.Store.now = func() time.Time { return now }
	tonight, _ := w.Store.AddGoal("Leave it as it is", []string{"demo"}, "manual", "", "goal-1")
	later, _ := w.Store.AddGoal("Not tonight", []string{"demo"}, "manual", "", "goal-2")
	_ = w.Store.SetGoalNight(tonight.ID, true)
	ctx := context.Background()

	if err := w.RunNight(ctx); err != nil || must2(w.goalView(tonight.ID)).State != "draft" {
		t.Fatalf("started before the night: %v", err)
	}
	now = now.Add(4*time.Hour + 30*time.Minute) // 22:30
	if err := w.RunNight(ctx); err != nil {
		t.Fatal(err)
	}
	view := must2(w.goalView(tonight.ID))
	if view.State == "draft" || view.Night {
		t.Fatalf("did not start tonight's goal: %+v", view)
	}
	if must2(w.goalView(later.ID)).State != "draft" {
		t.Fatalf("started a goal not marked")
	}
	waitFor(t, w.Agents, view.Tasks[0].Agent, func(a *Agent) bool { return a.Status == Waiting || a.Status == Done })

	now = now.Add(5 * time.Hour) // 03:30, still the night
	if err := w.RunNight(ctx); err != nil || len(hermodr.messages) != 0 {
		t.Fatalf("reported during the night: %v %+v", err, hermodr.messages)
	}
	now = now.Add(4 * time.Hour) // 07:30
	if err := w.RunNight(ctx); err != nil || len(hermodr.messages) != 1 {
		t.Fatalf("no morning report: %v %+v", err, hermodr.messages)
	}
	if m := hermodr.messages[0]; !strings.Contains(m.Text, "Leave it as it is") || strings.Contains(m.Text, "Not tonight") {
		t.Fatalf("got %+v", m)
	}
	if err := w.RunNight(ctx); err != nil || len(hermodr.messages) != 1 {
		t.Fatalf("reported twice: %v", err)
	}
}
