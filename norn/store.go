package norn

// Norn's own database: conversations, memories, dispatch decisions, model
// calls, mutes and settings. Ported from my-agent's memory/store.ts, without
// what moved to Sindri (goals, tasks, approvals). An agent's messages are
// turns here too: Sindri posts them into the conversation the agent works
// for, so the chat, its search and "Needs you" read one place.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"yggdrasil/internal/llm"
)

const (
	// llmCallDays is how long the debug view's model calls are kept, and
	// llmDetailDays how long their text (up to ~800 kB a call) is.
	llmCallDays   = 30
	llmDetailDays = 3
)

const schema = `
CREATE TABLE IF NOT EXISTS turns (
	id INTEGER PRIMARY KEY,
	session TEXT NOT NULL,
	role TEXT NOT NULL,
	content TEXT NOT NULL,
	at INTEGER NOT NULL,
	-- The agent that posted it; NULL for you and Norn.
	author TEXT,
	-- That agent's task as a name to show; on yours, who it went to.
	label TEXT,
	-- On yours: the dispatch decision that routed it.
	decision INTEGER,
	-- On yours: what handled it, e.g. "dispatcher: delegate".
	via TEXT,
	-- On an agent's: when you read it.
	read_at INTEGER
);
CREATE INDEX IF NOT EXISTS turns_by_session ON turns (session, id);
CREATE INDEX IF NOT EXISTS turns_by_author ON turns (author, id);

CREATE TABLE IF NOT EXISTS agent_sessions (
	agent TEXT PRIMARY KEY,
	session TEXT NOT NULL,
	seen TEXT,
	notes_seen INTEGER NOT NULL DEFAULT 0,
	at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS agent_sessions_by_session ON agent_sessions (session, at);

CREATE TABLE IF NOT EXISTS decisions (
	id INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	session TEXT NOT NULL,
	message TEXT NOT NULL,
	prompt TEXT NOT NULL,
	-- model (the dispatcher decided), fallback (the rules did) or person (you picked).
	source TEXT NOT NULL,
	model TEXT,
	decision TEXT NOT NULL,
	guards TEXT,
	ms INTEGER,
	shadow TEXT,
	correction TEXT
);

CREATE TABLE IF NOT EXISTS joke_ratings (
	text TEXT PRIMARY KEY,
	topic TEXT,
	form TEXT,
	rating INTEGER NOT NULL,
	at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS llm_calls (
	id INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	session TEXT,
	exchange INTEGER,
	purpose TEXT NOT NULL,
	backend TEXT NOT NULL,
	model TEXT NOT NULL,
	prompt_tokens INTEGER,
	cached_tokens INTEGER,
	output_tokens INTEGER,
	prompt_ms REAL,
	output_ms REAL,
	total_ms INTEGER NOT NULL,
	error TEXT,
	messages TEXT,
	response TEXT,
	thinking TEXT
);
CREATE INDEX IF NOT EXISTS llm_calls_by_session ON llm_calls (session, at);
CREATE INDEX IF NOT EXISTS llm_calls_by_at ON llm_calls (at);

CREATE TABLE IF NOT EXISTS inbox_mutes (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS app_settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS memories (
	id INTEGER PRIMARY KEY,
	text TEXT NOT NULL,
	source TEXT NOT NULL,
	embedding TEXT,
	model TEXT,
	at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS session_communication (session TEXT PRIMARY KEY, communication TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS session_mode (session TEXT PRIMARY KEY, mode TEXT NOT NULL);
`

// Store is Norn's database.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// OpenStore opens (and creates) the database; ":memory:" for tests.
func OpenStore(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + draftSchema + meetingPrepSchema + pseudoSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("norn schema: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) ms() int64 {
	return s.now().UnixMilli()
}

// --- settings ----------------------------------------------------------------

// Setting reads a stored setting into v; false when there is none.
func (s *Store) Setting(ctx context.Context, key string, v any) (bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(value), v)
}

// SetSetting stores a setting.
func (s *Store) SetSetting(ctx context.Context, key string, v any) error {
	value, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO app_settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, string(value))
	return err
}

