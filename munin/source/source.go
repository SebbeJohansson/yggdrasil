// Package source is what a Munin source is: a module for one outside system
// that needs more than Munin's plain targets (a sign-in, an alert feed, its
// own idea of healthy). Each lives in its own package under munin/ and is
// turned on per machine by naming it in MUNIN_SOURCES; one not named does
// not run, keeps no state, and shows nowhere.
//
// Munin owns everything shared: the loop (so it shows on /health and
// Ratatoskr notices a stall), flap damping of checks, raising on Gjallar,
// skipping a round while the machine is offline, and keeping a feed's cursor
// across restarts. A source only fetches and says what it saw.
package source

import (
	"context"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/watch"
)

// Source is one module.
type Source interface {
	// Name is the name in MUNIN_SOURCES, its loop on /health, and the start
	// of every key it raises on Gjallar.
	Name() string
	// Every is how often Poll runs.
	Every() time.Duration
	// Poll checks the system once. cursor is the Cursor of the last Report
	// whose events all reached Gjallar ("" the first time ever).
	Poll(ctx context.Context, cursor string) Report
}

// Report is one Poll's findings.
type Report struct {
	// Checks go through Munin's flap damping (down after 2 failures, up after
	// 2 passes). Keys are the source's own: Munin puts "<name>:" in front.
	Checks []watch.Result
	// Events are alerts the system raised itself, raised or resolved on
	// Gjallar as they come, in order, without damping.
	Events []Event
	// Cursor is where the event feed got to. Munin keeps it once Gjallar has
	// taken every event, and hands it to the next Poll; "" keeps the old one.
	Cursor string
	// Up and Detail are the one-line summary for the dashboard.
	Up     bool
	Detail string
	// Data is anything else the dashboard may show, served as is.
	Data any
}

// Event is one alert from the system's own feed.
type Event struct {
	// Key names the condition in the system's feed. Munin raises it on
	// Gjallar as "feed:<source>:<key>", so a later Resolved event with the
	// same key closes it.
	Key      string
	Resolved bool
	Severity alert.Severity
	Title    string
	Detail   string
	Link     string
	// ExpiresAt closes the alert on its own when nothing resolves it.
	ExpiresAt *time.Time
}

// Settings reads one setting of the source: Setting("URL") is
// MUNIN_<NAME>_URL.
type Settings func(name string) string

// Factory makes a source from its settings. It fails only when a setting is
// wrong, not when one is missing that the source can do without.
type Factory func(settings Settings) (Source, error)
