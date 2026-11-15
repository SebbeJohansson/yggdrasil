package gjallar

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"yggdrasil/internal/alert"
)

// State is where an alert is in its life.
type State string

const (
	Open         State = "open"
	Acknowledged State = "acknowledged"
	Resolved     State = "resolved"
)

// Alert is one problem, identified by its source and key.
type Alert struct {
	ID             int64          `json:"id"`
	Source         string         `json:"source"`
	Key            string         `json:"key"`
	Severity       alert.Severity `json:"severity"`
	Title          string         `json:"title"`
	Detail         string         `json:"detail,omitempty"`
	Link           string         `json:"link,omitempty"`
	TraceID        string         `json:"traceId,omitempty"`
	State          State          `json:"state"`
	Count          int            `json:"count"`
	FirstSeenAt    time.Time      `json:"firstSeenAt"`
	LastSeenAt     time.Time      `json:"lastSeenAt"`
	AcknowledgedAt *time.Time     `json:"acknowledgedAt,omitempty"`
	ResolvedAt     *time.Time     `json:"resolvedAt,omitempty"`
	SnoozedUntil   *time.Time     `json:"snoozedUntil,omitempty"`
	ExpiresAt      *time.Time     `json:"expiresAt,omitempty"`
	Muted          bool           `json:"muted"`

	pushedAt *time.Time
}

// Event is one step in an alert's history.
type Event struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	By   string    `json:"by"`
	Note string    `json:"note,omitempty"`
}

// Change is what a raise did, which decides whether to notify.
type Change string

const (
	Opened    Change = "opened"
	Reopened  Change = "reopened"
	Escalated Change = "escalated"
	Updated   Change = "updated"
)

// Store keeps alerts in SQLite.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS alerts (
	id INTEGER PRIMARY KEY,
	source TEXT NOT NULL,
	key TEXT NOT NULL,
	severity TEXT NOT NULL,
	title TEXT NOT NULL,
	detail TEXT NOT NULL DEFAULT '',
	link TEXT NOT NULL DEFAULT '',
	trace_id TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL,
	count INTEGER NOT NULL,
	first_seen INTEGER NOT NULL,
	last_seen INTEGER NOT NULL,
	acked_at INTEGER,
	resolved_at INTEGER,
	snoozed_until INTEGER,
	expires_at INTEGER,
	pushed_at INTEGER,
	UNIQUE (source, key)
);
CREATE INDEX IF NOT EXISTS alerts_state ON alerts (state, last_seen);
CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY,
	alert_id INTEGER NOT NULL REFERENCES alerts (id),
	at INTEGER NOT NULL,
	kind TEXT NOT NULL,
	by TEXT NOT NULL,
	note TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS events_alert ON events (alert_id, at);
CREATE TABLE IF NOT EXISTS mutes (
	source TEXT NOT NULL,
	key TEXT NOT NULL,
	at INTEGER NOT NULL,
	PRIMARY KEY (source, key)
);
CREATE TABLE IF NOT EXISTS heartbeats (
	source TEXT PRIMARY KEY,
	at INTEGER NOT NULL
);
`

// OpenStore opens (and creates) the database at path. ":memory:" is fine for tests.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// One connection: writes are tiny, and it keeps ":memory:" one database.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("gjallar schema: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

func ms(t time.Time) int64 {
	return t.UnixMilli()
}

func optional(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func fromMs(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	t := time.UnixMilli(value.Int64)
	return &t
}

const columns = `a.id, a.source, a.key, a.severity, a.title, a.detail, a.link, a.trace_id, a.state, a.count,
	a.first_seen, a.last_seen, a.acked_at, a.resolved_at, a.snoozed_until, a.expires_at, a.pushed_at,
	EXISTS (SELECT 1 FROM mutes m WHERE m.source = a.source AND m.key = a.key)`

type scanner interface {
	Scan(dest ...any) error
}

func scanAlert(row scanner) (Alert, error) {
	var a Alert
	var first, last int64
	var acked, resolved, snoozed, expires, pushed sql.NullInt64
	err := row.Scan(&a.ID, &a.Source, &a.Key, &a.Severity, &a.Title, &a.Detail, &a.Link, &a.TraceID, &a.State, &a.Count,
		&first, &last, &acked, &resolved, &snoozed, &expires, &pushed, &a.Muted)
	if err != nil {
		return a, err
	}
	a.FirstSeenAt, a.LastSeenAt = time.UnixMilli(first), time.UnixMilli(last)
	a.AcknowledgedAt, a.ResolvedAt, a.SnoozedUntil, a.ExpiresAt, a.pushedAt = fromMs(acked), fromMs(resolved), fromMs(snoozed), fromMs(expires), fromMs(pushed)
	return a, nil
}

// ErrNotFound is returned for an alert id that does not exist.
var ErrNotFound = errors.New("no such alert")

// Get is one alert by id.
func (s *Store) Get(ctx context.Context, id int64) (Alert, error) {
	a, err := scanAlert(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM alerts a WHERE a.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

func (s *Store) byKey(ctx context.Context, tx *sql.Tx, source, key string) (Alert, error) {
	return scanAlert(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM alerts a WHERE a.source = ? AND a.key = ?`, source, key))
}

