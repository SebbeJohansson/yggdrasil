package part

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const (
	ownerToken = "owner-token-0123456789abcdef0123456789"
	lokiToken  = "loki-token-0123456789abcdef01234567890"
	otherToken = "norn-token-0123456789abcdef01234567890"
)

func trickPart(t *testing.T) (*Part, *time.Time) {
	t.Helper()
	p := New("test", map[string]string{ownerToken: Owner, lokiToken: TrickCaller, otherToken: "norn"}, nil)
	clock := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	p.tricks.now = func() time.Time { return clock }
	p.Loop("inbox", time.Minute, func(context.Context) error { return nil })
	p.Handle("GET /v1/thing", func(w http.ResponseWriter, _ *http.Request) { JSON(w, http.StatusOK, map[string]bool{"ok": true}) })
	return p, &clock
}

func do(t *testing.T, p *Part, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		payload, _ := json.Marshal(body)
		reader = bytes.NewReader(payload)
	}
	request := httptest.NewRequest(method, path, reader)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, request)
	var answer map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &answer)
	return recorder.Code, answer
}

func TestTheTrickSwitchIsNotThereWithoutLokiOn(t *testing.T) {
	t.Setenv("LOKI", "")
	p, clock := trickPart(t)
	for _, method := range []string{"GET", "POST", "DELETE"} {
		code, _ := do(t, p, method, "/v1/loki/trick", ownerToken, map[string]any{"kind": "health", "until": clock.Add(time.Minute).UnixMilli()})
		if code != http.StatusNotFound {
			t.Errorf("%s without LOKI=on: %d, want 404", method, code)
		}
	}
	if code, _ := do(t, p, "GET", "/health", "", nil); code != http.StatusOK {
		t.Fatalf("health: %d", code)
	}
	for _, route := range p.Routes() {
		if route == "GET /v1/loki/trick" {
			t.Error("the trick switch is listed as the part's own route")
		}
	}
}

func TestOnlyTheOwnerAndLokiMayTrick(t *testing.T) {
	t.Setenv("LOKI", "on")
	p, clock := trickPart(t)
	until := clock.Add(time.Minute).UnixMilli()
	if code, _ := do(t, p, "POST", "/v1/loki/trick", "", map[string]any{"kind": "health", "until": until}); code != http.StatusUnauthorized {
		t.Errorf("no token: %d", code)
	}
	if code, _ := do(t, p, "POST", "/v1/loki/trick", otherToken, map[string]any{"kind": "health", "until": until}); code != http.StatusForbidden {
		t.Errorf("another part: %d", code)
	}
	if code, _ := do(t, p, "POST", "/v1/loki/trick", lokiToken, map[string]any{"kind": "health", "until": until}); code != http.StatusOK {
		t.Errorf("loki: %d", code)
	}
	if code, _ := do(t, p, "POST", "/v1/loki/trick", ownerToken, map[string]any{"kind": "health", "until": until}); code != http.StatusConflict {
		t.Errorf("a second trick: %d, want 409", code)
	}
	if code, _ := do(t, p, "DELETE", "/v1/loki/trick", ownerToken, nil); code != http.StatusOK {
		t.Errorf("owner ends it: %d", code)
	}
	if _, answer := do(t, p, "GET", "/v1/loki/trick", ownerToken, nil); answer["trick"] != nil {
		t.Errorf("after DELETE: %v", answer)
	}
}

func TestATrickMustEndWithinTenMinutes(t *testing.T) {
	t.Setenv("LOKI", "on")
	p, clock := trickPart(t)
	for _, c := range []map[string]any{
		{"kind": "health", "until": clock.Add(-time.Second).UnixMilli()},
		{"kind": "health", "until": clock.Add(11 * time.Minute).UnixMilli()},
		{"kind": "health"},
		{"kind": "sleep", "until": clock.Add(time.Minute).UnixMilli()},
		{"kind": "loop", "loop": "nope", "until": clock.Add(time.Minute).UnixMilli()},
		{"kind": "slow", "until": clock.Add(time.Minute).UnixMilli()},
		{"kind": "slow", "delayMs": 10001, "until": clock.Add(time.Minute).UnixMilli()},
	} {
		if code, _ := do(t, p, "POST", "/v1/loki/trick", lokiToken, c); code != http.StatusBadRequest {
			t.Errorf("%v: %d, want 400", c, code)
		}
	}
}

