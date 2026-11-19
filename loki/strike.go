package loki

// A strike: one part tricked for a few minutes, Ratatoskr's status and
// Gjallar's alerts read every Poll for what should notice, then, after the
// trick ends, a wait of up to HealWait for the part to heal and the alerts to
// resolve. Caught when every watcher noticed, missed when one did not; a
// missed strike is raised on Gjallar as missed:<part>:<kind>.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/part"
	"yggdrasil/internal/tell"
)

// Watchers that may be expected to notice.
const (
	Ratatoskr = "ratatoskr"
	Gjallar   = "gjallar"
)

// Expect is who should notice a trick on a part, and by what.
func Expect(name string, trick Trick) []Check {
	ratatoskr := func(key string) Check {
		return Check{Watcher: Ratatoskr, Expected: "ratatoskr:" + key, Key: key}
	}
	gjallar := func(expected, source, key string) Check {
		return Check{Watcher: Gjallar, Expected: expected, Source: source, Key: key}
	}
	switch trick.Kind {
	case part.TrickHealth:
		if name == Ratatoskr {
			// Nobody reads Ratatoskr's /health; its heartbeat keeps going.
			return []Check{gjallar("gjallar:silent:ratatoskr", Gjallar, "silent:ratatoskr")}
		}
		checks := []Check{ratatoskr("down:" + name)}
		if name != Gjallar {
			// Gjallar's own downtime is told around it, never raised on it.
			checks = append(checks, gjallar("gjallar:alert", Ratatoskr, "down:"+name))
		}
		return checks
	case part.TrickLoop:
		if name == Ratatoskr && trick.Loop == "heartbeat" {
			return []Check{gjallar("gjallar:silent:ratatoskr", Gjallar, "silent:ratatoskr")}
		}
		key := "failing:" + name + ":" + trick.Loop
		return []Check{ratatoskr(key), gjallar("gjallar:alert", Ratatoskr, key)}
	case part.TrickSlow:
		// Nothing watches how fast a part answers yet: Ratatoskr reads /health only.
		return []Check{ratatoskr("slow:" + name)}
	}
	return nil
}

// ExpectStrings is what each check expects, as /v1/targets lists it.
func ExpectStrings(name string, trick Trick) []string {
	list := []string{}
	for _, c := range Expect(name, trick) {
		list = append(list, c.Expected)
	}
	return list
}

// Label is a trick in plain words.
func Label(name string, trick Trick) string {
	switch trick.Kind {
	case part.TrickHealth:
		return fmt.Sprintf("%s stops answering /health", Title(name))
	case part.TrickLoop:
		return fmt.Sprintf("%s's %s loop fails every run", Title(name), trick.Loop)
	case part.TrickSlow:
		return fmt.Sprintf("%s answers every call %s late", Title(name), words(time.Duration(trick.DelayMs)*time.Millisecond))
	}
	return trick.Kind
}

