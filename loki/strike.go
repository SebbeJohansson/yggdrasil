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
	case part.TrickHealth, KindDrop, KindHang, KindFreeze:
		// Each makes the part look down from where Ratatoskr stands.
		if name == Ratatoskr {
			// Nobody reads Ratatoskr's /health; its heartbeat to Gjallar is
			// what stops, when anything does.
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
	case KindError:
		// Nothing reads a part's answers but /health yet: what would, if anything did.
		return []Check{ratatoskr("errors:" + name)}
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
	delay := words(time.Duration(trick.DelayMs) * time.Millisecond)
	switch trick.via() + ":" + trick.Kind {
	case ViaSwitch + ":" + part.TrickHealth:
		return fmt.Sprintf("%s stops answering /health", Title(name))
	case ViaSwitch + ":" + part.TrickLoop:
		return fmt.Sprintf("%s's %s loop fails every run", Title(name), trick.Loop)
	case ViaSwitch + ":" + part.TrickSlow:
		return fmt.Sprintf("%s answers every call %s late", Title(name), delay)
	case ViaProxy + ":" + part.TrickHealth:
		return fmt.Sprintf("%s's /health answers 503 on the way, from Loki's proxy", Title(name))
	case ViaProxy + ":" + part.TrickSlow:
		return fmt.Sprintf("every call to %s arrives %s late, held by Loki's proxy", Title(name), delay)
	case ViaProxy + ":" + KindError:
		status := trick.Status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		if trick.Path != "" {
			return fmt.Sprintf("%s's routes under %s answer %d, from Loki's proxy, while /health stays fine", Title(name), trick.Path, status)
		}
		return fmt.Sprintf("%s's routes answer %d, from Loki's proxy, while /health stays fine", Title(name), status)
	case ViaProxy + ":" + KindDrop:
		return fmt.Sprintf("every call to %s is cut off unanswered by Loki's proxy", Title(name))
	case ViaProxy + ":" + KindHang:
		return fmt.Sprintf("every call to %s hangs unanswered in Loki's proxy", Title(name))
	case ViaFreeze + ":" + KindFreeze:
		return fmt.Sprintf("%s's process is frozen", Title(name))
	}
	return trick.Kind
}

// Kinds is the kinds of trick each via plays.
var Kinds = map[string][]string{
	ViaSwitch: {part.TrickHealth, part.TrickLoop, part.TrickSlow},
	ViaProxy:  {part.TrickHealth, part.TrickSlow, KindError, KindDrop, KindHang},
	ViaFreeze: {KindFreeze},
}

