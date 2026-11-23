// Package saga is Sága, who keeps the story the same on every device.
//
// The desktop and the phone each keep their own alerts. Sága sends this
// machine's side to Mímir, the event log on the VPS, and brings the other
// devices' side back, so either can be off and catch up when it starts
// (github.com/SebbeJohansson/mimir, docs/events.md there).
//
// For alerts, that is:
//
//   - This machine's Gjallar alerts go to Mímir as `gjallar.alert`, each
//     time something about one changes (not each time it is seen again).
//   - Another device's alerts come back as mirrors in Gjallar: the source is
//     the device's name and the key `<source>/<key>`, so the widget lists them
//     with the rest. Gjallar never pushes them (GJALLAR_QUIET): that device
//     already told you.
//   - Acknowledging, snoozing or muting a mirror here asks the device that
//     owns it to do the same (`gjallar.alert.ack|snooze|mute`). What other
//     devices ask of this machine's alerts, Sága does in Gjallar.
//
// Sága talks to Gjallar over its API only: as you (the owner token) to read
// and act on alerts, and with a sender token named after each other device
// to raise and resolve its mirrors.
//
// Routes (all but /health need a token):
//
//	GET  /health
//	GET  /v1/status   how syncing goes
//	POST /v1/sync     sync now
package saga

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
	"yggdrasil/internal/part"
)

// Name is this part's name.
const Name = "saga"

// Event kinds (docs/events.md in Mímir).
const (
	kindAlert  = "gjallar.alert"
	kindAck    = "gjallar.alert.ack"
	kindSnooze = "gjallar.alert.snooze"
	kindMute   = "gjallar.alert.mute"
)

// maxSnooze is the longest snooze Gjallar takes.
const maxSnooze = 30 * 24 * time.Hour

// AlertState is an alert as it goes to Mímir. Times are epoch millis.
type AlertState struct {
	Source       string `json:"source"`
	Key          string `json:"key"`
	Severity     string `json:"severity"`
	Title        string `json:"title"`
	Detail       string `json:"detail,omitempty"`
	Link         string `json:"link,omitempty"`
	State        string `json:"state"`
	OpenedAt     int64  `json:"openedAt"`
	LastSeenAt   int64  `json:"lastSeenAt"`
	ResolvedAt   *int64 `json:"resolvedAt,omitempty"`
	SnoozedUntil *int64 `json:"snoozedUntil,omitempty"`
	Muted        bool   `json:"muted"`
}

// fingerprint is what matters about an alert: everything but when it was
// last seen, which moves each time a part raises it again.
func (a AlertState) fingerprint() string {
	a.LastSeenAt = 0
	data, _ := json.Marshal(a)
	return string(data)
}

// request is what one device asks of another's alert.
type request struct {
	Origin string `json:"origin"`
	Source string `json:"source"`
	Key    string `json:"key"`
	Until  int64  `json:"until,omitempty"`
	Muted  *bool  `json:"muted,omitempty"`
}

func millis(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	v := t.UnixMilli()
	return &v
}

func stateOf(a gAlert) AlertState {
	return AlertState{
		Source: a.Source, Key: a.Key, Severity: string(a.Severity), Title: a.Title,
		Detail: a.Detail, Link: a.Link, State: a.State,
		OpenedAt: a.FirstSeenAt.UnixMilli(), LastSeenAt: a.LastSeenAt.UnixMilli(),
		ResolvedAt: millis(a.ResolvedAt), SnoozedUntil: millis(a.SnoozedUntil), Muted: a.Muted,
	}
}

// Saga is the running part.
type Saga struct {
	Store   *Store
	Mimir   *Mimir
	Gjallar *Gjallar
	// Device is this machine's name at Mímir ("desktop").
	Device string
	// Sindri is this machine's Sindri, whose sessions go to Mímir (sessions.go); off leaves sessions out.
	Sindri *call.Part
	// DriveFrom is the devices whose requests to this machine's sessions are carried out.
	DriveFrom []string

	now func() time.Time
	mu  sync.Mutex // one sync at a time
	// runMu: one round of requests at a time.
	runMu  sync.Mutex
	nudges chan struct{}

	statusMu   sync.Mutex
	lastSyncAt time.Time
	lastError  string
}

