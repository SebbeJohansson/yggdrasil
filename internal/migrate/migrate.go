// Package migrate moves your data between my-agent and Yggdrasil, either way:
//
//	ygg migrate from-my-agent    my-agent's agent.db, sign-ins and runner agents into Norn and Sindri
//	ygg migrate to-my-agent      the same back, so my-agent can take over again
//
// What goes where:
//
//	Norn    conversations (turns), memories, dispatch decisions, model calls,
//	        joke ratings, inbox mutes, widget settings, conversation modes,
//	        which agent is in which conversation, and the Microsoft, Google
//	        and Slack sign-ins (the token files beside the database)
//	Sindri  tasks, goals and their pictures, automatic approvals, the
//	        coding-agent and automation settings, what each agent's
//	        conversation was told, and the agents themselves (the runner's
//	        agents.json, with their screenshots)
//
// The target is backed up first (<file>.bak-<time>). A target that already
// holds data is refused unless Replace is set, which empties those tables
// first: merging two histories would mix ids from both.
package migrate

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"yggdrasil/norn"
	"yggdrasil/sindri"
)

// Paths say where each side keeps its data.
type Paths struct {
	// my-agent
	AgentDB     string // my-agent/data/agent.db
	AgentData   string // my-agent/data (sign-in files)
	RunnerState string // ~/.local/state/my-agent-runner/agents.json
	// Yggdrasil
	NornDB      string // data/norn.db
	NornData    string // where Norn keeps sign-in files (beside norn.db)
	SindriDB    string // ~/.local/state/yggdrasil/sindri.db
	SindriState string // ~/.local/state/yggdrasil/sindri-agents.json
}

// Options change how a migration runs.
type Options struct {
	// Replace empties the target's tables first instead of refusing.
	Replace bool
	Logf    func(string, ...any)
}

// Report says what was moved.
type Report struct {
	Rows   map[string]int
	Files  []string
	Backup []string
}

func (r *Report) count(table string, n int64) {
	if r.Rows == nil {
		r.Rows = map[string]int{}
	}
	r.Rows[table] += int(n)
}

// table is one table moved as it is, with the columns both sides share.
type table struct {
	name    string
	columns string
}

// The tables that move unchanged between my-agent's agent.db and Norn's norn.db.
var nornTables = []table{
	{"turns", "id, session, role, content, at, author, label, decision, via, read_at"},
	{"memories", "id, text, source, embedding, model, at"},
	{"decisions", "id, at, session, message, prompt, source, model, decision, guards, ms, shadow, correction"},
	{"llm_calls", "id, at, session, exchange, purpose, backend, model, prompt_tokens, cached_tokens, output_tokens, prompt_ms, output_ms, total_ms, error, messages, response, thinking"},
	{"joke_ratings", "text, topic, form, rating, at"},
	{"inbox_mutes", "id, name, at"},
	{"session_communication", "session, communication"},
	{"session_mode", "session, mode"},
	{"agent_sessions", "agent, session, seen, notes_seen, at"},
}

// The tables that move unchanged between agent.db and Sindri's sindri.db.
// Tasks move by hand: a waiting task's settings changed shape.
var sindriTables = []table{
	{"goals", "id, at, title, repos, source, session, context, linear_synced"},
	{"goal_images", "id, goal_id, at, name, media_type, kind, note, data"},
	{"auto_allowed", "id, at, agent, task_id, tool, summary, rule"},
}

// Settings live in agent.db's app_settings, and in Norn's or Sindri's own:
// the coding ones in Sindri's, the rest (widget visibility) in Norn's.
func isSindriSetting(key string) bool {
	return key == "coding-agent" || key == "automation" || strings.HasPrefix(key, "follow-through-retried:")
}