// Normalize checks a trick and fills in its defaults. With no via, a freeze
// strikes by freeze, an error, drop or hang by proxy, and the rest by switch.
func Normalize(trick Trick) (Trick, error) {
	if trick.Via == "" {
		switch trick.Kind {
		case KindFreeze:
			trick.Via = ViaFreeze
		case KindError, KindDrop, KindHang:
			trick.Via = ViaProxy
		default:
			trick.Via = ViaSwitch
		}
	}
	kinds, ok := Kinds[trick.Via]
	if !ok {
		return Trick{}, refuse(http.StatusBadRequest, "trick.via is switch, proxy or freeze")
	}
	if !slices.Contains(kinds, trick.Kind) {
		return Trick{}, refuse(http.StatusBadRequest, "by %s, trick.kind is %s", trick.Via, strings.Join(kinds, ", "))
	}
	loop, delayMs, status, path := trick.Loop, trick.DelayMs, trick.Status, trick.Path
	trick.Loop, trick.DelayMs, trick.Status, trick.Path = "", 0, 0, ""
	switch trick.Kind {
	case part.TrickLoop:
		if loop == "" {
			return Trick{}, refuse(http.StatusBadRequest, "a loop trick names the loop")
		}
		trick.Loop = loop
	case part.TrickSlow:
		if delayMs == 0 {
			delayMs = DefaultDelayMs
		}
		if delayMs < 1 || time.Duration(delayMs)*time.Millisecond > part.MaxTrickDelay {
			return Trick{}, refuse(http.StatusBadRequest, "delayMs is 1 to 10000")
		}
		trick.DelayMs = delayMs
	case KindError:
		if status == 0 {
			status = http.StatusInternalServerError
		}
		if status != http.StatusInternalServerError && status != http.StatusBadGateway {
			return Trick{}, refuse(http.StatusBadRequest, "an error trick's status is 500 or 502")
		}
		if path != "" && !strings.HasPrefix(path, "/") {
			return Trick{}, refuse(http.StatusBadRequest, "an error trick's path starts with /")
		}
		trick.Status, trick.Path = status, path
	}
	return trick, nil
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
	trick, err := Normalize(trick)
	if err != nil {
		return Strike{}, err
	}
	switch trick.Via {
	case ViaSwitch:
		if !l.Clients[name].On() {
			return Strike{}, refuse(http.StatusConflict, "Loki has no token at %s: %s_TOKEN in loki.env", Title(name), strings.ToUpper(name))
		}
	case ViaProxy:
		if l.Proxies[name] == nil {
			return Strike{}, refuse(http.StatusConflict, "%s isn't behind Loki's proxy: add it to LOKI_PROXY and point YGG_%s_URL at the proxy", Title(name), strings.ToUpper(name))
		}
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

	if err := l.trick(ctx, strike); err != nil {
		l.mu.Lock()
		l.strikes = slices.DeleteFunc(l.strikes, func(s *Strike) bool { return s == strike })
		l.mu.Unlock()
		return Strike{}, err
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

// trick puts the strike's trick on its part, by its via.
func (l *Loki) trick(ctx context.Context, strike *Strike) error {
	name, trick := strike.Part, strike.Trick
	switch trick.Via {
	case ViaProxy:
		l.Proxies[name].Set(ProxyTrick{Kind: trick.Kind, DelayMs: trick.DelayMs, Status: trick.Status, Path: trick.Path, Until: strike.EndsAt})
		return nil
	case ViaFreeze:
		pids, err := l.freeze(strike.ID, name, strike.EndsAt.Sub(l.now()))
		if err != nil {
			return err
		}
		l.mu.Lock()
		strike.Pids = pids
		l.mu.Unlock()
		return nil
	}
	err := l.Clients[name].Do(ctx, http.MethodPost, "/v1/loki/trick", map[string]any{
		"kind": trick.Kind, "loop": trick.Loop, "delayMs": trick.DelayMs, "until": strike.EndsAt.UnixMilli(),
	}, nil)
	if err == nil {
		return nil
	}
	text := err.Error()
	switch {
	case strings.Contains(text, "answered 404"):
		return refuse(http.StatusConflict, "%s's trick switch is off: set LOKI=on in parts.env (dev mode only) and restart it", Title(name))
	case strings.Contains(text, "answered 400"), strings.Contains(text, "answered 409"):
		return refuse(http.StatusBadRequest, "%s refused the trick: %s", Title(name), text)
	}
	return fmt.Errorf("could not trick %s: %w", Title(name), err)
}

// untrick takes the strike's trick off its part, by its via.
func (l *Loki) untrick(ctx context.Context, strike *Strike) error {
	switch strike.Trick.via() {
	case ViaProxy:
		if proxy := l.Proxies[strike.Part]; proxy != nil {
			proxy.Clear()
		}
		return nil
	case ViaFreeze:
		l.thaw(strike.ID)
		return nil
	}
	return l.Clients[strike.Part].Do(ctx, http.MethodDelete, "/v1/loki/trick", nil, nil)
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
	if err := l.untrick(ctx, strike); err != nil {
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
	// Belt and braces: every trick ends at its until anyway.
	_ = l.untrick(ctx, strike)

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
	Via     string   `json:"via"`
	Kind    string   `json:"kind"`
	Loop    string   `json:"loop,omitempty"`
	DelayMs int      `json:"delayMs,omitempty"`
	Status  int      `json:"status,omitempty"`
	Label   string   `json:"label"`
	Expect  []string `json:"expect"`
}

// Trick is the trick itself, as a strike takes it.
func (t TargetTrick) Trick() Trick {
	return Trick{Via: t.Via, Kind: t.Kind, Loop: t.Loop, DelayMs: t.DelayMs, Status: t.Status}
}

// Via is whether a part can be struck one way, and why not.
type Via struct {
	Via       string `json:"via"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// Target is one part, whether it can be tricked, and how.
type Target struct {
	Name   string `json:"name"`
	Up     bool   `json:"up"`
	LokiOn bool   `json:"lokiOn"`
	// Proxied is whether Loki's proxy stands in front of it (LOKI_PROXY).
	Proxied bool `json:"proxied"`
	// Freezable is whether Loki can see and signal its process.
	Freezable bool          `json:"freezable"`
	Vias      []Via         `json:"vias"`
	Tricks    []TargetTrick `json:"tricks"`
}

// available is whether the target can be struck by via.
func (t Target) available(via string) bool {
	for _, v := range t.Vias {
		if v.Via == via {
			return v.Available
		}
	}
	return false
}

// Tricks is every trick a part offers by its switch, given its health.
func Tricks(name string, health *part.Health) []Trick {
	tricks := []Trick{{Via: ViaSwitch, Kind: part.TrickHealth}}
	if health != nil {
		var loops []string
		for loop := range health.Loops {
			loops = append(loops, loop)
		}
		slices.Sort(loops)
		for _, loop := range loops {
			tricks = append(tricks, Trick{Via: ViaSwitch, Kind: part.TrickLoop, Loop: loop})
		}
	}
	return append(tricks, Trick{Via: ViaSwitch, Kind: part.TrickSlow, DelayMs: DefaultDelayMs})
}

// ProxyTricks is every trick Loki's proxy plays on a part.
func ProxyTricks() []Trick {
	return []Trick{
		{Via: ViaProxy, Kind: part.TrickHealth},
		{Via: ViaProxy, Kind: part.TrickSlow, DelayMs: DefaultDelayMs},
		{Via: ViaProxy, Kind: KindError, Status: http.StatusInternalServerError},
		{Via: ViaProxy, Kind: KindDrop},
		{Via: ViaProxy, Kind: KindHang},
	}
}

// Targets is every part Loki knows, with its tricks: by its switch always,
// by proxy when it is proxied, by freeze when Loki can freeze it. Ratatoskr's
// are listed only with allowWatchers on. Vias says, for each way, whether it
// is open and why not.
func (l *Loki) Targets(ctx context.Context) []Target {
	probes := l.probeAll(ctx, true)
	allow := l.Settings().AllowWatchers
	targets := []Target{}
	for _, name := range l.Parts {
		probe := probes[name]
		pids, frozenWhy := l.processes(name)
		proxy := l.Proxies[name]
		target := Target{Name: name, Up: probe.Up, LokiOn: probe.LokiOn, Proxied: proxy != nil, Freezable: len(pids) > 0, Tricks: []TargetTrick{}}

		switchWhy := ""
		switch {
		case !l.Clients[name].On():
			switchWhy = fmt.Sprintf("Loki has no token at %s: %s_TOKEN in loki.env", Title(name), strings.ToUpper(name))
		case !probe.LokiOn && !probe.Up:
			switchWhy = fmt.Sprintf("%s doesn't answer", Title(name))
		case !probe.LokiOn:
			switchWhy = fmt.Sprintf("%s's trick switch is off: LOKI=on in parts.env (dev mode only)", Title(name))
		}
		proxyWhy := ""
		if proxy == nil {
			proxyWhy = fmt.Sprintf("%s isn't behind Loki's proxy: add it to LOKI_PROXY and point YGG_%s_URL at the proxy (dev only)", Title(name), strings.ToUpper(name))
		}
		target.Vias = []Via{
			{Via: ViaSwitch, Available: switchWhy == "", Reason: switchWhy},
			{Via: ViaProxy, Available: proxyWhy == "", Reason: proxyWhy},
			{Via: ViaFreeze, Available: frozenWhy == "", Reason: frozenWhy},
		}
		if name == Ratatoskr && !allow {
			for i := range target.Vias {
				target.Vias[i] = Via{Via: target.Vias[i].Via, Reason: "Ratatoskr is the main watcher: turn on allowWatchers in Loki's settings to trick it"}
			}
			targets = append(targets, target)
			continue
		}
		tricks := Tricks(name, probe.Health)
		if proxy != nil {
			tricks = append(tricks, ProxyTricks()...)
		}
		if len(pids) > 0 {
			tricks = append(tricks, Trick{Via: ViaFreeze, Kind: KindFreeze})
		}
		for _, trick := range tricks {
			target.Tricks = append(target.Tricks, TargetTrick{Via: trick.Via, Kind: trick.Kind, Loop: trick.Loop, DelayMs: trick.DelayMs, Status: trick.Status,
				Label: Label(name, trick), Expect: ExpectStrings(name, trick)})
		}
		targets = append(targets, target)
	}
	return targets
}
