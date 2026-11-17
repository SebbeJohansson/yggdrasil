package huginn

import (
	"testing"

	"yggdrasil/internal/call"
)

// The vectors are my-widget's claudeSession, run in Node.
func TestClaudeSessionMatchesWidget(t *testing.T) {
	for _, c := range []struct {
		meeting CalendarEvent
		want    string
	}{
		{CalendarEvent{ID: "AAMk-x=", Start: 1763380800000}, "meeting-prep-claude-em9nkpdgs5y9"},
		{CalendarEvent{Subject: "Snacka Jev", Start: 1763382600000}, "meeting-prep-claude-1ttai3tfkfwb"},
		{CalendarEvent{Subject: "Möte åäö", Start: 1}, "meeting-prep-claude-12rjfgj1jmvhpn"},
	} {
		if got := ClaudeSession(c.meeting); got != c.want {
			t.Errorf("%+v: %s, want %s", c.meeting, got, c.want)
		}
	}
}

func TestLastAnswer(t *testing.T) {
	agent := "a1"
	turns := []call.Turn{{ID: 1, Role: "user", Content: "brief"}, {ID: 2, Role: "assistant", Content: "handed over"}}
	if text, asked := lastAnswer(turns); text != "" || !asked {
		t.Fatalf("waiting: %q %v", text, asked)
	}
	turns = append(turns, call.Turn{ID: 3, Role: "assistant", Author: &agent, Content: "- **Moa**: the Jev API"})
	if text, _ := lastAnswer(turns); text != "- **Moa**: the Jev API" {
		t.Fatalf("answered: %q", text)
	}
	if _, asked := lastAnswer(nil); asked {
		t.Fatal("never asked")
	}
}

func TestBriefNamesWhoIsInvited(t *testing.T) {
	people := meetingPeople(CalendarEvent{Organizer: "Lisa Ek", Attendees: []string{"Lisa Ek", "Kim"}})
	if len(people) != 2 || people[0] != "Lisa Ek" || people[1] != "Kim" {
		t.Fatalf("%v", people)
	}
}
