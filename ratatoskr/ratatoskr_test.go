package ratatoskr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/part"
)

// fakeGjallar records what Ratatoskr sends.
type fakeGjallar struct {
	mu       sync.Mutex
	raised   []string
	resolved []string
	down     bool
}

func (f *fakeGjallar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch r.URL.Path {
	case "/v1/alerts":
		f.raised = append(f.raised, body.Key)
	case "/v1/alerts/resolve":
		f.resolved = append(f.resolved, body.Key)
	}
	w.WriteHeader(http.StatusOK)
}

func healthServer(t *testing.T, health *part.Health, status *int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *status != http.StatusOK {
			w.WriteHeader(*status)
			return
		}
		part.JSON(w, http.StatusOK, health)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestAPartThatFailsTwiceIsRaisedAndTwoPassesResolveIt(t *testing.T) {
	status := http.StatusOK
	norn := healthServer(t, &part.Health{OK: true, Part: "norn", StartedAt: time.Now()}, &status)
	gjallar := &fakeGjallar{}
	gjallarServer := httptest.NewServer(gjallar)
	defer gjallarServer.Close()

	r := New([]Target{{Name: "norn", URL: norn.URL}}, alert.NewClient(gjallarServer.URL, "t"), nil)
	ctx := context.Background()
	status = http.StatusInternalServerError
	_ = r.check(ctx)
	if len(gjallar.raised) != 0 {
		t.Fatalf("raised after one failure: %v", gjallar.raised)
	}
	_ = r.check(ctx)
	if len(gjallar.raised) != 1 || gjallar.raised[0] != "down:norn" {
		t.Fatalf("raised %v, want [down:norn]", gjallar.raised)
	}
	_ = r.check(ctx)
	if len(gjallar.raised) != 1 {
		t.Fatalf("raised again: %v", gjallar.raised)
	}
	status = http.StatusOK
	_ = r.check(ctx)
	if len(gjallar.resolved) != 0 {
		t.Fatalf("resolved after one pass")
	}
	_ = r.check(ctx)
	if len(gjallar.resolved) != 1 || gjallar.resolved[0] != "down:norn" {
		t.Fatalf("resolved %v, want [down:norn]", gjallar.resolved)
	}
}

func TestAStalledLoopIsAWarning(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	last := now.Add(-10 * time.Minute)
	health := &part.Health{OK: true, Part: "norn", StartedAt: now.Add(-time.Hour), Loops: map[string]part.LoopHealth{
		"inbox":  {EverySeconds: 120, LastRunAt: &last},
		"linear": {EverySeconds: 600, LastRunAt: &last},
	}}
	results := Results(Target{Name: "norn"}, health, nil, now)
	got := map[string]bool{}
	for _, result := range results {
		got[result.Key] = result.OK
	}
	if !got["down:norn"] {
		t.Fatalf("norn counted as down")
	}
	if got["stalled:norn:inbox"] {
		t.Fatalf("inbox runs every 2 min and was last seen 10 min ago: that is stalled")
	}
	if !got["stalled:norn:linear"] {
		t.Fatalf("linear runs every 10 min and was last seen 10 min ago: that is fine")
	}
}

func TestLoopsOfAPartThatIsDownAreNotChecked(t *testing.T) {
	results := Results(Target{Name: "norn"}, nil, context.DeadlineExceeded, time.Now())
	if len(results) != 1 || results[0].Key != "down:norn" {
		t.Fatalf("got %v", results)
	}
}

func TestGjallarBeingDownIsNotSentToGjallar(t *testing.T) {
	status := http.StatusInternalServerError
	down := healthServer(t, nil, &status)
	gjallar := &fakeGjallar{}
	gjallarServer := httptest.NewServer(gjallar)
	defer gjallarServer.Close()

	r := New([]Target{{Name: "gjallar", URL: down.URL}}, alert.NewClient(gjallarServer.URL, "t"), nil)
	_ = r.check(context.Background())
	_ = r.check(context.Background())
	if len(gjallar.raised) != 0 {
		t.Fatalf("sent %v to Gjallar", gjallar.raised)
	}
	if r.Snapshot().GjallarUp {
		t.Fatalf("status says Gjallar is up")
	}
}

func TestAnAlertGjallarDidNotTakeIsSentAgain(t *testing.T) {
	status := http.StatusInternalServerError
	norn := healthServer(t, nil, &status)
	gjallar := &fakeGjallar{down: true}
	gjallarServer := httptest.NewServer(gjallar)
	defer gjallarServer.Close()

	r := New([]Target{{Name: "norn", URL: norn.URL}}, alert.NewClient(gjallarServer.URL, "t"), nil)
	_ = r.check(context.Background())
	if err := r.check(context.Background()); err == nil {
		t.Fatalf("no error while Gjallar refused")
	}
	gjallar.mu.Lock()
	gjallar.down = false
	gjallar.mu.Unlock()
	_ = r.check(context.Background())
	if len(gjallar.raised) != 1 {
		t.Fatalf("raised %v once Gjallar was back", gjallar.raised)
	}
}