// New makes a Sága.
func New(store *Store, mimir *Mimir, gjallar *Gjallar, device string) *Saga {
	return &Saga{Store: store, Mimir: mimir, Gjallar: gjallar, Device: device, now: time.Now, nudges: make(chan struct{}, 1)}
}

// nudge asks for a sync soon.
func (s *Saga) nudge() {
	select {
	case s.nudges <- struct{}{}:
	default:
	}
}

// on says whether Gjallar is set up here. A laptop that only shares its
// coding sessions runs Sága without it.
func (g *Gjallar) on() bool { return g != nil && g.Owner.On() }

// Mount adds the routes and the sync loop.
func (s *Saga) Mount(p *part.Part) {
	p.Handle("GET /v1/status", s.status)
	p.Handle("POST /v1/sync", s.syncNow)
	p.Handle("GET /v1/sessions", s.sessionsView)
	p.Handle("POST /v1/sessions/{device}/requests", s.ask)
	p.Loop("sync", 30*time.Second, s.Sync)
	p.Loop("requests", 10*time.Second, s.RunRequests)
}

// isMirror says whether an alert in Gjallar is another device's.
func (s *Saga) isMirror(a gAlert) bool {
	if s.Gjallar == nil {
		return false
	}
	_, ok := s.Gjallar.Devices[a.Source]
	return ok
}

// Sync does one round: read what the other devices sent and act on it, send
// what changed here, and hand it all to Mímir.
func (s *Saga) Sync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.Mimir.on() {
		err = s.sync(ctx)
	}
	s.statusMu.Lock()
	s.lastSyncAt = s.now()
	s.lastError = ""
	if err != nil {
		s.lastError = err.Error()
	}
	s.statusMu.Unlock()
	return err
}

func (s *Saga) sync(ctx context.Context) error {
	if err := s.startCount(ctx); err != nil {
		return err
	}
	// Read first, so a mirror is current before a difference from it counts as yours.
	readErr := s.pull(ctx)
	if s.Gjallar.on() {
		if err := s.scan(ctx); err != nil {
			return err
		}
	}
	// Sessions not read (Sindri down) don't hold up the alerts.
	sessionsErr := s.scanSessions(ctx)
	if err := s.push(ctx); err != nil {
		return err
	}
	if readErr != nil {
		return readErr
	}
	return sessionsErr
}

// startCount picks up this device's numbering from Mímir the first time, so
// a fresh database never sends a seq Mímir already holds.
func (s *Saga) startCount(ctx context.Context) error {
	if _, set, err := s.Store.Mark(ctx, "seq"); err != nil || set {
		return err
	}
	last, err := s.Mimir.LastSeq(ctx)
	if err != nil {
		return err
	}
	return s.Store.SetMark(ctx, "seq", last)
}

