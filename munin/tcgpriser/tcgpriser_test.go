package tcgpriser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/watch"
)

const token = "test-token"

// fakeAPI is a tcgpriser API: healthy, degraded, two alerts, and usage.
type fakeAPI struct {
	mu      sync.Mutex
	health  int
	alerts  []string // ids, oldest first
	afters  []string
	usage   int
	unknown bool // answers 404 on the admin routes, as an API without them
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/health" {
		if f.health != 0 {
			w.WriteHeader(f.health)
			fmt.Fprint(w, `{"status":"unhealthy","mongodb":"disconnected","uptime":1}`)
			return
		}
		fmt.Fprint(w, `{"status":"healthy","mongodb":"connected","uptime":100}`)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+token {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"unauthorized","message":"Invalid token"}}`)
		return
	}
	if f.unknown {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"notFound","message":"Cannot GET `+r.URL.Path+`"}}`)
		return
	}
	switch r.URL.Path {
	case "/admin/status":
		fmt.Fprint(w, `{"status":"degraded","checkedAt":"2026-11-17T20:00:00Z","mongodb":"connected",
			"delivery":{"configured":true,"ready":false,"failingSince":"2026-11-17T19:30:00Z"},
			"cmapi":{"cap":100,"used":10,"remaining":90,"planDailyBudget":100,"windowEndsAt":null,"pausedUntil":null},
			"freshness":[],"openAlerts":1}`)
	case "/admin/alerts":
		after := r.URL.Query().Get("after")
		f.afters = append(f.afters, after)
		var items []string
		var last string
		for _, id := range f.alerts {
			if id > after {
				resolved := id == "b2"
				items = append(items, fmt.Sprintf(`{"id":%q,"source":"delivery","trigger":"health","severity":"critical","key":"delivery|health",
					"resolved":%t,"title":"Delivery %s","message":"msg %s","data":null,"discordDelivered":true,"createdAt":"2026-11-17T19:59:00Z"}`, id, resolved, id, id))
				last = id
			}
		}
		next := "null"
		if last != "" {
			next = fmt.Sprintf("%q", last)
		}
		fmt.Fprintf(w, `{"items":[%s],"nextCursor":%s}`, strings.Join(items, ","), next)
	case "/admin/usage":
		if r.URL.Query().Has("from") || r.URL.Query().Has("to") {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":"validationFailed","message":"Invalid from format. Use YYYY-MM-DD."}}`)
			return
		}
		f.usage++
		fmt.Fprint(w, `{"from":"2026-11-16T20:00:00Z","to":"2026-11-17T20:00:00Z",
			"totals":{"requests":1000,"clientErrors":10,"serverErrors":2,"avgMs":40.5},
			"byCallerType":[{"callerType":"user","requests":600,"clientErrors":5,"serverErrors":1,"avgMs":30,"callers":12}],
			"topCallers":[{"callerType":"user","callerId":"u1","requests":300,"clientErrors":0,"serverErrors":0,"avgMs":20,
				"user":{"id":"u1","name":"Ash","email":"ash@example.com"}}],
			"byOperation":[],"byDay":[]}`)
	default:
		http.NotFound(w, r)
	}
}

func newSource(t *testing.T, api *fakeAPI, tok string) *Source {
	t.Helper()
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	made, err := New(func(name string) string {
		return map[string]string{"URL": server.URL, "TOKEN": tok}[name]
	})
	if err != nil {
		t.Fatal(err)
	}
	s := made.(*Source)
	s.now = func() time.Time { return time.Date(2026, 11, 17, 20, 0, 0, 0, time.UTC) }
	return s
}

func byKey(results []watch.Result) map[string]watch.Result {
	keyed := map[string]watch.Result{}
	for _, result := range results {
		keyed[result.Key] = result
	}
	return keyed
}

func TestWithoutATokenOnlyHealthIsChecked(t *testing.T) {
	api := &fakeAPI{alerts: []string{"a1"}}
	report := newSource(t, api, "").Poll(context.Background(), "")
	checks := byKey(report.Checks)
	if !report.Up || !checks[DownKey].OK || !checks[StatusKey].OK || !checks[AdminKey].OK || len(report.Events) != 0 {
		t.Fatalf("got %+v", report)
	}
	if len(api.afters) != 0 || api.usage != 0 {
		t.Fatalf("admin routes were called without a token")
	}
}

