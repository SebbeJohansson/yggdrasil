// Package loki tests the watchers. Loki finds what nobody would notice if it
// broke (the blind spots), tells short stories of the real alerts lately (the
// tales), and, when you ask or now and then on its own, tricks one part for a
// few minutes to see whether Ratatoskr and Gjallar catch it (a strike).
//
// Loki never breaks anything. A trick is an in-process behavior of the part
// itself, offered by its trick switch (internal/part, only with LOKI=on), and
// every trick ends on its own at its until, even if Loki dies.
//
//	GET  /v1/plan                 the blind spots and the tales
//	GET  /v1/targets              every part, its loops, and what each trick should be caught by
//	POST /v1/strikes              owner: trick one part for a few minutes
//	GET  /v1/strikes              the strikes, newest first
//	GET  /v1/strikes/{id}         one strike
//	POST /v1/strikes/{id}/stop    owner: end it now
//	GET  /v1/settings             { passive, perDay, allowWatchers }
//	PATCH /v1/settings            owner
package loki

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
	"yggdrasil/internal/part"
	"yggdrasil/internal/where"
)

// Name is this part's name, and its source on Gjallar.
const Name = "loki"

const (
	// MaxMinutes is the longest a strike may last.
	MaxMinutes = 10
	// DefaultMinutes is a strike's length when none is asked for.
	DefaultMinutes = 3
	// DefaultDelayMs is a slow trick's delay when none is asked for.
	DefaultDelayMs = 3000
	// KeepStrikes is how many strikes are kept.
	KeepStrikes = 200
	// TaleDays is how far back the tales go.
	TaleDays = 14
)

// The states of a strike.
const (
	Striking = "striking"
	Watching = "watching"
	Caught   = "caught"
	Missed   = "missed"
	Stopped  = "stopped"
)

// Trick is the trick a strike plays, without its until.
type Trick struct {
	Kind    string `json:"kind"`
	Loop    string `json:"loop,omitempty"`
	DelayMs int    `json:"delayMs,omitempty"`
}

// Check is one watcher that should notice a strike.
type Check struct {
	Watcher   string     `json:"watcher"`
	Expected  string     `json:"expected"`
	NoticedAt *time.Time `json:"noticedAt,omitempty"`
	OK        *bool      `json:"ok,omitempty"`

	// What it looks for: a Ratatoskr check key, or a Gjallar alert's source and key.
	Source string `json:"-"`
	Key    string `json:"-"`
}

// Strike is one part tricked for a few minutes, and who noticed.
type Strike struct {
	ID        string     `json:"id"`
	Part      string     `json:"part"`
	Trick     Trick      `json:"trick"`
	By        string     `json:"by"`
	StartedAt time.Time  `json:"startedAt"`
	EndsAt    time.Time  `json:"endsAt"`
	HealedAt  *time.Time `json:"healedAt,omitempty"`
	State     string     `json:"state"`
	Checks    []Check    `json:"checks"`
	Tale      string     `json:"tale,omitempty"`

	stop chan struct{}
}

func (s *Strike) done() bool {
	return s.State == Caught || s.State == Missed || s.State == Stopped
}

// running is whether the strike still holds its part: striking or watching.
func (s *Strike) running() bool {
	return s.State == Striking || s.State == Watching
}

func (s *Strike) copy() Strike {
	c := *s
	c.Checks = append([]Check{}, s.Checks...)
	c.stop = nil
	return c
}

// Settings is GET and PATCH /v1/settings.
type Settings struct {
	Passive       bool `json:"passive"`
	PerDay        int  `json:"perDay"`
	AllowWatchers bool `json:"allowWatchers"`
}

// DefaultSettings is Loki before you set anything: passive off.
func DefaultSettings() Settings {
	return Settings{Passive: false, PerDay: 1, AllowWatchers: false}
}

// MaxPerDay is the most passive strikes a day you may ask for.
const MaxPerDay = 5

// State is what Loki keeps across restarts.
type State struct {
	Settings *Settings `json:"settings,omitempty"`
	Strikes  []Strike  `json:"strikes"`
}

