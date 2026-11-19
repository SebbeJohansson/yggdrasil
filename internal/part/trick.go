package part

// The trick switch: how Loki tests the watchers. With LOKI=on in a part's
// environment (meant for dev mode, set in parts.env), the part answers
//
//	GET    /v1/loki/trick   { trick: Trick | null }
//	POST   /v1/loki/trick   { kind: health|loop|slow, loop?, delayMs?, until }
//	DELETE /v1/loki/trick
//
// to the owner and to Loki only. Without LOKI=on these routes are not there
// (404) and nothing can be tricked.
//
// A trick is an in-process behavior and nothing else: it never touches data,
// files, processes or containers. Every trick has an `until` (epoch ms, at
// most MaxTrick ahead) and is over then, read from the clock on every use,
// so it ends on time even if Loki dies.
//
//   - health: /health answers 503 with `ok: false`.
//   - loop:   the named loop's runs are skipped and fail with TrickError, and
//     /health shows that as the loop's lastError until the trick ends.
//   - slow:   every route but /health and the trick routes waits delayMs first.

import (
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// MaxTrick is the longest a trick may last.
const MaxTrick = 10 * time.Minute

// MaxTrickDelay is the longest a slow trick may hold each request.
const MaxTrickDelay = 10 * time.Second

// TrickError is what a tricked loop's runs fail with.
const TrickError = "Loki's trick"

// TrickCaller is the name on Loki's token at every part.
const TrickCaller = "loki"

// The kinds of trick.
const (
	TrickHealth = "health"
	TrickLoop   = "loop"
	TrickSlow   = "slow"
)

// Trick is what a part is doing to be caught.
type Trick struct {
	Kind    string `json:"kind"`
	Loop    string `json:"loop,omitempty"`
	DelayMs int    `json:"delayMs,omitempty"`
	// Until is when it ends, in milliseconds since 1970.
	Until int64 `json:"until"`
}

type tricks struct {
	on    bool
	mu    sync.Mutex
	trick *Trick
	now   func() time.Time
}

// LokiOn reports whether the trick switch answers: LOKI=on.
func LokiOn() bool {
	return strings.TrimSpace(os.Getenv("LOKI")) == "on"
}

// active is the trick now, or nil; a trick past its until is dropped here.
func (t *tricks) active() *Trick {
	if t == nil || !t.on {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.trick == nil {
		return nil
	}
	if t.now().UnixMilli() >= t.trick.Until {
		t.trick = nil
		return nil
	}
	copied := *t.trick
	return &copied
}

func (p *Part) mountTricks() {
	p.tricks.on = LokiOn()
	if !p.tricks.on {
		return
	}
	// Not in p.routes: the trick switch is Loki's contract with every part
	// (api/loki.yaml), not a part's own.
	p.mux.HandleFunc("GET /v1/loki/trick", p.trickGuard(p.getTrick))
	p.mux.HandleFunc("POST /v1/loki/trick", p.trickGuard(p.setTrick))
	p.mux.HandleFunc("DELETE /v1/loki/trick", p.trickGuard(p.endTrick))
	p.Logf("LOKI=on: the trick switch answers the owner and Loki")
}

func (p *Part) trickGuard(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, ok := p.authenticate(r)
		if !ok {
			Error(w, http.StatusUnauthorized, "missing or wrong bearer token")
			return
		}
		if caller != Owner && caller != TrickCaller {
			Error(w, http.StatusForbidden, "only the owner and Loki may trick a part")
			return
		}
		handler(w, r)
	}
}

func (p *Part) getTrick(w http.ResponseWriter, _ *http.Request) {
	JSON(w, http.StatusOK, map[string]any{"trick": p.tricks.active()})
}

func (p *Part) setTrick(w http.ResponseWriter, r *http.Request) {
	var trick Trick
	if err := Decode(r, &trick); err != nil {
		Error(w, http.StatusBadRequest, err.Error())
		return
	}
	now := p.tricks.now()
	switch trick.Kind {
	case TrickHealth:
		trick.Loop, trick.DelayMs = "", 0
	case TrickLoop:
		trick.DelayMs = 0
		if !slices.ContainsFunc(p.loops, func(l *Loop) bool { return l.Name == trick.Loop }) {
			Error(w, http.StatusBadRequest, "no loop called "+trick.Loop+" here")
			return
		}
	case TrickSlow:
		trick.Loop = ""
		if trick.DelayMs < 1 || time.Duration(trick.DelayMs)*time.Millisecond > MaxTrickDelay {
			Error(w, http.StatusBadRequest, "delayMs is 1 to 10000")
			return
		}
	default:
		Error(w, http.StatusBadRequest, "kind is health, loop or slow")
		return
	}
	// A few seconds of slack for the trip from Loki.
	if trick.Until <= now.UnixMilli() || trick.Until > now.Add(MaxTrick+5*time.Second).UnixMilli() {
		Error(w, http.StatusBadRequest, "until is a time in milliseconds, in the next 10 minutes")
		return
	}
	if p.tricks.active() != nil {
		Error(w, http.StatusConflict, "a trick is already on; end it first")
		return
	}
	p.tricks.mu.Lock()
	p.tricks.trick = &trick
	p.tricks.mu.Unlock()
	p.Logf("tricked by %s: %s %s until %s", callerName(p, r), trick.Kind, trick.Loop, time.UnixMilli(trick.Until).Format(time.TimeOnly))
	JSON(w, http.StatusOK, map[string]any{"trick": trick})
}

func callerName(p *Part, r *http.Request) string {
	name, _ := p.authenticate(r)
	return name
}

func (p *Part) endTrick(w http.ResponseWriter, _ *http.Request) {
	p.tricks.mu.Lock()
	was := p.tricks.trick != nil
	p.tricks.trick = nil
	p.tricks.mu.Unlock()
	if was {
		p.Logf("trick ended")
	}
	JSON(w, http.StatusOK, map[string]any{"trick": nil})
}

// loopTricked is whether the loop's runs fail now.
func (p *Part) loopTricked(name string) bool {
	trick := p.tricks.active()
	return trick != nil && trick.Kind == TrickLoop && trick.Loop == name
}

// slowFor is how long a request to path waits first; zero for none.
func (p *Part) slowFor(path string) time.Duration {
	if path == "/health" || path == "/v1/loki/trick" {
		return 0
	}
	trick := p.tricks.active()
	if trick == nil || trick.Kind != TrickSlow {
		return 0
	}
	delay := time.Duration(trick.DelayMs) * time.Millisecond
	if left := time.UnixMilli(trick.Until).Sub(p.tricks.now()); left < delay {
		delay = left
	}
	return max(delay, 0)
}
