package gjallar

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/part"
	"yggdrasil/internal/tell"
)

const (
	ownerToken = "owner-token-0123456789abcdef0123456789"
	ratToken   = "ratatoskr-token-0123456789abcdef012345"
	muninToken = "munin-token-0123456789abcdef0123456789"
	glimToken  = "glim-token-0123456789abcdef01234567890"
)

type fixture struct {
	t     *testing.T
	g     *Gjallar
	p     *part.Part
	clock time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f := &fixture{t: t, clock: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	store.now = func() time.Time { return f.clock }
	f.g = New(store, nil)
	f.g.now = store.now
	f.g.started = f.clock
	f.g.LimitedSources["glim"] = true
	f.p = part.New(Name, map[string]string{ownerToken: part.Owner, ratToken: "ratatoskr", muninToken: "munin", glimToken: "glim"}, nil)
	f.g.Mount(f.p)
	return f
}

func (f *fixture) call(method, path, token string, body any) (int, map[string]any) {
	f.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		payload, _ := json.Marshal(body)
		reader = bytes.NewReader(payload)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, reader)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	f.p.ServeHTTP(recorder, request)
	var decoded map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return recorder.Code, decoded
}

func alertOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	a, ok := body["alert"].(map[string]any)
	if !ok {
		t.Fatalf("no alert in %v", body)
	}
	return a
}

func TestRaiseTakesTheSourceFromTheToken(t *testing.T) {
	f := newFixture(t)
	status, body := f.call("POST", "/v1/alerts", muninToken, alert.Raise{Key: "down:ollama", Severity: alert.Warning, Title: "Ollama doesn't answer"})
	if status != http.StatusCreated {
		t.Fatalf("status %d: %v", status, body)
	}
	if got := alertOf(t, body)["source"]; got != "munin" {
		t.Fatalf("source %v, want munin", got)
	}
}

func TestTheSameKeyUpdatesInsteadOfAddingASecond(t *testing.T) {
	f := newFixture(t)
	raise := alert.Raise{Key: "disk:/", Severity: alert.Warning, Title: "Disk is 91% full"}
	f.call("POST", "/v1/alerts", ratToken, raise)
	f.clock = f.clock.Add(time.Minute)
	raise.Title = "Disk is 92% full"
	status, body := f.call("POST", "/v1/alerts", ratToken, raise)
	if status != http.StatusOK || body["change"] != string(Updated) {
		t.Fatalf("status %d, change %v", status, body["change"])
	}
	a := alertOf(t, body)
	if a["count"] != float64(2) || a["title"] != "Disk is 92% full" {
		t.Fatalf("got %v", a)
	}
	_, list := f.call("GET", "/v1/alerts", ownerToken, nil)
	if n := len(list["alerts"].([]any)); n != 1 {
		t.Fatalf("%d alerts, want 1", n)
	}
}

func TestTwoSourcesMayUseTheSameKey(t *testing.T) {
	f := newFixture(t)
	raise := alert.Raise{Key: "down:x", Severity: alert.Info, Title: "x"}
	f.call("POST", "/v1/alerts", ratToken, raise)
	f.call("POST", "/v1/alerts", muninToken, raise)
	_, list := f.call("GET", "/v1/alerts", ownerToken, nil)
	if n := len(list["alerts"].([]any)); n != 2 {
		t.Fatalf("%d alerts, want 2", n)
	}
}

func TestAPartCannotResolveAnothersAlert(t *testing.T) {
	f := newFixture(t)
	f.call("POST", "/v1/alerts", muninToken, alert.Raise{Key: "down:laya", Severity: alert.Warning, Title: "Laya is down"})
	_, body := f.call("POST", "/v1/alerts/resolve", ratToken, map[string]string{"key": "down:laya"})
	if body["resolved"] != false {
		t.Fatalf("ratatoskr resolved munin's alert: %v", body)
	}
	_, body = f.call("POST", "/v1/alerts/resolve", muninToken, map[string]string{"key": "down:laya"})
	if body["resolved"] != true {
		t.Fatalf("munin could not resolve its own alert: %v", body)
	}
}