const agentDBSchema = `
CREATE TABLE IF NOT EXISTS turns (id INTEGER PRIMARY KEY, session TEXT NOT NULL, role TEXT NOT NULL, content TEXT NOT NULL, at INTEGER NOT NULL, author TEXT, label TEXT, decision INTEGER, read_at INTEGER, via TEXT);
CREATE TABLE IF NOT EXISTS agent_sessions (agent TEXT PRIMARY KEY, session TEXT NOT NULL, seen TEXT, at INTEGER NOT NULL, notes_seen INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS decisions (id INTEGER PRIMARY KEY, at INTEGER NOT NULL, session TEXT NOT NULL, message TEXT NOT NULL, prompt TEXT NOT NULL, source TEXT NOT NULL, model TEXT, decision TEXT NOT NULL, guards TEXT, ms INTEGER, shadow TEXT, correction TEXT);
CREATE TABLE IF NOT EXISTS joke_ratings (text TEXT PRIMARY KEY, topic TEXT, form TEXT, rating INTEGER NOT NULL, at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS llm_calls (id INTEGER PRIMARY KEY, at INTEGER NOT NULL, session TEXT, exchange INTEGER, purpose TEXT NOT NULL, backend TEXT NOT NULL, model TEXT NOT NULL, prompt_tokens INTEGER, cached_tokens INTEGER, output_tokens INTEGER, prompt_ms REAL, output_ms REAL, total_ms INTEGER NOT NULL, error TEXT, messages TEXT, response TEXT, thinking TEXT);
CREATE TABLE IF NOT EXISTS inbox_mutes (id TEXT PRIMARY KEY, name TEXT NOT NULL, at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS app_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS tasks (id INTEGER PRIMARY KEY, at INTEGER NOT NULL, session TEXT NOT NULL, goal TEXT NOT NULL, kind TEXT NOT NULL, project TEXT, branch TEXT, agent TEXT NOT NULL, outcome TEXT, result TEXT, ended_at INTEGER, part_of INTEGER, scheduled TEXT, goal_id INTEGER);
CREATE TABLE IF NOT EXISTS goals (id INTEGER PRIMARY KEY, at INTEGER NOT NULL, title TEXT NOT NULL, repos TEXT NOT NULL, source TEXT NOT NULL, session TEXT NOT NULL, context TEXT NOT NULL DEFAULT '', linear_synced TEXT);
CREATE TABLE IF NOT EXISTS goal_images (id INTEGER PRIMARY KEY, goal_id INTEGER NOT NULL, at INTEGER NOT NULL, name TEXT NOT NULL, media_type TEXT NOT NULL, kind TEXT NOT NULL, note TEXT NOT NULL DEFAULT '', data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS auto_allowed (id INTEGER PRIMARY KEY, at INTEGER NOT NULL, agent TEXT NOT NULL, task_id INTEGER, tool TEXT NOT NULL, summary TEXT NOT NULL, rule TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS memories (id INTEGER PRIMARY KEY, text TEXT NOT NULL, source TEXT NOT NULL, embedding TEXT, model TEXT, at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS session_communication (session TEXT PRIMARY KEY, communication TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS session_mode (session TEXT PRIMARY KEY, mode TEXT NOT NULL);
`

func open(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func backup(path string, report *Report) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	target := fmt.Sprintf("%s.bak-%s", path, time.Now().Format("20060102-150405"))
	if err := copyFile(path, target); err != nil {
		return fmt.Errorf("could not back up %s: %w", path, err)
	}
	report.Backup = append(report.Backup, target)
	return nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	info, _ := in.Stat()
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// quote makes a path safe inside an ATTACH statement.
func quote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "''") + "'"
}

// moveTables copies tables from the attached database `src` into main,
// refusing when main already has rows unless replace.
func moveTables(tx *sql.Tx, tables []table, replace bool, report *Report) error {
	for _, t := range tables {
		var existing int64
		if err := tx.QueryRow(`SELECT count(*) FROM main.` + t.name).Scan(&existing); err != nil {
			return fmt.Errorf("%s: %w", t.name, err)
		}
		if existing > 0 && !replace {
			return fmt.Errorf("the target's %s already holds %d rows; run with --replace to empty it first (it is backed up)", t.name, existing)
		}
		if replace {
			if _, err := tx.Exec(`DELETE FROM main.` + t.name); err != nil {
				return err
			}
		}
		result, err := tx.Exec(`INSERT INTO main.` + t.name + ` (` + t.columns + `) SELECT ` + t.columns + ` FROM src.` + t.name)
		if err != nil {
			return fmt.Errorf("%s: %w", t.name, err)
		}
		n, _ := result.RowsAffected()
		report.count(t.name, n)
	}
	return nil
}

// scheduledOldShape is how my-agent kept a task waiting for Claude usage.
type scheduledOldShape struct {
	Prompt             string   `json:"prompt"`
	Label              string   `json:"label"`
	PermissionMode     string   `json:"permissionMode"`
	Model              string   `json:"model,omitempty"`
	AllowedTools       []string `json:"allowedTools,omitempty"`
	DisallowedTools    []string `json:"disallowedTools,omitempty"`
	AppendSystemPrompt string   `json:"appendSystemPrompt,omitempty"`
	Slug               string   `json:"slug,omitempty"`
	NextTryAt          int64    `json:"nextTryAt,omitempty"`
	Retries            int      `json:"retries,omitempty"`
}

