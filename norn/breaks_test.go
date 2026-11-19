package norn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// graphEvent is one event as calendarView gives it, times in UTC.
func graphEvent(id, subject string, start, end time.Time, categories ...string) map[string]any {
	at := func(t time.Time) map[string]string {
		return map[string]string{"dateTime": t.UTC().Format("2006-01-02T15:04:05.0000000"), "timeZone": "UTC"}
	}
	return map[string]any{"id": id, "subject": subject, "start": at(start), "end": at(end), "showAs": "busy",
		"responseStatus": map[string]string{"response": "organizer"}, "categories": categories}
}

func TestABreakIsKnownByItsCategory(t *testing.T) {
	start := time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)
	page, _ := json.Marshal(map[string]any{"value": []any{
		graphEvent("b", "Break", start, start.Add(30*time.Minute), "Blue", BreakCategory),
		graphEvent("m", "Break", start.Add(time.Hour), start.Add(2*time.Hour)),
	}})
	events, _ := GraphEvents(page)
	if len(events) != 2 || !events[0].Break || events[1].Break {
		t.Fatalf("got %+v", events)
	}
	// Only the one with the category: a meeting someone named "Break" is a meeting.
	encoded, _ := json.Marshal(events[0])
	if !strings.Contains(string(encoded), `"break":true`) {
		t.Fatalf("the payload has no break flag: %s", encoded)
	}
	if encoded, _ = json.Marshal(events[1]); strings.Contains(string(encoded), `"break"`) {
		t.Fatalf("a meeting carries a break flag: %s", encoded)
	}
}

func TestCheckBreak(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	for _, c := range []struct {
		slot BreakRequest
		ok   bool
	}{
		{BreakRequest{ms(time.Hour), ms(time.Hour + 15*time.Minute)}, true},
		{BreakRequest{ms(-10 * time.Minute), ms(20 * time.Minute)}, true},
		{BreakRequest{ms(time.Hour), ms(time.Hour + 10*time.Minute)}, false},
		{BreakRequest{ms(time.Hour), ms(3*time.Hour + time.Minute)}, false},
		{BreakRequest{ms(-time.Hour), ms(-30 * time.Minute)}, false},
		{BreakRequest{ms(8 * 24 * time.Hour), ms(8*24*time.Hour + time.Hour)}, false},
		{BreakRequest{}, false},
	} {
		if got := CheckBreak(c.slot, now) == ""; got != c.ok {
			t.Errorf("%+v: ok %v, want %v (%s)", c.slot, got, c.ok, CheckBreak(c.slot, now))
		}
	}
}

func TestKeepABreakFree(t *testing.T) {
	n, p := newNorn(t)
	now := time.Now().Truncate(time.Minute)
	meetingStart := now.Add(2 * time.Hour)
	calendar := []any{graphEvent("m1", "Planning", meetingStart, meetingStart.Add(time.Hour))}
	n.Accounts.Microsoft.stored = &microsoftStored{Scopes: MicrosoftScopes{Calendar: true}, RefreshToken: "r", AccessToken: "a", ExpiresAt: now.Add(time.Hour).UnixMilli()}
	n.Accounts.get = func(context.Context, string, string, map[string]string) (int, []byte, error) {
		page, _ := json.Marshal(map[string]any{"value": calendar})
		return http.StatusOK, page, nil
	}
	posts := 0
	var sent map[string]any
	n.Accounts.post = func(_ context.Context, target, _ string, body []byte) (int, []byte, error) {
		posts++
		_ = json.Unmarshal(body, &sent)
		start, _ := time.Parse("2006-01-02T15:04:05", sent["start"].(map[string]any)["dateTime"].(string))
		end, _ := time.Parse("2006-01-02T15:04:05", sent["end"].(map[string]any)["dateTime"].(string))
		made := graphEvent(fmt.Sprintf("b%d", posts), "Break", start, end, BreakCategory)
		calendar = append(calendar, made)
		answer, _ := json.Marshal(made)
		return http.StatusCreated, answer, nil
	}
	slot := map[string]int64{"start": meetingStart.Add(time.Hour).UnixMilli(), "end": meetingStart.Add(90 * time.Minute).UnixMilli()}

	status, body := call(t, p, http.MethodPost, "/v1/calendar/breaks", slot)
	if status != http.StatusCreated {
		t.Fatalf("keep: %d %v", status, body)
	}
	event := body.(map[string]any)["event"].(map[string]any)
	if event["break"] != true || int64(event["start"].(float64)) != slot["start"] {
		t.Fatalf("got %v", event)
	}
	if sent["showAs"] != "busy" || sent["attendees"] != nil || !strings.Contains(fmt.Sprint(sent["categories"]), BreakCategory) {
		t.Fatalf("asked Graph for %v", sent)
	}
	// The calendar shows it as a break.
	_, cal := call(t, p, http.MethodGet, "/v1/calendar", nil)
	found := false
	for _, e := range cal.(map[string]any)["events"].([]any) {
		found = found || e.(map[string]any)["break"] == true
	}
	if !found {
		t.Fatalf("no break in the calendar: %v", cal)
	}

	// The same slot again is the same event, not a second one.
	if status, body := call(t, p, http.MethodPost, "/v1/calendar/breaks", slot); status != http.StatusOK || posts != 1 {
		t.Fatalf("again: %d %v, %d posts", status, body, posts)
	}

	// A slot a meeting took is refused.
	taken := map[string]int64{"start": meetingStart.Add(30 * time.Minute).UnixMilli(), "end": meetingStart.Add(75 * time.Minute).UnixMilli()}
	if status, body := call(t, p, http.MethodPost, "/v1/calendar/breaks", taken); status != http.StatusConflict || posts != 1 {
		t.Fatalf("taken: %d %v", status, body)
	}

	// Too long, and not yours to ask.
	long := map[string]int64{"start": now.Add(5 * time.Hour).UnixMilli(), "end": now.Add(8 * time.Hour).UnixMilli()}
	if status, _ := call(t, p, http.MethodPost, "/v1/calendar/breaks", long); status != http.StatusBadRequest {
		t.Fatalf("too long: %d", status)
	}

	// A read-only sign-in says so.
	n.Accounts.post = func(context.Context, string, string, []byte) (int, []byte, error) {
		return http.StatusForbidden, []byte(`{"error":{"code":"ErrorAccessDenied"}}`), nil
	}
	free := map[string]int64{"start": now.Add(5 * time.Hour).UnixMilli(), "end": now.Add(5*time.Hour + 20*time.Minute).UnixMilli()}
	if status, _ := call(t, p, http.MethodPost, "/v1/calendar/breaks", free); status != http.StatusConflict {
		t.Fatalf("read-only sign-in: %d", status)
	}
}

func TestABreakIsNotAMeetingToBriefOn(t *testing.T) {
	now := time.Now()
	e := NewEvents(now)
	e.Calendar("work", []CalendarEvent{}, now)
	e.Calendar("work", []CalendarEvent{{ID: "b", Subject: "Break", Start: now.Add(30 * time.Minute).UnixMilli(), End: now.Add(time.Hour).UnixMilli(), Break: true}}, now)
	if len(e.kept) != 0 {
		t.Fatalf("a break was told as %s", e.kept[0].Kind)
	}
}