// Start tricks a part for minutes (DefaultMinutes when 0), by manual or passive.
func (l *Loki) Start(ctx context.Context, name string, trick Trick, minutes int, by string) (Strike, error) {
	if minutes == 0 {
		minutes = DefaultMinutes
	}
	if minutes < 1 || minutes > MaxMinutes {
		return Strike{}, refuse(http.StatusBadRequest, "minutes is 1 to %d", MaxMinutes)
	}
	if name == Name {
		return Strike{}, refuse(http.StatusForbidden, "Loki never tricks itself")
	}
	if !slices.Contains(l.Parts, name) {
		return Strike{}, refuse(http.StatusBadRequest, "%q is not a part Loki knows: %s", name, strings.Join(l.Parts, ", "))
	}
	switch trick.Kind {
	case part.TrickHealth:
		trick.Loop, trick.DelayMs = "", 0
	case part.TrickLoop:
		trick.DelayMs = 0
		if trick.Loop == "" {
			return Strike{}, refuse(http.StatusBadRequest, "a loop trick names the loop")
		}
	case part.TrickSlow:
		trick.Loop = ""
		if trick.DelayMs == 0 {
			trick.DelayMs = DefaultDelayMs
		}
		if trick.DelayMs < 1 || time.Duration(trick.DelayMs)*time.Millisecond > part.MaxTrickDelay {
			return Strike{}, refuse(http.StatusBadRequest, "delayMs is 1 to 10000")
		}
	default:
		return Strike{}, refuse(http.StatusBadRequest, "trick.kind is health, loop or slow")
	}
	client := l.Clients[name]
	if !client.On() {
		return Strike{}, refuse(http.StatusConflict, "Loki has no token at %s: %s_TOKEN in loki.env", Title(name), strings.ToUpper(name))
	}

	now := l.now()
	strike := &Strike{
		ID: newID(), Part: name, Trick: trick, By: by, StartedAt: now,
		EndsAt: now.Add(time.Duration(minutes) * l.Minute), State: Striking,
		Checks: Expect(name, trick), stop: make(chan struct{}),
	}
	l.mu.Lock()
	if name == Ratatoskr && !l.settings.AllowWatchers {
		l.mu.Unlock()
		return Strike{}, refuse(http.StatusForbidden, "Ratatoskr is the main watcher: turn on allowWatchers in Loki's settings to trick it")
	}
	for _, other := range l.strikes {
		if !other.running() {
			continue
		}
		switch {
		case other.Part == name:
			l.mu.Unlock()
			return Strike{}, refuse(http.StatusConflict, "%s is already being tricked", Title(name))
		case name == Gjallar:
			l.mu.Unlock()
			return Strike{}, refuse(http.StatusConflict, "Gjallar is tricked only when nothing else is: %s is being tricked", Title(other.Part))
		case other.Part == Gjallar:
			l.mu.Unlock()
			return Strike{}, refuse(http.StatusConflict, "Gjallar is being tricked; wait for it to end")
		}
	}
	// Held before the part is called, so no second strike slips in.
	l.strikes = append([]*Strike{strike}, l.strikes...)
	l.mu.Unlock()

	err := client.Do(ctx, http.MethodPost, "/v1/loki/trick", map[string]any{
		"kind": trick.Kind, "loop": trick.Loop, "delayMs": trick.DelayMs, "until": strike.EndsAt.UnixMilli(),
	}, nil)
	if err != nil {
		l.mu.Lock()
		l.strikes = slices.DeleteFunc(l.strikes, func(s *Strike) bool { return s == strike })
		l.mu.Unlock()
		text := err.Error()
		switch {
		case strings.Contains(text, "answered 404"):
			return Strike{}, refuse(http.StatusConflict, "%s's trick switch is off: set LOKI=on in parts.env (dev mode only) and restart it", Title(name))
		case strings.Contains(text, "answered 400"), strings.Contains(text, "answered 409"):
			return Strike{}, refuse(http.StatusBadRequest, "%s refused the trick: %s", Title(name), text)
		}
		return Strike{}, fmt.Errorf("could not trick %s: %w", Title(name), err)
	}
	l.Logf("strike %s: %s by %s until %s", strike.ID, Label(name, trick), by, strike.EndsAt.Format(time.TimeOnly))
	l.mu.Lock()
	l.trimLocked()
	copied := strike.copy()
	l.mu.Unlock()
	l.save()
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		l.run(strike)
	}()
	return copied, nil
}

func (l *Loki) trimLocked() {
	if len(l.strikes) > KeepStrikes {
		l.strikes = l.strikes[:KeepStrikes]
	}
}