// --- memories ----------------------------------------------------------------

// Memory is one thing Norn remembers about you.
type Memory struct {
	ID   int64  `json:"id"`
	Text string `json:"text"`
	// Source is where it came from: extracted from something you said, or manual.
	Source string `json:"source"`
	At     int64  `json:"at"`
}

// StoredMemory is a memory with its vector.
type StoredMemory struct {
	Memory
	Embedding []float64
	// Model made the embedding; vectors from two models are not comparable.
	Model string
}

// AddMemory stores a memory.
func (s *Store) AddMemory(ctx context.Context, text, source string, embedding []float64, model string) (Memory, error) {
	at := s.ms()
	var vector, vectorModel any
	if embedding != nil {
		encoded, _ := json.Marshal(embedding)
		vector, vectorModel = string(encoded), model
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO memories (text, source, embedding, model, at) VALUES (?, ?, ?, ?, ?)`, text, source, vector, vectorModel, at)
	if err != nil {
		return Memory{}, err
	}
	id, _ := result.LastInsertId()
	return Memory{ID: id, Text: text, Source: source, At: at}, nil
}

// Memories is every memory, newest first, with its vector.
func (s *Store) Memories(ctx context.Context) ([]StoredMemory, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, text, source, at, embedding, model FROM memories ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	memories := []StoredMemory{}
	for rows.Next() {
		var m StoredMemory
		var embedding, model sql.NullString
		if err := rows.Scan(&m.ID, &m.Text, &m.Source, &m.At, &embedding, &model); err != nil {
			return nil, err
		}
		if embedding.Valid {
			_ = json.Unmarshal([]byte(embedding.String), &m.Embedding)
		}
		m.Model = model.String
		memories = append(memories, m)
	}
	return memories, rows.Err()
}

// SetEmbedding puts a new vector, from model, on a memory.
func (s *Store) SetEmbedding(ctx context.Context, id int64, embedding []float64, model string) error {
	encoded, _ := json.Marshal(embedding)
	_, err := s.db.ExecContext(ctx, `UPDATE memories SET embedding = ?, model = ? WHERE id = ?`, string(encoded), model, id)
	return err
}

// DeleteMemory forgets one; false when there was none.
func (s *Store) DeleteMemory(ctx context.Context, id int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// --- jokes -------------------------------------------------------------------

// JokeRating is one of the widget's jokes with your thumb on it.
type JokeRating struct {
	Text   string  `json:"text"`
	Topic  *string `json:"topic"`
	Form   *string `json:"form"`
	Rating int     `json:"rating"`
	At     int64   `json:"at"`
}

// RateJoke records a thumb; 0 takes it back. Rating again changes your mind
// rather than voting twice.
func (s *Store) RateJoke(ctx context.Context, text string, topic, form *string, rating int) error {
	if rating == 0 {
		_, err := s.db.ExecContext(ctx, `DELETE FROM joke_ratings WHERE text = ?`, text)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO joke_ratings (text, topic, form, rating, at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (text) DO UPDATE SET rating = excluded.rating, at = excluded.at`, text, topic, form, rating, s.ms())
	return err
}

// JokeRatings is every rated joke, newest first.
func (s *Store) JokeRatings(ctx context.Context, limit int) ([]JokeRating, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT text, topic, form, rating, at FROM joke_ratings ORDER BY at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ratings := []JokeRating{}
	for rows.Next() {
		var r JokeRating
		if err := rows.Scan(&r.Text, &r.Topic, &r.Form, &r.Rating, &r.At); err != nil {
			return nil, err
		}
		ratings = append(ratings, r)
	}
	return ratings, rows.Err()
}

// --- turns -------------------------------------------------------------------

// Turn is one message in a conversation, as the chat shows it.
type Turn struct {
	ID      int64  `json:"id"`
	Role    string `json:"role"`
	Content string `json:"content"`
	At      int64  `json:"at"`
	// Author is the agent that posted it; nil for you and Norn.
	Author *string `json:"author"`
	Label  *string `json:"label"`
	// Decision is, on yours, the dispatch decision that routed it.
	Decision *int64 `json:"decision"`
	// Via is, on yours, what handled it.
	Via *string `json:"via"`
}

