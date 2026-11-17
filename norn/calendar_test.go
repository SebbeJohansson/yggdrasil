package norn

import (
	"context"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGraphEvents(t *testing.T) {
	events, next := GraphEvents([]byte(`{"value":[
		{"subject":" Standup ","start":{"dateTime":"2026-10-08T07:00:00.0000000"},"end":{"dateTime":"2026-10-08T07:15:00.0000000"},"isOnlineMeeting":true,"organizer":{"emailAddress":{"name":"Ada"}},"attendees":[{"emailAddress":{"name":"Ada","address":"ada@x"}},{"emailAddress":{"address":"bo@x"}},{"emailAddress":{"name":"Ada"}},{"type":"resource","emailAddress":{"name":"Mountain Top","address":"room@x"}}],"responseStatus":{"response":"accepted"},"showAs":"busy"},
		{"subject":"Gone","isCancelled":true,"start":{"dateTime":"2026-10-08T08:00:00"},"end":{"dateTime":"2026-10-08T09:00:00"}},
		{"subject":"","start":{"dateTime":"2026-10-09T00:00:00Z"},"end":{"dateTime":"2026-10-10T00:00:00Z"},"isAllDay":true}
	],"@odata.nextLink":"https://next"}`))
	if next != "https://next" || len(events) != 2 {
		t.Fatalf("got %d events, next %q", len(events), next)
	}
	if !reflect.DeepEqual(events[0].Attendees, []string{"Ada", "bo@x"}) {
		t.Fatalf("attendees %v", events[0].Attendees)
	}
	if events[0].Subject != "Standup" || events[0].Start != time.Date(2026, 10, 8, 7, 0, 0, 0, time.UTC).UnixMilli() || !events[0].Online || events[0].Organizer != "Ada" {
		t.Fatalf("got %+v", events[0])
	}
	if events[1].Subject != "(no title)" || !events[1].AllDay {
		t.Fatalf("got %+v", events[1])
	}
}

func TestGoogleEvents(t *testing.T) {
	events, _ := GoogleEvents([]byte(`{"items":[
		{"summary":"Dinner","start":{"dateTime":"2026-10-08T18:00:00+02:00"},"end":{"dateTime":"2026-10-08T20:00:00+02:00"},"organizer":{"displayName":"Sam"},"attendees":[{"self":true,"responseStatus":"tentative","displayName":"Me"},{"displayName":"Kim"},{"displayName":"Room 3","resource":true}],"transparency":"transparent"},
		{"summary":"Mine","start":{"date":"2026-10-09"},"end":{"date":"2026-10-10"},"organizer":{"self":true},"hangoutLink":"x"},
		{"status":"cancelled","summary":"x","start":{"date":"2026-10-09"},"end":{"date":"2026-10-10"}}
	]}`))
	if len(events) != 2 {
		t.Fatalf("got %+v", events)
	}
	if !reflect.DeepEqual(events[0].Attendees, []string{"Kim"}) {
		t.Fatalf("attendees %v", events[0].Attendees)
	}
	if events[0].Response != "tentativelyAccepted" || events[0].ShowAs != "free" || events[0].Organizer != "Sam" {
		t.Fatalf("got %+v", events[0])
	}
	if events[1].Response != "organizer" || events[1].Organizer != "" || !events[1].AllDay || !events[1].Online {
		t.Fatalf("got %+v", events[1])
	}
	if ids := CalendarIDs([]byte(`{"items":[{"id":"me","selected":true},{"id":"x#holiday@group.v.calendar.google.com","selected":true},{"id":"off"}]}`)); len(ids) != 1 || ids[0] != "me" {
		t.Fatalf("got %v", ids)
	}
	tasks, _ := DueTasks([]byte(`{"items":[{"title":"Pay rent","due":"2026-10-01T00:00:00.000Z"},{"title":"Done","due":"2026-10-01T00:00:00Z","status":"completed"},{"title":"Someday"}]}`), "")
	if len(tasks) != 1 || tasks[0].Due != "2026-10-01" {
		t.Fatalf("got %+v", tasks)
	}
}

func TestCalendarLinesSayTheNextMeetingAndOverlapsOutright(t *testing.T) {
	zone, _ := time.LoadLocation("Europe/Stockholm")
	now := time.Date(2026, 10, 8, 10, 0, 0, 0, zone)
	at := func(h, m int) int64 { return time.Date(2026, 10, 8, h, m, 0, 0, zone).UnixMilli() }
	snapshot := CalendarSnapshot{To: now.Add(7 * 24 * time.Hour).UnixMilli(), Events: []CalendarEvent{
		{Subject: "Retro", Start: at(13, 0), End: at(14, 0)},
		{Subject: "Lunch", Start: at(12, 0), End: at(13, 30)},
		{Subject: "Now", Start: at(9, 30), End: at(10, 30)},
		{Subject: "Skipped", Start: at(15, 0), End: at(16, 0), Response: "declined"},
		{Subject: "Over", Start: at(8, 0), End: at(9, 0)},
	}, Tasks: []DueTask{{Title: "Rent", Due: "2026-10-01"}, {Title: "Call", Due: "2026-10-08"}}}
	lines := strings.Join(CalendarLines(snapshot, now, zone), "\n")
	for _, want := range []string{
		"Now: Thu 8 Oct 10:00 (Europe/Stockholm)",
		"Happening now: Now, until 10:30.",
		"Next meeting: Lunch, today at 12:00 (in 2 h).",
		"Meetings left today: 3.",
		"Thu 8 Oct 12:00-13:30: Lunch [overlaps another meeting]",
		"overdue since 2026-10-01: Rent",
		"due today: Call",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("missing %q in\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "Skipped") || strings.Contains(lines, "Over") {
		t.Errorf("declined or finished meetings listed:\n%s", lines)
	}
}

func TestCalendarQuestions(t *testing.T) {
	for question, want := range map[string]bool{"what's my next meeting?": true, "vad har jag för möten imorgon": true, "what's the weather tomorrow?": false} {
		if AsksAboutCalendar(question) != want {
			t.Errorf("%q: want %v", question, want)
		}
	}
	if !BeyondCalendar("who is invited to the retro?") || BeyondCalendar("when is the retro?") {
		t.Errorf("beyond the feed is wrong")
	}
}

func TestMicrosoftDeviceCodeSignIn(t *testing.T) {
	a := NewMicrosoftAuth(t.TempDir() + "/microsoft.json")
	polls := 0
	a.post = func(_ context.Context, endpoint string, fields url.Values) (map[string]any, error) {
		switch {
		case strings.HasSuffix(endpoint, "/devicecode"):
			return map[string]any{"device_code": "secret", "user_code": "ABC", "interval": 3600.0, "expires_in": 900.0}, nil
		case fields.Get("grant_type") == "refresh_token":
			return map[string]any{"error": "invalid_grant", "error_description": "revoked"}, nil
		}
		polls++
		if polls == 1 {
			return map[string]any{"error": "authorization_pending"}, nil
		}
		return map[string]any{"refresh_token": "r", "access_token": "a", "expires_in": 3600.0}, nil
	}
	var changes []error
	a.OnChange = func(err error) { changes = append(changes, err) }
	status := a.SignIn(context.Background(), "organizations", MicrosoftScopes{Calendar: true}, false)
	if status.State != "linking" || status.Code.UserCode != "ABC" {
		t.Fatalf("got %+v", status)
	}
	if !a.Poll(context.Background()) || a.Poll(context.Background()) {
		t.Fatalf("polls did not end once the code was entered")
	}
	if a.Status().State != "signed-in" || len(changes) != 1 || changes[0] != nil {
		t.Fatalf("got %+v, changes %v", a.Status(), changes)
	}
	if token, err := a.Token(context.Background()); err != nil || token.AccessToken != "a" {
		t.Fatalf("got %v %v", token, err)
	}
	// Signed in with the calendar: asking for it again changes nothing.
	if again := a.SignIn(context.Background(), "organizations", MicrosoftScopes{Calendar: true}, false); again.State != "signed-in" {
		t.Fatalf("got %+v", again)
	}
	// Reloaded from its file, then the refresh is refused: signed out, and said so.
	reloaded := NewMicrosoftAuth(a.file)
	reloaded.post, reloaded.OnChange = a.post, a.OnChange
	reloaded.stored.ExpiresAt = 0
	if _, err := reloaded.Token(context.Background()); err != ErrSignedOut {
		t.Fatalf("got %v", err)
	}
	if reloaded.Status().State != "error" || len(changes) != 2 || changes[1] == nil {
		t.Fatalf("got %+v, changes %v", reloaded.Status(), changes)
	}
}

func TestGoogleIsOffWithoutAClient(t *testing.T) {
	a := NewGoogleAuth(t.TempDir()+"/google.json", "", "")
	if status, _ := a.SignIn(); status.State != "off" {
		t.Fatalf("got %+v", status)
	}
	if grants := GrantsFrom(GmailScope + " " + GoogleTasksScope); !grants.Gmail || grants.Calendar || !grants.Tasks {
		t.Fatalf("got %+v", grants)
	}
}

func TestRoomsAndJoinLinks(t *testing.T) {
	outlook, _ := GraphEvents([]byte(`{"value":[
		{"id":"o1","subject":"Snacka Jev","start":{"dateTime":"2026-11-17T12:30:00"},"end":{"dateTime":"2026-11-17T13:00:00"},
		 "isOnlineMeeting":true,"onlineMeeting":{"joinUrl":"https://teams.microsoft.com/l/meetup-join/19%3ameeting_abc/0"},
		 "locations":[{"displayName":"Mötesrum Ekeby","locationType":"conferenceRoom"},{"displayName":"Teams","locationType":"default"}],
		 "attendees":[{"type":"resource","emailAddress":{"name":"Rum Tallen","address":"tallen@x.se"}},{"type":"required","emailAddress":{"name":"Lisa Ek"}}]},
		{"id":"o2","subject":"Partner","start":{"dateTime":"2026-11-17T14:00:00"},"end":{"dateTime":"2026-11-17T15:00:00"},
		 "body":{"content":"Join with Google Meet: https://meet.google.com/abc-defg-hij\nOr dial in"}}
	]}`))
	if got := outlook[0]; got.Join != "https://teams.microsoft.com/l/meetup-join/19%3ameeting_abc/0" ||
		len(got.Rooms) != 2 || got.Rooms[0] != "Mötesrum Ekeby" || got.Rooms[1] != "Rum Tallen" || len(got.Attendees) != 1 {
		t.Fatalf("teams meeting: %+v", got)
	}
	if got := outlook[1].Join; got != "https://meet.google.com/abc-defg-hij" {
		t.Fatalf("meet link from the body: %q", got)
	}

	google, _ := GoogleEvents([]byte(`{"items":[
		{"id":"g1","summary":"Lunch sync","start":{"dateTime":"2026-11-17T11:00:00Z"},"end":{"dateTime":"2026-11-17T12:00:00Z"},
		 "description":"<a href=\"https://teams.microsoft.com/l/meetup-join/19%3ameeting_xyz/0?context=%7b%7d\">Join</a>",
		 "attendees":[{"displayName":"Konferensrum Ett","resource":true},{"email":"kim@x.se"}]},
		{"id":"g2","summary":"Meet","start":{"dateTime":"2026-11-17T13:00:00Z"},"end":{"dateTime":"2026-11-17T14:00:00Z"},
		 "conferenceData":{"entryPoints":[{"entryPointType":"phone","uri":"tel:+46"},{"entryPointType":"video","uri":"https://meet.google.com/xyz-abcd-efg"}]}}
	]}`))
	if got := google[0]; got.Join != "https://teams.microsoft.com/l/meetup-join/19%3ameeting_xyz/0?context=%7b%7d" || len(got.Rooms) != 1 || got.Rooms[0] != "Konferensrum Ett" {
		t.Fatalf("teams link in a google description: %+v", got)
	}
	if got := google[1].Join; got != "https://meet.google.com/xyz-abcd-efg" {
		t.Fatalf("meet entry point: %q", got)
	}
}