func TestTheHealthTrickAnswers503UntilItEnds(t *testing.T) {
	t.Setenv("LOKI", "on")
	p, clock := trickPart(t)
	do(t, p, "POST", "/v1/loki/trick", lokiToken, map[string]any{"kind": "health", "until": clock.Add(3 * time.Minute).UnixMilli()})
	code, answer := do(t, p, "GET", "/health", "", nil)
	if code != http.StatusServiceUnavailable || answer["ok"] != false {
		t.Fatalf("tricked health: %d %v", code, answer)
	}
	*clock = clock.Add(3 * time.Minute)
	code, answer = do(t, p, "GET", "/health", "", nil)
	if code != http.StatusOK || answer["ok"] != true {
		t.Fatalf("after until: %d %v", code, answer)
	}
	if _, answer := do(t, p, "GET", "/v1/loki/trick", lokiToken, nil); answer["trick"] != nil {
		t.Errorf("trick still listed after until: %v", answer)
	}
}

func TestTheLoopTrickFailsTheLoopAndHealsAfter(t *testing.T) {
	t.Setenv("LOKI", "on")
	p, clock := trickPart(t)
	ran := 0
	p.loops[0].run = func(context.Context) error { ran++; return nil }
	do(t, p, "POST", "/v1/loki/trick", lokiToken, map[string]any{"kind": "loop", "loop": "inbox", "until": clock.Add(time.Minute).UnixMilli()})
	logger := log.New(io.Discard, "", 0)
	p.loops[0].once(context.Background(), logger)
	if ran != 0 {
		t.Error("a tricked loop did its work")
	}
	_, answer := do(t, p, "GET", "/health", "", nil)
	if got := answer["loops"].(map[string]any)["inbox"].(map[string]any)["lastError"]; got != TrickError {
		t.Fatalf("lastError while tricked: %v", got)
	}
	*clock = clock.Add(time.Minute)
	_, answer = do(t, p, "GET", "/health", "", nil)
	if got, there := answer["loops"].(map[string]any)["inbox"].(map[string]any)["lastError"]; there {
		t.Fatalf("lastError after the trick: %v", got)
	}
	p.loops[0].once(context.Background(), logger)
	if ran != 1 {
		t.Error("the loop did not run again after the trick")
	}
}

func TestTheSlowTrickHoldsRoutesButNotHealth(t *testing.T) {
	t.Setenv("LOKI", "on")
	p, _ := trickPart(t)
	p.tricks.now = time.Now
	server := httptest.NewServer(p)
	defer server.Close()
	post, _ := http.NewRequest("POST", server.URL+"/v1/loki/trick", bytes.NewReader([]byte(fmt.Sprintf(`{"kind":"slow","delayMs":300,"until":%d}`, time.Now().Add(time.Minute).UnixMilli()))))
	post.Header.Set("Authorization", "Bearer "+lokiToken)
	if response, err := http.DefaultClient.Do(post); err != nil || response.StatusCode != 200 {
		t.Fatalf("set slow: %v %v", err, response)
	}
	timed := func(path string) time.Duration {
		request, _ := http.NewRequest("GET", server.URL+path, nil)
		request.Header.Set("Authorization", "Bearer "+ownerToken)
		start := time.Now()
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return time.Since(start)
	}
	if took := timed("/v1/thing"); took < 300*time.Millisecond {
		t.Errorf("a route answered in %s while slow", took)
	}
	if took := timed("/health"); took >= 300*time.Millisecond {
		t.Errorf("/health waited %s", took)
	}
	if took := timed("/v1/loki/trick"); took >= 300*time.Millisecond {
		t.Errorf("the trick switch waited %s", took)
	}
}
