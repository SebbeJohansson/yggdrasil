package norn

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// The week of 24 November 2026: four meetings copied from an email, then
// the invites that came after.
func suggestionWeek(t *testing.T) (events, cancelled []CalendarEvent, now time.Time) {
	t.Helper()
	at := func(day, hour, minute int) int64 {
		return time.Date(2026, 11, day, hour, minute, 0, 0, time.UTC).UnixMilli()
	}
	own := func(id, subject string, day, h1, m1, h2, m2 int) CalendarEvent {
		return CalendarEvent{ID: id, Subject: subject, Start: at(day, h1, m1), End: at(day, h2, m2), Response: "organizer", ShowAs: "free"}
	}
	invite := func(id, subject, organizer string, day, h1, m1, h2, m2 int) CalendarEvent {
		return CalendarEvent{ID: id, Subject: subject, Organizer: organizer, Start: at(day, h1, m1), End: at(day, h2, m2),
			Response: "notResponded", Attendees: []string{organizer, "Team Acme"}}
	}
	events = []CalendarEvent{
		invite("i-breakfast", "November breakfast 🍂☕🥐", "Sara Nyberg", 24, 8, 30, 10, 0),
		own("c-breakfast", "☕ Cozy breakfast", 24, 8, 30, 10, 0),
		invite("i-121", "One-to-one Alex & Johan", "Johan Olsson", 24, 9, 30, 10, 0),
		own("c-townhall", "🎤 Coffee & sandwiches + Townhall", 25, 9, 0, 10, 30),
		own("c-pizza", "🍕 Pizza lunch", 26, 12, 0, 13, 0),
		invite("i-pizza", "🍕 Pizza lunch ", "Karin Lund", 26, 12, 0, 13, 0),
		invite("i-ai", "🤖 AI – nuläge & möjligheter med Alex", "Eva Strand", 26, 15, 0, 16, 0),
		own("c-friday", "🖤 Black Friday mingle", 27, 14, 14, 15, 14),
		invite("i-friday", "🖤 BLACK FRIDAY! mingle with afternoon snacks and bubbly 🥂", "Karin Lund", 27, 14, 14, 15, 14),
		// A meeting of yours with people in it is never a copy.
		{ID: "mine-with-people", Subject: "Pizza planning", Start: at(26, 12, 0), End: at(26, 12, 30), Response: "organizer", Attendees: []string{"Vera"}},
	}
	cancelled = []CalendarEvent{invite("i-townhall", "Avbruten: Townhall", "Karin Lund", 25, 9, 30, 10, 30)}
	return events, cancelled, time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
}

