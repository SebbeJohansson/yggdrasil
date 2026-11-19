package norn

import (
	"context"
	"net/http"
	"testing"
	"time"

	"yggdrasil/internal/contract/contracttest"
)

func TestTheMeetingARecapIsOfferedBefore(t *testing.T) {
	soon := CalendarEvent{ID: "m", Subject: "Sync", Start: recapNow.Add(8 * time.Minute).UnixMilli(), End: recapNow.Add(38 * time.Minute).UnixMilli(),
		Attendees: []string{"Anna Berg"}, Response: "accepted", ShowAs: "busy"}
	if got, ok := NextRecapMeeting([]CalendarEvent{soon}, recapNow); !ok || got.ID != "m" {
		t.Fatalf("%v %+v", ok, got)
	}
	later := soon
	later.Start = recapNow.Add(20 * time.Minute).UnixMilli()
	if _, ok := NextRecapMeeting([]CalendarEvent{later}, recapNow); ok {
		t.Error("offered twenty minutes ahead")
	}
	for name, change := range map[string]func(*CalendarEvent){
		"declined": func(e *CalendarEvent) { e.Response = "declined" },
		"free":     func(e *CalendarEvent) { e.ShowAs = "free" },
		"alone":    func(e *CalendarEvent) { e.Attendees = nil },
		"started":  func(e *CalendarEvent) { e.Start = recapNow.Add(-time.Minute).UnixMilli() },
	} {
		skipped := soon
		change(&skipped)
		if _, ok := NextRecapMeeting([]CalendarEvent{skipped}, recapNow); ok {
			t.Errorf("offered for a %s meeting", name)
		}
	}
	brk := CalendarEvent{ID: "b", Subject: "Break", Start: recapNow.Add(-5 * time.Minute).UnixMilli(), End: recapNow.Add(5 * time.Minute).UnixMilli(), Break: true}
	if _, ok := NextRecapMeeting([]CalendarEvent{brk, soon}, recapNow); ok {
		t.Error("offered during a break")
	}
	focus := brk
	focus.Break, focus.Focus = false, true
	if _, ok := NextRecapMeeting([]CalendarEvent{focus, soon}, recapNow); ok {
		t.Error("offered during a focus block")
	}
}

func TestTheMainPeopleOfAMeeting(t *testing.T) {
	small := []invitee{{Name: "Bo"}, {Name: "Anna", Organizer: true}}
	if got := MainPeople(small); len(got) != 2 || got[0].Name != "Anna" {
		t.Errorf("small: %+v", got)
	}
	big := []invitee{{Name: "A"}, {Name: "B"}, {Name: "C", Organizer: true}, {Name: "D"}}
	if got := MainPeople(big); len(got) != 1 || got[0].Name != "C" {
		t.Errorf("big: %+v", got)
	}
	if got := MainPeople(big[:2:2]); len(got) != 2 {
		t.Errorf("two: %+v", got)
	}
	yours := []invitee{{Name: "A"}, {Name: "B"}, {Name: "C"}, {Name: "D"}}
	if got := MainPeople(yours); len(got) != 0 {
		t.Errorf("a big meeting you organize: %+v", got)
	}
}

func TestTheNextRecapRoute(t *testing.T) {
	n, p := newNorn(t)
	n.Recaps.Exchanges = func(_ context.Context, person RecapPerson, _ time.Time) ([]Exchange, RecapPerson) { return nil, person }
	n.Recaps.Invited = func(context.Context, CalendarEvent) []invitee {
		return []invitee{{Name: "Anna Berg", Address: "anna@example.com", Organizer: true}}
	}
	if code, body := call(t, p, "GET", "/v1/people/recap/next", nil); code != http.StatusOK || body.(map[string]any)["meeting"] != nil {
		t.Fatalf("without a calendar: %d %v", code, body)
	}
	now := time.Now()
	n.Accounts.Work.Set(CalendarSnapshot{At: now.UnixMilli(), Events: []CalendarEvent{{ID: "m", Subject: "Sync", Start: now.Add(5 * time.Minute).UnixMilli(),
		End: now.Add(35 * time.Minute).UnixMilli(), Attendees: []string{"Anna Berg"}, Response: "accepted", ShowAs: "busy"}}})
	contracttest.Run(t, p)
	code, body := call(t, p, "GET", "/v1/people/recap/next", nil)
	next := body.(map[string]any)
	if code != http.StatusOK || next["meeting"] == nil || len(next["people"].([]any)) != 1 {
		t.Fatalf("next: %d %v", code, body)
	}
}
