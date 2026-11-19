package norn

import (
	"context"
	"net/http"
	"testing"
	"time"

	"yggdrasil/internal/contract/contracttest"
)

func TestTheRecapSetting(t *testing.T) {
	n, p := newNorn(t)
	n.Recaps.Exchanges = func(_ context.Context, person RecapPerson, _ time.Time) ([]Exchange, RecapPerson) {
		t.Error("read while off")
		return nil, person
	}
	n.Recaps.Invited = func(context.Context, CalendarEvent) []invitee { return []invitee{{Name: "Anna Berg", Organizer: true}} }
	now := time.Now()
	n.Accounts.Work.Set(CalendarSnapshot{At: now.UnixMilli(), Events: []CalendarEvent{{ID: "m", Subject: "Sync", Start: now.Add(5 * time.Minute).UnixMilli(),
		End: now.Add(35 * time.Minute).UnixMilli(), Attendees: []string{"Anna Berg"}, Response: "accepted", ShowAs: "busy"}}})
	if _, body := call(t, p, "GET", "/v1/settings/person-recap", nil); body.(map[string]any)["on"] != true {
		t.Fatalf("on by default: %v", body)
	}
	if code, body := call(t, p, "PUT", "/v1/settings/person-recap", map[string]any{"on": false}); code != http.StatusOK || body.(map[string]any)["on"] != false {
		t.Fatalf("off: %d %v", code, body)
	}
	contracttest.Run(t, p)
	_, body := call(t, p, "GET", "/v1/people/recap?who=anna@example.com", nil)
	if off := body.(map[string]any); off["on"] != false || len(off["promises"].([]any)) != 0 {
		t.Fatalf("off still recaps: %v", body)
	}
	if _, body := call(t, p, "GET", "/v1/people/recap/next", nil); len(body.(map[string]any)["people"].([]any)) != 0 {
		t.Fatalf("off still recaps the meeting: %v", body)
	}
	if code, _ := call(t, p, "PUT", "/v1/settings/person-recap", map[string]any{}); code != http.StatusBadRequest {
		t.Errorf("no on: %d", code)
	}
	if _, body := call(t, p, "GET", "/v1/settings/person-recap", nil); body.(map[string]any)["on"] != false {
		t.Errorf("setting: %v", body)
	}
}
