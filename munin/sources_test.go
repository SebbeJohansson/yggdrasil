package munin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/watch"
	"yggdrasil/munin/source"
)

// fakeSource answers each Poll with the next report.
type fakeSource struct {
	reports []source.Report
	cursors []string
}

func (f *fakeSource) Name() string         { return "site" }
func (f *fakeSource) Every() time.Duration { return time.Minute }
func (f *fakeSource) Poll(_ context.Context, cursor string) source.Report {
	f.cursors = append(f.cursors, cursor)
	report := f.reports[0]
	if len(f.reports) > 1 {
		f.reports = f.reports[1:]
	}
	return report
}

// fakeGjallar records what Munin sent it, and holds what it is told to.
type fakeGjallar struct {
	mu     sync.Mutex
	calls  []string
	held   []alert.Raise
	refuse bool
}

func (g *fakeGjallar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.URL.Path == "/v1/alerts/mine" {
		_ = json.NewEncoder(w).Encode(map[string]any{"alerts": g.held})
		return
	}
	if g.refuse {
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	verb := "raise"
	if strings.HasSuffix(r.URL.Path, "/resolve") {
		verb = "resolve"
	}
	g.calls = append(g.calls, verb+" "+body.Key)
	w.Write([]byte(`{}`))
}

func newSourceMunin(t *testing.T, s source.Source, g *fakeGjallar) *Munin {
	t.Helper()
	server := httptest.NewServer(g)
	t.Cleanup(server.Close)
	m := New(nil, nil, "", alert.NewClient(server.URL, "t"))
	state, _ := LoadState("")
	m.UseSources([]source.Source{s}, filepath.Join(t.TempDir(), "munin.json"), state)
	return m
}

func TestASourcesChecksAreDampedAndPrefixed(t *testing.T) {
	down := source.Report{Checks: []watch.Result{{Key: "down", Severity: alert.Critical, Title: "site is down"}}}
	g := &fakeGjallar{}
	m := newSourceMunin(t, &fakeSource{reports: []source.Report{down}}, g)
	_ = m.poll(context.Background(), m.Sources[0])
	if len(g.calls) != 0 {
		t.Fatalf("raised after one failure: %v", g.calls)
	}
	_ = m.poll(context.Background(), m.Sources[0])
	if strings.Join(g.calls, ",") != "raise site:down" {
		t.Fatalf("got %v", g.calls)
	}
	if !m.checked("site:down") || m.checked("other:down") {
		t.Fatalf("checked does not know the source's keys")
	}
}

func TestFeedEventsAreRaisedAndResolvedInOrderAndTheCursorKept(t *testing.T) {
	soon := time.Now().Add(time.Hour)
	gone := time.Now().Add(-time.Hour)
	report := source.Report{Cursor: "c2", Events: []source.Event{
		{Key: "delivery|health", Severity: alert.Critical, Title: "down", ExpiresAt: &soon},
		{Key: "old", Severity: alert.Warning, Title: "over", ExpiresAt: &gone},
		{Key: "delivery|health", Resolved: true},
	}}
	g := &fakeGjallar{}
	s := &fakeSource{reports: []source.Report{report, {}}}
	m := newSourceMunin(t, s, g)
	_ = m.poll(context.Background(), s)
	if got := strings.Join(g.calls, ","); got != "raise feed:site:delivery|health,resolve feed:site:delivery|health" {
		t.Fatalf("got %s", got)
	}
	_ = m.poll(context.Background(), s)
	if s.cursors[1] != "c2" {
		t.Fatalf("the next poll got cursor %q", s.cursors[1])
	}
	saved, err := LoadState(m.StatePath)
	if err != nil || saved.Cursors["site"] != "c2" {
		t.Fatalf("saved %+v %v", saved, err)
	}
}

func TestACursorIsNotKeptWhenGjallarRefused(t *testing.T) {
	report := source.Report{Cursor: "c2", Events: []source.Event{{Key: "k", Severity: alert.Warning, Title: "t"}}}
	g := &fakeGjallar{refuse: true}
	s := &fakeSource{reports: []source.Report{report}}
	m := newSourceMunin(t, s, g)
	if err := m.poll(context.Background(), s); err == nil {
		t.Fatalf("a refused raise was not an error")
	}
	_ = m.poll(context.Background(), s)
	if s.cursors[1] != "" {
		t.Fatalf("cursor moved past an event Gjallar never took: %q", s.cursors[1])
	}
	if _, err := os.Stat(m.StatePath); err == nil {
		t.Fatalf("state written")
	}
}

func TestFeedAlertsHeldFromBeforeARestartAreLeftAlone(t *testing.T) {
	g := &fakeGjallar{held: []alert.Raise{{Key: "feed:site:k", Severity: alert.Critical, Title: "t"}}}
	s := &fakeSource{reports: []source.Report{{}}}
	m := newSourceMunin(t, s, g)
	for range 3 {
		_ = m.poll(context.Background(), s)
	}
	if len(g.calls) != 0 {
		t.Fatalf("the watch touched a feed alert: %v", g.calls)
	}
}

func TestASourceIsNotPolledWhileOffline(t *testing.T) {
	s := &fakeSource{reports: []source.Report{{}}}
	m := newSourceMunin(t, s, &fakeGjallar{})
	m.watch.Observe([]watch.Result{{Key: OfflineKey}, {Key: OfflineKey}})
	_ = m.poll(context.Background(), s)
	if len(s.cursors) != 0 {
		t.Fatalf("polled while offline")
	}
}

func TestMakeSources(t *testing.T) {
	sources, err := MakeSources([]string{"tcgpriser"}, func(name string) string {
		return map[string]string{"MUNIN_TCGPRISER_URL": "http://127.0.0.1:5000"}[name]
	})
	if err != nil || len(sources) != 1 || sources[0].Name() != "tcgpriser" {
		t.Fatalf("got %v %v", sources, err)
	}
	if _, err := MakeSources([]string{"nope"}, os.Getenv); err == nil || !strings.Contains(err.Error(), "tcgpriser") {
		t.Fatalf("an unknown source: %v", err)
	}
	if _, err := MakeSources([]string{"tcgpriser"}, func(string) string { return "not a url" }); err == nil {
		t.Fatalf("a bad URL was accepted")
	}
}