func toSindriScheduled(old, project, session string) string {
	var parsed scheduledOldShape
	if json.Unmarshal([]byte(old), &parsed) != nil {
		return old
	}
	start := sindri.StartRequest{Cwd: project, Prompt: parsed.Prompt, Label: parsed.Label, PermissionMode: parsed.PermissionMode, Model: parsed.Model,
		Worktree: true, AllowedTools: parsed.AllowedTools, DisallowedTools: parsed.DisallowedTools, AppendSystemPrompt: parsed.AppendSystemPrompt, Slug: parsed.Slug, Session: session}
	prompt, _ := json.Marshal(start)
	value, _ := json.Marshal(sindri.Scheduled{Prompt: string(prompt), NextTryAt: parsed.NextTryAt, Retries: parsed.Retries})
	return string(value)
}

func toAgentScheduled(value string) string {
	var scheduled sindri.Scheduled
	var start sindri.StartRequest
	if json.Unmarshal([]byte(value), &scheduled) != nil || json.Unmarshal([]byte(scheduled.Prompt), &start) != nil {
		return value
	}
	old, _ := json.Marshal(scheduledOldShape{Prompt: start.Prompt, Label: start.Label, PermissionMode: start.PermissionMode, Model: start.Model,
		AllowedTools: start.AllowedTools, DisallowedTools: start.DisallowedTools, AppendSystemPrompt: start.AppendSystemPrompt, Slug: start.Slug,
		NextTryAt: scheduled.NextTryAt, Retries: scheduled.Retries})
	return string(old)
}

const taskColumns = "id, at, session, goal, kind, project, branch, agent, outcome, result, ended_at, part_of, goal_id, scheduled"

// moveTasks copies the task ledger, turning a waiting task's settings into
// the other side's shape.
func moveTasks(tx *sql.Tx, toSindri, replace bool, report *Report) error {
	var existing int64
	_ = tx.QueryRow(`SELECT count(*) FROM main.tasks`).Scan(&existing)
	if existing > 0 && !replace {
		return fmt.Errorf("the target's tasks already holds %d rows; run with --replace to empty it first (it is backed up)", existing)
	}
	if replace {
		if _, err := tx.Exec(`DELETE FROM main.tasks`); err != nil {
			return err
		}
	}
	rows, err := tx.Query(`SELECT ` + taskColumns + ` FROM src.tasks`)
	if err != nil {
		return err
	}
	type row struct {
		values    []any
		scheduled sql.NullString
		project   sql.NullString
		session   string
	}
	var all []row
	for rows.Next() {
		var r row
		values := make([]any, 13)
		pointers := make([]any, 14)
		for i := range values {
			pointers[i] = &values[i]
		}
		pointers[13] = &r.scheduled
		if err := rows.Scan(pointers...); err != nil {
			rows.Close()
			return err
		}
		r.values = values
		if text, ok := values[5].(string); ok {
			r.project = sql.NullString{String: text, Valid: true}
		}
		r.session, _ = values[2].(string)
		all = append(all, r)
	}
	rows.Close()
	for _, r := range all {
		var scheduled any
		if r.scheduled.Valid {
			if toSindri {
				scheduled = toSindriScheduled(r.scheduled.String, r.project.String, r.session)
			} else {
				scheduled = toAgentScheduled(r.scheduled.String)
			}
		}
		if _, err := tx.Exec(`INSERT INTO main.tasks (`+taskColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, append(r.values, scheduled)...); err != nil {
			return fmt.Errorf("tasks: %w", err)
		}
	}
	report.count("tasks", int64(len(all)))
	return nil
}

