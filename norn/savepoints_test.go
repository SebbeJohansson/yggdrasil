package norn

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"yggdrasil/internal/contract/contracttest"
)

func TestSavePointMeetingSkipsWhatIsNotAMeeting(t *testing.T) {
	meeting := CalendarEvent{ID: "a", Subject: "Standup", ShowAs: "busy", Response: "accepted"}
	if !SavePointMeeting(meeting) {
		t.Fatal("a meeting you accepted does not count")
	}
	for name, change := range map[string]func(*CalendarEvent){
		"declined": func(e *CalendarEvent) { e.Response = "declined" },
		"all day":  func(e *CalendarEvent) { e.AllDay = true },
		"free":     func(e *CalendarEvent) { e.ShowAs = "free" },
		"a break":  func(e *CalendarEvent) { e.Break = true },
		"no id":    func(e *CalendarEvent) { e.ID = "" },
	} {
		skipped := meeting
		change(&skipped)
		if SavePointMeeting(skipped) {
			t.Errorf("%s counts as a meeting", name)
		}
	}
}

func TestPickSavePointTakesWhatMovedInTheLastHalfHour(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	closed := ago(time.Hour)
	goal := func(id int64, title, state string, made int64, active ...int64) GoalActivity {
		g := GoalActivity{ID: id, Title: title, State: state, At: made}
		for _, at := range active {
			g.Tasks = append(g.Tasks, struct {
				LastActivity int64 `json:"lastActivity"`
			}{at})
		}
		return g
	}
	goals := []GoalActivity{
		goal(1, "Old goal", "running", ago(48*time.Hour), ago(2*time.Hour)),
		goal(2, "Save points", "running", ago(24*time.Hour), ago(40*time.Minute), ago(5*time.Minute)),
		goal(3, "Merged one", "merged", ago(time.Hour), ago(time.Minute)),
		{ID: 4, Title: "Closed one", State: "draft", At: ago(time.Minute), ClosedAt: &closed},
		goal(5, "Quiet draft", "draft", ago(20*time.Minute)),
	}
	long := strings.Repeat("ä", 300)
	sessions := []OwnSession{
		{ID: "s1", Title: "Older session", LastActivity: ago(45 * time.Minute)},
		{ID: "s2", Title: "Fix the login", Cwd: "/home/me/projects/demo", Project: "demo", Branch: "feature/login", LastPrompt: long, LastActivity: ago(12 * time.Minute)},
		{ID: "s3", Title: "Earlier today", LastActivity: ago(25 * time.Minute)},
	}
	g, s := PickSavePoint(goals, sessions, now)
	if g == nil || g.ID != 2 || g.LastActivity != ago(5*time.Minute) {
		t.Fatalf("goal %+v", g)
	}
	if s == nil || s.ID != "s2" || s.Branch != "feature/login" || s.Project != "demo" {
		t.Fatalf("session %+v", s)
	}
	if got := []rune(s.LastPrompt); len(got) != SavePointPrompt || got[len(got)-1] != '…' {
		t.Errorf("the prompt is %d characters", len(got))
	}

	sp := NewSavePoint(CalendarEvent{ID: "m", Subject: "Standup"}, g, s, now)
	if sp.Title != "Save points" || sp.Link != "/dashboard/goals/2" {
		t.Errorf("the goal moved last, so it leads: %+v", sp)
	}
	sp = NewSavePoint(CalendarEvent{ID: "m"}, nil, s, now)
	if sp.Title != "Fix the login" || sp.Link != "/dashboard/sessions/s2?dir=%2Fhome%2Fme%2Fprojects%2Fdemo&name=Fix+the+login" {
		t.Errorf("without a goal the session leads: %+v", sp)
	}

	if g, s := PickSavePoint(goals[:1], sessions[:1], now); g != nil || s != nil {
		t.Errorf("nothing moved in the last half hour, but got %+v %+v", g, s)
	}
	if NewSavePoint(CalendarEvent{}, nil, nil, now) != nil {
		t.Error("a save point with nothing in it")
	}
}

