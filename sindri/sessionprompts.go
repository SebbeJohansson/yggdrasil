package sindri

// Your own prompts in your Claude Code sessions, kept in Sindri's store so
// Norn's Rewind can say what you were doing on any day. Claude Code deletes
// transcripts after its cleanupPeriodDays (30 by default); this index outlives
// that. It holds only what you typed (clipped), when, and which session,
// project and branch: never Claude's answers or tool output.
//
// A loop reads each transcript from where it left off, so a long session is
// read once. The first pass after start reads every transcript still on disk
// (the backfill). Sindri's own agents are left out, as on /v1/sessions/recent.
//
//	GET /v1/sessions/prompts?from=&to=   your prompts in a range, oldest first

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"yggdrasil/internal/part"
)

const promptIndexSchema = `
CREATE TABLE IF NOT EXISTS session_prompts (
	session TEXT NOT NULL,
	at INTEGER NOT NULL,
	prompt TEXT NOT NULL,
	PRIMARY KEY (session, at)
);
CREATE INDEX IF NOT EXISTS session_prompts_by_at ON session_prompts (at);
CREATE TABLE IF NOT EXISTS session_index (
	session TEXT PRIMARY KEY,
	path TEXT NOT NULL,
	offset INTEGER NOT NULL DEFAULT 0,
	mtime INTEGER NOT NULL DEFAULT 0,
	title TEXT NOT NULL DEFAULT '',
	cwd TEXT NOT NULL DEFAULT '',
	branch TEXT NOT NULL DEFAULT '',
	skip INTEGER NOT NULL DEFAULT 0
);`

const (
	// SessionPromptChars is how much of each prompt is kept.
	SessionPromptChars = 200
	// SessionPromptsEvery is how often new transcript lines are read.
	SessionPromptsEvery = 3 * time.Minute
	// sessionPromptsMax is the most prompts one answer carries.
	sessionPromptsMax = 5000
	// sessionPromptRange is the longest range one call reads.
	sessionPromptRange = 93 * 24 * time.Hour
)

// SessionPrompt is one thing you typed in one of your sessions.
type SessionPrompt struct {
	At      int64  `json:"at"`
	Session string `json:"session"`
	Title   string `json:"title"`
	Cwd     string `json:"cwd,omitempty"`
	Project string `json:"project,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Prompt  string `json:"prompt"`
}

// indexedSession is one transcript's place in the index.
type indexedSession struct {
	session, path, title, cwd, branch string
	offset, mtime                     int64
	skip                              bool
}

// ScannedPrompt is a prompt read off a transcript, before it has a title.
type ScannedPrompt struct {
	At     int64
	Prompt string
}

// ScanTranscript reads complete lines from r: your prompts, and the title,
// folder and branch the lines carry (empty when they say none). read is how
// many bytes were whole lines, so the next read starts there.
func ScanTranscript(r io.Reader) (prompts []ScannedPrompt, title, cwd, branch string, read int64) {
	reader := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			break // a last line without its newline is still being written
		}
		read += int64(len(line))
		isUser := bytes.Contains(line, []byte(`"type":"user"`))
		if !isUser && !bytes.Contains(line, []byte(`"ai-title"`)) {
			continue
		}
		var e event
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		if e["type"] == "ai-title" {
			if t := e.str("aiTitle"); t != "" {
				title = t
			}
			continue
		}
		if e["type"] != "user" {
			continue
		}
		if c := e.str("cwd"); c != "" {
			cwd = c
		}
		if b := e.str("gitBranch"); b != "" && b != "HEAD" {
			branch = b
		}
		if e["isSidechain"] == true || e["isCompactSummary"] == true {
			continue
		}
		text := typed(e)
		if text == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, e.str("timestamp"))
		if err != nil {
			continue
		}
		prompts = append(prompts, ScannedPrompt{At: at.UnixMilli(), Prompt: clipText(text, SessionPromptChars)})
	}
	return prompts, title, cwd, branch, read
}

func (s *Store) initSessionPrompts() error {
	_, err := s.db.Exec(promptIndexSchema)
	return err
}

func (s *Store) sessionIndex() (map[string]indexedSession, error) {
	rows, err := s.db.Query(`SELECT session, path, offset, mtime, title, cwd, branch, skip FROM session_index`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]indexedSession{}
	for rows.Next() {
		var i indexedSession
		if err := rows.Scan(&i.session, &i.path, &i.offset, &i.mtime, &i.title, &i.cwd, &i.branch, &i.skip); err != nil {
			return nil, err
		}
		out[i.session] = i
	}
	return out, rows.Err()
}

func (s *Store) saveIndexed(i indexedSession, prompts []ScannedPrompt) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range prompts {
		// The same session and time is the same prompt: read twice, kept once.
		if _, err := tx.Exec(`INSERT OR IGNORE INTO session_prompts (session, at, prompt) VALUES (?, ?, ?)`, i.session, p.At, p.Prompt); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO session_index (session, path, offset, mtime, title, cwd, branch, skip) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (session) DO UPDATE SET path = excluded.path, offset = excluded.offset, mtime = excluded.mtime,
		title = excluded.title, cwd = excluded.cwd, branch = excluded.branch, skip = excluded.skip`,
		i.session, i.path, i.offset, i.mtime, i.title, i.cwd, i.branch, i.skip); err != nil {
		return err
	}
	return tx.Commit()
}