// Loki is the running part.
type Loki struct {
	// Parts are the parts Loki knows, in order; never Loki itself.
	Parts []string
	// URLs is where each part is found, for its /health.
	URLs map[string]string
	// Clients is Loki's token at each part, for its trick switch and routes.
	Clients map[string]*call.Part
	// Gjallar raises missed:<part>:<kind> with Loki's own token.
	Gjallar *alert.Client
	// Heartbeats are the senders Gjallar expects to beat (GJALLAR_HEARTBEATS).
	Heartbeats []string
	// Watch is Ratatoskr's watch list when Ratatoskr doesn't answer (RATATOSKR_WATCH).
	Watch []string
	// Hours is your working day, for passive strikes.
	Hours Hours
	// StatePath is where settings and strikes are kept; empty keeps them in memory.
	StatePath string
	HTTP      *http.Client
	Logf      func(format string, args ...any)

	// Poll is how often a strike looks at the watchers; HealWait how long it
	// waits for healing after the trick ends; Minute is a strike's minute.
	Poll, HealWait, Minute time.Duration

	ctx context.Context
	now func() time.Time
	// start starts a passive strike: Start, or a fake in tests.
	start func(ctx context.Context, name string, trick Trick, minutes int, by string) (Strike, error)
	rand  *mathrand.Rand

	mu         sync.Mutex
	settings   Settings
	strikes    []*Strike // newest first
	plan       Plan
	plannedDay string
	slots      []time.Time
	wg         sync.WaitGroup
}

// New makes Loki over the parts. clients holds Loki's token at each part.
func New(parts []string, urls map[string]string, clients map[string]*call.Part, gjallar *alert.Client) *Loki {
	return &Loki{
		Parts:    parts,
		URLs:     urls,
		Clients:  clients,
		Gjallar:  gjallar,
		Hours:    DefaultHours(),
		HTTP:     &http.Client{Timeout: 3 * time.Second},
		Logf:     func(string, ...any) {},
		Poll:     10 * time.Second,
		HealWait: 5 * time.Minute,
		Minute:   time.Minute,
		ctx:      context.Background(),
		now:      time.Now,
		rand:     mathrand.New(mathrand.NewPCG(uint64(time.Now().UnixNano()), 7)),
		settings: DefaultSettings(),
		plan:     Plan{BlindSpots: []BlindSpot{}, Tales: []Tale{}},
	}
}

// KnownParts is every part but Loki, by port.
func KnownParts() []string {
	var names []string
	for name := range where.Ports {
		if name != Name {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool { return where.Ports[names[i]] < where.Ports[names[j]] })
	return names
}

// Load reads the state file: settings and strikes. A strike that was running
// when Loki stopped is marked stopped; its trick ended on its own.
func (l *Loki) Load() error {
	if l.StatePath == "" {
		return nil
	}
	text, err := os.ReadFile(l.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var state State
	if err := json.Unmarshal(text, &state); err != nil {
		return fmt.Errorf("%s: %w", l.StatePath, err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if state.Settings != nil {
		l.settings = *state.Settings
	}
	l.strikes = nil
	for _, s := range state.Strikes {
		strike := s
		if strike.running() {
			strike.State = Stopped
			if strike.Tale == "" {
				strike.Tale = fmt.Sprintf("Loki was interrupted halfway through tricking %s. The trick ended on its own at %s, as every trick does.", Title(strike.Part), strike.EndsAt.In(l.Hours.Zone).Format("15:04"))
			}
		}
		l.strikes = append(l.strikes, &strike)
	}
	return nil
}

// save writes the state file, aside and renamed. Call it without l.mu.
func (l *Loki) save() {
	if l.StatePath == "" {
		return
	}
	l.mu.Lock()
	settings := l.settings
	state := State{Settings: &settings, Strikes: []Strike{}}
	for _, s := range l.strikes {
		state.Strikes = append(state.Strikes, s.copy())
	}
	l.mu.Unlock()
	text, err := json.MarshalIndent(state, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(l.StatePath), 0o700)
	}
	if err == nil {
		temp := l.StatePath + ".tmp"
		if err = os.WriteFile(temp, text, 0o600); err == nil {
			err = os.Rename(temp, l.StatePath)
		}
	}
	if err != nil {
		l.Logf("keeping the state: %v", err)
	}
}

// Mount adds the routes and loops to the part. ctx is what strikes run
// under; they end with it.
func (l *Loki) Mount(ctx context.Context, p *part.Part) {
	l.ctx = ctx
	p.Handle("GET /v1/plan", l.getPlan)
	p.Handle("GET /v1/targets", l.getTargets)
	p.Handle("POST /v1/strikes", l.owner(l.postStrike))
	p.Handle("GET /v1/strikes", l.listStrikes)
	p.Handle("GET /v1/strikes/{id}", l.getStrike)
	p.Handle("POST /v1/strikes/{id}/stop", l.owner(l.stopStrike))
	p.Handle("GET /v1/settings", l.getSettings)
	p.Handle("PATCH /v1/settings", l.owner(l.patchSettings))
	p.Loop("plan", time.Hour, l.BuildPlan)
	p.Loop("passive", time.Minute, l.PassiveTick)
}

func (l *Loki) owner(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if part.Caller(r) != part.Owner {
			part.Error(w, http.StatusForbidden, "only the owner may do this")
			return
		}
		handler(w, r)
	}
}

// Settings is what is set now.
func (l *Loki) Settings() Settings {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.settings
}

func (l *Loki) getSettings(w http.ResponseWriter, _ *http.Request) {
	part.JSON(w, http.StatusOK, l.Settings())
}

func (l *Loki) patchSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Passive       *bool `json:"passive"`
		PerDay        *int  `json:"perDay"`
		AllowWatchers *bool `json:"allowWatchers"`
	}
	if err := part.Decode(r, &body); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.PerDay != nil && (*body.PerDay < 1 || *body.PerDay > MaxPerDay) {
		part.Error(w, http.StatusBadRequest, fmt.Sprintf("perDay is 1 to %d", MaxPerDay))
		return
	}
	l.mu.Lock()
	if body.Passive != nil {
		l.settings.Passive = *body.Passive
	}
	if body.PerDay != nil {
		l.settings.PerDay = *body.PerDay
	}
	if body.AllowWatchers != nil {
		l.settings.AllowWatchers = *body.AllowWatchers
	}
	settings := l.settings
	l.mu.Unlock()
	l.save()
	part.JSON(w, http.StatusOK, settings)
}

