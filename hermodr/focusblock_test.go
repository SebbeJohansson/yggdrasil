package hermodr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/part"
)

func TestAFocusBlockHoldsWhatAsksToHold(t *testing.T) {
	now := time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC)
	h, _, other := newTest(t, &now, "")
	h.SetMeetings([]CalendarEvent{{Start: now.Add(-30 * time.Minute).UnixMilli(), End: now.Add(90 * time.Minute).UnixMilli(), ShowAs: "busy", Focus: true}})
	held := h.Take(context.Background(), Message{ID: "1", Title: "A full week", Priority: "low", Hold: true, Push: true, At: now})
	free := h.Take(context.Background(), Message{ID: "2", Title: "Build done", Priority: "normal", Push: true, At: now})
	urgent := h.Take(context.Background(), Message{ID: "3", Title: "Critical", Priority: "urgent", Hold: true, Push: true, At: now})
	if held.State != "held" || held.Reason != "in a focus block until 11:00" || free.State != "sent" || urgent.State != "sent" {
		t.Fatalf("%+v / %+v / %+v", held, free, urgent)
	}

	p := part.New(Name, contracttest.Tokens(), nil)
	h.Mount(p)
	request := httptest.NewRequest(http.MethodGet, "/v1/channels", nil)
	request.Header.Set("Authorization", "Bearer "+contracttest.Owner)
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if !strings.Contains(body, `"inFocusBlock":true`) || !strings.Contains(body, `"inMeeting":false`) || !strings.Contains(body, `"inBreak":false`) ||
		!strings.Contains(body, `"focusEndsAt":"2026-11-24T11:00:00Z"`) {
		t.Fatalf("channels: %s", body)
	}

	now = now.Add(91 * time.Minute)
	if err := h.release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m, _ := h.message(context.Background(), "1"); m.State != "sent" || len(other.sent) != 3 {
		t.Fatalf("after the focus block %+v, other %d", m, len(other.sent))
	}
}
