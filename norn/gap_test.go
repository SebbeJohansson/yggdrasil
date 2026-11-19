package norn

import (
	"context"
	"testing"
	"time"

	"yggdrasil/internal/contract/contracttest"
)

var gapZone, _ = time.LoadLocation("Europe/Stockholm")

// at is 2026-11-19 (a Thursday) at hh:mm in Stockholm.
func gapAt(hour, minute int) time.Time {
	return time.Date(2026, 11, 19, hour, minute, 0, 0, gapZone)
}

func meeting(id string, from, to time.Time) CalendarEvent {
	return CalendarEvent{ID: id, Subject: id, Start: from.UnixMilli(), End: to.UnixMilli(), ShowAs: "busy", Response: "accepted"}
}

func TestParseWorkHours(t *testing.T) {
	day, err := ParseWorkHours("08:30-16:00")
	if err != nil || day != (WorkDay{From: 510, To: 960}) {
		t.Fatalf("%+v %v", day, err)
	}
	for _, bad := range []string{"", "9-17", "17:00-09:00", "09:00"} {
		if _, err := ParseWorkHours(bad); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
}

func TestFreeUntilTheNextMeetingOrTheEndOfTheDay(t *testing.T) {
	day := WorkDay{From: 9 * 60, To: 17 * 60}
	events := []CalendarEvent{
		meeting("standup", gapAt(9, 0), gapAt(9, 15)),
		meeting("sync", gapAt(14, 0), gapAt(15, 0)),
	}
	until, free := FreeUntil(events, gapAt(10, 0), gapZone, day)
	if !free || !until.Equal(gapAt(14, 0)) {
		t.Fatalf("at 10: %v %v", until, free)
	}
	if _, free := FreeUntil(events, gapAt(14, 30), gapZone, day); free {
		t.Error("free in the middle of a meeting")
	}
	if until, free := FreeUntil(events, gapAt(15, 30), gapZone, day); !free || !until.Equal(gapAt(17, 0)) {
		t.Errorf("after the last meeting: %v %v", until, free)
	}
	if _, free := FreeUntil(nil, gapAt(17, 30), gapZone, day); free {
		t.Error("free after the working day")
	}
	if _, free := FreeUntil(nil, gapAt(8, 0), gapZone, day); free {
		t.Error("free before the working day")
	}
	if _, free := FreeUntil(nil, gapAt(10, 0).AddDate(0, 0, 2), gapZone, day); free {
		t.Error("free on a Saturday")
	}
}

func TestWhatHoldsYouForTheGap(t *testing.T) {
	day := WorkDay{From: 9 * 60, To: 17 * 60}
	next := meeting("sync", gapAt(11, 0), gapAt(12, 0))
	for name, change := range map[string]func(*CalendarEvent){
		"declined": func(e *CalendarEvent) { e.Response = "declined" },
		"all day":  func(e *CalendarEvent) { e.AllDay = true },
		"free":     func(e *CalendarEvent) { e.ShowAs = "free" },
	} {
		skipped := next
		change(&skipped)
		if until, _ := FreeUntil([]CalendarEvent{skipped}, gapAt(10, 0), gapZone, day); !until.Equal(gapAt(17, 0)) {
			t.Errorf("a %s meeting ends the gap: %v", name, until)
		}
	}
	brk := meeting("break", gapAt(10, 0), gapAt(10, 30))
	brk.Break = true
	if _, free := FreeUntil([]CalendarEvent{brk}, gapAt(10, 10), gapZone, day); free {
		t.Error("a break you kept free is a gap for work")
	}
	if until, _ := FreeUntil([]CalendarEvent{brk}, gapAt(9, 30), gapZone, day); !until.Equal(gapAt(10, 0)) {
		t.Errorf("a break ahead does not end the gap: %v", until)
	}
}

func goalWith(id int64, title, state, action, agent string, since int64) GapGoal {
	g := GapGoal{ID: id, Title: title, State: state, At: since}
	g.Next.Action, g.Next.Agent, g.Next.Text = action, agent, "Next "+action
	return g
}

func TestGoalsAndPromisesBecomeSizedItems(t *testing.T) {
	cases := []struct {
		goal    GapGoal
		kind    string
		minutes int
		link    string
	}{
		{goalWith(1, "Login", "needs-you", "answer", "a1", 0), "answer", 3, "/dashboard/goals/1?part=a1"},
		{goalWith(2, "Theme", "done", "review", "a2", 0), "review", 20, "/dashboard/goals/2?part=a2&view=review"},
		{goalWith(3, "Race", "done", "pick", "", 0), "pick", 10, "/dashboard/races/3"},
		{goalWith(4, "Draft", "draft", "start", "", 0), "start", 2, "/dashboard/goals/4"},
	}
	for _, c := range cases {
		item, ok := GoalGapItem(c.goal)
		if !ok || item.Kind != c.kind || item.Minutes != c.minutes || item.Link != c.link || item.GoalID != c.goal.ID {
			t.Errorf("%s: %+v %v", c.goal.Title, item, ok)
		}
	}
	night := goalWith(5, "Tonight", "draft", "start", "", 0)
	night.Night = true
	closed := int64(1)
	gone := goalWith(6, "Closed", "needs-you", "answer", "a", 0)
	gone.ClosedAt = &closed
	for _, g := range []GapGoal{night, gone, goalWith(7, "Busy", "running", "none", "", 0), goalWith(8, "Waits", "running", "resume", "", 0)} {
		if item, ok := GoalGapItem(g); ok {
			t.Errorf("%s offered: %+v", g.Title, item)
		}
	}

	promise := Promise{ID: 9, What: "Send the deck", To: "Anna", State: "open", Due: "2026-11-20", CreatedAt: 5}
	item, ok := PromiseGapItem(promise, "2026-11-20")
	if !ok || item.Kind != "promise" || item.Minutes != 10 || item.Link != PromisesLink || item.PromiseID != 9 || item.Text != "promised to Anna" {
		t.Errorf("promise: %+v %v", item, ok)
	}
	for _, p := range []Promise{{State: "open", Due: "2026-11-21"}, {State: "open"}, {State: "done", Due: "2026-11-19"}} {
		if _, ok := PromiseGapItem(p, "2026-11-20"); ok {
			t.Errorf("offered %+v", p)
		}
	}
}

func TestPickTheOneThatFits(t *testing.T) {
	review := GapItem{Kind: "review", Minutes: 20, Since: 1}
	promise := GapItem{Kind: "promise", Minutes: 10, Since: 2}
	answer := GapItem{Kind: "answer", Minutes: 3, Since: 9}
	start := GapItem{Kind: "start", Minutes: 2, Since: 3}

	if got := PickGapItem([]GapItem{review, promise, answer}, time.Hour); got == nil || got.Kind != "answer" {
		t.Errorf("what blocks an agent comes first: %+v", got)
	}
	if got := PickGapItem([]GapItem{start, review, promise}, time.Hour); got == nil || got.Kind != "review" {
		t.Errorf("then the oldest: %+v", got)
	}
	if got := PickGapItem([]GapItem{review, promise}, 20*time.Minute); got == nil || got.Kind != "promise" {
		t.Errorf("a review does not fit 20 minutes: %+v", got)
	}
	if got := PickGapItem([]GapItem{review}, 24*time.Minute); got != nil {
		t.Errorf("no slack left: %+v", got)
	}
	if got := PickGapItem([]GapItem{answer}, 9*time.Minute); got != nil {
		t.Errorf("under ten minutes: %+v", got)
	}
	if got := PickGapItem(nil, time.Hour); got != nil {
		t.Errorf("nothing waits: %+v", got)
	}
}

func TestTheGapRoute(t *testing.T) {
	n, p := newNorn(t)
	ctx := context.Background()
	now := gapAt(10, 0) // a Thursday morning, inside the default working day
	n.Gap.Goals = func(context.Context) []GapGoal {
		return []GapGoal{goalWith(1, "Theme", "done", "review", "a2", 1), goalWith(2, "Login", "needs-you", "answer", "a1", 2)}
	}

	if gap := n.GapAt(ctx, now); gap.Free || gap.Item != nil {
		t.Errorf("without a calendar: %+v", gap)
	}
	n.Accounts.Work.Set(CalendarSnapshot{At: now.UnixMilli(), Events: []CalendarEvent{meeting("later", gapAt(11, 30), gapAt(12, 0))}})
	contracttest.Run(t, p)
	if code, body := call(t, p, "GET", "/v1/gap", nil); code != 200 || body.(map[string]any)["free"] == nil {
		t.Errorf("route: %d %v", code, body)
	}

	gap := n.GapAt(ctx, now)
	if !gap.Free || gap.FreeUntil != gapAt(11, 30).UnixMilli() || gap.Item == nil || gap.Item.Kind != "answer" || gap.Item.GoalID != 2 || gap.Item.Link != "/dashboard/goals/2?part=a1" {
		t.Fatalf("free with goals waiting: %+v %+v", gap, gap.Item)
	}

	// A save point handed back and not put away comes first.
	sp := &SavePoint{At: gapAt(9, 0).UnixMilli(), MeetingID: "m", MeetingEnd: gapAt(9, 50).UnixMilli(), Title: "x"}
	if err := n.Store.AddSavePoint(ctx, sp); err != nil {
		t.Fatal(err)
	}
	_ = n.Store.SavePointReady(ctx, sp.ID, gapAt(9, 50).UnixMilli())
	if gap := n.GapAt(ctx, now); !gap.Free || gap.Item != nil {
		t.Errorf("offered over a save point: %+v", gap.Item)
	}
	if _, err := n.Store.SavePointDone(ctx, sp.ID, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if gap := n.GapAt(ctx, now); gap.Item == nil {
		t.Error("nothing after the save point was put away")
	}

	// A promise due tomorrow waits too; the older one goes first once no agent waits.
	n.Gap.Goals = func(context.Context) []GapGoal {
		return []GapGoal{goalWith(1, "Theme", "done", "review", "a2", gapAt(9, 0).UnixMilli())}
	}
	if _, err := n.Store.db.ExecContext(ctx, `INSERT INTO promises (source_key, norm, source, what, to_name, due, created_at, changed_at)
		VALUES ('k', 'send the deck', 'outlook', 'Send the deck', 'Anna', '2026-11-20', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if gap := n.GapAt(ctx, now); gap.Item == nil || gap.Item.Kind != "promise" || gap.Item.PromiseID == 0 {
		t.Errorf("the older promise: %+v", gap.Item)
	}

	// A gap under ten minutes offers nothing.
	n.Accounts.Work.Set(CalendarSnapshot{At: now.UnixMilli() + 1, Events: []CalendarEvent{meeting("soon", gapAt(10, 5), gapAt(11, 0))}})
	if gap := n.GapAt(ctx, now); !gap.Free || gap.Item != nil {
		t.Errorf("a five minute gap: %+v", gap)
	}
}
