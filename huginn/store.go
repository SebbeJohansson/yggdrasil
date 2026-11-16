package huginn

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS rules (
	id   TEXT PRIMARY KEY,
	body TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS reactions (
	id    TEXT PRIMARY KEY,
	at    INTEGER NOT NULL,
	state TEXT NOT NULL,
	body  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS reactions_at ON reactions (at);
`

// Store keeps the rules and the last week of reactions.
type Store struct {
	db *sql.DB
}

// ErrNotFound is a rule that isn't there.
var ErrNotFound = errors.New("no such rule")

// OpenStore opens (or makes) the SQLite file.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("huginn schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Rules is every rule kept, in no order.
func (s *Store) Rules(ctx context.Context) ([]Rule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM rules`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []Rule
	for rows.Next() {
		var body string
		var rule Rule
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(body), &rule) == nil {
			rules = append(rules, rule)
		}
	}
	return rules, rows.Err()
}

// PutRule writes the rule, new or changed.
func (s *Store) PutRule(ctx context.Context, rule Rule) error {
	body, err := json.Marshal(rule)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO rules (id, body) VALUES (?, ?) ON CONFLICT (id) DO UPDATE SET body = excluded.body`, rule.ID, string(body))
	return err
}

// DeleteRule removes it.
func (s *Store) DeleteRule(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM rules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PutReaction writes the reaction, new or changed.
func (s *Store) PutReaction(ctx context.Context, reaction Reaction) error {
	body, err := json.Marshal(reaction)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO reactions (id, at, state, body) VALUES (?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET state = excluded.state, body = excluded.body`,
		reaction.ID, reaction.At.UnixMilli(), reaction.State, string(body))
	return err
}

// Reactions is the reactions in the state given ("" for all), newest first.
func (s *Store) Reactions(ctx context.Context, state string, limit int) ([]Reaction, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM reactions WHERE ? = '' OR state = ? ORDER BY at DESC LIMIT ?`, state, state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reactions := []Reaction{}
	for rows.Next() {
		var body string
		var reaction Reaction
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(body), &reaction) == nil {
			reactions = append(reactions, reaction)
		}
	}
	return reactions, rows.Err()
}

// Forget removes reactions older than before, except held ones.
func (s *Store) Forget(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM reactions WHERE at < ? AND state != 'held'`, before.UnixMilli())
	return err
}
