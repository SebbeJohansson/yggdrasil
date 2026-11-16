package hermodr

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/part"
)

type fakeChannel struct {
	name string
	fail bool
	mu   sync.Mutex
	sent []Out
}

func (f *fakeChannel) Name() string { return f.name }
func (f *fakeChannel) On() bool     { return true }
func (f *fakeChannel) Send(_ context.Context, out Out) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("refused")
	}
	f.sent = append(f.sent, out)
	return nil
}

func newTest(t *testing.T, at *time.Time, quiet string) (*Hermodr, *fakeChannel, *fakeChannel) {
	t.Helper()
	phone, other := &fakeChannel{name: "ntfy"}, &fakeChannel{name: "other"}
	q, err := ParseQuiet(quiet)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(":memory:", []Channel{phone, other}, map[string]string{"ntfy": "high", "other": "low"}, q, time.UTC,
		call.New("norn", "", "", time.Second), alert.NewClient("", ""), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	h.now = func() time.Time { return *at }
	return h, phone, other
}

func TestMeetsItsContract(t *testing.T) {
	now := time.Now()
	h, _, _ := newTest(t, &now, "")
	h.Channels = h.Channels[:1] // the contract names ntfy only; the other fake is for routing tests
	p := part.New(Name, contracttest.Tokens(), nil)
	h.Mount(p)
	contracttest.Run(t, p)
}

func TestEachChannelTakesWhatIsAtItsPriority(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	h, phone, other := newTest(t, &now, "")
	normal := h.Take(context.Background(), Message{ID: "1", Title: "Standup notes", Priority: "normal", Push: true, At: now})
	urgent := h.Take(context.Background(), Message{ID: "2", Title: "Anthropic is down", Priority: "urgent", Push: true, At: now})
	listed := h.Take(context.Background(), Message{ID: "3", Title: "Only for the widget", Priority: "urgent", Push: false, At: now})
	if normal.State != "sent" || urgent.State != "sent" || listed.State != "listed" {
		t.Fatalf("%s %s %s", normal.State, urgent.State, listed.State)
	}
	if len(phone.sent) != 1 || phone.sent[0].Title != "Anthropic is down" || len(other.sent) != 2 {
		t.Fatalf("phone %v, other %v", phone.sent, other.sent)
	}
}

func TestQuietHoursAndMeetingsHoldAllButUrgent(t *testing.T) {
	now := time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)
	h, _, other := newTest(t, &now, "22:00-07:00")
	held := h.Take(context.Background(), Message{ID: "1", Title: "Job done", Priority: "normal", Push: true, At: now})
	urgent := h.Take(context.Background(), Message{ID: "2", Title: "Critical", Priority: "urgent", Push: true, At: now})
	if held.State != "held" || held.Reason != "quiet hours until 07:00" || urgent.State != "sent" {
		t.Fatalf("%+v / %+v", held, urgent)
	}

	// Morning, in a meeting until 10:30: a message that asks to hold waits; one that doesn't goes.
	now = time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	h.SetMeetings([]CalendarEvent{{Start: now.Add(-time.Hour).UnixMilli(), End: now.Add(30 * time.Minute).UnixMilli(), ShowAs: "busy"}})
	brief := h.Take(context.Background(), Message{ID: "3", Title: "Brief", Priority: "normal", Hold: true, Push: true, At: now})
	plain := h.Take(context.Background(), Message{ID: "4", Title: "Plain", Priority: "normal", Push: true, At: now})
	if brief.State != "held" || brief.Reason != "in a meeting until 10:30" || plain.State != "sent" {
		t.Fatalf("%+v / %+v", brief, plain)
	}
	if err := h.release(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The night's message goes now (quiet hours are over); the brief still waits.
	if m, _ := h.message(context.Background(), "1"); m.State != "sent" {
		t.Fatalf("night message %+v", m)
	}
	if m, _ := h.message(context.Background(), "3"); m.State != "held" {
		t.Fatalf("brief %+v", m)
	}
	now = now.Add(31 * time.Minute)
	if err := h.release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m, _ := h.message(context.Background(), "3"); m.State != "sent" || len(other.sent) != 4 {
		t.Fatalf("brief after the meeting %+v, other %d", m, len(other.sent))
	}
}

func TestLaterHoldsEvenUrgentUntilItsTime(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	h, phone, _ := newTest(t, &now, "")
	at := now.Add(5 * time.Hour)
	later := h.Take(context.Background(), Message{ID: "1", Title: "Call Anna back", Priority: "urgent", Push: true, NotBefore: &at, At: now})
	if later.State != "held" || later.Reason != "later, at Tue 15:00" {
		t.Fatalf("%+v", later)
	}
	now = at.Add(-time.Minute)
	if err := h.release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m, _ := h.message(context.Background(), "1"); m.State != "held" {
		t.Fatalf("a minute early %+v", m)
	}
	now = at
	if err := h.release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m, _ := h.message(context.Background(), "1"); m.State != "sent" || len(phone.sent) != 1 {
		t.Fatalf("at its time %+v, phone %d", m, len(phone.sent))
	}
}

func TestFocusHoldsAllButUrgentUntilItEnds(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	h, _, other := newTest(t, &now, "")
	until := now.Add(90 * time.Minute)
	if err := h.setFocus(context.Background(), Focus{Until: &until, StartedAt: &now, Goal: &FocusGoal{ID: 14, Title: "Retries"}}); err != nil {
		t.Fatal(err)
	}
	if f := h.focusNow(); !f.On || f.Goal.ID != 14 {
		t.Fatalf("focus %+v", f)
	}
	held := h.Take(context.Background(), Message{ID: "1", Title: "Inbox", Priority: "high", Push: true, At: now})
	listed := h.Take(context.Background(), Message{ID: "2", Title: "For the widget", Priority: "normal", Push: false, At: now})
	urgent := h.Take(context.Background(), Message{ID: "3", Title: "Norn is down", Priority: "urgent", Push: true, At: now})
	if held.State != "held" || held.Reason != "you are focusing until 11:30" || listed.State != "held" || urgent.State != "sent" {
		t.Fatalf("%+v / %+v / %+v", held, listed, urgent)
	}
	// Its time up, the focus is off by itself and what waited goes.
	now = until
	if f := h.focusNow(); f.On {
		t.Fatalf("still on %+v", f)
	}
	if err := h.release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m, _ := h.message(context.Background(), "1"); m.State != "sent" || len(other.sent) != 2 {
		t.Fatalf("after focus %+v, other %d", m, len(other.sent))
	}
	if m, _ := h.message(context.Background(), "2"); m.State != "listed" {
		t.Fatalf("widget one after focus %+v", m)
	}
}

func TestFocusOverHTTPIsTheOwnersAndOutlivesARestart(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	path := t.TempDir() + "/hermodr.db"
	open := func() *Hermodr {
		h, err := New(path, nil, nil, Quiet{}, time.UTC, call.New("norn", "", "", time.Second), alert.NewClient("", ""), t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		h.now = func() time.Time { return now }
		return h
	}
	h := open()
	p := part.New(Name, contracttest.Tokens(), nil)
	h.Mount(p)
	server := httptest.NewServer(p)
	defer server.Close()
	do := func(method, token, body string) int {
		req, _ := http.NewRequest(method, server.URL+"/v1/focus", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := do("PUT", contracttest.Sender, `{"until":"2026-11-17T11:00:00Z"}`); code != http.StatusForbidden {
		t.Fatalf("sender: %d", code)
	}
	if code := do("PUT", contracttest.Owner, `{"until":"2026-11-18T11:00:00Z"}`); code != http.StatusBadRequest {
		t.Fatalf("a day: %d", code)
	}
	if code := do("PUT", contracttest.Owner, `{"until":"2026-11-17T11:00:00Z","goal":{"id":3,"title":"Dark mode"}}`); code != http.StatusOK {
		t.Fatalf("owner: %d", code)
	}
	h.Close()
	again := open()
	defer again.Close()
	if f := again.focusNow(); !f.On || f.Goal == nil || f.Goal.Title != "Dark mode" {
		t.Fatalf("after a restart %+v", f)
	}
}

func TestTheSenderIsItsTokenAndOnlyYouMarkSeen(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	h, _, _ := newTest(t, &now, "")
	p := part.New(Name, contracttest.Tokens(), nil)
	h.Mount(p)
	server := httptest.NewServer(p)
	defer server.Close()
	do := func(token, method, path, body string) *http.Response {
		request, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := do(contracttest.Sender, "POST", "/v1/messages", `{"title":"Hi","source":"someone-else"}`)
	if response.StatusCode != 201 {
		t.Fatalf("%d", response.StatusCode)
	}
	list, _ := h.messages(context.Background(), "", 10)
	if len(list) != 1 || list[0].Source != "sender" {
		t.Fatalf("%+v", list)
	}
	if response := do(contracttest.Sender, "POST", "/v1/messages/"+list[0].ID+"/seen", ""); response.StatusCode != 403 {
		t.Fatalf("a part marked it seen: %d", response.StatusCode)
	}
	if response := do(contracttest.Owner, "POST", "/v1/messages/"+list[0].ID+"/seen", ""); response.StatusCode != 200 {
		t.Fatalf("%d", response.StatusCode)
	}
}

func TestQuietHoursCrossMidnight(t *testing.T) {
	q, _ := ParseQuiet("22:00-07:00")
	if _, in := q.Until(time.Date(2026, 1, 1, 21, 59, 0, 0, time.UTC)); in {
		t.Error("21:59 is not quiet")
	}
	if end, in := q.Until(time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)); !in || end.Hour() != 7 || end.Day() != 1 {
		t.Errorf("03:00: %v %v", end, in)
	}
	if end, in := q.Until(time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC)); !in || end.Day() != 2 {
		t.Errorf("23:00: %v %v", end, in)
	}
	if _, err := ParseQuiet("22-7"); err == nil {
		t.Error("22-7 parsed")
	}
}