func TestOnlyTheOwnerAcknowledges(t *testing.T) {
	f := newFixture(t)
	_, body := f.call("POST", "/v1/alerts", muninToken, alert.Raise{Key: "k", Severity: alert.Warning, Title: "t"})
	id := int(alertOf(t, body)["id"].(float64))
	path := "/v1/alerts/" + strconv.Itoa(id) + "/ack"
	if status, _ := f.call("POST", path, muninToken, nil); status != http.StatusForbidden {
		t.Fatalf("a sender acknowledged: %d", status)
	}
	status, body := f.call("POST", path, ownerToken, nil)
	if status != http.StatusOK || alertOf(t, body)["state"] != string(Acknowledged) {
		t.Fatalf("status %d: %v", status, body)
	}
}

func TestTheOwnerDoesNotRaiseOrResolve(t *testing.T) {
	f := newFixture(t)
	if status, _ := f.call("POST", "/v1/alerts", ownerToken, alert.Raise{Key: "k", Severity: alert.Info, Title: "t"}); status != http.StatusForbidden {
		t.Fatalf("owner raised: %d", status)
	}
	if status, _ := f.call("POST", "/v1/alerts/resolve", ownerToken, map[string]string{"key": "k"}); status != http.StatusForbidden {
		t.Fatalf("owner resolved: %d", status)
	}
}

func TestGlimMayNotSendCritical(t *testing.T) {
	f := newFixture(t)
	if status, _ := f.call("POST", "/v1/alerts", glimToken, alert.Raise{Key: "k", Severity: alert.Critical, Title: "t"}); status != http.StatusForbidden {
		t.Fatalf("glim sent critical: %d", status)
	}
	if status, _ := f.call("POST", "/v1/alerts", glimToken, alert.Raise{Key: "k", Severity: alert.Warning, Title: "t"}); status != http.StatusCreated {
		t.Fatalf("glim could not send a warning: %d", status)
	}
}

func TestAResolvedAlertOpensAgainWithItsHistory(t *testing.T) {
	f := newFixture(t)
	raise := alert.Raise{Key: "down:norn", Severity: alert.Critical, Title: "Norn is down"}
	_, body := f.call("POST", "/v1/alerts", ratToken, raise)
	id := int(alertOf(t, body)["id"].(float64))
	f.call("POST", "/v1/alerts/resolve", ratToken, map[string]string{"key": "down:norn"})
	_, body = f.call("POST", "/v1/alerts", ratToken, raise)
	if body["change"] != string(Reopened) || int(alertOf(t, body)["id"].(float64)) != id {
		t.Fatalf("got %v", body)
	}
	_, detail := f.call("GET", "/v1/alerts/"+strconv.Itoa(id), ownerToken, nil)
	var kinds []string
	for _, e := range detail["events"].([]any) {
		kinds = append(kinds, e.(map[string]any)["kind"].(string))
	}
	if strings.Join(kinds, ",") != "opened,resolved,reopened" {
		t.Fatalf("history %v", kinds)
	}
}

func TestAnAcknowledgedAlertOpensAgainWhenItEscalates(t *testing.T) {
	f := newFixture(t)
	_, body := f.call("POST", "/v1/alerts", muninToken, alert.Raise{Key: "k", Severity: alert.Warning, Title: "t"})
	id := int(alertOf(t, body)["id"].(float64))
	f.call("POST", "/v1/alerts/"+strconv.Itoa(id)+"/ack", ownerToken, nil)
	_, body = f.call("POST", "/v1/alerts", muninToken, alert.Raise{Key: "k", Severity: alert.Warning, Title: "t"})
	if alertOf(t, body)["state"] != string(Acknowledged) {
		t.Fatalf("the same severity reopened it: %v", body)
	}
	_, body = f.call("POST", "/v1/alerts", muninToken, alert.Raise{Key: "k", Severity: alert.Critical, Title: "t"})
	if body["change"] != string(Escalated) || alertOf(t, body)["state"] != string(Open) {
		t.Fatalf("got %v", body)
	}
}