func TestAnUnhealthyAPIIsOneCriticalAlertNotThree(t *testing.T) {
	report := newSource(t, &fakeAPI{health: http.StatusServiceUnavailable}, token).Poll(context.Background(), "")
	checks := byKey(report.Checks)
	if report.Up || checks[DownKey].OK || checks[DownKey].Severity != alert.Critical || !checks[StatusKey].Skip || !checks[AdminKey].Skip {
		t.Fatalf("got %+v", report)
	}
	if !strings.Contains(checks[DownKey].Detail, "disconnected") {
		t.Fatalf("detail %q does not say why", checks[DownKey].Detail)
	}
}

func TestStatusAlertsAndUsageAreRead(t *testing.T) {
	api := &fakeAPI{alerts: []string{"a1", "b2"}}
	s := newSource(t, api, token)
	report := s.Poll(context.Background(), "")
	checks := byKey(report.Checks)
	if !checks[AdminKey].OK {
		t.Fatalf("admin: %+v", checks[AdminKey])
	}
	status := checks[StatusKey]
	if status.OK || status.Severity != alert.Warning || !strings.Contains(status.Detail, "delivery not ready for 30 min") {
		t.Fatalf("status: %+v", status)
	}
	if len(report.Events) != 2 || report.Events[0].Resolved || !report.Events[1].Resolved || report.Cursor != "b2" {
		t.Fatalf("events %+v cursor %q", report.Events, report.Cursor)
	}
	if event := report.Events[0]; event.Key != "delivery|health" || event.Severity != alert.Critical || event.Title != "tcgpriser: Delivery a1" || event.ExpiresAt == nil {
		t.Fatalf("event %+v", event)
	}
	data := report.Data.(*Data)
	if data.Usage == nil || data.Usage.Totals.Requests != 1000 || data.Usage.TopCallers[0].Name != "Ash" {
		t.Fatalf("usage %+v", data.Usage)
	}
	if len(data.Recent) != 2 || data.Recent[0].ID != "b2" {
		t.Fatalf("recent %+v", data.Recent)
	}

	// The next poll goes on from the cursor, and usage waits its 15 minutes.
	api.alerts = append(api.alerts, "c3")
	report = s.Poll(context.Background(), "b2")
	if len(report.Events) != 1 || report.Cursor != "c3" || api.usage != 1 {
		t.Fatalf("events %+v cursor %q usage calls %d", report.Events, report.Cursor, api.usage)
	}
	if recent := report.Data.(*Data).Recent; len(recent) != 3 || recent[0].ID != "c3" {
		t.Fatalf("recent %+v", recent)
	}
}

func TestTheEmailIsNotKept(t *testing.T) {
	report := newSource(t, &fakeAPI{}, token).Poll(context.Background(), "")
	if usage := fmt.Sprintf("%+v", report.Data.(*Data).Usage); strings.Contains(usage, "ash@example.com") {
		t.Fatalf("usage keeps the email: %s", usage)
	}
}

func TestAnAPIWithoutTheAdminRoutesIsAnAdminAlert(t *testing.T) {
	report := newSource(t, &fakeAPI{unknown: true}, token).Poll(context.Background(), "x")
	checks := byKey(report.Checks)
	if !report.Up || checks[AdminKey].OK || !strings.Contains(checks[AdminKey].Detail, "Cannot GET /admin/status") || !checks[StatusKey].Skip {
		t.Fatalf("got %+v", report)
	}
	if report.Cursor != "x" {
		t.Fatalf("a failed feed moved the cursor to %q", report.Cursor)
	}
}

func TestAWrongTokenIsAnAdminAlert(t *testing.T) {
	report := newSource(t, &fakeAPI{}, "wrong").Poll(context.Background(), "")
	if check := byKey(report.Checks)[AdminKey]; check.OK || !strings.Contains(check.Detail, "401") {
		t.Fatalf("got %+v", check)
	}
}

func TestAfterARestartTheDashboardStillGetsTheLastDay(t *testing.T) {
	api := &fakeAPI{alerts: []string{"a1", "b2"}}
	report := newSource(t, api, token).Poll(context.Background(), "b2")
	if len(report.Events) != 0 {
		t.Fatalf("old alerts raised again: %+v", report.Events)
	}
	if recent := report.Data.(*Data).Recent; len(recent) != 2 {
		t.Fatalf("recent %+v", recent)
	}
}
