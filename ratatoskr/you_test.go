package ratatoskr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"yggdrasil/internal/call"
)

var stockholm, _ = time.LoadLocation("Europe/Stockholm")

func rhythm() Rhythm {
	r := DefaultRhythm()
	r.Zone = stockholm
	return r
}

// day is Thursday 2026-11-19 at hh:mm in Stockholm.
func day(hh, mm int) time.Time {
	return time.Date(2026, 11, 19, hh, mm, 0, 0, stockholm)
}

func meeting(fromH, fromM, toH, toM int) Span {
	return Span{day(fromH, fromM), day(toH, toM)}
}

func TestBackToBackMeetingsForFourHoursAreAStretchUntilTheFirstRealGap(t *testing.T) {
	meetings := []Span{
		meeting(9, 0, 10, 0),
		meeting(10, 0, 11, 0),
		meeting(11, 5, 12, 0), // a 5 minute pause: not a real gap
		meeting(12, 10, 13, 30),
		meeting(13, 30, 14, 0),
		meeting(14, 30, 15, 0), // after a 30 minute gap
	}
	seen := Stretch(meetings, day(10, 30), rhythm())
	if !seen.Active {
		t.Fatalf("09:00-14:00 has no gap of 15 minutes: that is a stretch")
	}
	if !seen.Since.Equal(day(9, 0)) || !seen.NextGap.Equal(day(14, 0)) {
		t.Fatalf("since %v, next gap %v; want 09:00 and 14:00", seen.Since, seen.NextGap)
	}
	if !seen.PauseAt.Equal(day(11, 0)) || !seen.PauseEnds.Equal(day(11, 5)) {
		t.Fatalf("pause %v-%v; want 11:00-11:05", seen.PauseAt, seen.PauseEnds)
	}
	if later := Stretch(meetings, day(11, 30), rhythm()); !later.PauseAt.Equal(day(12, 0)) {
		t.Fatalf("at 11:30 the next pause is the one at 12:00, not %v", later.PauseAt)
	}
	if after := Stretch(meetings, day(14, 10), rhythm()); after.Active {
		t.Fatalf("at 14:10 you are in the gap: not a stretch")
	}
}

func TestAGapOfFifteenMinutesBreaksTheStretch(t *testing.T) {
	meetings := []Span{meeting(9, 0, 11, 0), meeting(11, 15, 13, 30)}
	if Stretch(meetings, day(12, 0), rhythm()).Active {
		t.Fatalf("a 15 minute gap at 11:00 is a real break")
	}
	short := []Span{meeting(9, 0, 11, 0), meeting(11, 0, 12, 30)}
	if Stretch(short, day(10, 0), rhythm()).Active {
		t.Fatalf("3.5 hours is shorter than the 4 hour stretch")
	}
}

func TestOverlappingMeetingsCountOnce(t *testing.T) {
	joined := Join([]Span{meeting(10, 0, 11, 0), meeting(9, 0, 10, 30), meeting(12, 0, 13, 0)}, 0)
	if len(joined) != 2 || !joined[0].Start.Equal(day(9, 0)) || !joined[0].End.Equal(day(11, 0)) {
		t.Fatalf("got %v", joined)
	}
}

func TestASessionPastElevenIsWorkingLate(t *testing.T) {
	r := rhythm()
	now := day(23, 20)
	if !Late([]time.Time{day(23, 15)}, now, r).Active {
		t.Fatalf("a session active at 23:15 is late")
	}
	if Late([]time.Time{day(22, 50)}, now, r).Active {
		t.Fatalf("a session last active at 22:50 is not past 23:00")
	}
	if Late([]time.Time{day(23, 1)}, day(23, 40), r).Active {
		t.Fatalf("a session quiet for 39 minutes is not you working now")
	}
	if Late([]time.Time{day(21, 59)}, day(22, 0), r).Active {
		t.Fatalf("22:00 is not late")
	}
	afterMidnight := day(23, 0).Add(2 * time.Hour)
	seen := Late([]time.Time{afterMidnight.Add(-time.Minute), afterMidnight.Add(-5 * time.Minute)}, afterMidnight, r)
	if !seen.Active || !seen.Since.Equal(afterMidnight.Add(-5*time.Minute)) {
		t.Fatalf("01:00 is still late, since the earliest activity: got %+v", seen)
	}
	if Late([]time.Time{day(6, 0)}, day(6, 1), r).Active {
		t.Fatalf("06:00 is morning")
	}
}

