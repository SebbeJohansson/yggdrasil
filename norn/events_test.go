package norn

import (
	"testing"
	"time"
)

func kinds(e *Events, after int64) []string {
	var out []string
	for _, event := range e.kept {
		if event.ID > after {
			out = append(out, event.Kind)
		}
	}
	return out
}

func TestCalendarEventsTellWhatChanged(t *testing.T) {
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	at := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	e := NewEvents(now)
	standup := CalendarEvent{ID: "a", Subject: "Standup", Start: at(time.Hour), End: at(90 * time.Minute)}
	review := CalendarEvent{ID: "b", Subject: "Review", Start: at(3 * time.Hour), End: at(4 * time.Hour)}
	e.Calendar("work", []CalendarEvent{standup, review}, now)
	if got := kinds(e, -1); len(got) != 0 {
		t.Fatalf("the first read sent %v", got)
	}

	moved := standup
	moved.Start, moved.End = at(2*time.Hour), at(150*time.Minute)
	planning := CalendarEvent{ID: "c", Subject: "Planning", Start: at(30 * time.Minute), End: at(time.Hour)}
	nextWeek := CalendarEvent{ID: "d", Subject: "Later", Start: at(72 * time.Hour), End: at(73 * time.Hour)}
	e.Calendar("work", []CalendarEvent{moved, planning, nextWeek}, now)
	got := map[string]int{}
	for _, kind := range kinds(e, -1) {
		got[kind]++
	}
	if got["calendar.moved"] != 1 || got["calendar.added"] != 1 || got["calendar.removed"] != 1 || len(got) != 3 {
		t.Fatalf("got %v", got)
	}

	e.Ended(now.Add(time.Hour + time.Minute))
	all := kinds(e, 3)
	if len(all) != 1 || all[0] != "calendar.ended" {
		t.Fatalf("after the planning meeting ended: %v", all)
	}
}

func TestNewInboxItemsSkipTheFirstPollAndQuietOnes(t *testing.T) {
	e := NewEvents(time.Now())
	old := Ranked{Item: Item{ID: "1"}, Band: "urgent"}
	if fresh := e.NewInboxItems([]Ranked{old}); len(fresh) != 0 {
		t.Fatalf("the first poll sent %v", fresh)
	}
	urgent := Ranked{Item: Item{ID: "2"}, Band: "urgent"}
	quiet := Ranked{Item: Item{ID: "3"}, Band: "quiet"}
	fresh := e.NewInboxItems([]Ranked{old, urgent, quiet})
	if len(fresh) != 1 || fresh[0].Item.ID != "2" {
		t.Fatalf("got %v", fresh)
	}
	if fresh := e.NewInboxItems([]Ranked{old, urgent, quiet}); len(fresh) != 0 {
		t.Fatalf("sent again: %v", fresh)
	}
}
