package huginn

// What learning keeps in huginn.db: your observations and the events they
// follow (kinds and filter fields only, never a message's text), the
// suggestions made from them, and the learning setting.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const learnSchema = `
CREATE TABLE IF NOT EXISTS observations (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	at     INTEGER NOT NULL,
	action TEXT NOT NULL,
	target TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS observations_at ON observations (at);
CREATE TABLE IF NOT EXISTS seen_events (
	key  TEXT PRIMARY KEY,
	at   INTEGER NOT NULL,
	body TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS seen_events_at ON seen_events (at);
CREATE TABLE IF NOT EXISTS suggestions (
	id    TEXT PRIMARY KEY,
	state TEXT NOT NULL,
	body  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS learn_state (
	name  TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// ErrNoSuggestion is a suggestion that isn't there.
var ErrNoSuggestion = errors.New("no such suggestion")

// Observe keeps one thing you did.
func (s *Store) Observe(ctx context.Context, o Observation) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO observations (at, action, target) VALUES (?, ?, ?)`, o.At.UnixMilli(), o.Action, o.Target)
	return err
}

// Observations is what you did since then, oldest first.
func (s *Store) Observations(ctx context.Context, since time.Time) ([]Observation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, action, target FROM observations WHERE at >= ? ORDER BY at`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Observation
	for rows.Next() {
		var o Observation
		var at int64
		if err := rows.Scan(&at, &o.Action, &o.Target); err != nil {
			return nil, err
		}
		o.At = time.UnixMilli(at).UTC()
		list = append(list, o)
	}
	return list, rows.Err()
}

// See keeps one event as learning knows it; the same event twice is kept once.
func (s *Store) See(ctx context.Context, seen Seen) error {
	body, err := json.Marshal(seen)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO seen_events (key, at, body) VALUES (?, ?, ?) ON CONFLICT (key) DO NOTHING`,
		seen.Key, seen.At.UnixMilli(), string(body))
	return err
}

// SeenSince is the events seen since then, oldest first.
func (s *Store) SeenSince(ctx context.Context, since time.Time) ([]Seen, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM seen_events WHERE at >= ? ORDER BY at`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Seen
	for rows.Next() {
		var body string
		var seen Seen
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(body), &seen) == nil {
			list = append(list, seen)
		}
	}
	return list, rows.Err()
}

// ForgetLearning removes observations and events older than before. Decided
// suggestions stay, so a pattern you said no to is never suggested again.
func (s *Store) ForgetLearning(ctx context.Context, before time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM observations WHERE at < ?`, before.UnixMilli()); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM seen_events WHERE at < ?`, before.UnixMilli())
	return err
}

// PutSuggestion writes the suggestion, new or changed.
func (s *Store) PutSuggestion(ctx context.Context, suggestion Suggestion) error {
	body, err := json.Marshal(suggestion)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO suggestions (id, state, body) VALUES (?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET state = excluded.state, body = excluded.body`, suggestion.ID, suggestion.State, string(body))
	return err
}

// Suggestions is every suggestion kept, in any state.
func (s *Store) Suggestions(ctx context.Context) ([]Suggestion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM suggestions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Suggestion
	for rows.Next() {
		var body string
		var suggestion Suggestion
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(body), &suggestion) == nil {
			list = append(list, suggestion)
		}
	}
	return list, rows.Err()
}

// Suggestion is one by its id.
func (s *Store) Suggestion(ctx context.Context, id string) (Suggestion, error) {
	var body string
	var suggestion Suggestion
	err := s.db.QueryRowContext(ctx, `SELECT body FROM suggestions WHERE id = ?`, id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return suggestion, ErrNoSuggestion
	}
	if err != nil {
		return suggestion, err
	}
	return suggestion, json.Unmarshal([]byte(body), &suggestion)
}

// LearnState reads one of learning's values into v; false when it was never set.
func (s *Store) LearnState(ctx context.Context, name string, v any) (bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM learn_state WHERE name = ?`, name).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(value), v)
}

// SetLearnState keeps one of learning's values.
func (s *Store) SetLearnState(ctx context.Context, name string, v any) error {
	value, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO learn_state (name, value) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET value = excluded.value`, name, string(value))
	return err
}
