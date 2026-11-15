// Package watch turns repeated checks into alerts without flapping: a check
// must fail twice in a row before it is down, and pass twice in a row before
// it is up again. Ratatoskr and Munin both use it.
//
// Sync raises and resolves through Gjallar, and keeps trying on the next
// round when Gjallar did not take it, so an alert is never lost because
// Gjallar was briefly away.
package watch

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/alert"
)

// Result is one check's outcome this round.
type Result struct {
	Key      string
	OK       bool
	Severity alert.Severity
	// Title says what is wrong, for the alert. Detail is the error.
	Title  string
	Detail string
	// Skip leaves the check as it was: it was not run this round (for
	// Munin, an internet check while the internet is down).
	Skip bool
}

// State is one check as it stands.
type State struct {
	Key       string         `json:"key"`
	Down      bool           `json:"down"`
	Severity  alert.Severity `json:"severity,omitempty"`
	Title     string         `json:"title,omitempty"`
	Detail    string         `json:"detail,omitempty"`
	Fails     int            `json:"fails"`
	Passes    int            `json:"passes"`
	Since     time.Time      `json:"since"`
	CheckedAt time.Time      `json:"checkedAt"`
	// Raised is whether Gjallar holds an open alert for it.
	Raised bool `json:"raised"`

	gone bool
}

// Watch holds every check's state.
type Watch struct {
	FailAfter int
	PassAfter int
	// OutOfBand names checks never sent to Gjallar: Ratatoskr's check of
	// Gjallar itself, which it reports another way.
	OutOfBand func(key string) bool

	mu     sync.Mutex
	states map[string]*State
	now    func() time.Time
}

// New makes a watch that goes down after 2 failures and up after 2 passes.
func New() *Watch {
	return &Watch{FailAfter: 2, PassAfter: 2, states: map[string]*State{}, now: time.Now}
}

// Observe records this round's results.
func (w *Watch) Observe(results []Result) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	for _, result := range results {
		if result.Skip {
			continue
		}
		state, known := w.states[result.Key]
		if !known {
			state = &State{Key: result.Key, Since: now}
			w.states[result.Key] = state
		}
		state.gone = false
		state.CheckedAt = now
		if result.OK {
			state.Fails = 0
			state.Passes++
			if state.Down && state.Passes >= w.PassAfter {
				state.Down = false
				state.Since = now
			}
			continue
		}
		state.Passes = 0
		state.Fails++
		state.Severity, state.Title, state.Detail = result.Severity, result.Title, result.Detail
		if !state.Down && state.Fails >= w.FailAfter {
			state.Down = true
			state.Since = now
		}
	}
}

// Forget drops checks that are no longer run, so a target taken out of the
// settings does not stay down forever. Their open alerts are resolved by
// the next Sync.
func (w *Watch) Forget(keep func(key string) bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for key, state := range w.states {
		if !keep(key) {
			state.Down = false
			state.gone = true
		}
	}
}

// Sync raises every down check Gjallar does not hold yet, and resolves every
// check that came back. It returns the first error; the rest are tried anyway.
func (w *Watch) Sync(ctx context.Context, client *alert.Client) error {
	var first error
	for _, state := range w.States() {
		if w.OutOfBand != nil && w.OutOfBand(state.Key) {
			continue
		}
		var err error
		switch {
		case state.Down && !state.Raised:
			err = client.Raise(ctx, alert.Raise{Key: state.Key, Severity: state.Severity, Title: state.Title, Detail: state.Detail})
		case !state.Down && state.Raised:
			err = client.Resolve(ctx, state.Key)
		default:
			continue
		}
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		w.mu.Lock()
		if current := w.states[state.Key]; current != nil {
			current.Raised = state.Down
		}
		w.mu.Unlock()
	}
	w.mu.Lock()
	for key, state := range w.states {
		if state.gone && !state.Raised {
			delete(w.states, key)
		}
	}
	w.mu.Unlock()
	return first
}

// Get is one check's state.
func (w *Watch) Get(key string) (State, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	state, ok := w.states[key]
	if !ok {
		return State{}, false
	}
	return *state, true
}

// States is every check, the down ones first, then by key.
func (w *Watch) States() []State {
	w.mu.Lock()
	defer w.mu.Unlock()
	list := make([]State, 0, len(w.states))
	for _, state := range w.states {
		list = append(list, *state)
	}
	slices.SortFunc(list, func(a, b State) int {
		if a.Down != b.Down {
			if a.Down {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Key, b.Key)
	})
	return list
}