func TestOnlyAnAgentYouAnsweredCountsAsYouWorking(t *testing.T) {
	before := map[string]AgentSeen{
		"asked":   {ID: "asked", AsksYou: true, LastActivity: day(23, 0).UnixMilli()},
		"working": {ID: "working", LastActivity: day(23, 0).UnixMilli()},
		"waiting": {ID: "waiting", AsksYou: true, LastActivity: day(23, 0).UnixMilli()},
	}
	now := []AgentSeen{
		{ID: "asked", LastActivity: day(23, 10).UnixMilli()},
		{ID: "working", LastActivity: day(23, 10).UnixMilli()},
		{ID: "waiting", AsksYou: true, LastActivity: day(23, 0).UnixMilli()},
		{ID: "new", LastActivity: day(23, 10).UnixMilli()},
	}
	got := Answered(before, now)
	if len(got) != 1 || !got[0].Equal(day(23, 10)) {
		t.Fatalf("got %v, want only the agent you answered at 23:10", got)
	}
}

func TestAWeekMostlyInMeetingsIsPackedAndNamesItsLongestFreeBlock(t *testing.T) {
	r := rhythm()
	now := day(8, 0) // Thursday morning: Thursday and Friday are left, 16 working hours
	friday := func(fromH, toH int) Span {
		return Span{day(fromH, 0).AddDate(0, 0, 1), day(toH, 0).AddDate(0, 0, 1)}
	}
	meetings := []Span{
		meeting(9, 0, 12, 0), meeting(13, 0, 17, 0), // Thursday: 7 h, free 12-13
		friday(9, 0), friday(9, 11), friday(14, 17), // Friday: 5 h, free 11-14
		meeting(7, 0, 8, 30),  // outside working hours: not counted
		meeting(11, 0, 12, 0), // overlaps: counted once
	}
	seen := Week(meetings, now, r)
	if !seen.Active {
		t.Fatalf("12 of 16 working hours are meetings: packed")
	}
	if seen.Free == nil || !seen.Free.Start.Equal(day(11, 0).AddDate(0, 0, 1)) || !seen.Free.End.Equal(day(14, 0).AddDate(0, 0, 1)) {
		t.Fatalf("free block %v, want Friday 11:00-14:00", seen.Free)
	}
	r.Packed = 80
	if Week(meetings, now, r).Active {
		t.Fatalf("75%% is under 80%%")
	}
	saturday := day(10, 0).AddDate(0, 0, 2)
	if Week([]Span{{saturday, saturday.Add(8 * time.Hour)}}, saturday, rhythm()).Active {
		t.Fatalf("a weekend is never packed")
	}
}

func TestDeclinedFreeAndAllDayEventsAreNotMeetings(t *testing.T) {
	ms := func(t time.Time) int64 { return t.UnixMilli() }
	events := []CalendarEvent{
		{Start: ms(day(9, 0)), End: ms(day(10, 0)), Response: "accepted", ShowAs: "busy"},
		{Start: ms(day(10, 0)), End: ms(day(11, 0)), Response: "declined"},
		{Start: ms(day(11, 0)), End: ms(day(12, 0)), ShowAs: "free"},
		{Start: ms(day(0, 0)), End: ms(day(23, 59)), AllDay: true},
		{Start: ms(day(13, 0)), End: ms(day(14, 0)), ShowAs: "oof"},
	}
	if got := Meetings(events); len(got) != 1 || !got[0].Start.Equal(day(9, 0)) {
		t.Fatalf("got %v", got)
	}
}

