package saga

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

// Store is Sága's own state: what waits to go to Mímir, how far it has read,
// what it last told Mímir about each desktop alert, and the newest state each
// other device sent of its alerts.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS outbox (
	seq INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	kind TEXT NOT NULL,
	body TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS marks (name TEXT PRIMARY KEY, value INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS published (
	source TEXT NOT NULL, key TEXT NOT NULL, fingerprint TEXT NOT NULL,
	PRIMARY KEY (source, key)
);
CREATE TABLE IF NOT EXISTS mirrors (
	device TEXT NOT NULL, source TEXT NOT NULL, key TEXT NOT NULL, body TEXT NOT NULL,
	PRIMARY KEY (device, source, key)
);
CREATE TABLE IF NOT EXISTS requested (
	device TEXT NOT NULL, source TEXT NOT NULL, key TEXT NOT NULL, kind TEXT NOT NULL, value TEXT NOT NULL,
	PRIMARY KEY (device, source, key, kind)
);
`

// OpenStore opens (and creates) the database at path. ":memory:" is fine for tests.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("saga schema: %w", err)
	}
	if _, err := db.Exec(sessionsSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("saga sessions schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Mark is a stored number: "read" (the last Mímir pos applied) or "seq"
// (the last seq given to an event of this device). Zero when unset.
func (s *Store) Mark(ctx context.Context, name string) (int64, bool, error) {
	var v int64
	err := s.db.QueryRowContext(ctx, `SELECT value FROM marks WHERE name = ?`, name).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return v, err == nil, err
}

func setMark(ctx context.Context, tx *sql.Tx, name string, v int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO marks (name, value) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET value = excluded.value`, name, v)
	return err
}

// SetMark sets a mark on its own.
func (s *Store) SetMark(ctx context.Context, name string, v int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error { return setMark(ctx, tx, name, v) })
}

func (s *Store) tx(ctx context.Context, do func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := do(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Outgoing is one event waiting for Mímir.
type Outgoing struct {
	Seq  int64           `json:"seq"`
	At   int64           `json:"at"`
	Kind string          `json:"kind"`
	Body json.RawMessage `json:"body"`
}

// enqueue numbers an event and puts it in the outbox, in tx.
func enqueue(ctx context.Context, tx *sql.Tx, at int64, kind string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	var last int64
	if err := tx.QueryRowContext(ctx, `SELECT value FROM marks WHERE name = 'seq'`).Scan(&last); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox (seq, at, kind, body) VALUES (?, ?, ?, ?)`, last+1, at, kind, string(payload)); err != nil {
		return err
	}
	return setMark(ctx, tx, "seq", last+1)
}

// Outbox is up to limit waiting events, oldest first.
func (s *Store) Outbox(ctx context.Context, limit int) ([]Outgoing, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, at, kind, body FROM outbox ORDER BY seq LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Outgoing
	for rows.Next() {
		var o Outgoing
		var body string
		if err := rows.Scan(&o.Seq, &o.At, &o.Kind, &body); err != nil {
			return nil, err
		}
		o.Body = json.RawMessage(body)
		out = append(out, o)
	}
	return out, rows.Err()
}

// OutboxLen is how many events wait.
func (s *Store) OutboxLen(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox`).Scan(&n)
	return n, err
}

// Sent drops what Mímir holds: every seq up to lastSeq.
func (s *Store) Sent(ctx context.Context, lastSeq int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM outbox WHERE seq <= ?`, lastSeq)
	return err
}

// Renumber moves the waiting events to follow lastSeq: Mímir lost events
// (it was restored from a backup), and a gap would never be taken.
func (s *Store) Renumber(ctx context.Context, lastSeq int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var first sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MIN(seq) FROM outbox`).Scan(&first); err != nil {
			return err
		}
		if !first.Valid {
			return setMark(ctx, tx, "seq", lastSeq)
		}
		shift := lastSeq + 1 - first.Int64
		// Negative seqs first, so no two rows share one on the way.
		if _, err := tx.ExecContext(ctx, `UPDATE outbox SET seq = -(seq + ?)`, shift); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE outbox SET seq = -seq`); err != nil {
			return err
		}
		var last int64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM outbox`).Scan(&last); err != nil {
			return err
		}
		return setMark(ctx, tx, "seq", last)
	})
}

func published(ctx context.Context, tx *sql.Tx, source, key string) (string, error) {
	var fingerprint string
	err := tx.QueryRowContext(ctx, `SELECT fingerprint FROM published WHERE source = ? AND key = ?`, source, key).Scan(&fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return fingerprint, err
}

func setPublished(ctx context.Context, tx *sql.Tx, source, key, fingerprint string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO published (source, key, fingerprint) VALUES (?, ?, ?)
		ON CONFLICT (source, key) DO UPDATE SET fingerprint = excluded.fingerprint`, source, key, fingerprint)
	return err
}

// Mirror is another device's alert, as it last sent it.
type Mirror struct {
	Device string
	State  AlertState
}

// Mirrors is every mirror held.
func (s *Store) Mirrors(ctx context.Context) ([]Mirror, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT device, body FROM mirrors ORDER BY device, source, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mirror
	for rows.Next() {
		var m Mirror
		var body string
		if err := rows.Scan(&m.Device, &body); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(body), &m.State); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) mirror(ctx context.Context, device, source, key string) (*AlertState, error) {
	var body string
	err := s.db.QueryRowContext(ctx, `SELECT body FROM mirrors WHERE device = ? AND source = ? AND key = ?`, device, source, key).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state AlertState
	return &state, json.Unmarshal([]byte(body), &state)
}

// setMirror keeps the device's newest state and forgets the requests made of
// it: the device has spoken since, so a request still needed is made again.
func setMirror(ctx context.Context, tx *sql.Tx, device string, state AlertState) error {
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mirrors (device, source, key, body) VALUES (?, ?, ?, ?)
		ON CONFLICT (device, source, key) DO UPDATE SET body = excluded.body`, device, state.Source, state.Key, string(body)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM requested WHERE device = ? AND source = ? AND key = ?`, device, state.Source, state.Key)
	return err
}

func requestedValue(ctx context.Context, tx *sql.Tx, device, source, key, kind string) (string, error) {
	var v string
	err := tx.QueryRowContext(ctx, `SELECT value FROM requested WHERE device = ? AND source = ? AND key = ? AND kind = ?`, device, source, key, kind).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func setRequested(ctx context.Context, tx *sql.Tx, device, source, key, kind, value string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO requested (device, source, key, kind, value) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (device, source, key, kind) DO UPDATE SET value = excluded.value`, device, source, key, kind, value)
	return err
}