func (l *Loki) getPlan(w http.ResponseWriter, _ *http.Request) {
	l.mu.Lock()
	plan := l.plan
	l.mu.Unlock()
	part.JSON(w, http.StatusOK, plan)
}

func (l *Loki) getTargets(w http.ResponseWriter, r *http.Request) {
	part.JSON(w, http.StatusOK, map[string]any{"parts": l.Targets(r.Context())})
}

// Strikes is the strikes, newest first, at most limit.
func (l *Loki) Strikes(limit int) []Strike {
	l.mu.Lock()
	defer l.mu.Unlock()
	list := []Strike{}
	for _, s := range l.strikes {
		if len(list) >= limit {
			break
		}
		list = append(list, s.copy())
	}
	return list
}

// Strike is one strike by id.
func (l *Loki) Strike(id string) (Strike, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.strikes {
		if s.ID == id {
			return s.copy(), true
		}
	}
	return Strike{}, false
}

func (l *Loki) listStrikes(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if text := r.URL.Query().Get("limit"); text != "" {
		n, err := strconv.Atoi(text)
		if err != nil || n < 1 || n > KeepStrikes {
			part.Error(w, http.StatusBadRequest, fmt.Sprintf("limit is 1 to %d", KeepStrikes))
			return
		}
		limit = n
	}
	part.JSON(w, http.StatusOK, map[string]any{"strikes": l.Strikes(limit)})
}

func (l *Loki) getStrike(w http.ResponseWriter, r *http.Request) {
	strike, ok := l.Strike(r.PathValue("id"))
	if !ok {
		part.Error(w, http.StatusNotFound, "no such strike")
		return
	}
	part.JSON(w, http.StatusOK, strike)
}

func (l *Loki) postStrike(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Part    string `json:"part"`
		Trick   Trick  `json:"trick"`
		Minutes int    `json:"minutes"`
	}
	if err := part.Decode(r, &body); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	strike, err := l.Start(r.Context(), body.Part, body.Trick, body.Minutes, "manual")
	if err != nil {
		var refused *Refused
		if errors.As(err, &refused) {
			part.Error(w, refused.Status, refused.Message)
			return
		}
		part.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	part.JSON(w, http.StatusCreated, strike)
}

func (l *Loki) stopStrike(w http.ResponseWriter, r *http.Request) {
	strike, err := l.Stop(r.Context(), r.PathValue("id"))
	if err != nil {
		var refused *Refused
		if errors.As(err, &refused) {
			part.Error(w, refused.Status, refused.Message)
			return
		}
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, strike)
}

// Refused is a strike Loki won't start or stop, with the status to answer.
type Refused struct {
	Status  int
	Message string
}

func (r *Refused) Error() string { return r.Message }

func refuse(status int, format string, args ...any) error {
	return &Refused{Status: status, Message: fmt.Sprintf(format, args...)}
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Title is a part's name as a person reads it.
func Title(name string) string {
	switch name {
	case "vedrfolnir":
		return "Veðrfölnir"
	case "hermodr":
		return "Hermóðr"
	case "":
		return ""
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// Wait waits for the strikes running in the background (tests).
func (l *Loki) Wait() {
	l.wg.Wait()
}