// moveSettings copies app settings into Norn's or Sindri's settings table.
func moveSettings(tx *sql.Tx, from, to string, keep func(string) bool, report *Report) error {
	rows, err := tx.Query(`SELECT key, value FROM ` + from)
	if err != nil {
		return err
	}
	type setting struct{ key, value string }
	var settings []setting
	for rows.Next() {
		var s setting
		if err := rows.Scan(&s.key, &s.value); err != nil {
			rows.Close()
			return err
		}
		if keep(s.key) {
			settings = append(settings, s)
		}
	}
	rows.Close()
	for _, s := range settings {
		if _, err := tx.Exec(`INSERT INTO `+to+` (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, s.key, s.value); err != nil {
			return err
		}
	}
	report.count("settings", int64(len(settings)))
	return nil
}

// feed rows: Sindri keeps, per agent, the conversation and how it talks.
func moveFeed(tx *sql.Tx, toSindri, replace bool, report *Report) error {
	if toSindri {
		if replace {
			if _, err := tx.Exec(`DELETE FROM main.feed`); err != nil {
				return err
			}
		}
		result, err := tx.Exec(`INSERT OR REPLACE INTO main.feed (agent, session, communication, mode, seen, notes_seen)
			SELECT a.agent, a.session, coalesce(c.communication, ''), coalesce(m.mode, ''), a.seen, a.notes_seen FROM src.agent_sessions a
			LEFT JOIN src.session_communication c ON c.session = a.session LEFT JOIN src.session_mode m ON m.session = a.session`)
		if err != nil {
			return fmt.Errorf("feed: %w", err)
		}
		n, _ := result.RowsAffected()
		report.count("feed", n)
		return nil
	}
	// Back to my-agent: what each conversation was told about an agent.
	_, err := tx.Exec(`UPDATE main.agent_sessions SET seen = coalesce((SELECT f.seen FROM src.feed f WHERE f.agent = main.agent_sessions.agent), seen),
		notes_seen = coalesce((SELECT f.notes_seen FROM src.feed f WHERE f.agent = main.agent_sessions.agent), notes_seen)`)
	return err
}

func signInFiles() []string {
	return []string{"microsoft.json", "google.json", "slack.json"}
}

func copySignIns(fromDir, toDir string, report *Report) error {
	for _, name := range signInFiles() {
		from := filepath.Join(fromDir, name)
		if _, err := os.Stat(from); err != nil {
			continue
		}
		to := filepath.Join(toDir, name)
		if _, err := os.Stat(to); err == nil {
			if err := backup(to, report); err != nil {
				return err
			}
		}
		if err := copyFile(from, to); err != nil {
			return err
		}
		_ = os.Chmod(to, 0o600)
		report.Files = append(report.Files, to)
	}
	return nil
}

// moveAgents copies the agents' state file (the same shape on both sides)
// with their screenshots, setting each agent's conversation.
func moveAgents(from, to string, sessions map[string]string, replace bool, report *Report) error {
	data, err := os.ReadFile(from)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var agents []map[string]any
	if err := json.Unmarshal(data, &agents); err != nil {
		return fmt.Errorf("%s is not a list of agents: %w", from, err)
	}
	var existing []map[string]any
	if current, err := os.ReadFile(to); err == nil {
		_ = json.Unmarshal(current, &existing)
		if len(existing) > 0 && !replace {
			return fmt.Errorf("%s already holds %d agents; run with --replace to overwrite it (it is backed up)", to, len(existing))
		}
		if err := backup(to, report); err != nil {
			return err
		}
	}
	for _, agent := range agents {
		id, _ := agent["id"].(string)
		if session, ok := sessions[id]; ok {
			agent["session"] = session
		} else if sessions != nil {
			delete(agent, "session")
		}
		shots := filepath.Join(filepath.Dir(from), "screenshots", id)
		if entries, err := os.ReadDir(shots); err == nil && id != "" {
			for _, entry := range entries {
				_ = copyFile(filepath.Join(shots, entry.Name()), filepath.Join(filepath.Dir(to), "screenshots", id, entry.Name()))
			}
		}
	}
	out, _ := json.MarshalIndent(agents, "", " ")
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(to, out, 0o600); err != nil {
		return err
	}
	report.count("agents", int64(len(agents)))
	report.Files = append(report.Files, to)
	return nil
}

func sessionsOf(db *sql.DB, query string) map[string]string {
	sessions := map[string]string{}
	rows, err := db.Query(query)
	if err != nil {
		return sessions
	}
	defer rows.Close()
	for rows.Next() {
		var agent, session string
		if rows.Scan(&agent, &session) == nil {
			sessions[agent] = session
		}
	}
	return sessions
}

// FromMyAgent moves my-agent's data into Norn and Sindri. Stop both first.
func FromMyAgent(paths Paths, options Options) (*Report, error) {
	report := &Report{}
	if _, err := os.Stat(paths.AgentDB); err != nil {
		return nil, fmt.Errorf("no my-agent database at %s: %w", paths.AgentDB, err)
	}
	for _, target := range []string{paths.NornDB, paths.SindriDB} {
		if err := backup(target, report); err != nil {
			return nil, err
		}
	}
	// Opening the stores makes their tables.
	nornStore, err := norn.OpenStore(paths.NornDB)
	if err != nil {
		return nil, err
	}
	nornStore.Close()
	sindriStore, err := sindri.OpenStore(paths.SindriDB)
	if err != nil {
		return nil, err
	}
	sindriStore.Close()

	if err := into(paths.NornDB, paths.AgentDB, func(tx *sql.Tx) error {
		if err := moveTables(tx, nornTables, options.Replace, report); err != nil {
			return err
		}
		return moveSettings(tx, "src.app_settings", "main.app_settings", func(key string) bool { return !isSindriSetting(key) }, report)
	}); err != nil {
		return nil, fmt.Errorf("norn: %w", err)
	}
	if err := into(paths.SindriDB, paths.AgentDB, func(tx *sql.Tx) error {
		if err := moveTables(tx, sindriTables, options.Replace, report); err != nil {
			return err
		}
		if err := moveTasks(tx, true, options.Replace, report); err != nil {
			return err
		}
		if err := moveFeed(tx, true, options.Replace, report); err != nil {
			return err
		}
		return moveSettings(tx, "src.app_settings", "main.settings", isSindriSetting, report)
	}); err != nil {
		return nil, fmt.Errorf("sindri: %w", err)
	}
	if err := copySignIns(paths.AgentData, paths.NornData, report); err != nil {
		return nil, err
	}
	source, err := open(paths.AgentDB)
	if err != nil {
		return nil, err
	}
	sessions := sessionsOf(source, `SELECT agent, session FROM agent_sessions`)
	source.Close()
	if err := moveAgents(paths.RunnerState, paths.SindriState, sessions, options.Replace, report); err != nil {
		return nil, err
	}
	return report, nil
}

// ToMyAgent moves Norn's and Sindri's data back into my-agent. Stop all three first.
func ToMyAgent(paths Paths, options Options) (*Report, error) {
	report := &Report{}
	for _, source := range []string{paths.NornDB, paths.SindriDB} {
		if _, err := os.Stat(source); err != nil {
			return nil, fmt.Errorf("no database at %s: %w", source, err)
		}
	}
	if err := backup(paths.AgentDB, report); err != nil {
		return nil, err
	}
	db, err := open(paths.AgentDB)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(agentDBSchema); err != nil {
		db.Close()
		return nil, err
	}
	db.Close()
	if err := into(paths.AgentDB, paths.NornDB, func(tx *sql.Tx) error {
		if err := moveTables(tx, nornTables, options.Replace, report); err != nil {
			return err
		}
		if options.Replace {
			if _, err := tx.Exec(`DELETE FROM main.app_settings WHERE key IN ('widget-visibility')`); err != nil {
				return err
			}
		}
		return moveSettings(tx, "src.app_settings", "main.app_settings", func(key string) bool { return !isSindriSetting(key) }, report)
	}); err != nil {
		return nil, fmt.Errorf("from norn: %w", err)
	}
	if err := into(paths.AgentDB, paths.SindriDB, func(tx *sql.Tx) error {
		if err := moveTables(tx, sindriTables, options.Replace, report); err != nil {
			return err
		}
		if err := moveTasks(tx, false, options.Replace, report); err != nil {
			return err
		}
		if err := moveFeed(tx, false, options.Replace, report); err != nil {
			return err
		}
		return moveSettings(tx, "src.settings", "main.app_settings", isSindriSetting, report)
	}); err != nil {
		return nil, fmt.Errorf("from sindri: %w", err)
	}
	if err := copySignIns(paths.NornData, paths.AgentData, report); err != nil {
		return nil, err
	}
	if err := moveAgents(paths.SindriState, paths.RunnerState, nil, options.Replace, report); err != nil {
		return nil, err
	}
	return report, nil
}

// into runs work in one transaction on target, with source attached as src.
func into(target, source string, work func(*sql.Tx) error) error {
	db, err := open(target)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`ATTACH DATABASE ` + quote(source) + ` AS src`); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := work(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, err = db.Exec(`DETACH DATABASE src`)
	return err
}
