package sindri

// Sindri's own database: the task ledger, goals and their pictures, the
// tool calls allowed on your behalf, settings, and what each agent's
// conversation has been told. Ported from the coding half of my-agent's
// memory/store.ts.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const storeSchema = `
CREATE TABLE IF NOT EXISTS tasks (
	id INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	session TEXT NOT NULL,
	goal TEXT NOT NULL,
	kind TEXT NOT NULL,
	project TEXT,
	branch TEXT,
	agent TEXT NOT NULL,
	outcome TEXT,
	result TEXT,
	ended_at INTEGER,
	part_of INTEGER,
	goal_id INTEGER,
	scheduled TEXT
);
CREATE INDEX IF NOT EXISTS tasks_by_agent ON tasks (agent, id);
CREATE INDEX IF NOT EXISTS tasks_by_at ON tasks (at);
CREATE INDEX IF NOT EXISTS tasks_by_goal ON tasks (goal_id);
CREATE TABLE IF NOT EXISTS goals (
	id INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	title TEXT NOT NULL,
	repos TEXT NOT NULL,
	source TEXT NOT NULL,
	session TEXT NOT NULL,
	context TEXT NOT NULL DEFAULT '',
	linear_synced TEXT
);
CREATE TABLE IF NOT EXISTS goal_images (
	id INTEGER PRIMARY KEY,
	goal_id INTEGER NOT NULL,
	at INTEGER NOT NULL,
	name TEXT NOT NULL,
	media_type TEXT NOT NULL,
	kind TEXT NOT NULL,
	note TEXT NOT NULL DEFAULT '',
	data BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS goal_images_by_goal ON goal_images (goal_id, id);
CREATE TABLE IF NOT EXISTS auto_allowed (
	id INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	agent TEXT NOT NULL,
	task_id INTEGER,
	tool TEXT NOT NULL,
	summary TEXT NOT NULL,
	rule TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS auto_allowed_by_at ON auto_allowed (at);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS feed (
	agent TEXT PRIMARY KEY,
	session TEXT NOT NULL,
	communication TEXT NOT NULL DEFAULT '',
	mode TEXT NOT NULL DEFAULT '',
	seen TEXT,
	notes_seen INTEGER NOT NULL DEFAULT 0
);
`

// Store is Sindri's database.
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
	if _, err := db.Exec(storeSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("sindri schema: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) ms() int64 { return s.now().UnixMilli() }

// Setting reads a setting into v; false when there is none.
func (s *Store) Setting(key string, v any) bool {
	var value string
	if s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value) != nil {
		return false
	}
	return json.Unmarshal([]byte(value), v) == nil
}

// SetSetting keeps a setting.
func (s *Store) SetSetting(key string, v any) error {
	value, _ := json.Marshal(v)
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, string(value))
	return err
}

// --- tasks --------------------------------------------------------------------

// Scheduled is a coding task waiting for Claude usage to come back.
type Scheduled struct {
	AgentID   string `json:"agentId,omitempty"`
	Prompt    string `json:"prompt"`
	NextTryAt int64  `json:"nextTryAt,omitempty"`
	Retries   int    `json:"retries,omitempty"`
}

// Task is one piece of work handed to an agent.
type Task struct {
	ID        int64      `json:"id"`
	At        int64      `json:"at"`
	Session   string     `json:"session"`
	Goal      string     `json:"goal"`
	Kind      string     `json:"kind"` // code or ask
	Project   *string    `json:"project"`
	Branch    *string    `json:"branch"`
	Agent     string     `json:"agent"`
	PartOf    *int64     `json:"partOf"`
	GoalID    *int64     `json:"goalId,omitempty"`
	Outcome   *string    `json:"outcome"` // done, asked, failed, stopped, scheduled; nil while open
	Result    *string    `json:"result"`
	EndedAt   *int64     `json:"endedAt"`
	Scheduled *Scheduled `json:"scheduled,omitempty"`
}

const taskColumns = `id, at, session, goal, kind, project, branch, agent, part_of, goal_id, outcome, result, ended_at, scheduled`

func scanTask(row interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var scheduled sql.NullString
	err := row.Scan(&t.ID, &t.At, &t.Session, &t.Goal, &t.Kind, &t.Project, &t.Branch, &t.Agent, &t.PartOf, &t.GoalID, &t.Outcome, &t.Result, &t.EndedAt, &scheduled)
	if err == nil && scheduled.Valid {
		t.Scheduled = &Scheduled{}
		_ = json.Unmarshal([]byte(scheduled.String), t.Scheduled)
	}
	return t, err
}

func (s *Store) tasks(query string, args ...any) ([]Task, error) {
	rows, err := s.db.Query(`SELECT `+taskColumns+` FROM tasks `+query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func nullable(text string) any {
	if text == "" {
		return nil
	}
	return text
}

// AddTask records work handed to an agent. partOf "self" (-1) makes it the
// first of several parts.
func (s *Store) AddTask(session, goal, kind, agent, project, branch string, partOf int64, goalID *int64) (int64, error) {
	result, err := s.db.Exec(`INSERT INTO tasks (at, session, goal, kind, project, branch, agent, goal_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ms(), session, goal, kind, nullable(project), nullable(branch), agent, goalID)
	if err != nil {
		return 0, err
	}
	id, _ := result.LastInsertId()
	switch {
	case partOf == -1:
		_, err = s.db.Exec(`UPDATE tasks SET part_of = id WHERE id = ?`, id)
	case partOf > 0:
		_, err = s.db.Exec(`UPDATE tasks SET part_of = ? WHERE id = ?`, partOf, id)
	}
	return id, err
}

// TaskOfAgent is an agent's latest task.
func (s *Store) TaskOfAgent(agent string) (*Task, error) {
	t, err := scanTask(s.db.QueryRow(`SELECT `+taskColumns+` FROM tasks WHERE agent = ? ORDER BY id DESC LIMIT 1`, agent))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &t, err
}

// PartsOf is every part of a task across repositories.
func (s *Store) PartsOf(group int64) ([]Task, error) {
	return s.tasks(`WHERE part_of = ? ORDER BY id`, group)
}

// MakeParts makes a task the first of several parts; its group id.
func (s *Store) MakeParts(id int64) (int64, error) {
	var partOf sql.NullInt64
	if err := s.db.QueryRow(`SELECT part_of FROM tasks WHERE id = ?`, id).Scan(&partOf); err != nil {
		return 0, err
	}
	if partOf.Valid {
		return partOf.Int64, nil
	}
	_, err := s.db.Exec(`UPDATE tasks SET part_of = id WHERE id = ?`, id)
	return id, err
}

// EndTask closes an agent's open task.
func (s *Store) EndTask(agent, outcome, result string) error {
	_, err := s.db.Exec(`UPDATE tasks SET outcome = ?, result = ?, ended_at = ? WHERE id = (SELECT max(id) FROM tasks WHERE agent = ?) AND (outcome IS NULL OR outcome = 'asked')`,
		outcome, nullable(result), s.ms(), agent)
	return err
}

// ReopenTask opens an agent's task again: a reply went to it.
func (s *Store) ReopenTask(agent string) error {
	_, err := s.db.Exec(`UPDATE tasks SET outcome = NULL, result = NULL, ended_at = NULL WHERE id = (SELECT max(id) FROM tasks WHERE agent = ?) AND outcome IS NOT NULL AND outcome != 'scheduled'`, agent)
	return err
}

// Schedule parks an agent's task until Claude usage comes back.
func (s *Store) Schedule(agent string, scheduled Scheduled, result string) error {
	value, _ := json.Marshal(scheduled)
	_, err := s.db.Exec(`UPDATE tasks SET outcome = 'scheduled', result = ?, ended_at = ?, scheduled = ? WHERE id = (SELECT max(id) FROM tasks WHERE agent = ?)`,
		result, s.ms(), string(value), agent)
	return err
}

// Unschedule opens a parked task again under the agent now doing it.
func (s *Store) Unschedule(id int64, agent string) error {
	_, err := s.db.Exec(`UPDATE tasks SET outcome = NULL, result = NULL, ended_at = NULL, scheduled = NULL, agent = ? WHERE id = ?`, agent, id)
	return err
}

// ScheduledTasks is every task waiting for Claude usage.
func (s *Store) ScheduledTasks() ([]Task, error) {
	return s.tasks(`WHERE outcome = 'scheduled' ORDER BY id`)
}

// TasksSince is every task started since a time, or still open.
func (s *Store) TasksSince(since int64) ([]Task, error) {
	return s.tasks(`WHERE at >= ? OR outcome IS NULL OR outcome = 'scheduled' ORDER BY id DESC`, since)
}

// GoalTasks is every task linked to a goal.
func (s *Store) GoalTasks() ([]Task, error) {
	return s.tasks(`WHERE goal_id IS NOT NULL ORDER BY id`)
}

// --- goals ------------------------------------------------------------------------

// Goal is long-running work across repos. Its state is read from its tasks.
type Goal struct {
	ID      int64    `json:"id"`
	At      int64    `json:"at"`
	Title   string   `json:"title"`
	Repos   []string `json:"repos"`
	Source  string   `json:"source"`
	Context string   `json:"context"`
	Session string   `json:"session"`
}

func scanGoal(row interface{ Scan(...any) error }) (Goal, error) {
	var g Goal
	var repos string
	err := row.Scan(&g.ID, &g.At, &g.Title, &repos, &g.Source, &g.Session, &g.Context)
	_ = json.Unmarshal([]byte(repos), &g.Repos)
	if g.Repos == nil {
		g.Repos = []string{}
	}
	return g, err
}

// AddGoal makes a draft goal, with a conversation of its own.
func (s *Store) AddGoal(title string, repos []string, source, context, session string) (Goal, error) {
	encoded, _ := json.Marshal(repos)
	at := s.ms()
	result, err := s.db.Exec(`INSERT INTO goals (at, title, repos, source, session, context) VALUES (?, ?, ?, ?, ?, ?)`, at, title, string(encoded), source, session, context)
	if err != nil {
		return Goal{}, err
	}
	id, _ := result.LastInsertId()
	return Goal{ID: id, At: at, Title: title, Repos: repos, Source: source, Context: context, Session: session}, nil
}

// Goal is one goal; nil when there is none.
func (s *Store) Goal(id int64) (*Goal, error) {
	g, err := scanGoal(s.db.QueryRow(`SELECT id, at, title, repos, source, session, context FROM goals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &g, err
}

// Goals is every goal, newest first.
func (s *Store) Goals() ([]Goal, error) {
	rows, err := s.db.Query(`SELECT id, at, title, repos, source, session, context FROM goals ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	goals := []Goal{}
	for rows.Next() {
		g, err := scanGoal(rows)
		if err != nil {
			return nil, err
		}
		goals = append(goals, g)
	}
	return goals, rows.Err()
}

// UpdateGoal changes a goal's title, repos or context.
func (s *Store) UpdateGoal(id int64, title *string, repos []string, context *string) error {
	if title != nil {
		if _, err := s.db.Exec(`UPDATE goals SET title = ? WHERE id = ?`, *title, id); err != nil {
			return err
		}
	}
	if repos != nil {
		encoded, _ := json.Marshal(repos)
		if _, err := s.db.Exec(`UPDATE goals SET repos = ? WHERE id = ?`, string(encoded), id); err != nil {
			return err
		}
	}
	if context != nil {
		if _, err := s.db.Exec(`UPDATE goals SET context = ? WHERE id = ?`, *context, id); err != nil {
			return err
		}
	}
	return nil
}

// DeleteGoal removes the goal record only; its tasks and agents stay.
func (s *Store) DeleteGoal(id int64) (bool, error) {
	result, err := s.db.Exec(`DELETE FROM goals WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	if n > 0 {
		_, err = s.db.Exec(`DELETE FROM goal_images WHERE goal_id = ?`, id)
	}
	return n > 0, err
}

// GoalImage is a goal's picture without its bytes.
type GoalImage struct {
	ID        int64  `json:"id"`
	Goal      int64  `json:"goal"`
	At        int64  `json:"at"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Kind      string `json:"kind"` // design or current
	Note      string `json:"note"`
	Bytes     int    `json:"bytes"`
}

// AddGoalImage keeps a picture for a goal.
func (s *Store) AddGoalImage(goal int64, name, mediaType, kind, note string, data []byte) (GoalImage, error) {
	at := s.ms()
	result, err := s.db.Exec(`INSERT INTO goal_images (goal_id, at, name, media_type, kind, note, data) VALUES (?, ?, ?, ?, ?, ?, ?)`, goal, at, name, mediaType, kind, note, data)
	if err != nil {
		return GoalImage{}, err
	}
	id, _ := result.LastInsertId()
	return GoalImage{ID: id, Goal: goal, At: at, Name: name, MediaType: mediaType, Kind: kind, Note: note, Bytes: len(data)}, nil
}

// GoalImages is a goal's pictures, oldest first.
func (s *Store) GoalImages(goal int64) ([]GoalImage, error) {
	rows, err := s.db.Query(`SELECT id, goal_id, at, name, media_type, kind, note, length(data) FROM goal_images WHERE goal_id = ? ORDER BY id`, goal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	images := []GoalImage{}
	for rows.Next() {
		var i GoalImage
		if err := rows.Scan(&i.ID, &i.Goal, &i.At, &i.Name, &i.MediaType, &i.Kind, &i.Note, &i.Bytes); err != nil {
			return nil, err
		}
		images = append(images, i)
	}
	return images, rows.Err()
}

// GoalImageData is a picture's bytes and type; false when there is none.
func (s *Store) GoalImageData(goal, id int64) ([]byte, string, bool) {
	var data []byte
	var mediaType string
	if s.db.QueryRow(`SELECT data, media_type FROM goal_images WHERE goal_id = ? AND id = ?`, goal, id).Scan(&data, &mediaType) != nil {
		return nil, "", false
	}
	return data, mediaType, true
}

// UpdateGoalImage changes a picture's kind or note.
func (s *Store) UpdateGoalImage(goal, id int64, kind, note *string) (bool, error) {
	result, err := s.db.Exec(`UPDATE goal_images SET kind = coalesce(?, kind), note = coalesce(?, note) WHERE goal_id = ? AND id = ?`, kind, note, goal, id)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// DeleteGoalImage removes a picture.
func (s *Store) DeleteGoalImage(goal, id int64) (bool, error) {
	result, err := s.db.Exec(`DELETE FROM goal_images WHERE goal_id = ? AND id = ?`, goal, id)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// --- approvals ------------------------------------------------------------------

// AutoAllowed is one tool call Sindri allowed on your behalf.
type AutoAllowed struct {
	ID      int64  `json:"id"`
	At      int64  `json:"at"`
	Agent   string `json:"agent"`
	TaskID  *int64 `json:"taskId"`
	Tool    string `json:"tool"`
	Summary string `json:"summary"`
	Rule    string `json:"rule"`
}

// AddAutoAllowed writes down an automatic yes.
func (s *Store) AddAutoAllowed(agent, tool, summary, rule string) error {
	_, err := s.db.Exec(`INSERT INTO auto_allowed (at, agent, task_id, tool, summary, rule) VALUES (?, ?, (SELECT max(id) FROM tasks WHERE agent = ?), ?, ?, ?)`,
		s.ms(), agent, agent, tool, summary, rule)
	return err
}

// AutoAllowedSince is every automatic yes since a time, newest first.
func (s *Store) AutoAllowedSince(since int64) ([]AutoAllowed, error) {
	rows, err := s.db.Query(`SELECT id, at, agent, task_id, tool, summary, rule FROM auto_allowed WHERE at >= ? ORDER BY id DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []AutoAllowed{}
	for rows.Next() {
		var a AutoAllowed
		if err := rows.Scan(&a.ID, &a.At, &a.Agent, &a.TaskID, &a.Tool, &a.Summary, &a.Rule); err != nil {
			return nil, err
		}
		list = append(list, a)
	}
	return list, rows.Err()
}

// --- the feed ------------------------------------------------------------------------

// Binding is the conversation an agent posts into, how it should talk, and
// what the conversation was last told about it.
type Binding struct {
	Agent, Session, Communication, Mode, Seen string
	NotesSeen                                 int
}

// Bind puts an agent in a conversation.
func (s *Store) Bind(agent, session, communication, mode, seen string) error {
	_, err := s.db.Exec(`INSERT INTO feed (agent, session, communication, mode, seen) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (agent) DO UPDATE SET session = excluded.session, communication = excluded.communication, mode = excluded.mode, seen = coalesce(excluded.seen, feed.seen)`,
		agent, session, communication, mode, nullable(seen))
	return err
}

// BindingOf is where an agent posts; nil when nowhere.
func (s *Store) BindingOf(agent string) (*Binding, error) {
	var b Binding
	var seen sql.NullString
	err := s.db.QueryRow(`SELECT agent, session, communication, mode, seen, notes_seen FROM feed WHERE agent = ?`, agent).Scan(&b.Agent, &b.Session, &b.Communication, &b.Mode, &seen, &b.NotesSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	b.Seen = seen.String
	return &b, err
}

// SetSeen records what the conversation was last told about an agent.
func (s *Store) SetSeen(agent, seen string) error {
	_, err := s.db.Exec(`UPDATE feed SET seen = ? WHERE agent = ?`, seen, agent)
	return err
}

// SetNotesSeen records the last of an agent's notes acted on.
func (s *Store) SetNotesSeen(agent string, n int) error {
	_, err := s.db.Exec(`UPDATE feed SET notes_seen = ? WHERE agent = ?`, n, agent)
	return err
}
