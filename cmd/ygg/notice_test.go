package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLatest serves tag as GitHub's newest release and counts the asks.
func fakeLatest(t *testing.T, tag string, status int) *atomic.Int32 {
	t.Helper()
	var asks atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asks.Add(1)
		if status != http.StatusOK {
			http.Error(w, "no", status)
			return
		}
		_, _ = w.Write([]byte(`{"tag_name":"` + tag + `"}`))
	}))
	t.Cleanup(server.Close)
	before := latestURL
	latestURL = server.URL
	t.Cleanup(func() { latestURL = before })
	return &asks
}

func notice(version, cache string, now time.Time) string {
	var out bytes.Buffer
	startUpdateCheck(version, cache, http.DefaultClient, now).finish(&out, 5*time.Second)
	return out.String()
}

func TestUpdateNoticeWhenBehind(t *testing.T) {
	fakeLatest(t, "v0.3.0", http.StatusOK)
	got := notice("v0.2.1", filepath.Join(t.TempDir(), "latest.json"), fixedNow)
	if !strings.Contains(got, "Yggdrasil v0.3.0 is out (this machine runs v0.2.1): ygg upgrade v0.3.0") {
		t.Fatalf("notice = %q", got)
	}
}

func TestNoUpdateNoticeWhenCurrentOrNewer(t *testing.T) {
	fakeLatest(t, "v0.3.0", http.StatusOK)
	for _, version := range []string{"v0.3.0", "v0.4.0-rc.1"} {
		if got := notice(version, filepath.Join(t.TempDir(), "latest.json"), fixedNow); got != "" {
			t.Errorf("%s: notice = %q, want none", version, got)
		}
	}
}

func TestUpdateCheckAsksAtMostEverySixHours(t *testing.T) {
	asks := fakeLatest(t, "v0.3.0", http.StatusOK)
	cache := filepath.Join(t.TempDir(), "latest.json")
	notice("v0.2.1", cache, fixedNow)
	if got := notice("v0.2.1", cache, fixedNow.Add(5*time.Hour)); got == "" {
		t.Fatal("no notice from the cached answer")
	}
	if asks.Load() != 1 {
		t.Fatalf("asked %d times within 6 hours, want 1", asks.Load())
	}
	notice("v0.2.1", cache, fixedNow.Add(7*time.Hour))
	if asks.Load() != 2 {
		t.Fatalf("asked %d times after 6 hours, want 2", asks.Load())
	}
}

func TestUpdateCheckKeepsLastAnswerWhenGitHubFails(t *testing.T) {
	fakeLatest(t, "v0.3.0", http.StatusOK)
	cache := filepath.Join(t.TempDir(), "latest.json")
	notice("v0.2.1", cache, fixedNow)
	fakeLatest(t, "", http.StatusForbidden)
	if got := notice("v0.2.1", cache, fixedNow.Add(7*time.Hour)); !strings.Contains(got, "v0.3.0 is out") {
		t.Fatalf("notice = %q, want the last answer", got)
	}
}

func TestUpdateCheckDoesNotHoldACommandUp(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	before := latestURL
	latestURL = server.URL
	t.Cleanup(func() { latestURL = before })

	var out bytes.Buffer
	start := time.Now()
	startUpdateCheck("v0.2.1", filepath.Join(t.TempDir(), "latest.json"), http.DefaultClient, fixedNow).finish(&out, 50*time.Millisecond)
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("waited %s for GitHub", waited)
	}
	if out.Len() != 0 {
		t.Fatalf("notice = %q, want none", out.String())
	}
}