// AddTurn adds your message or Norn's to a conversation, and returns its id.
func (s *Store) AddTurn(ctx context.Context, session, role, content string, label *string, decision *int64, via *string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO turns (session, role, content, at, label, decision, via) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		session, role, content, s.ms(), label, decision, via)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// AddAgentTurn adds a message an agent posted into its conversation. It is
// unread until you read it.
func (s *Store) AddAgentTurn(ctx context.Context, session, agent, label, content string) (int64, error) {
	at := s.ms()
	result, err := s.db.ExecContext(ctx, `INSERT INTO turns (session, role, content, at, author, label) VALUES (?, 'assistant', ?, ?, ?, ?)`, session, content, at, agent, label)
	if err != nil {
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE agent_sessions SET at = ? WHERE agent = ?`, at, agent); err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// Chat is a conversation after turn `after`, oldest first.
func (s *Store) Chat(ctx context.Context, session string, after int64, limit int) ([]Turn, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, role, content, at, author, label, decision, via FROM turns WHERE session = ? AND id > ? ORDER BY id LIMIT ?`, session, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	turns := []Turn{}
	for rows.Next() {
		var t Turn
		if err := rows.Scan(&t.ID, &t.Role, &t.Content, &t.At, &t.Author, &t.Label, &t.Decision, &t.Via); err != nil {
			return nil, err
		}
		turns = append(turns, t)
	}
	return turns, rows.Err()
}

// RecentTurns is the last limit turns of a conversation, oldest first.
func (s *Store) RecentTurns(ctx context.Context, session string, limit int) ([]Turn, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, role, content, at, author, label, decision, via FROM turns WHERE session = ? ORDER BY id DESC LIMIT ?`, session, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var turns []Turn
	for rows.Next() {
		var t Turn
		if err := rows.Scan(&t.ID, &t.Role, &t.Content, &t.At, &t.Author, &t.Label, &t.Decision, &t.Via); err != nil {
			return nil, err
		}
		turns = append([]Turn{t}, turns...)
	}
	return turns, rows.Err()
}

// Conversation is one conversation as the chat's history lists it.
type Conversation struct {
	Session string `json:"session"`
	// Title is your first message in it, or nil when it only holds agents' messages.
	Title    *string `json:"title"`
	Messages int     `json:"messages"`
	Started  int64   `json:"started"`
	Last     int64   `json:"last"`
}

const titleOf = `(SELECT u.content FROM turns u WHERE u.session = t.session AND u.role = 'user' AND u.author IS NULL ORDER BY u.id LIMIT 1)`

// Conversations is every conversation, the most recent first.
func (s *Store) Conversations(ctx context.Context, limit int) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.session, count(*), min(t.at), max(t.at), `+titleOf+`
		FROM turns t GROUP BY t.session ORDER BY max(t.at) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	conversations := []Conversation{}
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.Session, &c.Messages, &c.Started, &c.Last, &c.Title); err != nil {
			return nil, err
		}
		conversations = append(conversations, c)
	}
	return conversations, rows.Err()
}

// SearchTurn is a turn that holds the searched text.
type SearchTurn struct {
	Turn
	Session string
	Title   *string
}

// SearchTurns is the turns whose text holds query, newest first. Case is
// ignored for ASCII only.
func (s *Store) SearchTurns(ctx context.Context, query string, limit int) ([]SearchTurn, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.session, t.role, t.author, t.label, t.content, t.at, `+titleOf+`
		FROM turns t WHERE instr(lower(t.content), lower(?)) > 0 ORDER BY t.id DESC LIMIT ?`, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var turns []SearchTurn
	for rows.Next() {
		var t SearchTurn
		if err := rows.Scan(&t.ID, &t.Session, &t.Role, &t.Author, &t.Label, &t.Content, &t.At, &t.Title); err != nil {
			return nil, err
		}
		turns = append(turns, t)
	}
	return turns, rows.Err()
}

// UnreadTurn is an agent's latest message that you have not read.
type UnreadTurn struct {
	ID      int64   `json:"id"`
	Session string  `json:"session"`
	Agent   string  `json:"agent"`
	Label   *string `json:"label"`
	Content string  `json:"content"`
	At      int64   `json:"at"`
}

// UnreadAgentTurns is each agent's latest message when you have not read it
// yet, oldest first: an agent that said something newer has moved on.
func (s *Store) UnreadAgentTurns(ctx context.Context) ([]UnreadTurn, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.session, t.author, t.label, t.content, t.at FROM turns t
		WHERE t.author IS NOT NULL AND t.read_at IS NULL AND t.id = (SELECT max(u.id) FROM turns u WHERE u.author = t.author)
		ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	turns := []UnreadTurn{}
	for rows.Next() {
		var t UnreadTurn
		if err := rows.Scan(&t.ID, &t.Session, &t.Agent, &t.Label, &t.Content, &t.At); err != nil {
			return nil, err
		}
		turns = append(turns, t)
	}
	return turns, rows.Err()
}

// MarkAgentRead marks everything an agent said as read; whether anything was unread.
func (s *Store) MarkAgentRead(ctx context.Context, agent string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE turns SET read_at = ? WHERE author = ? AND read_at IS NULL`, s.ms(), agent)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// TurnCount is one turn without its text, for the activity heatmap.
type TurnCount struct {
	Role   string
	Author *string
	At     int64
}

// TurnsSince is every turn since a time, without its text.
func (s *Store) TurnsSince(ctx context.Context, since int64) ([]TurnCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT role, author, at FROM turns WHERE at >= ?`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var turns []TurnCount
	for rows.Next() {
		var t TurnCount
		if err := rows.Scan(&t.Role, &t.Author, &t.At); err != nil {
			return nil, err
		}
		turns = append(turns, t)
	}
	return turns, rows.Err()
}

// --- conversation settings and agents ------------------------------------------

// SessionSetting is a conversation's mode (auto or manual) or communication
// (concise, detailed or verbose); empty when none was set.
func (s *Store) SessionSetting(ctx context.Context, kind, session string) (string, error) {
	table, column := sessionTable(kind)
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT `+column+` FROM `+table+` WHERE session = ?`, session).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

// SetSessionSetting keeps a conversation's mode or communication.
func (s *Store) SetSessionSetting(ctx context.Context, kind, session, value string) error {
	table, column := sessionTable(kind)
	_, err := s.db.ExecContext(ctx, `INSERT INTO `+table+` (session, `+column+`) VALUES (?, ?) ON CONFLICT (session) DO UPDATE SET `+column+` = excluded.`+column, session, value)
	return err
}

func sessionTable(kind string) (string, string) {
	if kind == "mode" {
		return "session_mode", "mode"
	}
	return "session_communication", "communication"
}

// BindAgent puts an agent in a conversation (moving it from another) and
// makes it the conversation's agent.
func (s *Store) BindAgent(ctx context.Context, session, agent string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_sessions (agent, session, at) VALUES (?, ?, ?)
		ON CONFLICT (agent) DO UPDATE SET session = excluded.session, at = excluded.at`, agent, session, s.ms())
	return err
}

