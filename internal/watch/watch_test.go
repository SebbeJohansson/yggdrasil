package watch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"yggdrasil/internal/alert"
)

func down(key string) Result { return Result{Key: key, OK: false, Severity: alert.Warning, Title: key} }
func up(key string) Result   { return Result{Key: key, OK: true} }

func TestTwoFailuresGoDownAndTwoPassesComeBack(t *testing.T) {
	w := New()
	w.Observe([]Result{down("a")})
	if s, _ := w.Get("a"); s.Down {
		t.Fatalf("down after one failure")
	}
	w.Observe([]Result{down("a")})
	if s, _ := w.Get("a"); !s.Down {
		t.Fatalf("not down after two failures")
	}
	w.Observe([]Result{up("a")})
	if s, _ := w.Get("a"); !s.Down {
		t.Fatalf("up after one pass")
	}
	w.Observe([]Result{up("a")})
	if s, _ := w.Get("a"); s.Down {
		t.Fatalf("still down after two passes")
	}
}

func TestAPassBetweenFailuresStartsOver(t *testing.T) {
	w := New()
	w.Observe([]Result{down("a")})
	w.Observe([]Result{up("a")})
	w.Observe([]Result{down("a")})
	if s, _ := w.Get("a"); s.Down {
		t.Fatalf("flapping counted as down")
	}
}

func TestSkipLeavesACheckAsItWas(t *testing.T) {
	w := New()
	w.Observe([]Result{down("a")})
	w.Observe([]Result{down("a")})
	w.Observe([]Result{{Key: "a", Skip: true}})
	w.Observe([]Result{{Key: "a", Skip: true}})
	if s, _ := w.Get("a"); !s.Down {
		t.Fatalf("skipping brought it back up")
	}
}

func TestAForgottenCheckIsDroppedOnceResolved(t *testing.T) {
	w := New()
	w.Observe([]Result{down("a")})
	w.Observe([]Result{down("a")})
	w.Forget(func(string) bool { return false })
	// With no Gjallar nothing is raised, so nothing is left to resolve.
	_ = w.Sync(context.Background(), nil)
	if _, known := w.Get("a"); known {
		t.Fatalf("a forgotten check stayed")
	}
}

// fakeGjallar holds alerts the way Gjallar does and records what it is sent.
type fakeGjallar struct {
	held     []alert.Raise
	raised   []string
	resolved []string
}

func (f *fakeGjallar) client(t *testing.T) *alert.Client {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Key string `json:"key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/alerts/mine":
			_ = json.NewEncoder(w).Encode(map[string]any{"alerts": f.held})
		case "POST /v1/alerts":
			f.raised = append(f.raised, body.Key)
		case "POST /v1/alerts/resolve":
			f.resolved = append(f.resolved, body.Key)
		}
	}))
	t.Cleanup(server.Close)
	return alert.NewClient(server.URL, "token")
}

func TestARestartedWatchResolvesWhatItRaisedBefore(t *testing.T) {
	gjallar := &fakeGjallar{held: []alert.Raise{
		{Key: "back", Severity: alert.Warning, Title: "back"},
		{Key: "still", Severity: alert.Warning, Title: "still"},
		{Key: "removed", Severity: alert.Warning, Title: "removed"},
	}}
	client := gjallar.client(t)
	w := New()
	checked := func(key string) bool { return key != "removed" }
	for range 2 {
		w.Observe([]Result{up("back"), down("still")})
		w.Forget(checked)
		if err := w.Sync(context.Background(), client); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(gjallar.raised, nil) {
		t.Errorf("raised again what Gjallar already holds: %v", gjallar.raised)
	}
	slices.Sort(gjallar.resolved)
	if !slices.Equal(gjallar.resolved, []string{"back", "removed"}) {
		t.Errorf("resolved %v, want back and removed", gjallar.resolved)
	}
	if s, _ := w.Get("still"); !s.Down || !s.Raised {
		t.Errorf("a check still failing is %+v, want down and raised", s)
	}
}

func TestAnAdoptedCheckNeedsTwoPassesLikeAnyOther(t *testing.T) {
	gjallar := &fakeGjallar{held: []alert.Raise{{Key: "a", Severity: alert.Warning, Title: "a"}}}
	client := gjallar.client(t)
	w := New()
	w.Observe([]Result{down("a")})
	_ = w.Sync(context.Background(), client)
	w.Observe([]Result{up("a")})
	_ = w.Sync(context.Background(), client)
	if len(gjallar.resolved) != 0 {
		t.Fatalf("resolved after one pass")
	}
	w.Observe([]Result{up("a")})
	_ = w.Sync(context.Background(), client)
	if !slices.Equal(gjallar.resolved, []string{"a"}) {
		t.Fatalf("resolved %v after two passes, want a", gjallar.resolved)
	}
}

func TestAWatchKeepsAskingUntilGjallarAnswers(t *testing.T) {
	w := New()
	w.Observe([]Result{up("a")})
	if err := w.Sync(context.Background(), alert.NewClient("http://127.0.0.1:1", "token")); err == nil {
		t.Fatal("no error with Gjallar away")
	}
	gjallar := &fakeGjallar{held: []alert.Raise{{Key: "a", Severity: alert.Warning, Title: "a"}}}
	client := gjallar.client(t)
	w.Observe([]Result{up("a")})
	_ = w.Sync(context.Background(), client)
	w.Observe([]Result{up("a")})
	_ = w.Sync(context.Background(), client)
	if !slices.Equal(gjallar.resolved, []string{"a"}) {
		t.Fatalf("resolved %v, want a once Gjallar answered", gjallar.resolved)
	}
}
