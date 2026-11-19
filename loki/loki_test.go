package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	mathrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/part"
)

const lokiToken = "loki-token-0123456789abcdef0123456789ab"

// world is a fake Yggdrasil: a real Norn-like part with the trick switch,
// and Ratatoskr, Gjallar and Hermóðr that see it as the real ones would
// (or not at all, when blind).
type world struct {
	t       *testing.T
	norn    *httptest.Server
	rata    *httptest.Server
	gjallar *httptest.Server
	hermodr *httptest.Server
	blind   bool // Ratatoskr and Gjallar never notice

	mu        sync.Mutex
	raised    []alert.Raise
	resolved  []string
	messages  []map[string]any
	inMeeting bool
	focus     bool
	since     time.Time // when Norn was first seen down
}

func newWorld(t *testing.T) *world {
	t.Helper()
	t.Setenv("LOKI", "on")
	w := &world{t: t}
	norn := part.New("norn", map[string]string{lokiToken: part.TrickCaller}, nil)
	norn.Loop("inbox", time.Hour, func(context.Context) error { return nil })
	w.norn = httptest.NewServer(norn)
	t.Cleanup(w.norn.Close)

	nornDown := func() bool {
		response, err := http.Get(w.norn.URL + "/health")
		if err != nil {
			return true
		}
		response.Body.Close()
		return response.StatusCode != http.StatusOK
	}
	w.rata = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		down := !w.blind && nornDown()
		part.JSON(rw, http.StatusOK, map[string]any{
			"parts":  []map[string]any{{"name": "norn"}},
			"checks": []map[string]any{{"key": "down:norn", "down": down}},
		})
	}))
	t.Cleanup(w.rata.Close)
	w.gjallar = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/alerts":
			alerts := []map[string]any{}
			w.mu.Lock()
			if !w.blind && nornDown() && w.since.IsZero() {
				w.since = time.Now()
			}
			if !w.since.IsZero() {
				a := map[string]any{"id": 7, "source": "ratatoskr", "key": "down:norn", "severity": "critical", "title": "Norn is down",
					"state": "open", "firstSeenAt": w.since, "lastSeenAt": time.Now()}
				if !nornDown() {
					a["state"], a["resolvedAt"], a["lastSeenAt"] = "resolved", time.Now(), w.since
				}
				alerts = append(alerts, a)
			}
			w.mu.Unlock()
			part.JSON(rw, http.StatusOK, map[string]any{"alerts": alerts})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/alerts":
			var raise alert.Raise
			_ = json.NewDecoder(r.Body).Decode(&raise)
			w.mu.Lock()
			w.raised = append(w.raised, raise)
			w.mu.Unlock()
			part.JSON(rw, http.StatusCreated, map[string]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/alerts/resolve":
			var body struct{ Key string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.mu.Lock()
			w.resolved = append(w.resolved, body.Key)
			w.mu.Unlock()
			part.JSON(rw, http.StatusOK, map[string]any{"resolved": true})
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(w.gjallar.Close)
	w.hermodr = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		switch r.URL.Path {
		case "/v1/channels":
			part.JSON(rw, http.StatusOK, map[string]any{"channels": []map[string]any{{"name": "ntfy", "on": true}}, "inMeeting": w.inMeeting, "inBreak": false})
		case "/v1/focus":
			part.JSON(rw, http.StatusOK, map[string]any{"on": w.focus})
		case "/v1/messages":
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			w.messages = append(w.messages, m)
			part.JSON(rw, http.StatusCreated, map[string]any{})
		}
	}))
	t.Cleanup(w.hermodr.Close)
	return w
}

func (w *world) loki() *Loki {
	urls := map[string]string{"norn": w.norn.URL, "ratatoskr": w.rata.URL, "gjallar": w.gjallar.URL, "hermodr": w.hermodr.URL}
	clients := map[string]*call.Part{}
	for name, url := range urls {
		clients[name] = call.New(name, url, lokiToken, 2*time.Second)
	}
	l := New([]string{"norn", "gjallar", "ratatoskr", "hermodr"}, urls, clients, alert.NewClient(w.gjallar.URL, lokiToken))
	l.Minute, l.Poll, l.HealWait = 150*time.Millisecond, 20*time.Millisecond, 2*time.Second
	l.Hours.Zone = time.UTC
	return l
}