// AgentsOf is every agent in a conversation, the most recent first.
func (s *Store) AgentsOf(ctx context.Context, session string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent FROM agent_sessions WHERE session = ? ORDER BY at DESC`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agents := []string{}
	for rows.Next() {
		var agent string
		if err := rows.Scan(&agent); err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

// SessionOf is the conversation an agent works for; empty when none.
func (s *Store) SessionOf(ctx context.Context, agent string) (string, error) {
	var session string
	err := s.db.QueryRowContext(ctx, `SELECT session FROM agent_sessions WHERE agent = ?`, agent).Scan(&session)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return session, err
}

// --- model calls ---------------------------------------------------------------

// AddCall keeps one model call for the debug view, and drops old ones: the
// row after llmCallDays, its text after llmDetailDays.
func (s *Store) AddCall(ctx context.Context, call llm.Call) error {
	var messages any
	if call.Messages != nil {
		encoded, _ := json.Marshal(call.Messages)
		messages = string(encoded)
	}
	nullable := func(text string) any {
		if text == "" {
			return nil
		}
		return text
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO llm_calls (at, session, exchange, purpose, backend, model, prompt_tokens, cached_tokens, output_tokens, prompt_ms, output_ms, total_ms, error, messages, response, thinking)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		call.At, nullable(call.Session), nullableInt(call.Exchange), call.Purpose, call.Backend, call.Model, call.PromptTokens, call.CachedTokens, call.OutputTokens,
		call.PromptMS, call.OutputMS, call.TotalMS, nullable(call.Error), messages, nullable(call.Response), nullable(call.Thinking))
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM llm_calls WHERE at < ?`, call.At-llmCallDays*86_400_000); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE llm_calls SET messages = NULL, response = NULL, thinking = NULL
		WHERE at < ? AND (messages IS NOT NULL OR response IS NOT NULL OR thinking IS NOT NULL)`, call.At-llmDetailDays*86_400_000)
	return err
}

func nullableInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

// StoredCall is a model call as the debug view lists it.
type StoredCall struct {
	At           int64         `json:"at"`
	Session      *string       `json:"session"`
	Exchange     *int64        `json:"exchange"`
	Purpose      string        `json:"purpose"`
	Backend      string        `json:"backend"`
	Model        string        `json:"model"`
	PromptTokens *int          `json:"promptTokens"`
	CachedTokens *int          `json:"cachedTokens"`
	OutputTokens *int          `json:"outputTokens"`
	PromptMS     *float64      `json:"promptMs"`
	OutputMS     *float64      `json:"outputMs"`
	TotalMS      int64         `json:"totalMs"`
	Error        *string       `json:"error"`
	Messages     []llm.Message `json:"messages"`
	Response     *string       `json:"response"`
	Thinking     *string       `json:"thinking"`
}

// Calls is a conversation's model calls, oldest first.
func (s *Store) Calls(ctx context.Context, session string) ([]StoredCall, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, session, exchange, purpose, backend, model, prompt_tokens, cached_tokens, output_tokens, prompt_ms, output_ms, total_ms, error, messages, response, thinking
		FROM llm_calls WHERE session = ? ORDER BY at, id`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	calls := []StoredCall{}
	for rows.Next() {
		var c StoredCall
		var messages sql.NullString
		if err := rows.Scan(&c.At, &c.Session, &c.Exchange, &c.Purpose, &c.Backend, &c.Model, &c.PromptTokens, &c.CachedTokens, &c.OutputTokens,
			&c.PromptMS, &c.OutputMS, &c.TotalMS, &c.Error, &messages, &c.Response, &c.Thinking); err != nil {
			return nil, err
		}
		if messages.Valid {
			_ = json.Unmarshal([]byte(messages.String), &c.Messages)
		}
		calls = append(calls, c)
	}
	return calls, rows.Err()
}

// --- inbox mutes -----------------------------------------------------------------

// Mute is a chat you muted in the inbox.
type Mute struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// InboxMutes is every muted chat, oldest first.
func (s *Store) InboxMutes(ctx context.Context) ([]Mute, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM inbox_mutes ORDER BY at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	mutes := []Mute{}
	for rows.Next() {
		var m Mute
		if err := rows.Scan(&m.ID, &m.Name); err != nil {
			return nil, err
		}
		mutes = append(mutes, m)
	}
	return mutes, rows.Err()
}

// SetInboxMute mutes or unmutes a chat.
func (s *Store) SetInboxMute(ctx context.Context, id, name string, muted bool) error {
	if muted {
		_, err := s.db.ExecContext(ctx, `INSERT INTO inbox_mutes (id, name, at) VALUES (?, ?, ?) ON CONFLICT (id) DO NOTHING`, id, name, s.ms())
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM inbox_mutes WHERE id = ?`, id)
	return err
}