func TestASavePointIsKeptAtTheStartAndHandedBackAtTheEnd(t *testing.T) {
	n, _ := newNorn(t)
	ctx := context.Background()
	start := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	at := func(d time.Duration) int64 { return start.Add(d).UnixMilli() }
	sessions := []OwnSession{{ID: "s2", Title: "Fix the login", Cwd: "/p/demo", LastPrompt: "make the button blue", LastActivity: at(-10 * time.Minute)}}
	n.SavePoints.Goals = func(context.Context) []GoalActivity { return nil }
	n.SavePoints.Sessions = func(context.Context) []OwnSession { return sessions }
	n.SavePoints.startedAfter = start.Add(-time.Hour)

	standup := CalendarEvent{ID: "m1", Subject: "Standup", Start: at(0), End: at(15 * time.Minute)}
	lunch := CalendarEvent{ID: "m2", Subject: "Lunch", Start: at(0), End: at(time.Hour), ShowAs: "free"}
	n.Events.Calendar("work", []CalendarEvent{standup, lunch}, start.Add(-time.Hour))

	if err := n.SavePointTick(ctx, start.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	points, _ := n.Store.SavePoints(ctx, 10)
	if len(points) != 1 || points[0].Meeting != "Standup" || points[0].ReadyAt != nil || points[0].Session.LastPrompt != "make the button blue" {
		t.Fatalf("at the start: %+v", points)
	}
	before := n.Events.next
	if err := n.SavePointTick(ctx, start.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := kinds(n.Events, before); len(got) != 0 {
		t.Fatalf("during the meeting: %v", got)
	}

	if err := n.SavePointTick(ctx, start.Add(15*time.Minute+10*time.Second)); err != nil {
		t.Fatal(err)
	}
	got := kinds(n.Events, before)
	if len(got) != 1 || got[0] != "savepoint.ready" {
		t.Fatalf("at the end: %v", got)
	}
	var ready SavePoint
	_ = json.Unmarshal(n.Events.kept[len(n.Events.kept)-1].Data, &ready)
	if ready.ID != points[0].ID || ready.ReadyAt == nil || ready.Title != "Fix the login" || ready.Session.LastPrompt != "make the button blue" {
		t.Errorf("savepoint.ready carried %+v", ready)
	}
	if err := n.SavePointTick(ctx, start.Add(16*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := kinds(n.Events, before); len(got) != 1 {
		t.Errorf("handed back twice: %v", got)
	}

	done, err := n.Store.SavePointDone(ctx, ready.ID, at(20*time.Minute))
	if err != nil || done == nil || done.DoneAt == nil {
		t.Fatalf("done: %+v %v", done, err)
	}
	if missing, _ := n.Store.SavePointDone(ctx, 999, at(20*time.Minute)); missing != nil {
		t.Error("put away one that is not there")
	}
}

func TestNoSavePointWhenNothingWasActiveOrTheMeetingStartedBeforeNorn(t *testing.T) {
	n, _ := newNorn(t)
	ctx := context.Background()
	start := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	at := func(d time.Duration) int64 { return start.Add(d).UnixMilli() }
	sessions := []OwnSession{{ID: "s", Title: "Yesterday", LastActivity: at(-3 * time.Hour)}}
	n.SavePoints.Goals = func(context.Context) []GoalActivity { return nil }
	n.SavePoints.Sessions = func(context.Context) []OwnSession { return sessions }
	n.SavePoints.startedAfter = start.Add(-time.Minute)
	n.Events.Calendar("work", []CalendarEvent{{ID: "m", Subject: "Review", Start: at(0), End: at(time.Hour)}}, start.Add(-time.Hour))
	_ = n.SavePointTick(ctx, start.Add(10*time.Second))
	if points, _ := n.Store.SavePoints(ctx, 10); len(points) != 0 {
		t.Fatalf("nothing was active, but got %+v", points)
	}

	// Norn started after the meeting did: no save point for it.
	sessions[0].LastActivity = at(-time.Minute)
	n.SavePoints.startedAfter = start.Add(time.Minute)
	_ = n.SavePointTick(ctx, start.Add(2*time.Minute))
	if points, _ := n.Store.SavePoints(ctx, 10); len(points) != 0 {
		t.Fatalf("saved for a meeting already running: %+v", points)
	}
}

func TestSavePointRoutesMeetTheContract(t *testing.T) {
	n, p := newNorn(t)
	ctx := context.Background()
	now := time.Now()
	sp := NewSavePoint(CalendarEvent{ID: "m", Subject: "Standup", Start: now.UnixMilli(), End: now.Add(time.Hour).UnixMilli()},
		&SavedGoal{ID: 7, Title: "Save points", LastActivity: now.UnixMilli()}, &SavedSession{ID: "s", Title: "x", LastPrompt: "y"}, now)
	if err := n.Store.AddSavePoint(ctx, sp); err != nil {
		t.Fatal(err)
	}
	contracttest.Run(t, p)

	code, body := call(t, p, "GET", "/v1/savepoints?limit=5", nil)
	list, _ := body.(map[string]any)["savepoints"].([]any)
	if code != 200 || len(list) != 1 {
		t.Fatalf("%d %v", code, body)
	}
	if code, _ := call(t, p, "GET", "/v1/savepoints?limit=0", nil); code != 400 {
		t.Errorf("limit 0: %d", code)
	}
	if code, body := call(t, p, "POST", "/v1/savepoints/1/done", nil); code != 200 || body.(map[string]any)["doneAt"] == nil {
		t.Errorf("done: %d %v", code, body)
	}
	if code, _ := call(t, p, "POST", "/v1/savepoints/42/done", nil); code != 404 {
		t.Errorf("a missing one: %d", code)
	}
}