func TestAStrikeThatEveryWatcherNoticesIsCaught(t *testing.T) {
	w := newWorld(t)
	l := w.loki()
	strike, err := l.Start(context.Background(), "norn", Trick{Kind: "health"}, 1, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if strike.State != Striking || len(strike.Checks) != 2 {
		t.Fatalf("started: %+v", strike)
	}
	l.Wait()
	done, _ := l.Strike(strike.ID)
	if done.State != Caught {
		t.Fatalf("state %s, checks %+v", done.State, done.Checks)
	}
	for _, c := range done.Checks {
		if c.NoticedAt == nil || c.OK == nil || !*c.OK {
			t.Errorf("check %+v not noticed", c)
		}
	}
	if done.HealedAt == nil {
		t.Error("never healed")
	}
	if done.Tale == "" || !strings.Contains(done.Tale, "Norn") {
		t.Errorf("tale: %q", done.Tale)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.raised) != 0 || len(w.resolved) != 1 || w.resolved[0] != "missed:norn:health" {
		t.Errorf("raised %v, resolved %v", w.raised, w.resolved)
	}
}

func TestAStrikeNobodyNoticesIsMissedAndRaisedOnGjallar(t *testing.T) {
	w := newWorld(t)
	w.blind = true
	l := w.loki()
	l.HealWait = 200 * time.Millisecond
	strike, err := l.Start(context.Background(), "norn", Trick{Kind: "loop", Loop: "inbox"}, 1, "passive")
	if err != nil {
		t.Fatal(err)
	}
	l.Wait()
	done, _ := l.Strike(strike.ID)
	if done.State != Missed {
		t.Fatalf("state %s", done.State)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.raised) != 1 || w.raised[0].Key != "missed:norn:loop" || w.raised[0].Severity != alert.Warning {
		t.Fatalf("raised %+v", w.raised)
	}
	if !strings.Contains(w.raised[0].Detail, "ratatoskr:failing:norn:inbox did not notice") {
		t.Errorf("detail: %s", w.raised[0].Detail)
	}
	if len(w.messages) != 1 || w.messages[0]["push"] != false || w.messages[0]["text"] != done.Tale {
		t.Errorf("passive message: %+v", w.messages)
	}
}

func TestAStoppedStrikeTakesTheTrickOffAndHasNoVerdict(t *testing.T) {
	w := newWorld(t)
	l := w.loki()
	l.Minute = time.Minute
	strike, err := l.Start(context.Background(), "norn", Trick{Kind: "health"}, 5, "manual")
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := l.Stop(context.Background(), strike.ID)
	if err != nil || stopped.State != Stopped {
		t.Fatalf("stop: %v %+v", err, stopped)
	}
	response, _ := http.Get(w.norn.URL + "/health")
	if response.StatusCode != http.StatusOK {
		t.Errorf("Norn still tricked after stop: %d", response.StatusCode)
	}
	l.Wait()
	done, _ := l.Strike(strike.ID)
	if done.State != Stopped {
		t.Errorf("after the run: %s", done.State)
	}
	if _, err := l.Stop(context.Background(), strike.ID); err == nil {
		t.Error("stopped twice")
	}
}

func TestStrikeRules(t *testing.T) {
	w := newWorld(t)
	l := w.loki()
	l.Minute = time.Minute
	ctx := context.Background()
	status := func(err error) int {
		if refused, ok := err.(*Refused); ok {
			return refused.Status
		}
		return 0
	}
	if _, err := l.Start(ctx, "loki", Trick{Kind: "health"}, 1, "manual"); status(err) != http.StatusForbidden {
		t.Errorf("Loki itself: %v", err)
	}
	if _, err := l.Start(ctx, "ratatoskr", Trick{Kind: "health"}, 1, "manual"); status(err) != http.StatusForbidden {
		t.Errorf("Ratatoskr without allowWatchers: %v", err)
	}
	if _, err := l.Start(ctx, "norn", Trick{Kind: "health"}, 11, "manual"); status(err) != http.StatusBadRequest {
		t.Errorf("11 minutes: %v", err)
	}
	if _, err := l.Start(ctx, "norn", Trick{Kind: "dance"}, 1, "manual"); status(err) != http.StatusBadRequest {
		t.Errorf("unknown kind: %v", err)
	}
	if _, err := l.Start(ctx, "norn", Trick{Kind: "loop", Loop: "nope"}, 1, "manual"); status(err) != http.StatusBadRequest {
		t.Errorf("unknown loop: %v", err)
	}
	first, err := l.Start(ctx, "norn", Trick{Kind: "health"}, 2, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if first.EndsAt.Sub(first.StartedAt) != 2*time.Minute {
		t.Errorf("length: %s", first.EndsAt.Sub(first.StartedAt))
	}
	if _, err := l.Start(ctx, "norn", Trick{Kind: "slow"}, 1, "manual"); status(err) != http.StatusConflict {
		t.Errorf("a second strike on Norn: %v", err)
	}
	if _, err := l.Start(ctx, "gjallar", Trick{Kind: "health"}, 1, "manual"); status(err) != http.StatusConflict {
		t.Errorf("Gjallar while Norn is struck: %v", err)
	}
	_, _ = l.Stop(ctx, first.ID)
	l.Wait()
}

func TestATrickSwitchThatIsOffSaysSo(t *testing.T) {
	w := newWorld(t)
	t.Setenv("LOKI", "")
	off := httptest.NewServer(part.New("huginn", map[string]string{lokiToken: part.TrickCaller}, nil))
	defer off.Close()
	l := w.loki()
	l.Parts = append(l.Parts, "huginn")
	l.URLs["huginn"] = off.URL
	l.Clients["huginn"] = call.New("huginn", off.URL, lokiToken, time.Second)
	_, err := l.Start(context.Background(), "huginn", Trick{Kind: "health"}, 1, "manual")
	if refused, ok := err.(*Refused); !ok || refused.Status != http.StatusConflict || !strings.Contains(refused.Message, "LOKI=on") {
		t.Fatalf("got %v", err)
	}
	if len(l.Strikes(10)) != 0 {
		t.Error("a strike that never started is listed")
	}
	targets := l.Targets(context.Background())
	for _, target := range targets {
		switch target.Name {
		case "huginn":
			if !target.Up || target.LokiOn {
				t.Errorf("huginn: %+v", target)
			}
		case "norn":
			if !target.Up || !target.LokiOn || len(target.Tricks) != 3 {
				t.Errorf("norn: %+v", target)
			}
			if got := strings.Join(target.Tricks[1].Expect, ","); target.Tricks[1].Loop != "inbox" || got != "ratatoskr:failing:norn:inbox,gjallar:alert" {
				t.Errorf("norn's loop trick: %+v", target.Tricks[1])
			}
		case "ratatoskr":
			if len(target.Tricks) != 0 {
				t.Errorf("Ratatoskr's tricks listed without allowWatchers: %+v", target.Tricks)
			}
		}
	}
}

func TestExpectations(t *testing.T) {
	for _, c := range []struct {
		part  string
		trick Trick
		want  string
	}{
		{"norn", Trick{Kind: "health"}, "ratatoskr:down:norn,gjallar:alert"},
		{"gjallar", Trick{Kind: "health"}, "ratatoskr:down:gjallar"},
		{"sindri", Trick{Kind: "loop", Loop: "feed"}, "ratatoskr:failing:sindri:feed,gjallar:alert"},
		{"ratatoskr", Trick{Kind: "loop", Loop: "heartbeat"}, "gjallar:silent:ratatoskr"},
		{"norn", Trick{Kind: "slow", DelayMs: 3000}, "ratatoskr:slow:norn"},
	} {
		if got := strings.Join(ExpectStrings(c.part, c.trick), ","); got != c.want {
			t.Errorf("%s %+v: %s, want %s", c.part, c.trick, got, c.want)
		}
	}
}

func health(loops ...string) Probe {
	h := &part.Health{OK: true, Part: "x", Loops: map[string]part.LoopHealth{}}
	for _, loop := range loops {
		h.Loops[loop] = part.LoopHealth{EverySeconds: 60}
	}
	return Probe{Up: true, Health: h}
}

func TestBlindSpotsComeFromWhatIsThere(t *testing.T) {
	seen := Seen{
		Parts:      []string{"norn", "huginn", "ratatoskr", "munin"},
		Probes:     map[string]Probe{"norn": health("inbox", "calendar"), "huginn": health("rules"), "ratatoskr": health("check", "heartbeat"), "munin": {}},
		Watched:    []string{"norn", "munin"},
		Heartbeats: nil,
		Alerts:     []Alert{{Source: "ratatoskr", Key: "failing:norn:calendar"}},
		Channels:   &Channels{Channels: []Channel{{Name: "ntfy", On: false}}},
		Strikes:    []Strike{{Part: "norn", Trick: Trick{Kind: "health"}, State: Caught}},
	}
	spots := BlindSpots(seen)
	byID := map[string]BlindSpot{}
	for _, s := range spots {
		byID[s.ID] = s
	}
	want := map[string]string{
		"unwatched:huginn":    "high",
		"heartbeat:ratatoskr": "high",
		"heartbeat:huginn":    "medium",
		"phone:hermodr":       "high",
		"loop:norn:inbox":     "low",
		"slow":                "medium",
	}
	for id, severity := range want {
		spot, ok := byID[id]
		if !ok {
			t.Errorf("no %s in %+v", id, spots)
			continue
		}
		if spot.Severity != severity || spot.Text == "" {
			t.Errorf("%s: %+v", id, spot)
		}
	}
	for _, id := range []string{"unwatched:munin", "unwatched:norn", "loop:norn:calendar", "unproven:norn", "unwatched:ratatoskr"} {
		if _, ok := byID[id]; ok {
			t.Errorf("%s should not be a blind spot", id)
		}
	}
	if trick := byID["heartbeat:ratatoskr"].Trick; trick == nil || trick.Kind != "loop" || trick.Loop != "heartbeat" {
		t.Errorf("Ratatoskr's heartbeat trick: %+v", trick)
	}
	if trick := byID["unwatched:huginn"].Trick; trick == nil || trick.Kind != "health" {
		t.Errorf("huginn's trick: %+v", trick)
	}
	if !strings.Contains(byID["phone:hermodr"].Text, "nobody would know") {
		t.Errorf("phone text: %s", byID["phone:hermodr"].Text)
	}
	if spots[0].Severity != "high" || spots[len(spots)-1].Severity != "low" {
		t.Errorf("not worst first: %+v", spots)
	}

	seen.Channels = &Channels{Channels: []Channel{{Name: "ntfy", On: true}}}
	seen.Heartbeats = []string{"ratatoskr"}
	for _, s := range BlindSpots(seen) {
		if s.ID == "heartbeat:ratatoskr" {
			t.Error("heartbeat set, still a blind spot")
		}
		if s.ID == "phone:hermodr" && (s.Severity != "low" || s.Text != "If Hermóðr's ntfy channel failed at night, nobody would know: the alert about it is a warning, and warnings stay on the widget.") {
			t.Errorf("ntfy on: %+v", s)
		}
	}
}

func TestATaleTellsWhenItWasNoticedAndHealed(t *testing.T) {
	opened := time.Date(2026, 11, 17, 14, 5, 0, 0, time.UTC)
	resolved := opened.Add(12 * time.Minute)
	a := Alert{ID: 7, Source: "ratatoskr", Key: "down:norn", Title: "Norn is down", FirstSeenAt: opened.Add(-time.Hour), ResolvedAt: &resolved}
	events := []Event{
		{At: opened.Add(-time.Hour), Kind: "opened"},
		{At: opened.Add(-50 * time.Minute), Kind: "resolved"},
		{At: opened, Kind: "reopened"},
		{At: opened.Add(4 * time.Minute), Kind: "acknowledged"},
		{At: resolved, Kind: "resolved"},
	}
	gotOpened, noticed, gotResolved := Timeline(a, events)
	if !gotOpened.Equal(opened) || noticed == nil || !noticed.Equal(opened.Add(4*time.Minute)) || !gotResolved.Equal(resolved) {
		t.Fatalf("timeline: %s %v %s", gotOpened, noticed, gotResolved)
	}
	tale := AlertTale(a, gotOpened, noticed, gotResolved, time.UTC)
	if tale.Part != "norn" || tale.AlertID != 7 || *tale.NoticedAfterMs != 4*60*1000 || *tale.HealedAfterMs != 12*60*1000 {
		t.Errorf("tale: %+v", tale)
	}
	for _, want := range []string{"Norn", "Tuesday", "14:05", "4 minutes", "12 minutes"} {
		if !strings.Contains(tale.Text, want) {
			t.Errorf("%q lacks %q", tale.Text, want)
		}
	}
	if again := AlertTale(a, gotOpened, noticed, gotResolved, time.UTC); again.Text != tale.Text {
		t.Error("the same alert tells a different tale")
	}
	quiet := AlertTale(a, gotOpened, nil, gotResolved, time.UTC)
	if quiet.NoticedAfterMs != nil || strings.Contains(quiet.Text, "4 minutes") {
		t.Errorf("not acknowledged: %+v", quiet)
	}
	texts := map[string]bool{}
	for id := range int64(40) {
		a.ID = id
		texts[AlertTale(a, gotOpened, noticed, gotResolved, time.UTC).Text] = true
	}
	if len(texts) < 3 {
		t.Errorf("only %d different tales", len(texts))
	}
}

func TestStrikeTales(t *testing.T) {
	start := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	noticed := start.Add(70 * time.Second)
	healed := start.Add(4 * time.Minute)
	strike := Strike{ID: "a", Part: "norn", Trick: Trick{Kind: "health"}, StartedAt: start, EndsAt: start.Add(3 * time.Minute), HealedAt: &healed, State: Caught,
		Checks: []Check{{Watcher: "ratatoskr", NoticedAt: &noticed}, {Watcher: "gjallar", NoticedAt: &noticed}}}
	text := StrikeTale(strike, time.UTC)
	if !strings.Contains(text, "Ratatoskr noticed after 1 minute") || !strings.Contains(text, "Gjallar") {
		t.Errorf("caught: %q", text)
	}
	strike.State, strike.Checks[1].NoticedAt = Missed, nil
	if text := StrikeTale(strike, time.UTC); !strings.Contains(text, "Gjallar") || !strings.Contains(text, "Norn stops answering /health") {
		t.Errorf("missed: %q", text)
	}
}

func TestAlertsCausedByAStrikeTellNoTale(t *testing.T) {
	start := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	strikes := []Strike{{Part: "norn", StartedAt: start, EndsAt: start.Add(3 * time.Minute)}}
	resolved := start.Add(4 * time.Minute)
	if !causedByStrike(Alert{Source: "ratatoskr", Key: "down:norn", FirstSeenAt: start.Add(time.Minute), ResolvedAt: &resolved}, strikes, 5*time.Minute) {
		t.Error("a strike's own alert tells a tale")
	}
	if causedByStrike(Alert{Source: "ratatoskr", Key: "down:sindri", FirstSeenAt: start.Add(time.Minute), ResolvedAt: &resolved}, strikes, 5*time.Minute) {
		t.Error("another part's alert was dropped")
	}
}

func TestWords(t *testing.T) {
	for d, want := range map[time.Duration]string{
		40 * time.Second: "40 seconds", time.Minute: "1 minute", 3 * time.Minute: "3 minutes",
		65 * time.Minute: "1 hour 5 minutes", 2 * time.Hour: "2 hours", 50 * time.Hour: "2 days",
	} {
		if got := words(d); got != want {
			t.Errorf("%s: %q, want %q", d, got, want)
		}
	}
}

func TestPassiveStrikesKeepToWorkingHoursAndYourDay(t *testing.T) {
	hours := Hours{From: 9 * 60, To: 17 * 60, Zone: time.UTC}
	wednesday := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		at   time.Time
		busy Busy
		ok   bool
	}{
		{wednesday.Add(10 * time.Hour), Busy{}, true},
		{wednesday.Add(8 * time.Hour), Busy{}, false},
		{wednesday.Add(17 * time.Hour), Busy{}, false},
		{wednesday.Add(10 * time.Hour), Busy{InMeeting: true}, false},
		{wednesday.Add(10 * time.Hour), Busy{InBreak: true}, false},
		{wednesday.Add(10 * time.Hour), Busy{Focus: true}, false},
		{wednesday.Add(3*24*time.Hour + 10*time.Hour), Busy{}, false}, // Saturday
	} {
		if ok, why := Allowed(c.at, hours, c.busy); ok != c.ok {
			t.Errorf("%s %+v: %v (%s)", c.at, c.busy, ok, why)
		}
	}
	night := Hours{From: 0, To: 24*60 - 1, Zone: time.UTC}
	if ok, _ := Allowed(wednesday.Add(3*time.Hour), night, Busy{}); ok {
		t.Error("struck at night because the working day said so")
	}

	random := mathrand.New(mathrand.NewPCG(1, 2))
	slots := PlanDay(wednesday.Add(6*time.Hour), hours, 3, random)
	if len(slots) != 3 {
		t.Fatalf("slots: %v", slots)
	}
	for _, slot := range slots {
		if slot.Before(wednesday.Add(9*time.Hour)) || !slot.Before(wednesday.Add(17*time.Hour-20*time.Minute)) {
			t.Errorf("slot outside the day: %s", slot)
		}
	}
	if slots := PlanDay(wednesday.Add(3*24*time.Hour), hours, 3, random); len(slots) != 0 {
		t.Errorf("weekend slots: %v", slots)
	}
}

func TestPassiveTickStrikesOncePerDayAndWaitsForYou(t *testing.T) {
	w := newWorld(t)
	l := w.loki()
	l.HealWait = 200 * time.Millisecond
	l.Hours = Hours{From: 9 * 60, To: 17 * 60, Zone: time.UTC}
	clock := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return clock }
	ctx := context.Background()
	// The parts' tricks run on the real clock, so the strike itself is recorded, not played.
	l.start = func(_ context.Context, name string, trick Trick, minutes int, by string) (Strike, error) {
		s := &Strike{ID: newID(), Part: name, Trick: trick, By: by, StartedAt: clock, EndsAt: clock.Add(time.Duration(minutes) * time.Minute), State: Caught}
		l.mu.Lock()
		l.strikes = append([]*Strike{s}, l.strikes...)
		l.mu.Unlock()
		return s.copy(), nil
	}

	if err := l.PassiveTick(ctx); err != nil || len(l.Strikes(10)) != 0 {
		t.Fatalf("struck with passive off: %v", err)
	}
	l.settings.Passive, l.settings.PerDay = true, 1
	l.mu.Lock()
	l.plannedDay, l.slots = "2026-11-18", []time.Time{clock.Add(-time.Minute)}
	l.mu.Unlock()

	w.mu.Lock()
	w.inMeeting = true
	w.mu.Unlock()
	if err := l.PassiveTick(ctx); err != nil || len(l.Strikes(10)) != 0 {
		t.Fatalf("struck in a meeting: %v", err)
	}
	l.mu.Lock()
	postponed := l.slots[0]
	l.mu.Unlock()
	if !postponed.Equal(clock.Add(Postpone)) {
		t.Errorf("postponed to %s", postponed)
	}

	w.mu.Lock()
	w.inMeeting, w.focus = false, true
	w.mu.Unlock()
	clock = clock.Add(Postpone)
	if err := l.PassiveTick(ctx); err != nil || len(l.Strikes(10)) != 0 {
		t.Fatalf("struck in focus: %v", err)
	}

	w.mu.Lock()
	w.focus = false
	w.mu.Unlock()
	clock = clock.Add(Postpone)
	if err := l.PassiveTick(ctx); err != nil {
		t.Fatal(err)
	}
	strikes := l.Strikes(10)
	if len(strikes) != 1 || strikes[0].By != "passive" || strikes[0].Part == "gjallar" || strikes[0].Part == "ratatoskr" {
		t.Fatalf("strikes: %+v", strikes)
	}

	l.mu.Lock()
	l.slots = []time.Time{clock}
	l.mu.Unlock()
	if err := l.PassiveTick(ctx); err != nil || len(l.Strikes(10)) != 1 {
		t.Fatalf("a second strike on a perDay=1 day: %v", err)
	}
}