func event(ctx context.Context, tx *sql.Tx, id int64, at time.Time, kind, by, note string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO events (alert_id, at, kind, by, note) VALUES (?, ?, ?, ?, ?)`, id, ms(at), kind, by, note)
	return err
}

// Raise opens the alert, or updates the one with the same source and key.
// A resolved alert opens again with its history kept, and an acknowledged one
// opens again when its severity goes up.
func (s *Store) Raise(ctx context.Context, source string, raise alert.Raise) (Alert, Change, error) {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Alert{}, "", err
	}
	defer tx.Rollback()
	existing, err := s.byKey(ctx, tx, source, raise.Key)
	var change Change
	switch {
	case errors.Is(err, sql.ErrNoRows):
		change = Opened
		result, err := tx.ExecContext(ctx, `INSERT INTO alerts (source, key, severity, title, detail, link, trace_id, state, count, first_seen, last_seen, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
			source, raise.Key, raise.Severity, raise.Title, raise.Detail, raise.Link, raise.TraceID, Open, ms(now), ms(now), optional(raise.ExpiresAt))
		if err != nil {
			return Alert{}, "", err
		}
		existing.ID, _ = result.LastInsertId()
	case err != nil:
		return Alert{}, "", err
	default:
		state := existing.State
		change = Updated
		switch {
		case existing.State == Resolved:
			change, state = Reopened, Open
		case raise.Severity.Rank() > existing.Severity.Rank():
			change, state = Escalated, Open
		}
		acked := existing.AcknowledgedAt
		if state == Open {
			acked = nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE alerts SET severity = ?, title = ?, detail = ?, link = ?, trace_id = ?, state = ?, count = count + 1,
			last_seen = ?, acked_at = ?, resolved_at = CASE WHEN ? = 'open' THEN NULL ELSE resolved_at END, expires_at = ? WHERE id = ?`,
			raise.Severity, raise.Title, raise.Detail, raise.Link, raise.TraceID, state, ms(now), optional(acked), state, optional(raise.ExpiresAt), existing.ID)
		if err != nil {
			return Alert{}, "", err
		}
	}
	if change != Updated {
		if err := event(ctx, tx, existing.ID, now, string(change), source, string(raise.Severity)); err != nil {
			return Alert{}, "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return Alert{}, "", err
	}
	updated, err := s.Get(ctx, existing.ID)
	return updated, change, err
}

// Resolve closes the source's open alert with that key. found is false when
// there was none open, which is not an error: senders resolve again and again.
func (s *Store) Resolve(ctx context.Context, source, key, by string) (Alert, bool, error) {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Alert{}, false, err
	}
	defer tx.Rollback()
	existing, err := s.byKey(ctx, tx, source, key)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && existing.State == Resolved) {
		return existing, false, nil
	}
	if err != nil {
		return Alert{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE alerts SET state = ?, resolved_at = ? WHERE id = ?`, Resolved, ms(now), existing.ID); err != nil {
		return Alert{}, false, err
	}
	if err := event(ctx, tx, existing.ID, now, "resolved", by, ""); err != nil {
		return Alert{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Alert{}, false, err
	}
	updated, err := s.Get(ctx, existing.ID)
	return updated, true, err
}

// ErrResolved is returned when acknowledging or snoozing a resolved alert.
var ErrResolved = errors.New("the alert is resolved")

// Acknowledge marks an open alert as seen by you.
func (s *Store) Acknowledge(ctx context.Context, id int64, by string) (Alert, error) {
	return s.ownerChange(ctx, id, by, "acknowledged", "", `UPDATE alerts SET state = 'acknowledged', acked_at = ? WHERE id = ?`, ms(s.now()), id)
}

// Snooze keeps an alert quiet until the time given.
func (s *Store) Snooze(ctx context.Context, id int64, until time.Time, by string) (Alert, error) {
	return s.ownerChange(ctx, id, by, "snoozed", until.UTC().Format(time.RFC3339), `UPDATE alerts SET snoozed_until = ? WHERE id = ?`, ms(until), id)
}

func (s *Store) ownerChange(ctx context.Context, id int64, by, kind, note, query string, args ...any) (Alert, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Alert{}, err
	}
	defer tx.Rollback()
	existing, err := scanAlert(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM alerts a WHERE a.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Alert{}, ErrNotFound
	}
	if err != nil {
		return Alert{}, err
	}
	if existing.State == Resolved {
		return Alert{}, ErrResolved
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return Alert{}, err
	}
	if err := event(ctx, tx, id, s.now(), kind, by, note); err != nil {
		return Alert{}, err
	}
	if err := tx.Commit(); err != nil {
		return Alert{}, err
	}
	return s.Get(ctx, id)
}

