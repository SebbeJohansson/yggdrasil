package huginn

// What the gossip keeps in huginn.db: the last GossipKeep lines, which events
// were already queued (so a restart tells nothing twice), and its setting.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const gossipTables = `
CREATE TABLE IF NOT EXISTS gossip_lines (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	exchange TEXT NOT NULL,
	at       INTEGER NOT NULL,
	speaker  TEXT NOT NULL,
	text     TEXT NOT NULL,
	about    TEXT NOT NULL,
	canned   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS gossip_lines_at ON gossip_lines (at);
CREATE TABLE IF NOT EXISTS gossip_told (
	ref TEXT PRIMARY KEY,
	at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS gossip_state (
	name  TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// AddGossip keeps one exchange's lines, in order.
func (s *Store) AddGossip(ctx context.Context, lines []GossipLine) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, line := range lines {
		about, _ := json.Marshal(line.About)
		if _, err := tx.ExecContext(ctx, `INSERT INTO gossip_lines (exchange, at, speaker, text, about, canned) VALUES (?, ?, ?, ?, ?, ?)`,
			line.Exchange, line.At.UnixMilli(), line.Speaker, line.Text, string(about), line.Canned); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Gossip is the lines after the id given, oldest first, at most limit; with
// since 0, the last limit lines.
func (s *Store) Gossip(ctx context.Context, since int64, limit int) ([]GossipLine, error) {
	query := `SELECT id, exchange, at, speaker, text, about, canned FROM gossip_lines WHERE id > ? ORDER BY id LIMIT ?`
	if since == 0 {
		query = `SELECT * FROM (SELECT id, exchange, at, speaker, text, about, canned FROM gossip_lines WHERE id > ? ORDER BY id DESC LIMIT ?) ORDER BY id`
	}
	rows, err := s.db.QueryContext(ctx, query, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lines := []GossipLine{}
	for rows.Next() {
		var line GossipLine
		var at int64
		var about string
		if err := rows.Scan(&line.ID, &line.Exchange, &at, &line.Speaker, &line.Text, &about, &line.Canned); err != nil {
			return nil, err
		}
		line.At = time.UnixMilli(at).UTC()
		line.Name = PartName(line.Speaker)
		_ = json.Unmarshal([]byte(about), &line.About)
		if line.About == nil {
			line.About = []string{}
		}
		lines = append(lines, line)
	}
	return lines, rows.Err()
}

// GossipExchanges is when the last exchange was, and how many there were since then.
func (s *Store) GossipExchanges(ctx context.Context, since time.Time) (time.Time, int, error) {
	var last sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(at) FROM gossip_lines`).Scan(&last); err != nil {
		return time.Time{}, 0, err
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT exchange) FROM gossip_lines WHERE at >= ?`, since.UnixMilli()).Scan(&count); err != nil {
		return time.Time{}, 0, err
	}
	if !last.Valid {
		return time.Time{}, count, nil
	}
	return time.UnixMilli(last.Int64), count, nil
}

// TrimGossip keeps the newest lines only, and forgets marks older than a week.
func (s *Store) TrimGossip(ctx context.Context, keep int) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM gossip_lines WHERE id NOT IN (SELECT id FROM gossip_lines ORDER BY id DESC LIMIT ?)`, keep); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM gossip_told WHERE at < ?`, time.Now().Add(-7*24*time.Hour).UnixMilli())
	return err
}

// GossipTold is whether the event was queued before.
func (s *Store) GossipTold(ctx context.Context, ref string) (bool, error) {
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT at FROM gossip_told WHERE ref = ?`, ref).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// TellGossip marks the event as queued.
func (s *Store) TellGossip(ctx context.Context, ref string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO gossip_told (ref, at) VALUES (?, ?) ON CONFLICT (ref) DO NOTHING`, ref, at.UnixMilli())
	return err
}

// GossipState reads one of the gossip's values into v; false when it was never set.
func (s *Store) GossipState(ctx context.Context, name string, v any) (bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM gossip_state WHERE name = ?`, name).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(value), v)
}

// SetGossipState keeps one of the gossip's values.
func (s *Store) SetGossipState(ctx context.Context, name string, v any) error {
	value, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO gossip_state (name, value) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET value = excluded.value`, name, string(value))
	return err
}