func TestMessagesAreGentleAndHoldTimesOnly(t *testing.T) {
	y := NewYou(rhythm(), nil, nil, nil)
	stretch := StretchSeen{Active: true, Since: day(9, 0), NextGap: day(14, 0), PauseAt: day(11, 0), PauseEnds: day(11, 5)}
	free := Span{day(11, 0).AddDate(0, 0, 1), day(14, 0).AddDate(0, 0, 1)}
	for _, pattern := range []string{PatternStretch, PatternLate, PatternWeek} {
		m := y.Message(pattern, stretch, WeekSeen{Active: true, Free: &free})
		if m.Priority != "low" || !m.Hold || m.Push == nil || *m.Push {
			t.Fatalf("%s: want low, held, not pushed: %+v", pattern, m)
		}
		if m.Text == "" || strings.ContainsAny(m.Text, "%!") {
			t.Fatalf("%s: %q", pattern, m.Text)
		}
	}
	if m := y.Message(PatternStretch, stretch, WeekSeen{}); !strings.Contains(m.Text, "14:00") || !strings.Contains(m.Text, "11:00") {
		t.Fatalf("stretch names the pause and the gap: %q", m.Text)
	}
	if m := y.Message(PatternWeek, stretch, WeekSeen{Active: true, Free: &free}); !strings.Contains(m.Text, "Friday 11:00–14:00") {
		t.Fatalf("week names the free block: %q", m.Text)
	}
}

// fakeParts serves Norn's calendar, Sindri's sessions and agents, and Hermóðr.
type fakeParts struct {
	mu       sync.Mutex
	events   []CalendarEvent
	quiet    bool
	sent     []map[string]any
	earlier  []map[string]any
	sessions []map[string]any
	calls    int
}

func (f *fakeParts) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	answer := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch r.Method + " " + r.URL.Path {
	case "GET /v1/calendar":
		answer(map[string]any{"events": f.events, "at": 1})
	case "GET /v1/sessions/recent":
		answer(f.sessions)
	case "GET /v1/agents":
		answer([]any{})
	case "GET /v1/channels":
		answer(map[string]any{"quiet": map[string]any{"now": f.quiet}})
	case "GET /v1/messages":
		answer(map[string]any{"messages": f.earlier})
	case "POST /v1/messages":
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		f.sent = append(f.sent, m)
		w.WriteHeader(http.StatusCreated)
		answer(m)
	default:
		http.NotFound(w, r)
	}
}

func youWith(t *testing.T, f *fakeParts) *You {
	t.Helper()
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	part := func(name string) *call.Part { return call.New(name, server.URL, "t", 5*time.Second) }
	return NewYou(rhythm(), part("norn"), part("sindri"), part("hermodr"))
}

func longDay() []CalendarEvent {
	var events []CalendarEvent
	for h := 9; h < 14; h++ {
		events = append(events, CalendarEvent{Start: day(h, 0).UnixMilli(), End: day(h+1, 0).UnixMilli(), ShowAs: "busy"})
	}
	return events
}

func TestAStretchIsToldOnceADayAndNotDuringQuietHours(t *testing.T) {
	f := &fakeParts{events: longDay(), quiet: true}
	y := youWith(t, f)
	now := day(10, 0)
	y.now = func() time.Time { return now }
	ctx := context.Background()

	if err := y.Round(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 0 {
		t.Fatalf("sent during quiet hours: %v", f.sent)
	}
	if p := y.Status().Patterns[0]; !p.Active || p.NextGap == nil || !p.NextGap.Equal(day(14, 0)) {
		t.Fatalf("status %+v", p)
	}
	f.quiet = false
	_ = y.Round(ctx)
	now = day(11, 0)
	_ = y.Round(ctx)
	if len(f.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(f.sent))
	}
	m := f.sent[0]
	if m["priority"] != "low" || m["hold"] != true || m["push"] != false {
		t.Fatalf("message %v", m)
	}
	if y.Status().Patterns[0].ToldAt == nil {
		t.Fatalf("status does not say it was told")
	}
}