// Stop ends a running strike now: the trick is taken off, and the strike
// ends as stopped, with no verdict.
func (l *Loki) Stop(ctx context.Context, id string) (Strike, error) {
	l.mu.Lock()
	var strike *Strike
	for _, s := range l.strikes {
		if s.ID == id {
			strike = s
		}
	}
	if strike == nil {
		l.mu.Unlock()
		return Strike{}, refuse(http.StatusNotFound, "no such strike")
	}
	if !strike.running() {
		l.mu.Unlock()
		return Strike{}, refuse(http.StatusConflict, "the strike is over: %s", strike.State)
	}
	strike.State = Stopped
	if now := l.now(); now.Before(strike.EndsAt) {
		strike.EndsAt = now
	}
	if strike.stop != nil {
		close(strike.stop)
		strike.stop = nil
	}
	name := strike.Part
	l.mu.Unlock()
	if err := l.Clients[name].Do(ctx, http.MethodDelete, "/v1/loki/trick", nil, nil); err != nil {
		l.Logf("strike %s: taking the trick off %s: %v (it ends on its own)", id, name, err)
	}
	l.save()
	copied, _ := l.Strike(id)
	return copied, nil
}

// Watched is what the watchers say at one moment.
type Watched struct {
	// Down is Ratatoskr's checks that are down, by key; nil when it didn't answer.
	Down map[string]bool
	// Alerts is Gjallar's alerts; nil when it didn't answer.
	Alerts []Alert
}

// Alert is one of Gjallar's alerts, as Loki reads it.
type Alert struct {
	ID          int64      `json:"id"`
	Source      string     `json:"source"`
	Key         string     `json:"key"`
	Severity    string     `json:"severity"`
	Title       string     `json:"title"`
	State       string     `json:"state"`
	FirstSeenAt time.Time  `json:"firstSeenAt"`
	LastSeenAt  time.Time  `json:"lastSeenAt"`
	ResolvedAt  *time.Time `json:"resolvedAt,omitempty"`
}

// RatatoskrStatus is the part of Ratatoskr's GET /v1/status Loki reads.
type RatatoskrStatus struct {
	Parts []struct {
		Name string `json:"name"`
	} `json:"parts"`
	Checks []struct {
		Key  string `json:"key"`
		Down bool   `json:"down"`
	} `json:"checks"`
}

func (l *Loki) ratatoskrStatus(ctx context.Context) (*RatatoskrStatus, error) {
	var status RatatoskrStatus
	if err := l.Clients[Ratatoskr].Do(ctx, http.MethodGet, "/v1/status", nil, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

func (l *Loki) alerts(ctx context.Context, limit int) ([]Alert, error) {
	var answer struct {
		Alerts []Alert `json:"alerts"`
	}
	if err := l.Clients[Gjallar].Do(ctx, http.MethodGet, fmt.Sprintf("/v1/alerts?state=all&limit=%d", limit), nil, &answer); err != nil {
		return nil, err
	}
	return answer.Alerts, nil
}

func (l *Loki) watched(ctx context.Context) Watched {
	var w Watched
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if status, err := l.ratatoskrStatus(ctx); err == nil {
			w.Down = map[string]bool{}
			for _, c := range status.Checks {
				w.Down[c.Key] = c.Down
			}
		}
	}()
	go func() {
		defer wg.Done()
		if alerts, err := l.alerts(ctx, 200); err == nil {
			w.Alerts = alerts
		}
	}()
	wg.Wait()
	return w
}

// sees is whether the watched state shows the check noticing a strike that
// started at since.
func (w Watched) sees(c Check, since time.Time) bool {
	switch c.Watcher {
	case Ratatoskr:
		return w.Down[c.Key]
	case Gjallar:
		return w.raised(c, since)
	}
	return false
}

func (w Watched) raised(c Check, since time.Time) bool {
	for _, a := range w.Alerts {
		if a.Source == c.Source && a.Key == c.Key && !a.LastSeenAt.Before(since) {
			return true
		}
	}
	return false
}

// still is whether the check is still raised: not yet healed.
func (w Watched) still(c Check, since time.Time) bool {
	switch c.Watcher {
	case Ratatoskr:
		return w.Down[c.Key]
	case Gjallar:
		for _, a := range w.Alerts {
			if a.Source == c.Source && a.Key == c.Key && !a.LastSeenAt.Before(since) && a.State != "resolved" {
				return true
			}
		}
	}
	return false
}

// observe records which checks noticed, as of now.
func (l *Loki) observe(strike *Strike, w Watched) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range strike.Checks {
		c := &strike.Checks[i]
		if c.NoticedAt == nil && w.sees(*c, strike.StartedAt) {
			at := now
			c.NoticedAt = &at
		}
	}
}