// pull applies every other device's event after the mark.
func (s *Saga) pull(ctx context.Context) error {
	for {
		mark, _, err := s.Store.Mark(ctx, "read")
		if err != nil {
			return err
		}
		events, last, more, err := s.Mimir.Read(ctx, mark, s.Device)
		if errors.Is(err, errGone) {
			// Start again where Mímir's snapshot leaves off, or from the start.
			// Each device's newest alert state is all that matters, and it is
			// sent again whenever something about the alert changes.
			pos, err := s.Mimir.SnapshotPos(ctx)
			if err != nil {
				return err
			}
			if err := s.Store.SetMark(ctx, "read", pos); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := s.apply(ctx, events); err != nil {
			return err
		}
		if err := s.Store.SetMark(ctx, "read", last); err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

// apply acts on a page of events. Only each alert's newest state in the page
// counts. It is safe to run again: a failure leaves the mark where it was.
func (s *Saga) apply(ctx context.Context, events []Event) error {
	if err := s.applySessions(ctx, events); err != nil {
		return err
	}
	if !s.Gjallar.on() {
		return nil
	}
	type id struct{ device, source, key string }
	newest := map[id]AlertState{}
	var order []id
	alerts, err := s.Gjallar.list(ctx)
	if err != nil {
		return err
	}
	for _, e := range events {
		switch e.Kind {
		case kindAlert:
			var state AlertState
			if err := json.Unmarshal(e.Body, &state); err != nil || state.Source == "" || state.Key == "" {
				continue // not one we understand; skip it, don't stop for it
			}
			k := id{e.Device, state.Source, state.Key}
			if _, seen := newest[k]; !seen {
				order = append(order, k)
			}
			newest[k] = state
		case kindAck, kindSnooze, kindMute:
			var r request
			if err := json.Unmarshal(e.Body, &r); err != nil || r.Origin != s.Device {
				continue
			}
			if err := s.handleRequest(ctx, alerts, e.Kind, r); err != nil {
				return err
			}
		}
	}
	for _, k := range order {
		if _, known := s.Gjallar.Devices[k.device]; !known {
			continue // a device this machine has no Gjallar token for
		}
		if err := s.applyMirror(ctx, alerts, k.device, newest[k]); err != nil {
			return err
		}
	}
	return nil
}

// handleRequest does in Gjallar what another device asked of an alert here.
func (s *Saga) handleRequest(ctx context.Context, alerts []gAlert, kind string, r request) error {
	var target *gAlert
	for i := range alerts {
		if alerts[i].Source == r.Source && alerts[i].Key == r.Key && !s.isMirror(alerts[i]) {
			target = &alerts[i]
		}
	}
	switch kind {
	case kindMute:
		if r.Muted == nil {
			return nil
		}
		// A mute is kept by source and key, even with no alert open.
		return s.Gjallar.mute(ctx, r.Source, r.Key, *r.Muted)
	case kindAck:
		if target == nil || target.State != "open" {
			return nil
		}
		return s.Gjallar.ack(ctx, target.ID)
	case kindSnooze:
		until := time.UnixMilli(r.Until)
		now := s.now()
		if target == nil || target.State == "resolved" || !until.After(now) {
			return nil
		}
		if until.Sub(now) > maxSnooze {
			until = now.Add(maxSnooze)
		}
		return s.Gjallar.snooze(ctx, target.ID, until)
	}
	return nil
}

func mirrorKey(state AlertState) string { return state.Source + "/" + state.Key }

func clip(text string, max int) string {
	if utf8.RuneCountInString(text) <= max {
		return text
	}
	return string([]rune(text)[:max-1]) + "…"
}

func findMirror(alerts []gAlert, device, key string) *gAlert {
	for i := range alerts {
		if alerts[i].Source == device && alerts[i].Key == key {
			return &alerts[i]
		}
	}
	return nil
}

// applyMirror makes Gjallar's mirror of another device's alert match it.
func (s *Saga) applyMirror(ctx context.Context, alerts []gAlert, device string, want AlertState) error {
	key := mirrorKey(want)
	have := findMirror(alerts, device, key)
	before, err := s.Store.mirror(ctx, device, want.Source, want.Key)
	if err != nil {
		return err
	}
	if want.State == "resolved" {
		if have != nil && have.State != "resolved" {
			if err := s.Gjallar.resolveAs(ctx, device, key); err != nil {
				return err
			}
		}
	} else {
		raise := alert.Raise{
			Key: key, Severity: alert.Severity(want.Severity), Title: clip(want.Title, 200),
			Detail: clip(want.Detail, 2000), Link: want.Link,
		}
		if !raise.Severity.Valid() {
			raise.Severity = alert.Warning
		}
		if raise.Title == "" {
			raise.Title = want.Key
		}
		// Opened again on the device (resolved, then raised) while we were
		// away: close the mirror first, so Gjallar opens it again too.
		reopened := before != nil && before.OpenedAt != want.OpenedAt && have != nil && have.State != "resolved"
		if reopened {
			if err := s.Gjallar.resolveAs(ctx, device, key); err != nil {
				return err
			}
		}
		changed := have == nil || reopened || have.State == "resolved" || string(have.Severity) != string(raise.Severity) ||
			have.Title != raise.Title || have.Detail != raise.Detail || have.Link != raise.Link
		if changed {
			if err := s.Gjallar.raiseAs(ctx, device, raise); err != nil {
				return err
			}
			if alerts, err = s.Gjallar.list(ctx); err != nil {
				return err
			}
			have = findMirror(alerts, device, key)
		}
		if have != nil && want.State == "acknowledged" && have.State == "open" {
			if err := s.Gjallar.ack(ctx, have.ID); err != nil {
				return err
			}
		}
		if have != nil && want.SnoozedUntil != nil {
			until := time.UnixMilli(*want.SnoozedUntil)
			now := s.now()
			if until.After(now) && (have.SnoozedUntil == nil || !have.SnoozedUntil.Equal(until)) && until.Sub(now) <= maxSnooze {
				if err := s.Gjallar.snooze(ctx, have.ID, until); err != nil {
					return err
				}
			}
		}
	}
	if (have == nil && want.Muted) || (have != nil && have.Muted != want.Muted) {
		if err := s.Gjallar.mute(ctx, device, key, want.Muted); err != nil {
			return err
		}
	}
	return s.Store.tx(ctx, func(tx *sql.Tx) error { return setMirror(ctx, tx, device, want) })
}

// scan finds what changed in Gjallar: this machine's alerts to send, and
// what you did here to a mirror, to ask of the device that owns it.
func (s *Saga) scan(ctx context.Context) error {
	alerts, err := s.Gjallar.list(ctx)
	if err != nil {
		return err
	}
	mirrors, err := s.Store.Mirrors(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	return s.Store.tx(ctx, func(tx *sql.Tx) error {
		for _, a := range alerts {
			if s.isMirror(a) {
				continue
			}
			state := stateOf(a)
			fingerprint := state.fingerprint()
			was, err := published(ctx, tx, a.Source, a.Key)
			if err != nil {
				return err
			}
			if was == fingerprint {
				continue
			}
			if err := enqueue(ctx, tx, now.UnixMilli(), kindAlert, state); err != nil {
				return err
			}
			if err := setPublished(ctx, tx, a.Source, a.Key, fingerprint); err != nil {
				return err
			}
		}
		for _, m := range mirrors {
			have := findMirror(alerts, m.Device, mirrorKey(m.State))
			if have == nil || m.State.State == "resolved" {
				continue
			}
			if err := s.askFor(ctx, tx, now, m, *have); err != nil {
				return err
			}
		}
		return nil
	})
}

// askFor sends the requests a mirror's difference from its device calls for,
// each once until the device answers with a new state.
func (s *Saga) askFor(ctx context.Context, tx *sql.Tx, now time.Time, m Mirror, have gAlert) error {
	want := m.State
	ask := func(kind, value string, r request) error {
		was, err := requestedValue(ctx, tx, m.Device, want.Source, want.Key, kind)
		if err != nil || was == value {
			return err
		}
		r.Origin, r.Source, r.Key = m.Device, want.Source, want.Key
		if err := enqueue(ctx, tx, now.UnixMilli(), kind, r); err != nil {
			return err
		}
		return setRequested(ctx, tx, m.Device, want.Source, want.Key, kind, value)
	}
	if have.State == "acknowledged" && want.State == "open" {
		if err := ask(kindAck, fmt.Sprintf("%d", want.OpenedAt), request{}); err != nil {
			return err
		}
	}
	if have.SnoozedUntil != nil && have.SnoozedUntil.After(now) {
		until := have.SnoozedUntil.UnixMilli()
		if want.SnoozedUntil == nil || until > *want.SnoozedUntil+1000 {
			if err := ask(kindSnooze, fmt.Sprintf("%d", until), request{Until: until}); err != nil {
				return err
			}
		}
	}
	if have.Muted != want.Muted {
		muted := have.Muted
		if err := ask(kindMute, fmt.Sprintf("%t", muted), request{Muted: &muted}); err != nil {
			return err
		}
	}
	return nil
}

// push hands the outbox to Mímir, 500 at a time.
func (s *Saga) push(ctx context.Context) error {
	for {
		waiting, err := s.Store.Outbox(ctx, 500)
		if err != nil || len(waiting) == 0 {
			return err
		}
		lastSeq, err := s.Mimir.Append(ctx, waiting)
		var gap *gapError
		if errors.As(err, &gap) {
			if gap.lastSeq >= waiting[0].Seq {
				// It holds some of these already; drop them and send the rest.
				if err := s.Store.Sent(ctx, gap.lastSeq); err != nil {
					return err
				}
				continue
			}
			// Mímir lost our newest events (a restore): follow on from what it has.
			if err := s.Store.Renumber(ctx, gap.lastSeq); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := s.Store.Sent(ctx, lastSeq); err != nil {
			return err
		}
		if len(waiting) < 500 {
			return nil
		}
	}
}

// Status is how syncing goes.
type Status struct {
	Device     string     `json:"device"`
	MimirURL   string     `json:"mimirUrl"`
	LastSyncAt *time.Time `json:"lastSyncAt,omitempty"`
	LastError  string     `json:"lastError,omitempty"`
	Read       int64      `json:"read"`
	Waiting    int        `json:"waiting"`
	Mirrors    int        `json:"mirrors"`
	// Alerts and Sessions say what this machine syncs: Gjallar's alerts, Sindri's sessions.
	Alerts    bool     `json:"alerts"`
	Sessions  bool     `json:"sessions"`
	DriveFrom []string `json:"driveFrom"`
}

func (s *Saga) currentStatus(ctx context.Context) (Status, error) {
	status := Status{Device: s.Device, MimirURL: s.Mimir.URL}
	s.statusMu.Lock()
	if !s.lastSyncAt.IsZero() {
		at := s.lastSyncAt
		status.LastSyncAt = &at
	}
	status.LastError = s.lastError
	s.statusMu.Unlock()
	status.Alerts = s.Gjallar.on()
	status.Sessions = s.Sindri.On()
	status.DriveFrom = s.DriveFrom
	if status.DriveFrom == nil {
		status.DriveFrom = []string{}
	}
	if !s.Mimir.on() {
		// Not an error on /health: Sága simply waits to be set up.
		status.LastError = "Mímir is not set up: set MIMIR_URL and MIMIR_TOKEN in saga.env"
	}
	var err error
	if status.Read, _, err = s.Store.Mark(ctx, "read"); err != nil {
		return status, err
	}
	if status.Waiting, err = s.Store.OutboxLen(ctx); err != nil {
		return status, err
	}
	mirrors, err := s.Store.Mirrors(ctx)
	for _, m := range mirrors {
		if m.State.State != "resolved" {
			status.Mirrors++
		}
	}
	return status, err
}

func (s *Saga) status(w http.ResponseWriter, r *http.Request) {
	status, err := s.currentStatus(r.Context())
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, status)
}

func (s *Saga) syncNow(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only the owner may do this")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	_ = s.Sync(ctx) // the error is in the status
	s.status(w, r)
}

// Nudge runs a sync soon: something changed in Gjallar or at Mímir. Nudges
// that come together make one sync.
func (s *Saga) Nudge(ctx context.Context, nudges <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-nudges:
		}
		// Let a burst settle.
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		for drained := false; !drained; {
			select {
			case <-nudges:
			default:
				drained = true
			}
		}
		syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_ = s.Sync(syncCtx) // a failure is in the status, and the loop tries again
		cancel()
	}
}