func TestARestartDoesNotTellAgainWhatHermodrAlreadyHas(t *testing.T) {
	f := &fakeParts{events: longDay()}
	y := youWith(t, f)
	f.earlier = []map[string]any{{"source": Name, "title": y.title(PatternStretch), "at": day(9, 30).Format(time.RFC3339)}}
	y.now = func() time.Time { return day(10, 0) }
	if err := y.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 0 {
		t.Fatalf("told again after a restart: %v", f.sent)
	}
}

func TestWorkingLateIsSeenFromYourSessions(t *testing.T) {
	f := &fakeParts{sessions: []map[string]any{{"lastActivity": day(23, 25).UnixMilli()}}}
	y := youWith(t, f)
	y.now = func() time.Time { return day(23, 30) }
	_ = y.Round(context.Background())
	late := y.Status().Patterns[1]
	if late.Name != PatternLate || !late.Active || late.Since == nil {
		t.Fatalf("late %+v", late)
	}
	if len(f.sent) != 1 || f.sent[0]["title"] != "It's past 23:00" {
		t.Fatalf("sent %v", f.sent)
	}
}

func TestABreakYouKeptFreeIsNotAMeetingAndEndsAStretch(t *testing.T) {
	r := rhythm()
	r.Break = 30 * time.Minute
	events := []CalendarEvent{
		{Start: day(9, 0).UnixMilli(), End: day(11, 0).UnixMilli(), ShowAs: "busy"},
		// A 15 minute break kept free: shorter than RATATOSKR_YOU_BREAK, still a break.
		{Start: day(11, 0).UnixMilli(), End: day(11, 15).UnixMilli(), ShowAs: "busy", Break: true},
		{Start: day(11, 15).UnixMilli(), End: day(14, 0).UnixMilli(), ShowAs: "busy"},
	}
	meetings, breaks := Meetings(events), Breaks(events)
	if len(meetings) != 2 || len(breaks) != 1 {
		t.Fatalf("meetings %v, breaks %v", meetings, breaks)
	}
	if Stretch(meetings, day(12, 0), r, breaks...).Active {
		t.Fatalf("the break at 11:00 ends the run: 11:15-14:00 is not a stretch")
	}
	if !Stretch(meetings, day(12, 0), r).Active {
		t.Fatalf("without the break, 09:00-14:00 with a 15 minute gap is a stretch when a real gap is 30")
	}
	// The run before the break ends at it.
	long := []CalendarEvent{
		{Start: day(8, 0).UnixMilli(), End: day(12, 30).UnixMilli(), ShowAs: "busy"},
		{Start: day(12, 30).UnixMilli(), End: day(12, 50).UnixMilli(), ShowAs: "busy", Break: true},
		{Start: day(12, 50).UnixMilli(), End: day(15, 0).UnixMilli(), ShowAs: "busy"},
	}
	seen := Stretch(Meetings(long), day(10, 0), r, Breaks(long)...)
	if !seen.Active || !seen.NextGap.Equal(day(12, 30)) {
		t.Fatalf("want a stretch until the break at 12:30, got %+v", seen)
	}
	// A whole week of breaks is not a packed week.
	var week []CalendarEvent
	for d := 0; d < 2; d++ {
		from := time.Date(2026, 11, 19+d, 9, 0, 0, 0, stockholm)
		week = append(week, CalendarEvent{Start: from.UnixMilli(), End: from.Add(8 * time.Hour).UnixMilli(), ShowAs: "busy", Break: true})
	}
	if Week(Meetings(week), day(8, 0), r).Active {
		t.Fatalf("breaks filled the week")
	}
}