func TestOwnerOnlyRoutesAndSettingsThatLast(t *testing.T) {
	w := newWorld(t)
	l := w.loki()
	l.StatePath = filepath.Join(t.TempDir(), "loki.json")
	p := part.New(Name, contracttest.Tokens(), nil)
	l.Mount(context.Background(), p)
	send := func(token, method, path string, body any) (int, map[string]any) {
		var reader io.Reader
		if body != nil {
			data, _ := json.Marshal(body)
			reader = bytes.NewReader(data)
		}
		request := httptest.NewRequest(method, path, reader)
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		p.ServeHTTP(recorder, request)
		var answer map[string]any
		_ = json.Unmarshal(recorder.Body.Bytes(), &answer)
		return recorder.Code, answer
	}
	if code, _ := send(contracttest.Sender, "POST", "/v1/strikes", map[string]any{"part": "norn", "trick": map[string]any{"kind": "health"}}); code != http.StatusForbidden {
		t.Errorf("a part strikes: %d", code)
	}
	if code, _ := send(contracttest.Sender, "PATCH", "/v1/settings", map[string]any{"passive": true}); code != http.StatusForbidden {
		t.Errorf("a part changes settings: %d", code)
	}
	if code, _ := send(contracttest.Owner, "PATCH", "/v1/settings", map[string]any{"perDay": 9}); code != http.StatusBadRequest {
		t.Errorf("perDay 9: %d", code)
	}
	code, answer := send(contracttest.Owner, "PATCH", "/v1/settings", map[string]any{"passive": true, "perDay": 2})
	if code != http.StatusOK || answer["passive"] != true || answer["perDay"] != float64(2) || answer["allowWatchers"] != false {
		t.Fatalf("patch: %d %v", code, answer)
	}
	l.Minute = time.Minute
	code, answer = send(contracttest.Owner, "POST", "/v1/strikes", map[string]any{"part": "norn", "trick": map[string]any{"kind": "health"}})
	if code != http.StatusCreated || answer["state"] != Striking || answer["by"] != "manual" {
		t.Fatalf("owner strikes: %d %v", code, answer)
	}
	id := answer["id"].(string)
	if code, _ := send(contracttest.Sender, "POST", "/v1/strikes/"+id+"/stop", nil); code != http.StatusForbidden {
		t.Errorf("a part stops a strike: %d", code)
	}
	if code, answer := send(contracttest.Owner, "POST", "/v1/strikes/"+id+"/stop", nil); code != http.StatusOK || answer["state"] != Stopped {
		t.Errorf("owner stops: %d %v", code, answer)
	}
	l.Wait()

	again := w.loki()
	again.StatePath = l.StatePath
	if err := again.Load(); err != nil {
		t.Fatal(err)
	}
	if s := again.Settings(); !s.Passive || s.PerDay != 2 {
		t.Errorf("settings after a restart: %+v", s)
	}
	if strikes := again.Strikes(10); len(strikes) != 1 || strikes[0].State != Stopped {
		t.Errorf("strikes after a restart: %+v", strikes)
	}
}

func TestMeetsItsContract(t *testing.T) {
	w := newWorld(t)
	l := w.loki()
	p := part.New(Name, contracttest.Tokens(), nil)
	l.Mount(context.Background(), p)
	contracttest.Run(t, p)
}
