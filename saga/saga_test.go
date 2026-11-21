package saga

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"yggdrasil/gjallar"
	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/part"
)

const (
	ownerToken = "owner-token-0123456789abcdef0123456789"
	phoneToken = "phone-token-0123456789abcdef0123456789"
	muninToken = "munin-token-0123456789abcdef0123456789"
)

// fakeMimir keeps events in memory, with Mímir's rules for seq and pos.
type fakeMimir struct {
	mu      sync.Mutex
	events  []Event
	lastSeq map[string]int64
	down    bool
}

func (m *fakeMimir) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	device := map[string]string{"Bearer desktop-mimir": "desktop"}[r.Header.Get("Authorization")]
	if device == "" {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/me":
		json.NewEncoder(w).Encode(map[string]any{"lastSeq": m.lastSeq[device]})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/events":
		var body struct {
			Events []Outgoing `json:"events"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		last := m.lastSeq[device]
		for _, e := range body.Events {
			if e.Seq <= last {
				continue
			}
			if e.Seq != last+1 {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]any{"error": "gap", "lastSeq": m.lastSeq[device]})
				return
			}
			last = e.Seq
		}
		for _, e := range body.Events {
			if e.Seq > m.lastSeq[device] {
				m.events = append(m.events, Event{Pos: int64(len(m.events) + 1), Device: device, Seq: e.Seq, At: e.At, Kind: e.Kind, Body: e.Body})
				m.lastSeq[device] = e.Seq
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"lastSeq": m.lastSeq[device]})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/events":
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		if since > int64(len(m.events)) {
			w.WriteHeader(http.StatusGone)
			json.NewEncoder(w).Encode(map[string]any{"error": "ahead", "floor": len(m.events)})
			return
		}
		out := []Event{}
		for _, e := range m.events[since:] {
			if e.Device != r.URL.Query().Get("from") {
				out = append(out, e)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"events": out, "last": len(m.events), "more": false})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/snapshot":
		http.Error(w, `{"error":"none"}`, http.StatusNotFound)
	default:
		http.NotFound(w, r)
	}
}

// fromPhone adds an event as the phone would send it.
func (m *fakeMimir) fromPhone(kind string, body any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	payload, _ := json.Marshal(body)
	m.lastSeq["phone"]++
	m.events = append(m.events, Event{Pos: int64(len(m.events) + 1), Device: "phone", Seq: m.lastSeq["phone"], Kind: kind, Body: payload})
}

// sent is every event of a kind the desktop sent.
func (m *fakeMimir) sent(kind string) []json.RawMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []json.RawMessage
	for _, e := range m.events {
		if e.Device == "desktop" && e.Kind == kind {
			out = append(out, e.Body)
		}
	}
	return out
}

type fixture struct {
	t       *testing.T
	saga    *Saga
	mimir   *fakeMimir
	gjallar *gjallar.Gjallar
	munin   *call.Part
	owner   *call.Part
	ctx     context.Context
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	gStore, err := gjallar.OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	g := gjallar.New(gStore, nil)
	g.QuietSources["phone"] = true
	gp := part.New(gjallar.Name, map[string]string{ownerToken: part.Owner, phoneToken: "phone", muninToken: "munin"}, nil)
	g.Mount(gp)
	gServer := httptest.NewServer(gp)

	m := &fakeMimir{lastSeq: map[string]int64{}}
	mServer := httptest.NewServer(m)

	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gServer.Close(); mServer.Close(); store.Close(); gStore.Close() })

	owner := call.New("gjallar", gServer.URL, ownerToken, 5*time.Second)
	s := New(store,
		&Mimir{URL: mServer.URL, Token: "desktop-mimir", HTTP: &http.Client{Timeout: 5 * time.Second}},
		&Gjallar{Owner: owner, Devices: map[string]*call.Part{"phone": call.New("gjallar", gServer.URL, phoneToken, 5*time.Second)}},
		"desktop")
	return &fixture{
		t: t, saga: s, mimir: m, gjallar: g, owner: owner, ctx: context.Background(),
		munin: call.New("gjallar", gServer.URL, muninToken, 5*time.Second),
	}
}

func (f *fixture) sync() {
	f.t.Helper()
	if err := f.saga.Sync(f.ctx); err != nil {
		f.t.Fatalf("sync: %v", err)
	}
}

func (f *fixture) alerts() []gAlert {
	f.t.Helper()
	alerts, err := f.saga.Gjallar.list(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return alerts
}

func (f *fixture) find(source, key string) gAlert {
	f.t.Helper()
	for _, a := range f.alerts() {
		if a.Source == source && a.Key == key {
			return a
		}
	}
	f.t.Fatalf("no alert %s %s in %+v", source, key, f.alerts())
	return gAlert{}
}

func (f *fixture) raise(key string, severity alert.Severity) {
	f.t.Helper()
	if err := f.munin.Do(f.ctx, "POST", "/v1/alerts", alert.Raise{Key: key, Severity: severity, Title: "Laya is down"}, nil); err != nil {
		f.t.Fatal(err)
	}
}

func states(t *testing.T, bodies []json.RawMessage) []AlertState {
	t.Helper()
	var out []AlertState
	for _, b := range bodies {
		var s AlertState
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func phoneAlert(state string) AlertState {
	return AlertState{Source: "vedrfolnir", Key: "battery-low", Severity: "warning", Title: "Battery at 12%",
		State: state, OpenedAt: 1000, LastSeenAt: 1000}
}

func TestSendsDesktopAlertsWhenTheyChangeOnly(t *testing.T) {
	f := newFixture(t)
	f.raise("down:laya", alert.Warning)
	f.sync()
	f.raise("down:laya", alert.Warning) // only seen again
	f.sync()
	sent := states(t, f.mimir.sent(kindAlert))
	if len(sent) != 1 || sent[0].Source != "munin" || sent[0].Key != "down:laya" || sent[0].State != "open" || sent[0].Severity != "warning" {
		t.Fatalf("sent: %+v", sent)
	}
	if err := f.saga.Gjallar.ack(f.ctx, f.find("munin", "down:laya").ID); err != nil {
		t.Fatal(err)
	}
	f.sync()
	sent = states(t, f.mimir.sent(kindAlert))
	if len(sent) != 2 || sent[1].State != "acknowledged" {
		t.Fatalf("after ack: %+v", sent)
	}
}

func TestMirrorsThePhonesAlerts(t *testing.T) {
	f := newFixture(t)
	f.mimir.fromPhone(kindAlert, phoneAlert("open"))
	f.sync()
	mirror := f.find("phone", "vedrfolnir/battery-low")
	if mirror.State != "open" || mirror.Severity != alert.Warning || mirror.Title != "Battery at 12%" {
		t.Fatalf("mirror: %+v", mirror)
	}
	// The mirror is never sent back as a desktop alert.
	if sent := f.mimir.sent(kindAlert); len(sent) != 0 {
		t.Fatalf("mirror sent back: %s", sent)
	}

	acked := phoneAlert("acknowledged")
	f.mimir.fromPhone(kindAlert, acked)
	f.sync()
	if got := f.find("phone", "vedrfolnir/battery-low"); got.State != "acknowledged" {
		t.Fatalf("after the phone acknowledged: %+v", got)
	}
	// Nothing asked back: the phone did it itself.
	if asked := f.mimir.sent(kindAck); len(asked) != 0 {
		t.Fatalf("asked the phone: %s", asked)
	}

	muted := phoneAlert("resolved")
	muted.Muted = true
	f.mimir.fromPhone(kindAlert, muted)
	f.sync()
	got := f.find("phone", "vedrfolnir/battery-low")
	if got.State != "resolved" || !got.Muted {
		t.Fatalf("after the phone resolved and muted: %+v", got)
	}

	again := phoneAlert("open")
	again.OpenedAt = 5000
	f.mimir.fromPhone(kindAlert, again)
	f.sync()
	if got := f.find("phone", "vedrfolnir/battery-low"); got.State != "open" {
		t.Fatalf("opened again: %+v", got)
	}
}

func TestReopenedWhileAwayOpensTheMirrorAgain(t *testing.T) {
	f := newFixture(t)
	f.mimir.fromPhone(kindAlert, phoneAlert("acknowledged"))
	f.sync()
	// While the desktop was off: resolved, then raised again. Only the newest reaches us.
	resolved := phoneAlert("resolved")
	f.mimir.fromPhone(kindAlert, resolved)
	again := phoneAlert("open")
	again.OpenedAt = 9000
	f.mimir.fromPhone(kindAlert, again)
	f.sync()
	if got := f.find("phone", "vedrfolnir/battery-low"); got.State != "open" {
		t.Fatalf("mirror: %+v", got)
	}
}

func TestYourActionsOnAMirrorAskThePhone(t *testing.T) {
	f := newFixture(t)
	f.mimir.fromPhone(kindAlert, phoneAlert("open"))
	f.sync()
	mirror := f.find("phone", "vedrfolnir/battery-low")
	if err := f.saga.Gjallar.ack(f.ctx, mirror.ID); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := f.saga.Gjallar.snooze(f.ctx, mirror.ID, until); err != nil {
		t.Fatal(err)
	}
	if err := f.saga.Gjallar.mute(f.ctx, "phone", "vedrfolnir/battery-low", true); err != nil {
		t.Fatal(err)
	}
	f.sync()
	f.sync() // asked once, not each round
	for kind, want := range map[string]string{kindAck: "", kindSnooze: fmt.Sprint(until.UnixMilli()), kindMute: "true"} {
		asked := f.mimir.sent(kind)
		if len(asked) != 1 {
			t.Fatalf("%s: asked %d times", kind, len(asked))
		}
		var r request
		json.Unmarshal(asked[0], &r)
		if r.Origin != "phone" || r.Source != "vedrfolnir" || r.Key != "battery-low" {
			t.Fatalf("%s: %+v", kind, r)
		}
		switch kind {
		case kindSnooze:
			if fmt.Sprint(r.Until) != want {
				t.Fatalf("snooze until %d, want %s", r.Until, want)
			}
		case kindMute:
			if r.Muted == nil || !*r.Muted {
				t.Fatalf("mute: %+v", r)
			}
		}
	}

	// The phone answers: the mirror matches, and nothing more is asked.
	answer := phoneAlert("acknowledged")
	snoozed := until.UnixMilli()
	answer.SnoozedUntil, answer.Muted = &snoozed, true
	f.mimir.fromPhone(kindAlert, answer)
	f.sync()
	if n := len(f.mimir.sent(kindAck)) + len(f.mimir.sent(kindSnooze)) + len(f.mimir.sent(kindMute)); n != 3 {
		t.Fatalf("asked again after the phone answered: %d requests", n)
	}
}

func TestDoesWhatThePhoneAsksOfDesktopAlerts(t *testing.T) {
	f := newFixture(t)
	f.raise("down:laya", alert.Critical)
	f.sync()
	f.mimir.fromPhone(kindAck, request{Origin: "desktop", Source: "munin", Key: "down:laya"})
	until := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	f.mimir.fromPhone(kindSnooze, request{Origin: "desktop", Source: "munin", Key: "down:laya", Until: until.UnixMilli()})
	muted := true
	f.mimir.fromPhone(kindMute, request{Origin: "desktop", Source: "munin", Key: "down:laya", Muted: &muted})
	// Asked of another device: not ours to do.
	f.mimir.fromPhone(kindMute, request{Origin: "tablet", Source: "munin", Key: "other", Muted: &muted})
	f.sync()

	got := f.find("munin", "down:laya")
	if got.State != "acknowledged" || got.SnoozedUntil == nil || !got.SnoozedUntil.Equal(until) || !got.Muted {
		t.Fatalf("desktop alert: %+v", got)
	}
	// And the new state goes back, in the same round.
	sent := states(t, f.mimir.sent(kindAlert))
	last := sent[len(sent)-1]
	if last.State != "acknowledged" || !last.Muted || last.SnoozedUntil == nil {
		t.Fatalf("sent back: %+v", last)
	}
	mutes := struct {
		Mutes []struct{ Source, Key string } `json:"mutes"`
	}{}
	if err := f.owner.Do(f.ctx, "GET", "/v1/mutes", nil, &mutes); err != nil {
		t.Fatal(err)
	}
	if len(mutes.Mutes) != 1 {
		t.Fatalf("mutes: %+v", mutes)
	}
}

func TestKeepsEventsWhileMimirIsDown(t *testing.T) {
	f := newFixture(t)
	f.mimir.down = true
	f.raise("down:laya", alert.Warning)
	if err := f.saga.Sync(f.ctx); err == nil {
		t.Fatal("no error with Mímir down")
	}
	// The first round could not learn our count, so it waited; Gjallar changed meanwhile.
	f.mimir.down = false
	f.sync()
	f.mimir.down = true
	if err := f.saga.Gjallar.ack(f.ctx, f.find("munin", "down:laya").ID); err != nil {
		t.Fatal(err)
	}
	if err := f.saga.Sync(f.ctx); err == nil {
		t.Fatal("no error with Mímir down")
	}
	if n, _ := f.saga.Store.OutboxLen(f.ctx); n != 1 {
		t.Fatalf("waiting: %d", n)
	}
	f.mimir.down = false
	f.sync()
	if n, _ := f.saga.Store.OutboxLen(f.ctx); n != 0 {
		t.Fatalf("still waiting: %d", n)
	}
	if sent := states(t, f.mimir.sent(kindAlert)); len(sent) != 2 || sent[1].State != "acknowledged" {
		t.Fatalf("sent: %+v", sent)
	}
}

func TestFollowsOnWhenMimirLostOurEvents(t *testing.T) {
	f := newFixture(t)
	f.raise("down:laya", alert.Warning)
	f.sync()
	// Mímir restored from an old backup: it no longer has our first event.
	f.mimir.mu.Lock()
	f.mimir.events, f.mimir.lastSeq["desktop"] = nil, 0
	f.mimir.mu.Unlock()
	f.raise("down:ollama", alert.Warning)
	f.sync()
	sent := states(t, f.mimir.sent(kindAlert))
	if len(sent) != 1 || sent[0].Key != "down:ollama" {
		t.Fatalf("sent: %+v", sent)
	}
	if n, _ := f.saga.Store.OutboxLen(f.ctx); n != 0 {
		t.Fatalf("still waiting: %d", n)
	}
}

func TestStartsAfterTheSeqMimirHolds(t *testing.T) {
	f := newFixture(t)
	f.mimir.lastSeq["desktop"] = 41 // a database lost here, not there
	f.raise("down:laya", alert.Warning)
	f.sync()
	f.mimir.mu.Lock()
	defer f.mimir.mu.Unlock()
	if len(f.mimir.events) != 1 || f.mimir.events[0].Seq != 42 {
		t.Fatalf("events: %+v", f.mimir.events)
	}
}

func TestMeetsItsContract(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p := part.New(Name, contracttest.Tokens(), nil)
	New(store, &Mimir{HTTP: http.DefaultClient}, &Gjallar{Owner: call.New("gjallar", "", "", time.Second)}, "desktop").Mount(p)
	contracttest.Run(t, p)
}
