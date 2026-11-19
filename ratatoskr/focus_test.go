package ratatoskr

import (
	"strings"
	"testing"
	"time"
)

func TestAFocusBlockIsNotAMeetingButDoesNotEndAStretch(t *testing.T) {
	events := []CalendarEvent{
		{Start: day(9, 0).UnixMilli(), End: day(11, 0).UnixMilli(), Response: "accepted", ShowAs: "busy"},
		{Start: day(11, 0).UnixMilli(), End: day(13, 0).UnixMilli(), Response: "organizer", ShowAs: "busy", Focus: true},
		{Start: day(13, 0).UnixMilli(), End: day(14, 0).UnixMilli(), Response: "accepted", ShowAs: "busy"},
	}
	meetings, focus := Meetings(events), FocusBlocks(events)
	if len(meetings) != 2 || len(focus) != 1 {
		t.Fatalf("meetings %v, focus %v", meetings, focus)
	}
	// Without the focus block, 11-13 is a gap; with it, 09-14 is one run of five hours at the screen.
	if Stretch(meetings, day(10, 0), rhythm()).Active {
		t.Fatal("two short runs of meetings are no stretch")
	}
	seen := StretchWithFocus(meetings, focus, day(10, 0), rhythm())
	if !seen.Active || !seen.Since.Equal(day(9, 0)) || !seen.NextGap.Equal(day(14, 0)) || !seen.Focus {
		t.Fatalf("seen %+v", seen)
	}
	y := &You{Rhythm: rhythm()}
	if text := y.Message(PatternStretch, seen, WeekSeen{}).Text; !strings.Contains(text, "Meetings and focus run back to back until 14:00") {
		t.Errorf("text %q", text)
	}
	// Focus alone is no stretch, however long.
	if StretchWithFocus(nil, []Span{meeting(9, 0, 15, 0)}, day(10, 0), rhythm()).Active {
		t.Error("a day of focus is a stretch")
	}
	// A break you kept free still ends the run.
	shorter := []Span{meeting(11, 0, 12, 30)}
	moved := []Span{meeting(9, 0, 11, 0), meeting(12, 35, 14, 0)}
	if s := StretchWithFocus(moved, shorter, day(10, 0), rhythm()); !s.Active {
		t.Fatalf("a five minute pause after the focus block ended the run: %+v", s)
	}
	if s := StretchWithFocus(moved, shorter, day(10, 0), rhythm(), Span{day(12, 30), day(12, 35)}); s.Active {
		t.Errorf("a kept break after the focus block did not end the run: %+v", s)
	}
}

func TestAFocusBlockDoesNotFillTheWeekAndIsNotFree(t *testing.T) {
	r := rhythm()
	monday := time.Date(2026, 11, 16, 8, 0, 0, 0, stockholm)
	var meetings []Span
	for d := 0; d < 5; d++ {
		date := monday.AddDate(0, 0, d)
		meetings = append(meetings, Span{at(date, 9*60), at(date, 13*60)}) // half of each day
	}
	focus := []Span{{at(monday, 13*60), at(monday.AddDate(0, 0, 4), 17*60)}} // every afternoon, as if one span
	if Week(meetings, monday, r, focus...).Active {
		t.Fatal("focus blocks counted as meetings")
	}
	r.Packed = 50
	seen := Week(meetings, monday, r, focus...)
	if !seen.Active || seen.Free != nil {
		t.Fatalf("free time named inside focus: %+v", seen)
	}
}
