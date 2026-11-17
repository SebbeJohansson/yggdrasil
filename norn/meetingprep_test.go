package norn

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPrepKeyIsTheSeriesSoEveryOccurrenceSharesIt(t *testing.T) {
	monday := CalendarEvent{ID: "occ-1", Series: "weekly", Subject: "1:1 Kim"}
	nextMonday := CalendarEvent{ID: "occ-2", Series: "weekly", Subject: "1:1 Kim"}
	if PrepKey(monday) != "series:weekly" || PrepKey(monday) != PrepKey(nextMonday) {
		t.Fatalf("%q %q", PrepKey(monday), PrepKey(nextMonday))
	}
	if got := PrepKey(CalendarEvent{ID: "x", Subject: "Once"}); got != "event:x" {
		t.Fatalf("one-off: %q", got)
	}
}

func TestCalendarsSayWhichSeriesAMeetingIsOf(t *testing.T) {
	graph, _ := GraphEvents([]byte(`{"value":[{"id":"o1","seriesMasterId":"m1","subject":"Standup","start":{"dateTime":"2026-11-17T08:00:00.0000000"},"end":{"dateTime":"2026-11-17T08:15:00.0000000"}}]}`))
	google, _ := GoogleEvents([]byte(`{"items":[{"id":"g1_20261117","recurringEventId":"g1","summary":"Gym","start":{"dateTime":"2026-11-17T17:00:00Z"},"end":{"dateTime":"2026-11-17T18:00:00Z"}}]}`))
	if len(graph) != 1 || graph[0].Series != "m1" || len(google) != 1 || google[0].Series != "g1" {
		t.Fatalf("graph %+v google %+v", graph, google)
	}
}

func TestMeetingPrepIsKeptPerSeriesAndRidesOnCalendarSoon(t *testing.T) {
	n, p := newNorn(t)
	if code, _ := call(t, p, "PUT", "/v1/meetings/prep", map[string]string{"key": "series:weekly", "prompt": "  focus on\n the budget ", "model": "claude"}); code != 200 {
		t.Fatalf("put: %d", code)
	}
	code, body := call(t, p, "GET", "/v1/meetings/prep?key=series:weekly", nil)
	got, _ := body.(map[string]any)
	if code != 200 || got["prompt"] != "focus on the budget" || got["model"] != "claude" {
		t.Fatalf("%d %v", code, body)
	}
	if code, _ := call(t, p, "PUT", "/v1/meetings/prep", map[string]string{"key": "series:weekly", "model": "gpt"}); code != 400 {
		t.Fatalf("an unknown model: %d", code)
	}

	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	e := NewEvents(now)
	e.PromptFor = n.prepPrompt
	meeting := CalendarEvent{ID: "occ-2", Series: "weekly", Subject: "1:1 Kim", Start: now.Add(2 * time.Hour).UnixMilli(), End: now.Add(150 * time.Minute).UnixMilli()}
	e.Calendar("work", []CalendarEvent{meeting}, now)
	e.Soon(now.Add(95 * time.Minute))
	var change CalendarChange
	for _, event := range e.kept {
		if event.Kind == "calendar.soon" {
			_ = json.Unmarshal(event.Data, &change)
		}
	}
	if change.Prompt != "focus on the budget" {
		t.Fatalf("calendar.soon carried %q", change.Prompt)
	}

	// Emptied, it is gone.
	call(t, p, "PUT", "/v1/meetings/prep", map[string]string{"key": "series:weekly"})
	_, body = call(t, p, "GET", "/v1/meetings/prep?key=series:weekly", nil)
	if got, _ := body.(map[string]any); got["prompt"] != "" || got["model"] != "" {
		t.Fatalf("still kept: %v", body)
	}
	if strings.TrimSpace(n.prepPrompt(meeting)) != "" {
		t.Fatal("still asked with it")
	}
}