// IndexSessionPrompts reads what is new in every transcript under dir into
// the index; exclude is Sindri's own agents, marked and never read. keep,
// above zero, removes prompts older than that; zero keeps them forever.
func (s *Store) IndexSessionPrompts(dir string, exclude map[string]bool, keep time.Duration, now time.Time) error {
	if err := s.initSessionPrompts(); err != nil {
		return err
	}
	state, err := s.sessionIndex()
	if err != nil {
		return err
	}
	for _, t := range transcripts(dir, nil, time.Time{}) {
		session := strings.TrimSuffix(filepath.Base(t.path), ".jsonl")
		known, seen := state[session]
		if seen && (known.skip || (known.mtime == t.mtime.UnixMilli() && known.path == t.path)) {
			continue
		}
		if exclude[session] {
			if err := s.saveIndexed(indexedSession{session: session, path: t.path, mtime: t.mtime.UnixMilli(), skip: true}, nil); err != nil {
				return err
			}
			continue
		}
		i := known
		i.session = session
		if i.path != t.path {
			i.offset = 0
		}
		i.path = t.path
		file, err := os.Open(t.path)
		if err != nil {
			continue
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			continue
		}
		if info.Size() < i.offset {
			i.offset = 0 // rewritten: read again; the same prompts are not kept twice
		}
		_, _ = file.Seek(i.offset, io.SeekStart)
		prompts, title, cwd, branch, read := ScanTranscript(file)
		file.Close()
		i.offset += read
		i.mtime = t.mtime.UnixMilli()
		if title != "" {
			i.title = title
		}
		if cwd != "" {
			i.cwd = cwd
		}
		if branch != "" {
			i.branch = branch
		}
		if i.title == "" && len(prompts) > 0 {
			i.title = clipText(prompts[0].Prompt, 120)
		}
		if err := s.saveIndexed(i, prompts); err != nil {
			return err
		}
	}
	if keep > 0 {
		if _, err := s.db.Exec(`DELETE FROM session_prompts WHERE at < ?`, now.Add(-keep).UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

// SessionPrompts is your prompts from `from` up to `to` (epoch ms), oldest
// first, at most sessionPromptsMax, leaving out exclude; with the oldest
// prompt the index holds, nil when it holds none.
func (s *Store) SessionPrompts(from, to int64, exclude map[string]bool) ([]SessionPrompt, *int64, error) {
	if err := s.initSessionPrompts(); err != nil {
		return nil, nil, err
	}
	rows, err := s.db.Query(`SELECT p.at, p.session, i.title, i.cwd, i.branch, p.prompt FROM session_prompts p
		JOIN session_index i ON i.session = p.session WHERE p.at >= ? AND p.at < ? ORDER BY p.at, p.session LIMIT ?`, from, to, sessionPromptsMax)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []SessionPrompt{}
	for rows.Next() {
		var p SessionPrompt
		if err := rows.Scan(&p.At, &p.Session, &p.Title, &p.Cwd, &p.Branch, &p.Prompt); err != nil {
			return nil, nil, err
		}
		if exclude[p.Session] {
			continue
		}
		if p.Title == "" {
			p.Title = "(untitled)"
		}
		if p.Cwd != "" {
			p.Project = filepath.Base(p.Cwd)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var oldest sql.NullInt64
	if err := s.db.QueryRow(`SELECT min(at) FROM session_prompts`).Scan(&oldest); err != nil || !oldest.Valid {
		return out, nil, err
	}
	return out, &oldest.Int64, nil
}

// --- the route and the loop ----------------------------------------------------------

func (s *Sindri) indexing() bool {
	return s.Work != nil && s.Work.Store != nil && s.ClaudeProjects != ""
}

func (s *Sindri) mountSessionPrompts(p *part.Part) {
	p.Handle("GET /v1/sessions/prompts", s.sessionPrompts)
	if !s.indexing() {
		return
	}
	// The first run reads every transcript still on disk; later ones only what is new.
	p.Loop("session-prompts", SessionPromptsEvery, func(context.Context) error {
		own, _ := s.ownIDs()
		return s.Work.Store.IndexSessionPrompts(s.ClaudeProjects, own, s.PromptsKeep, time.Now())
	})
}

func (s *Sindri) sessionPrompts(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	from, to := now.Add(-24*time.Hour).UnixMilli(), now.UnixMilli()
	query := r.URL.Query()
	for name, into := range map[string]*int64{"from": &from, "to": &to} {
		if raw := query.Get(name); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < 0 {
				part.Error(w, http.StatusBadRequest, "`"+name+"` is epoch ms")
				return
			}
			*into = value
		}
	}
	if to <= from || time.Duration(to-from)*time.Millisecond > sessionPromptRange {
		part.Error(w, http.StatusBadRequest, "`to` is after `from`, at most 93 days on")
		return
	}
	if !s.indexing() {
		part.JSON(w, http.StatusOK, map[string]any{"prompts": []SessionPrompt{}, "oldest": nil})
		return
	}
	own, _ := s.ownIDs()
	prompts, oldest, err := s.Work.Store.SessionPrompts(from, to, own)
	if err != nil {
		fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"prompts": prompts, "oldest": oldest})
}
