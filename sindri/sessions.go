package sindri

// Your own Claude Code sessions, read from their transcripts under
// ~/.claude/projects, and the activity heatmap: sessions started, agents
// started and branches merged per day. Sindri's own agents have transcripts
// too; they are left out of the sessions.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/part"
)

// Session is one of your Claude Code sessions.
type Session struct {
	ID string `json:"id"`
	// Path is the transcript, for an agent asked about this session to read.
	Path string `json:"path"`
	// Title is Claude's own title for it, else your first message.
	Title        string `json:"title"`
	Cwd          string `json:"cwd,omitempty"`
	Project      string `json:"project,omitempty"`
	Branch       string `json:"branch,omitempty"` // the git branch Claude Code last saw
	LastActivity int64  `json:"lastActivity"`
	LastPrompt   string `json:"lastPrompt,omitempty"`
	LastText     string `json:"lastText,omitempty"`
	// Working: the transcript changed in the last two minutes and the last turn has not ended.
	Working bool `json:"working"`
}

const (
	sessionHead    = 64 << 10
	sessionTail    = 256 << 10
	sessionText    = 600
	sessionWorking = 2 * time.Minute
)

func readSlice(path string, start, length int64) []byte {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	buffer := make([]byte, length)
	n, _ := file.ReadAt(buffer, start)
	return buffer[:n]
}

type event map[string]any