func TestCalendarSuggestions(t *testing.T) {
	events, cancelled, now := suggestionWeek(t)
	got := map[string]string{}
	for _, s := range CalendarSuggestions(events, cancelled, nil, now) {
		got[s.Copy.ID] = s.Kind + " of " + s.Invite.ID
	}
	want := map[string]string{
		"c-breakfast": "duplicate of i-breakfast",
		"c-townhall":  "stale of i-townhall",
		"c-pizza":     "duplicate of i-pizza",
		"c-friday":    "duplicate of i-friday",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for id, kind := range want {
		if got[id] != kind {
			t.Errorf("%s: got %q, want %q", id, got[id], kind)
		}
	}
}

func TestCalendarSuggestionsLeaveOutPastKeptAndDeclined(t *testing.T) {
	events, cancelled, _ := suggestionWeek(t)
	// On Thursday at noon the breakfast and the Townhall are over.
	thursday := time.Date(2026, 11, 26, 11, 0, 0, 0, time.UTC)
	kept := []string{suggestionID("c-pizza")}
	for i := range events {
		if events[i].ID == "i-friday" {
			events[i].Response = "declined"
		}
	}
	if got := CalendarSuggestions(events, cancelled, kept, thursday); len(got) != 0 {
		t.Fatalf("want none, got %+v", got)
	}
}

func TestSuggestionEventsOnlyForNewOnes(t *testing.T) {
	events, cancelled, now := suggestionWeek(t)
	e := NewEvents(now)
	all := CalendarSuggestions(events, cancelled, nil, now)
	e.Suggestions("work", all[:1])
	if len(e.kept) != 0 {
		t.Fatalf("the first look only takes note, sent %d", len(e.kept))
	}
	e.Suggestions("work", all)
	e.Suggestions("work", all)
	if len(e.kept) != len(all)-1 {
		t.Fatalf("want %d events, got %d", len(all)-1, len(e.kept))
	}
	var change CalendarChange
	_ = json.Unmarshal(e.kept[0].Data, &change)
	if !strings.HasPrefix(e.kept[0].Kind, "calendar.") || change.Invite == nil || change.Event.Response != "organizer" {
		t.Fatalf("got %s %+v", e.kept[0].Kind, change)
	}
}

func TestApplyAndKeepSuggestion(t *testing.T) {
	n, p := newNorn(t)
	events, cancelled, _ := suggestionWeek(t)
	now := time.Now()
	// Move the week to tomorrow, so it is ahead whenever the test runs.
	shift := now.Add(24*time.Hour).UnixMilli() - events[0].Start
	for _, list := range [][]CalendarEvent{events, cancelled} {
		for i := range list {
			list[i].Start += shift
			list[i].End += shift
		}
	}
	n.Accounts.Work.Set(CalendarSnapshot{Events: events, Cancelled: cancelled, At: now.UnixMilli()})
	n.Accounts.Microsoft.stored = &microsoftStored{Scopes: MicrosoftScopes{Calendar: true}, RefreshToken: "r", AccessToken: "a", ExpiresAt: now.Add(time.Hour).UnixMilli()}

	status, body := call(t, p, http.MethodGet, "/v1/calendar/suggestions", nil)
	if status != http.StatusOK || len(body.(map[string]any)["suggestions"].([]any)) != 4 {
		t.Fatalf("got %d %v", status, body)
	}

	var deleted string
	n.Accounts.del = func(_ context.Context, target, token string) (int, []byte, error) {
		deleted = target
		return http.StatusNoContent, nil, nil
	}
	n.Accounts.get = func(context.Context, string, string, map[string]string) (int, []byte, error) {
		return http.StatusOK, []byte(`{"value":[]}`), nil
	}
	if status, body := call(t, p, http.MethodPost, "/v1/calendar/suggestions/"+suggestionID("c-pizza")+"/apply", nil); status != http.StatusOK {
		t.Fatalf("apply: %d %v", status, body)
	}
	if !strings.HasSuffix(deleted, "/me/events/c-pizza") {
		t.Fatalf("deleted %q", deleted)
	}

	// Only a current suggestion's copy can be deleted: never someone else's meeting.
	n.Accounts.Work.Set(CalendarSnapshot{Events: events, Cancelled: cancelled, At: now.UnixMilli() + 1})
	if status, _ := call(t, p, http.MethodPost, "/v1/calendar/suggestions/"+suggestionID("i-pizza")+"/apply", nil); status != http.StatusNotFound {
		t.Fatalf("an invite: want 404, got %d", status)
	}

	n.Accounts.del = func(context.Context, string, string) (int, []byte, error) {
		return http.StatusForbidden, []byte(`{"error":{"code":"ErrorAccessDenied"}}`), nil
	}
	if status, _ := call(t, p, http.MethodPost, "/v1/calendar/suggestions/"+suggestionID("c-friday")+"/apply", nil); status != http.StatusConflict {
		t.Fatalf("read-only sign-in: want 409, got %d", status)
	}

	if status, _ := call(t, p, http.MethodPost, "/v1/calendar/suggestions/"+suggestionID("c-friday")+"/keep", nil); status != http.StatusOK {
		t.Fatalf("keep: %d", status)
	}
	_, body = call(t, p, http.MethodGet, "/v1/calendar/suggestions", nil)
	if left := len(body.(map[string]any)["suggestions"].([]any)); left != 3 {
		t.Fatalf("after keeping one: want 3, got %d", left)
	}
}

func TestAnInviteThatLeftTheCalendarMakesYourCopyStale(t *testing.T) {
	events, _, now := suggestionWeek(t)
	var c Calendar
	townhall := CalendarEvent{ID: "i-townhall", Subject: "Townhall", Organizer: "Karin Lund", Response: "notResponded",
		Start: events[3].Start + 30*60*1000, End: events[3].End, Attendees: []string{"Team Acme"}}
	c.Set(CalendarSnapshot{Events: append(slices.Clone(events), townhall), At: now.UnixMilli()})
	c.Set(CalendarSnapshot{Events: events, At: now.Add(2 * time.Minute).UnixMilli()})
	snapshot := c.Current(now.Add(3 * time.Minute))
	found := false
	for _, s := range CalendarSuggestions(snapshot.Events, snapshot.Cancelled, nil, now) {
		if s.Copy.ID == "c-townhall" {
			found = s.Kind == "stale" && s.Invite.ID == "i-townhall"
		}
	}
	if !found {
		t.Fatalf("the Townhall copy is not stale: %+v", snapshot.Cancelled)
	}
}