func TestExpiredAlertsResolve(t *testing.T) {
	f := newFixture(t)
	until := f.clock.Add(time.Minute)
	f.call("POST", "/v1/alerts", muninToken, alert.Raise{Key: "k", Severity: alert.Info, Title: "t", ExpiresAt: &until})
	f.clock = f.clock.Add(2 * time.Minute)
	if err := f.g.expire(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, list := f.call("GET", "/v1/alerts?state=resolved", ownerToken, nil)
	if n := len(list["alerts"].([]any)); n != 1 {
		t.Fatalf("%d resolved, want 1", n)
	}
}

func TestASilentHeartbeatIsRaisedAndResolved(t *testing.T) {
	f := newFixture(t)
	f.g.Heartbeats["ratatoskr"] = 3 * time.Minute
	f.call("POST", "/v1/heartbeat", ratToken, nil)
	f.clock = f.clock.Add(4 * time.Minute)
	if err := f.g.checkHeartbeats(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, list := f.call("GET", "/v1/alerts", ownerToken, nil)
	alerts := list["alerts"].([]any)
	if len(alerts) != 1 || alerts[0].(map[string]any)["key"] != "silent:ratatoskr" || alerts[0].(map[string]any)["source"] != Name {
		t.Fatalf("got %v", alerts)
	}
	f.call("POST", "/v1/heartbeat", ratToken, nil)
	if err := f.g.checkHeartbeats(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, list = f.call("GET", "/v1/alerts", ownerToken, nil)
	if n := len(list["alerts"].([]any)); n != 0 {
		t.Fatalf("%d active, want 0", n)
	}
}

func TestBadBodiesAreRefused(t *testing.T) {
	f := newFixture(t)
	for _, raise := range []alert.Raise{
		{Key: "", Severity: alert.Info, Title: "t"},
		{Key: "k", Severity: "loud", Title: "t"},
		{Key: "k", Severity: alert.Info, Title: " "},
		{Key: "k", Severity: alert.Info, Title: strings.Repeat("x", 201)},
	} {
		if status, _ := f.call("POST", "/v1/alerts", muninToken, raise); status != http.StatusBadRequest {
			t.Errorf("%+v: status %d, want 400", raise, status)
		}
	}
}

func TestEveryRouteButHealthNeedsAToken(t *testing.T) {
	f := newFixture(t)
	if status, _ := f.call("GET", "/v1/alerts", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("no token: %d", status)
	}
	if status, _ := f.call("GET", "/v1/alerts", "wrong-token-0123456789abcdef0123456789", nil); status != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", status)
	}
	if status, body := f.call("GET", "/health", "", nil); status != http.StatusOK || body["part"] != Name {
		t.Fatalf("health: %d %v", status, body)
	}
}

func TestMineIsThePartsOwnActiveAlerts(t *testing.T) {
	f := newFixture(t)
	f.call("POST", "/v1/alerts", muninToken, alert.Raise{Key: "down:laya", Severity: alert.Warning, Title: "Laya is down"})
	f.call("POST", "/v1/alerts", muninToken, alert.Raise{Key: "down:gone", Severity: alert.Warning, Title: "gone"})
	f.call("POST", "/v1/alerts/resolve", muninToken, map[string]string{"key": "down:gone"})
	f.call("POST", "/v1/alerts", ratToken, alert.Raise{Key: "down:norn", Severity: alert.Critical, Title: "Norn is down"})
	status, body := f.call("GET", "/v1/alerts/mine", muninToken, nil)
	alerts, _ := body["alerts"].([]any)
	if status != http.StatusOK || len(alerts) != 1 || alerts[0].(map[string]any)["key"] != "down:laya" {
		t.Fatalf("mine = %d %v, want only down:laya", status, body)
	}
	if _, body := f.call("GET", "/v1/alerts/mine", ownerToken, nil); len(body["alerts"].([]any)) != 0 {
		t.Fatalf("the owner has alerts of its own: %v", body)
	}
}

func TestHealedListsEveryHealingOldestFirst(t *testing.T) {
	f := newFixture(t)
	raise := alert.Raise{Key: "down:norn", Severity: alert.Warning, Title: "Norn doesn't answer"}
	f.call("POST", "/v1/alerts", ratToken, raise)
	opened := f.clock
	f.clock = f.clock.Add(4 * time.Minute)
	f.call("POST", "/v1/alerts/resolve", ratToken, map[string]string{"key": "down:norn"})
	f.clock = f.clock.Add(time.Hour)
	reopened := f.clock
	f.call("POST", "/v1/alerts", ratToken, raise)
	f.clock = f.clock.Add(2 * time.Minute)
	f.call("POST", "/v1/alerts/resolve", ratToken, map[string]string{"key": "down:norn"})
	f.call("POST", "/v1/alerts", muninToken, alert.Raise{Key: "disk", Severity: alert.Info, Title: "still open"})

	status, body := f.call("GET", "/v1/alerts/healed", ownerToken, nil)
	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, body)
	}
	healed := body["healed"].([]any)
	if len(healed) != 2 {
		t.Fatalf("%d healings, want 2: %v", len(healed), healed)
	}
	first, second := healed[0].(map[string]any), healed[1].(map[string]any)
	if first["alertId"] != second["alertId"] || first["source"] != "ratatoskr" || first["by"] != "ratatoskr" {
		t.Fatalf("got %v and %v", first, second)
	}
	if got, _ := time.Parse(time.RFC3339Nano, first["openedAt"].(string)); !got.Equal(opened) {
		t.Fatalf("first opened %v, want %v", got, opened)
	}
	if got, _ := time.Parse(time.RFC3339Nano, second["openedAt"].(string)); !got.Equal(reopened) {
		t.Fatalf("second opened %v, want %v", got, reopened)
	}
	_, page := f.call("GET", "/v1/alerts/healed?after="+strconv.Itoa(int(first["id"].(float64))), ownerToken, nil)
	if rest := page["healed"].([]any); len(rest) != 1 || rest[0].(map[string]any)["id"] != second["id"] {
		t.Fatalf("after the first: %v", rest)
	}
	if status, _ := f.call("GET", "/v1/alerts/healed?limit=0", ownerToken, nil); status != http.StatusBadRequest {
		t.Fatalf("limit 0: status %d", status)
	}
}

type recordingTeller struct{ told chan string }

func (r recordingTeller) On() bool { return true }
func (r recordingTeller) Tell(_ context.Context, m tell.Message) error {
	r.told <- m.Title
	return nil
}

func TestQuietSourcesAreNeverPushed(t *testing.T) {
	f := newFixture(t)
	teller := recordingTeller{told: make(chan string, 4)}
	f.g.Push = teller
	f.g.QuietSources["munin"] = true // as if munin were another device's mirror
	if status, _ := f.call("POST", "/v1/alerts", muninToken, alert.Raise{Key: "k", Severity: alert.Critical, Title: "mirrored"}); status != http.StatusCreated {
		t.Fatalf("raise: %d", status)
	}
	if status, _ := f.call("POST", "/v1/alerts", ratToken, alert.Raise{Key: "k", Severity: alert.Critical, Title: "local"}); status != http.StatusCreated {
		t.Fatalf("raise: %d", status)
	}
	select {
	case title := <-teller.told:
		if title != "local" {
			t.Fatalf("pushed %q, a quiet source", title)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the loud source was not pushed")
	}
	select {
	case title := <-teller.told:
		t.Fatalf("pushed %q as well", title)
	case <-time.After(100 * time.Millisecond):
	}
}
