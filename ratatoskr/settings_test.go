package ratatoskr

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/part"
)

func request(t *testing.T, p *part.Part, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(payload))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	var decoded map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &decoded)
	return w.Code, decoded
}

func TestWatchingYouIsTurnedOffByTheOwnerAndStaysOff(t *testing.T) {
	f := &fakeParts{events: longDay()}
	y := youWith(t, f)
	y.now = func() time.Time { return day(10, 0) }
	y.StatePath = filepath.Join(t.TempDir(), "state", "ratatoskr.json")
	r := New(nil, alert.NewClient("", ""), nil)
	r.You = y
	p := part.New(Name, contracttest.Tokens(), nil)
	r.Mount(p, 30*time.Second)

	if status, body := request(t, p, http.MethodGet, "/v1/settings/you", contracttest.Owner, nil); status != http.StatusOK || body["on"] != true {
		t.Fatalf("get: %d %v", status, body)
	}
	if status, _ := request(t, p, http.MethodPatch, "/v1/settings/you", contracttest.Sender, map[string]bool{"on": false}); status != http.StatusForbidden {
		t.Fatalf("a part turned it off: %d", status)
	}
	if status, _ := request(t, p, http.MethodPatch, "/v1/settings/you", contracttest.Owner, map[string]any{}); status != http.StatusBadRequest {
		t.Fatalf("no on: %d", status)
	}
	if status, body := request(t, p, http.MethodPatch, "/v1/settings/you", contracttest.Owner, map[string]bool{"on": false}); status != http.StatusOK || body["on"] != false {
		t.Fatalf("off: %d %v", status, body)
	}

	// Off: no reads, no messages, and /v1/you says so.
	if err := y.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.calls != 0 || len(f.sent) != 0 {
		t.Fatalf("off, but %d calls and %d messages", f.calls, len(f.sent))
	}
	if _, body := request(t, p, http.MethodGet, "/v1/you", contracttest.Owner, nil); body["on"] != false {
		t.Fatalf("/v1/you: %v", body)
	}

	// Kept across a restart.
	state, err := LoadState(y.StatePath)
	if err != nil || state.You == nil || state.You.On {
		t.Fatalf("state %+v, %v", state, err)
	}

	// On again: it looks at once.
	if status, body := request(t, p, http.MethodPatch, "/v1/settings/you", contracttest.Owner, map[string]bool{"on": true}); status != http.StatusOK || body["on"] != true {
		t.Fatalf("on: %d %v", status, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for y.Status().CheckedAt == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if status := y.Status(); !status.On || status.CheckedAt == nil || !status.Patterns[0].Active {
		t.Fatalf("after turning on: %+v", status)
	}
	if state, _ := LoadState(y.StatePath); state.You == nil || !state.You.On {
		t.Fatalf("state %+v", state)
	}
}

func TestNoSavedStateIsAnEmptyOne(t *testing.T) {
	state, err := LoadState(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || state.You != nil {
		t.Fatalf("%+v %v", state, err)
	}
}