// healed is whether the part is well again and nothing the strike raised is
// still raised.
func (l *Loki) healed(ctx context.Context, strike *Strike, w Watched) bool {
	probe := l.probe(ctx, strike.Part, false)
	if !probe.Up {
		return false
	}
	if strike.Trick.Kind == part.TrickLoop && probe.Health != nil && probe.Health.Loops[strike.Trick.Loop].LastError != "" {
		return false
	}
	for _, c := range strike.Checks {
		if w.still(c, strike.StartedAt) {
			return false
		}
	}
	return true
}

func (l *Loki) state(strike *Strike) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strike.State
}

// run follows a strike to its end.
func (l *Loki) run(strike *Strike) {
	ctx := l.ctx
	l.mu.Lock()
	stop, endsAt := strike.stop, strike.EndsAt
	l.mu.Unlock()
	ticker := time.NewTicker(l.Poll)
	defer ticker.Stop()
	end := time.NewTimer(endsAt.Sub(l.now()))
	defer end.Stop()

striking:
	for {
		l.observe(strike, l.watched(ctx))
		select {
		case <-ctx.Done():
			return
		case <-stop:
			break striking
		case <-end.C:
			break striking
		case <-ticker.C:
		}
	}

	l.mu.Lock()
	if strike.State == Striking {
		strike.State = Watching
	}
	l.mu.Unlock()
	// Belt and braces: the part ends it at until anyway.
	_ = l.Clients[strike.Part].Do(ctx, http.MethodDelete, "/v1/loki/trick", nil, nil)

	deadline := l.now().Add(l.HealWait)
	for {
		w := l.watched(ctx)
		l.observe(strike, w)
		if l.healed(ctx, strike, w) {
			at := l.now()
			l.mu.Lock()
			strike.HealedAt = &at
			l.mu.Unlock()
			break
		}
		if !l.now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
	l.finish(ctx, strike)
}

// finish gives the verdict, the tale, the alert when missed, and the message
// after a passive strike.
func (l *Loki) finish(ctx context.Context, strike *Strike) {
	l.mu.Lock()
	stopped := strike.State == Stopped
	caught := true
	for i := range strike.Checks {
		ok := strike.Checks[i].NoticedAt != nil
		strike.Checks[i].OK = &ok
		caught = caught && ok
	}
	if !stopped {
		strike.State = Missed
		if caught {
			strike.State = Caught
		}
	}
	strike.Tale = StrikeTale(*strike, l.Hours.Zone)
	done := strike.copy()
	l.mu.Unlock()
	l.save()
	l.Logf("strike %s on %s: %s", done.ID, done.Part, done.State)

	key := "missed:" + done.Part + ":" + done.Trick.Kind
	switch done.State {
	case Missed:
		var lines []string
		for _, c := range done.Checks {
			if c.NoticedAt != nil {
				lines = append(lines, fmt.Sprintf("%s noticed after %s", c.Expected, words(c.NoticedAt.Sub(done.StartedAt))))
			} else {
				lines = append(lines, fmt.Sprintf("%s did not notice", c.Expected))
			}
		}
		err := l.Gjallar.Raise(ctx, alert.Raise{
			Key:      key,
			Severity: alert.Warning,
			Title:    fmt.Sprintf("Loki tricked %s (%s) and it went unnoticed", Title(done.Part), done.Trick.Kind),
			Detail: fmt.Sprintf("%s for %s from %s. Expected: %s. %s.", Label(done.Part, done.Trick), words(done.EndsAt.Sub(done.StartedAt)),
				done.StartedAt.Format(time.RFC3339), strings.Join(ExpectStrings(done.Part, done.Trick), ", "), strings.Join(lines, "; ")),
		})
		if err != nil && err != alert.ErrOff {
			l.Logf("raising %s: %v", key, err)
		}
	case Caught:
		if err := l.Gjallar.Resolve(ctx, key); err != nil && err != alert.ErrOff {
			l.Logf("resolving %s: %v", key, err)
		}
	}
	if done.By == "passive" {
		hermodr := l.Clients["hermodr"]
		if hermodr.On() {
			push := false
			message := tell.Message{Title: fmt.Sprintf("Loki tricked %s: %s", Title(done.Part), done.State), Text: done.Tale, Priority: "low", Push: &push}
			if err := hermodr.Do(ctx, http.MethodPost, "/v1/messages", message, nil); err != nil {
				l.Logf("telling you about strike %s: %v", done.ID, err)
			}
		}
	}
	go func() { _ = l.BuildPlan(context.WithoutCancel(ctx)) }()
}

// Probe is one part as Loki sees it.
type Probe struct {
	Up     bool
	LokiOn bool
	Health *part.Health
}

// probe reads a part's /health, and, with trick, whether its trick switch answers.
func (l *Loki) probe(ctx context.Context, name string, trick bool) Probe {
	var probe Probe
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(l.URLs[name], "/")+"/health", nil)
	if err == nil {
		if response, err := l.HTTP.Do(request); err == nil {
			var health part.Health
			if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&health) == nil && health.Part != "" {
				probe.Health = &health
				probe.Up = response.StatusCode == http.StatusOK && health.OK
			}
			response.Body.Close()
		}
	}
	if trick && l.Clients[name].On() {
		probe.LokiOn = l.Clients[name].Do(ctx, http.MethodGet, "/v1/loki/trick", nil, nil) == nil
	}
	return probe
}

