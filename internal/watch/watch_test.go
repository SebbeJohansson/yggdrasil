package watch

import (
	"context"
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
