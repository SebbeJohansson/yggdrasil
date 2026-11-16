// Package ratatoskr is Yggdrasil's watchdog, named for the squirrel that runs
// up and down the world tree. It checks that every part is alive and that its
// loops run on time. It fixes nothing.
//
//   - Alive: GET /health answers within 2 s.
//   - Working: every loop on /health ran within three times its interval plus
//     a minute, and its last run did not fail.
//   - Two failures in a row raise an alert through Gjallar; two passes resolve it.
//   - When Gjallar itself is down, the alert goes around it: a direct phone
//     push (ntfy), and GET /v1/status, which the widget reads directly.
//   - It beats to Gjallar every minute, and Gjallar raises "ratatoskr is
//     silent" when the beats stop. Each watches the other.
//
// Ratatoskr runs outside the compose project (a systemd user unit), so a
// broken `docker compose up` cannot take it down with everything else.
//
// Routes:
//
//	GET /health
//	GET /v1/status   every part as last seen, and every check
package ratatoskr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/part"
	"yggdrasil/internal/tell"
	"yggdrasil/internal/watch"
)

// Name is this part's name, and its source on Gjallar.
const Name = "ratatoskr"

// Target is one part to watch.
type Target struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Seen is a part as Ratatoskr last saw it.
type Seen struct {
	Target
	Up        bool         `json:"up"`
	Error     string       `json:"error,omitempty"`
	Health    *part.Health `json:"health,omitempty"`
	CheckedAt time.Time    `json:"checkedAt"`
}

// Ratatoskr is the running watchdog.
type Ratatoskr struct {
	Targets []Target
	Gjallar *alert.Client
	// GjallarPart is the target that is Gjallar, whose own downtime is
	// reported around it.
	GjallarPart string
	// Fallback tells you Gjallar is down: Hermóðr, then ntfy straight.
	Fallback tell.Teller
	HTTP     *http.Client

	watch        *watch.Watch
	mu           sync.Mutex
	seen         map[string]Seen
	fallbackSent bool
	now          func() time.Time
}

// New makes a watchdog over the targets.
func New(targets []Target, gjallar *alert.Client, fallback tell.Teller) *Ratatoskr {
	r := &Ratatoskr{
		Targets:     targets,
		Gjallar:     gjallar,
		GjallarPart: "gjallar",
		Fallback:    fallback,
		HTTP:        &http.Client{Timeout: 2 * time.Second},
		watch:       watch.New(),
		seen:        map[string]Seen{},
		now:         time.Now,
	}
	r.watch.OutOfBand = func(key string) bool { return key == downKey(r.GjallarPart) }
	return r
}

func downKey(name string) string {
	return "down:" + name
}

func stalledKey(name, loop string) string {
	return "stalled:" + name + ":" + loop
}

func failingKey(name, loop string) string {
	return "failing:" + name + ":" + loop
}

// Title is a part's name as a person reads it.
func Title(name string) string {
	switch name {
	case "vedrfolnir":
		return "Veðrfölnir"
	case "":
		return ""
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// Allowance is how long a loop may go without running before it counts as
// stalled: three of its intervals and a minute.
func Allowance(every time.Duration) time.Duration {
	return 3*every + time.Minute
}

// Mount adds the routes and loops to the part.
func (r *Ratatoskr) Mount(p *part.Part, every time.Duration) {
	p.Handle("GET /v1/status", r.status)
	p.Loop("check", every, r.check)
	p.Loop("heartbeat", time.Minute, func(ctx context.Context) error {
		err := r.Gjallar.Heartbeat(ctx)
		if err == alert.ErrOff {
			return nil
		}
		return err
	})
}

// fetch reads one part's /health.
func (r *Ratatoskr) fetch(ctx context.Context, target Target) (*part.Health, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(target.URL, "/")+"/health", nil)
	if err != nil {
		return nil, err
	}
	response, err := r.HTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/health answered %d", response.StatusCode)
	}
	var health part.Health
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&health); err != nil {
		return nil, fmt.Errorf("/health is not a part's health: %w", err)
	}
	if !health.OK {
		return &health, fmt.Errorf("/health says it is not ok")
	}
	return &health, nil
}