// Filter picks alerts for List.
type Filter string

const (
	// Active is open and acknowledged: everything not yet resolved.
	Active Filter = "active"
	All    Filter = "all"
)

// List is up to limit alerts, the most recently seen first.
func (s *Store) List(ctx context.Context, filter Filter, limit int) ([]Alert, error) {
	where, args := "", []any{}
	switch filter {
	case Active:
		where, args = `WHERE a.state != ?`, []any{Resolved}
	case All:
	default:
		where, args = `WHERE a.state = ?`, []any{string(filter)}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM alerts a `+where+` ORDER BY a.last_seen DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	alerts := []Alert{}
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		alerts = append(alerts, a)
	}
	return alerts, rows.Err()
}

// Events is an alert's history, oldest first.
func (s *Store) Events(ctx context.Context, id int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, kind, by, note FROM events WHERE alert_id = ? ORDER BY at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&at, &e.Kind, &e.By, &e.Note); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at)
		events = append(events, e)
	}
	return events, rows.Err()
}

// Expired is every unresolved alert whose expiresAt has passed.
func (s *Store) Expired(ctx context.Context) ([]Alert, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM alerts a WHERE a.state != ? AND a.expires_at IS NOT NULL AND a.expires_at <= ?`, Resolved, ms(s.now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var alerts []Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		alerts = append(alerts, a)
	}
	return alerts, rows.Err()
}

// MarkPushed records that a push went out for the alert.
func (s *Store) MarkPushed(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE alerts SET pushed_at = ? WHERE id = ?`, ms(s.now()), id)
	return err
}

// Mute is one muted source and key.
type Mute struct {
	Source string    `json:"source"`
	Key    string    `json:"key"`
	At     time.Time `json:"at"`
}

// SetMute mutes or unmutes a source and key. A muted alert is still kept and
// listed, but never pushed.
func (s *Store) SetMute(ctx context.Context, source, key string, muted bool) error {
	if muted {
		_, err := s.db.ExecContext(ctx, `INSERT INTO mutes (source, key, at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, source, key, ms(s.now()))
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM mutes WHERE source = ? AND key = ?`, source, key)
	return err
}

// Mutes is every mute, newest first.
func (s *Store) Mutes(ctx context.Context) ([]Mute, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source, key, at FROM mutes ORDER BY at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	mutes := []Mute{}
	for rows.Next() {
		var m Mute
		var at int64
		if err := rows.Scan(&m.Source, &m.Key, &at); err != nil {
			return nil, err
		}
		m.At = time.UnixMilli(at)
		mutes = append(mutes, m)
	}
	return mutes, rows.Err()
}

// Beat records a heartbeat from source.
func (s *Store) Beat(ctx context.Context, source string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO heartbeats (source, at) VALUES (?, ?) ON CONFLICT (source) DO UPDATE SET at = excluded.at`, source, ms(s.now()))
	return err
}

// LastBeat is when source last sent a heartbeat; zero when it never has.
func (s *Store) LastBeat(ctx context.Context, source string) (time.Time, error) {
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT at FROM heartbeats WHERE source = ?`, source).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.UnixMilli(at), nil
}

// ParseFilter reads ?state=: active (the default), open, acknowledged,
// resolved or all.
func ParseFilter(text string) (Filter, error) {
	switch strings.TrimSpace(text) {
	case "", "active":
		return Active, nil
	case "all":
		return All, nil
	case string(Open), string(Acknowledged), string(Resolved):
		return Filter(text), nil
	}
	return "", fmt.Errorf("state must be active, open, acknowledged, resolved or all, not %q", text)
}