// probeAll probes every part at once.
func (l *Loki) probeAll(ctx context.Context, trick bool) map[string]Probe {
	probes := map[string]Probe{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, name := range l.Parts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			probe := l.probe(ctx, name, trick)
			mu.Lock()
			probes[name] = probe
			mu.Unlock()
		}()
	}
	wg.Wait()
	return probes
}

// TargetTrick is one trick a part can be struck with.
type TargetTrick struct {
	Kind    string   `json:"kind"`
	Loop    string   `json:"loop,omitempty"`
	DelayMs int      `json:"delayMs,omitempty"`
	Label   string   `json:"label"`
	Expect  []string `json:"expect"`
}

// Target is one part, whether it can be tricked, and how.
type Target struct {
	Name   string        `json:"name"`
	Up     bool          `json:"up"`
	LokiOn bool          `json:"lokiOn"`
	Tricks []TargetTrick `json:"tricks"`
}

// Tricks is every trick a part offers, given its health.
func Tricks(name string, health *part.Health) []Trick {
	tricks := []Trick{{Kind: part.TrickHealth}}
	if health != nil {
		var loops []string
		for loop := range health.Loops {
			loops = append(loops, loop)
		}
		slices.Sort(loops)
		for _, loop := range loops {
			tricks = append(tricks, Trick{Kind: part.TrickLoop, Loop: loop})
		}
	}
	return append(tricks, Trick{Kind: part.TrickSlow, DelayMs: DefaultDelayMs})
}

// Targets is every part Loki knows, with its tricks. Ratatoskr's are listed
// only with allowWatchers on.
func (l *Loki) Targets(ctx context.Context) []Target {
	probes := l.probeAll(ctx, true)
	allow := l.Settings().AllowWatchers
	targets := []Target{}
	for _, name := range l.Parts {
		probe := probes[name]
		target := Target{Name: name, Up: probe.Up, LokiOn: probe.LokiOn, Tricks: []TargetTrick{}}
		if name != Ratatoskr || allow {
			for _, trick := range Tricks(name, probe.Health) {
				target.Tricks = append(target.Tricks, TargetTrick{Kind: trick.Kind, Loop: trick.Loop, DelayMs: trick.DelayMs, Label: Label(name, trick), Expect: ExpectStrings(name, trick)})
			}
		}
		targets = append(targets, target)
	}
	return targets
}