// Results turns one part's health (or its error) into checks.
func Results(target Target, health *part.Health, err error, now time.Time) []watch.Result {
	title := Title(target.Name)
	results := []watch.Result{{
		Key:      downKey(target.Name),
		OK:       err == nil,
		Severity: alert.Critical,
		Title:    title + " is down",
		Detail:   errorText(err),
	}}
	if err != nil || health == nil {
		// Loops of a part that is down are not checked: one problem, one alert.
		return results
	}
	for name, loop := range health.Loops {
		every := time.Duration(loop.EverySeconds) * time.Second
		if every <= 0 {
			continue
		}
		since := health.StartedAt
		if loop.LastRunAt != nil {
			since = *loop.LastRunAt
		}
		late := now.Sub(since)
		results = append(results, watch.Result{
			Key:      stalledKey(target.Name, name),
			OK:       late <= Allowance(every),
			Severity: alert.Warning,
			Title:    fmt.Sprintf("%s's %s loop has not run in %s", title, name, late.Round(time.Minute)),
			Detail:   fmt.Sprintf("it runs every %s; last run %s", every, since.Format(time.RFC3339)),
		})
		results = append(results, watch.Result{
			Key:      failingKey(target.Name, name),
			OK:       loop.LastError == "",
			Severity: alert.Warning,
			Title:    fmt.Sprintf("%s's %s loop failed its last run", title, name),
			Detail:   loop.LastError,
		})
	}
	return results
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// check runs one round: every part, then Gjallar is told what changed.
func (r *Ratatoskr) check(ctx context.Context) error {
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []watch.Result
		keys    = map[string]bool{}
	)
	for _, target := range r.Targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			health, err := r.fetch(ctx, target)
			now := r.now()
			found := Results(target, health, err, now)
			mu.Lock()
			defer mu.Unlock()
			results = append(results, found...)
			for _, result := range found {
				keys[result.Key] = true
			}
			r.mu.Lock()
			r.seen[target.Name] = Seen{Target: target, Up: err == nil, Error: errorText(err), Health: health, CheckedAt: now}
			r.mu.Unlock()
		}()
	}
	wg.Wait()
	r.watch.Observe(results)
	// A loop that is no longer on a part's /health (renamed, removed) is dropped.
	r.watch.Forget(func(key string) bool { return keys[key] })
	r.aroundGjallar(ctx)
	err := r.watch.Sync(ctx, r.Gjallar)
	if err == alert.ErrOff {
		return nil
	}
	return err
}

// aroundGjallar pushes to the phone directly, once, when Gjallar is down.
func (r *Ratatoskr) aroundGjallar(ctx context.Context) {
	state, known := r.watch.Get(downKey(r.GjallarPart))
	r.mu.Lock()
	defer r.mu.Unlock()
	if !known || !state.Down {
		r.fallbackSent = false
		return
	}
	if r.fallbackSent || r.Fallback == nil || !r.Fallback.On() {
		return
	}
	r.fallbackSent = true
	go func() {
		pushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_ = r.Fallback.Tell(pushCtx, tell.Message{Title: "Gjallar is down", Text: "Alerts can't be delivered until it's back. " + state.Detail, Priority: "urgent"})
	}()
}

// Status is GET /v1/status's answer.
type Status struct {
	Parts  []Seen        `json:"parts"`
	Checks []watch.State `json:"checks"`
	// GjallarUp is false while alerts can't be delivered: the widget shows this itself.
	GjallarUp bool `json:"gjallarUp"`
}

// Snapshot is the status now.
func (r *Ratatoskr) Snapshot() Status {
	status := Status{Parts: []Seen{}, Checks: r.watch.States(), GjallarUp: true}
	r.mu.Lock()
	for _, target := range r.Targets {
		if seen, ok := r.seen[target.Name]; ok {
			status.Parts = append(status.Parts, seen)
		} else {
			status.Parts = append(status.Parts, Seen{Target: target})
		}
	}
	r.mu.Unlock()
	if state, ok := r.watch.Get(downKey(r.GjallarPart)); ok && state.Down {
		status.GjallarUp = false
	}
	return status
}

func (r *Ratatoskr) status(w http.ResponseWriter, _ *http.Request) {
	part.JSON(w, http.StatusOK, r.Snapshot())
}