// events are the whole lines of a slice: one cut mid-line loses that line, not the parse.
func events(data []byte) []event {
	out := []event{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64<<10), len(data)+1)
	for scanner.Scan() {
		var e event
		if json.Unmarshal(scanner.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

func (e event) str(key string) string {
	s, _ := e[key].(string)
	return s
}

func (e event) message() map[string]any {
	m, _ := e["message"].(map[string]any)
	return m
}

func textParts(content any) []string {
	parts := []string{}
	switch c := content.(type) {
	case string:
		parts = append(parts, c)
	case []any:
		for _, p := range c {
			if m, ok := p.(map[string]any); ok && m["type"] == "text" {
				if s, ok := m["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
	}
	return parts
}

var (
	wrapped     = regexp.MustCompile(`^\s*<[\w-]+>`)
	runsOfSpace = regexp.MustCompile(`\s+`)
)

// typed is what you typed, without the editor and harness context Claude Code wraps it in.
func typed(e event) string {
	if e["type"] != "user" || e["isMeta"] == true {
		return ""
	}
	kept := []string{}
	for _, p := range textParts(e.message()["content"]) {
		if !wrapped.MatchString(p) {
			kept = append(kept, p)
		}
	}
	return strings.TrimSpace(runsOfSpace.ReplaceAllString(strings.Join(kept, " "), " "))
}

func clipText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return text
}

// ReadSession summarises one transcript; nil when it has nothing worth naming.
func ReadSession(path string, now time.Time) *Session {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	size := info.Size()
	tail := events(readSlice(path, max(0, size-sessionTail), min(size, sessionTail)))
	head := tail
	if size > sessionTail {
		head = events(readSlice(path, 0, sessionHead))
	}
	var title, cwd, branch, lastPrompt, lastText string
	var lastTurn event
	for _, e := range tail {
		if e["type"] == "ai-title" && e.str("aiTitle") != "" {
			title = e.str("aiTitle")
		}
		if e["type"] == "last-prompt" && e.str("lastPrompt") != "" {
			lastPrompt = e.str("lastPrompt")
		}
		if e.str("cwd") != "" {
			cwd = e.str("cwd")
		}
		if b := e.str("gitBranch"); b != "" && b != "HEAD" {
			branch = b
		}
		sidechain := e["isSidechain"] == true
		if (e["type"] == "user" || e["type"] == "assistant") && !sidechain {
			lastTurn = e
		}
		if e["type"] == "assistant" && !sidechain {
			if content, ok := e.message()["content"].([]any); ok {
				if text := strings.TrimSpace(strings.Join(textParts(content), " ")); text != "" {
					lastText = text
				}
			}
		}
	}
	if title == "" {
		for _, e := range head {
			if title = typed(e); title != "" {
				break
			}
		}
	}
	if title == "" && lastText == "" {
		return nil
	}
	if title == "" {
		title = lastPrompt
	}
	if title == "" {
		title = "(untitled)"
	}
	ended := lastTurn != nil && lastTurn["type"] == "assistant" && lastTurn.message()["stop_reason"] == "end_turn"
	s := &Session{
		ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Path: path, Title: clipText(title, 120),
		LastActivity: info.ModTime().UnixMilli(), Working: !ended && now.Sub(info.ModTime()) < sessionWorking,
	}
	if cwd != "" {
		s.Cwd, s.Project = cwd, filepath.Base(cwd)
	}
	s.Branch = branch
	if lastPrompt != "" {
		s.LastPrompt = clipText(lastPrompt, 300)
	}
	if lastText != "" {
		s.LastText = clipText(runsOfSpace.ReplaceAllString(lastText, " "), sessionText)
	}
	return s
}

type transcript struct {
	path  string
	mtime time.Time
	birth time.Time
}

// transcripts are every transcript under dir changed since since, leaving out exclude.
func transcripts(dir string, exclude map[string]bool, since time.Time) []transcript {
	found := []transcript{}
	folders, _ := os.ReadDir(dir)
	for _, folder := range folders {
		files, err := os.ReadDir(filepath.Join(dir, folder.Name()))
		if err != nil {
			continue
		}
		for _, file := range files {
			name := file.Name()
			if !strings.HasSuffix(name, ".jsonl") || exclude[strings.TrimSuffix(name, ".jsonl")] {
				continue
			}
			info, err := file.Info()
			if err != nil || info.ModTime().Before(since) {
				continue
			}
			found = append(found, transcript{path: filepath.Join(dir, folder.Name(), name), mtime: info.ModTime()})
		}
	}
	return found
}

// ListSessions is the sessions active in the last day, newest first, at most limit.
func ListSessions(dir string, exclude map[string]bool, limit int, now time.Time) []Session {
	found := transcripts(dir, exclude, now.Add(-24*time.Hour))
	sort.Slice(found, func(i, j int) bool { return found[i].mtime.After(found[j].mtime) })
	out := []Session{}
	for _, t := range found[:min(len(found), limit)] {
		if s := ReadSession(t.path, now); s != nil {
			out = append(out, *s)
		}
	}
	return out
}

// DayCounts is one day of the heatmap.
type DayCounts struct {
	Sessions int `json:"sessions"`
	Agents   int `json:"agents"`
	Merges   int `json:"merges"`
}

var stamp = regexp.MustCompile(`"timestamp":"([^"]+)"`)

// SessionStart is when a transcript's session started: its first timestamp, else fallback.
func SessionStart(head []byte, fallback time.Time) time.Time {
	for _, line := range bytes.Split(head, []byte("\n")) {
		if m := stamp.FindSubmatch(line); m != nil {
			if at, err := time.Parse(time.RFC3339Nano, string(m[1])); err == nil {
				return at
			}
		}
	}
	return fallback
}

// starts remembers each transcript's start: it never changes, and reading the
// head of every transcript took seconds over WSL's filesystem.
var starts sync.Map

// SessionStarts is the start of every session transcript changed since since.
func SessionStarts(dir string, exclude map[string]bool, since time.Time) []time.Time {
	out := []time.Time{}
	for _, t := range transcripts(dir, exclude, since) {
		at, ok := starts.Load(t.path)
		if !ok {
			at = SessionStart(readSlice(t.path, 0, 16<<10), t.mtime)
			starts.Store(t.path, at)
		}
		if start := at.(time.Time); !start.Before(since) {
			out = append(out, start)
		}
	}
	return out
}

// Activity counts sessions, agents and merges per day in zone.
func Activity(sessionStarts []time.Time, agents [][2]int64, zone *time.Location, since time.Time) map[string]*DayCounts {
	days := map[string]*DayCounts{}
	add := func(at time.Time, field func(*DayCounts) *int) {
		if at.IsZero() || at.Before(since) {
			return
		}
		key := at.In(zone).Format("2006-01-02")
		if days[key] == nil {
			days[key] = &DayCounts{}
		}
		*field(days[key])++
	}
	for _, at := range sessionStarts {
		add(at, func(d *DayCounts) *int { return &d.Sessions })
	}
	for _, a := range agents {
		if a[0] > 0 {
			add(time.UnixMilli(a[0]), func(d *DayCounts) *int { return &d.Agents })
		}
		if a[1] > 0 {
			add(time.UnixMilli(a[1]), func(d *DayCounts) *int { return &d.Merges })
		}
	}
	return days
}

func (s *Sindri) ownIDs() (map[string]bool, [][2]int64) {
	s.Agents.mu.Lock()
	defer s.Agents.mu.Unlock()
	ids, times := map[string]bool{}, [][2]int64{}
	for id, a := range s.Agents.agents {
		ids[id] = true
		times = append(times, [2]int64{a.StartedAt, a.MergedAt})
	}
	return ids, times
}

func (s *Sindri) sessions(w http.ResponseWriter, _ *http.Request) {
	own, _ := s.ownIDs()
	part.JSON(w, http.StatusOK, ListSessions(s.ClaudeProjects, own, 20, time.Now()))
}

func (s *Sindri) activity(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	zone := time.Local
	if name := query.Get("tz"); name != "" {
		var err error
		if zone, err = time.LoadLocation(name); err != nil {
			part.Error(w, http.StatusBadRequest, "not a time zone: "+name)
			return
		}
	}
	days, _ := strconv.Atoi(query.Get("days"))
	if days <= 0 {
		days = 365
	}
	days = min(days, 800)
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	own, times := s.ownIDs()
	part.JSON(w, http.StatusOK, Activity(SessionStarts(s.ClaudeProjects, own, since), times, zone, since))
}
