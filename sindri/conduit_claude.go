package sindri

// Claude Code sessions as they run right now, ported from agent-conduit
// (claude/paths.ts, claude/status.ts, claude/live.ts, resume.ts).
//
// Two directories carry everything needed:
//
//	~/.claude/sessions/<pid>.json         one per session ever started: id, cwd, name
//	/run/user/<uid>/cc-socks/<pid>.sock   one per session running right now
//
// The registry is never pruned, so the socket listing is what makes it
// useful: intersecting the two gives the live sessions exactly. State then
// comes from the tail of each session's transcript, where stop_reason on
// the last assistant entry says it all: tool_use is busy, end_turn is your
// move.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EncodeProjectDir guesses Claude Code's project folder name for a directory:
// every character that is not a letter or digit becomes a dash. Lossy, so a
// caller that needs certainty searches the folders when the guess misses.
func EncodeProjectDir(directory string) string {
	return nonAlnum.ReplaceAllString(directory, "-")
}

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// TranscriptState is what a transcript ends in.
type TranscriptState struct {
	// Phase is busy while the agent works, turn-ended when it is your move.
	Phase string `json:"phase"`
	// Tool is the tool running, when the agent stopped to call one.
	Tool string `json:"tool,omitempty"`
	// Ending is how a turn ended when not normally: error or interrupted.
	Ending string `json:"ending,omitempty"`
	// LastText is Claude's last words when the turn ended, cut to LastTextMax.
	LastText string `json:"lastText,omitempty"`
	// At is when the entry was written, epoch milliseconds; 0 when it does not say.
	At int64 `json:"at,omitempty"`
}

// LastTextMax is enough of the last message to find what it asks of you.
const LastTextMax = 4000

var (
	turnEnding  = map[string]bool{"end_turn": true, "stop_sequence": true, "max_tokens": true}
	interrupted = regexp.MustCompile(`^\[Request interrupted by user`)
)

type transcriptEntry struct {
	Type              string `json:"type"`
	IsMeta            bool   `json:"isMeta"`
	IsCompactSummary  bool   `json:"isCompactSummary"`
	IsSidechain       bool   `json:"isSidechain"`
	IsAPIErrorMessage bool   `json:"isApiErrorMessage"`
	Timestamp         string `json:"timestamp"`
	Message           *struct {
		StopReason *string `json:"stop_reason"`
		Content    any     `json:"content"`
	} `json:"message"`
}

// contentText is the text blocks of a message, joined; a plain string is its own text.
func contentText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		parts := make([]string, 0, len(c))
		for _, block := range c {
			if m, ok := block.(map[string]any); ok && m["type"] == "text" {
				text, _ := m["text"].(string)
				parts = append(parts, text)
			} else {
				parts = append(parts, "")
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return ""
}

func lastWords(text string) string {
	flat := strings.TrimSpace(text)
	if runes := []rune(flat); len(runes) > LastTextMax {
		return "…" + string(runes[len(runes)-(LastTextMax-1):])
	}
	return flat
}

func toolNameOf(content any) string {
	blocks, _ := content.([]any)
	name := ""
	for _, block := range blocks {
		if m, ok := block.(map[string]any); ok && m["type"] == "tool_use" {
			name, _ = m["name"].(string)
		}
	}
	return name
}

// ReadTranscriptState reads the state out of the end of a transcript; nil
// when the tail holds no conversation yet. tail may start mid-line.
func ReadTranscriptState(tail string) *TranscriptState {
	lines := strings.Split(tail, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var e transcriptEntry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		// Only user and assistant entries say what the agent is doing; the rest
		// is bookkeeping. isMeta is injected context; a compaction summary is
		// written as a user turn but is neither side speaking.
		if (e.Type != "user" && e.Type != "assistant") || e.IsMeta || e.IsCompactSummary {
			continue
		}
		state := &TranscriptState{Phase: "busy"}
		if at, err := time.Parse(time.RFC3339Nano, e.Timestamp); err == nil {
			state.At = at.UnixMilli()
		}
		var content any
		if e.Message != nil {
			content = e.Message.Content
		}
		switch {
		case e.IsSidechain:
			// A subagent's turn: its parent waits on the tool call that spawned it.
		case e.Type == "assistant":
			stop := ""
			if e.Message != nil && e.Message.StopReason != nil {
				stop = *e.Message.StopReason
			}
			if stop == "tool_use" {
				state.Tool = toolNameOf(content)
			} else if turnEnding[stop] {
				state.Phase = "turn-ended"
				if e.IsAPIErrorMessage {
					state.Ending = "error"
				}
				state.LastText = lastWords(contentText(content))
			}
		case interrupted.MatchString(contentText(content)):
			// You pressed Escape: your move, though not a finished answer.
			state.Phase, state.Ending = "turn-ended", "interrupted"
		}
		return state
	}
	return nil
}

// TranscriptTitles are the titles a transcript carries: custom from a rename, which wins, and ai.
type TranscriptTitles struct {
	Custom, AI string
}

// ReadTranscriptTitles reads the latest titles out of a tail; empty fields when it holds none.
func ReadTranscriptTitles(tail string) TranscriptTitles {
	var titles TranscriptTitles
	for _, line := range strings.Split(tail, "\n") {
		if !strings.Contains(line, `-title"`) {
			continue
		}
		var e struct {
			Type        string `json:"type"`
			CustomTitle string `json:"customTitle"`
			AITitle     string `json:"aiTitle"`
		}
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if e.Type == "custom-title" && strings.TrimSpace(e.CustomTitle) != "" {
			titles.Custom = strings.TrimSpace(e.CustomTitle)
		} else if e.Type == "ai-title" && strings.TrimSpace(e.AITitle) != "" {
			titles.AI = strings.TrimSpace(e.AITitle)
		}
	}
	return titles
}

// readTail is the last bytes of a file as text, or "" when it is gone.
func readTail(path string, bytes int64) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	size := info.Size()
	return string(readSlice(path, max(0, size-bytes), min(size, bytes)))
}

// --- live sessions -----------------------------------------------------------

// ClaudeRoot is one Claude Code state directory, and its socket directory when there is one.
type ClaudeRoot struct {
	ClaudeDir string
	// SocketDir holds a <pid>.sock per live session; without it liveness is a guess.
	SocketDir string
}

const (
	liveTail       = 32 << 10
	liveWideTail   = 512 << 10
	registryTTL    = time.Minute
	indexTTL       = 30 * time.Second
	DegradedMaxAge = 15 * time.Minute
)

type registryEntry struct {
	PID             int    `json:"pid"`
	SessionID       string `json:"sessionId"`
	Cwd             string `json:"cwd"`
	StartedAt       *int64 `json:"startedAt"`
	Name            string `json:"name"`
	NameSource      string `json:"nameSource"`
	Entrypoint      string `json:"entrypoint"`
	BridgeSessionID string `json:"bridgeSessionId"`
}

// LiveClaudeSession is one running Claude Code session.
type LiveClaudeSession struct {
	SessionID string `json:"sessionId"`
	PID       int    `json:"pid"`
	Directory string `json:"directory"`
	// Name is a name you gave it, else its title, else the registry's placeholder, else the folder.
	Name            string             `json:"name"`
	StartedAt       *int64             `json:"startedAt,omitempty"`
	Entrypoint      string             `json:"entrypoint,omitempty"`
	BridgeSessionID string             `json:"bridgeSessionId,omitempty"`
	TranscriptPath  string             `json:"transcriptPath,omitempty"`
	LastActivity    int64              `json:"lastActivity"`
	State           *TranscriptState   `json:"state"`
	Permission      *PendingPermission `json:"permission,omitempty"`
}

// PendingPermission is a permission prompt a session shows, known from the
// agent-conduit Claude Code hook's marker.
type PendingPermission struct {
	Tool  string `json:"tool,omitempty"`
	Since int64  `json:"since"`
}

// LiveClaudeCache remembers what is worth not reading again; one per root.
type LiveClaudeCache struct {
	mu       sync.Mutex
	registry map[int]cachedRegistry
	paths    map[string]string
	states   map[string]cachedState
	titles   map[string]*seededTitles
	indexed  time.Time
}

type cachedRegistry struct {
	entry  *registryEntry
	readAt time.Time
}

type cachedState struct {
	modified time.Time
	state    *TranscriptState
}

type seededTitles struct {
	TranscriptTitles
	seeded bool
}

// NewLiveClaudeCache is an empty cache.
func NewLiveClaudeCache() *LiveClaudeCache {
	return &LiveClaudeCache{registry: map[int]cachedRegistry{}, paths: map[string]string{}, states: map[string]cachedState{}, titles: map[string]*seededTitles{}}
}

var (
	sockName = regexp.MustCompile(`^(\d+)\.sock$`)
	jsonName = regexp.MustCompile(`^(\d+)\.json$`)
)

func pidsIn(dir string, pattern *regexp.Regexp) []int {
	entries, _ := os.ReadDir(dir)
	var pids []int
	for _, e := range entries {
		if m := pattern.FindStringSubmatch(e.Name()); m != nil {
			pid, _ := strconv.Atoi(m[1])
			pids = append(pids, pid)
		}
	}
	return pids
}

func (c *LiveClaudeCache) registryEntry(root ClaudeRoot, pid int, now time.Time) *registryEntry {
	if cached, ok := c.registry[pid]; ok && now.Sub(cached.readAt) < registryTTL {
		return cached.entry
	}
	var entry *registryEntry
	data, _ := os.ReadFile(filepath.Join(root.ClaudeDir, "sessions", fmt.Sprintf("%d.json", pid)))
	var parsed registryEntry
	if json.Unmarshal(data, &parsed) == nil && parsed.SessionID != "" && parsed.Cwd != "" {
		entry = &parsed
	}
	c.registry[pid] = cachedRegistry{entry, now}
	return entry
}

func (c *LiveClaudeCache) index(root ClaudeRoot, now time.Time) {
	if now.Sub(c.indexed) < indexTTL {
		return
	}
	c.indexed = now
	projects := filepath.Join(root.ClaudeDir, "projects")
	folders, _ := os.ReadDir(projects)
	for _, folder := range folders {
		files, _ := os.ReadDir(filepath.Join(projects, folder.Name()))
		for _, f := range files {
			if id, ok := strings.CutSuffix(f.Name(), ".jsonl"); ok {
				c.paths[id] = filepath.Join(projects, folder.Name(), f.Name())
			}
		}
	}
}

func (c *LiveClaudeCache) transcript(root ClaudeRoot, entry *registryEntry, now time.Time) (string, time.Time, bool) {
	for _, candidate := range []string{c.paths[entry.SessionID], filepath.Join(root.ClaudeDir, "projects", EncodeProjectDir(entry.Cwd), entry.SessionID+".jsonl")} {
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			c.paths[entry.SessionID] = candidate
			return candidate, info.ModTime(), true
		}
	}
	delete(c.paths, entry.SessionID)
	c.index(root, now)
	if found := c.paths[entry.SessionID]; found != "" {
		if info, err := os.Stat(found); err == nil {
			return found, info.ModTime(), true
		}
	}
	return "", time.Time{}, false
}

func (c *LiveClaudeCache) state(sessionID, path string, modified time.Time) *TranscriptState {
	if cached, ok := c.states[path]; ok && cached.modified.Equal(modified) {
		return cached.state
	}
	titles := c.titles[sessionID]
	if titles == nil {
		titles = &seededTitles{}
		c.titles[sessionID] = titles
	}
	merge := func(t TranscriptTitles) {
		if t.Custom != "" {
			titles.Custom = t.Custom
		}
		if t.AI != "" {
			titles.AI = t.AI
		}
	}
	tail := readTail(path, liveTail)
	state := ReadTranscriptState(tail)
	merge(ReadTranscriptTitles(tail))
	// The window may sit inside one huge entry, or a long tool result may have
	// pushed the title out of it; the second needs searching once per session.
	if state == nil || (!titles.seeded && titles.Custom == "" && titles.AI == "") {
		tail = readTail(path, liveWideTail)
		if state == nil {
			state = ReadTranscriptState(tail)
		}
		merge(ReadTranscriptTitles(tail))
	}
	titles.seeded = true
	c.states[path] = cachedState{modified, state}
	return state
}

// DirectoryName is a path's last segment.
func DirectoryName(directory string) string {
	parts := strings.FieldsFunc(directory, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return directory
	}
	return parts[len(parts)-1]
}

func sessionName(entry *registryEntry, titles *seededTitles) string {
	derived := entry.NameSource == "" || entry.NameSource == "derived"
	if entry.Name != "" && !derived {
		return entry.Name
	}
	if titles != nil && titles.Custom != "" {
		return titles.Custom
	}
	if titles != nil && titles.AI != "" {
		return titles.AI
	}
	if entry.Name != "" {
		return entry.Name
	}
	return DirectoryName(entry.Cwd)
}

// pendingPermission is the prompt a session shows, from the hook's marker. A
// hook can miss, so the marker only counts while the session is busy and
// nothing has been written since the prompt opened.
func pendingPermission(root ClaudeRoot, sessionID string, state *TranscriptState) *PendingPermission {
	if state == nil || state.Phase != "busy" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(root.ClaudeDir, "agent-conduit", "permissions", sessionID+".json"))
	if err != nil {
		return nil
	}
	var marker struct {
		At   *float64 `json:"at"`
		Tool string   `json:"tool"`
	}
	if json.Unmarshal(data, &marker) != nil || marker.At == nil || int64(*marker.At) < state.At {
		return nil
	}
	return &PendingPermission{Since: int64(*marker.At), Tool: marker.Tool}
}

// ReadLiveClaudeSessions is the sessions running in a root right now.
// Degraded is true when there is no socket directory to prove liveness with,
// so recently active registry entries stand in.
func ReadLiveClaudeSessions(root ClaudeRoot, cache *LiveClaudeCache, now time.Time) (sessions []LiveClaudeSession, degraded bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	degraded = root.SocketDir == ""
	var pids []int
	if degraded {
		pids = pidsIn(filepath.Join(root.ClaudeDir, "sessions"), jsonName)
	} else {
		pids = pidsIn(root.SocketDir, sockName)
	}
	sessions = []LiveClaudeSession{}
	for _, pid := range pids {
		entry := cache.registryEntry(root, pid, now)
		if entry == nil {
			continue
		}
		path, modified, found := cache.transcript(root, entry, now)
		var state *TranscriptState
		lastActivity := now.UnixMilli()
		if entry.StartedAt != nil {
			lastActivity = *entry.StartedAt
		}
		if found {
			state = cache.state(entry.SessionID, path, modified)
			lastActivity = modified.UnixMilli()
		}
		if degraded && now.UnixMilli()-lastActivity > DegradedMaxAge.Milliseconds() {
			continue
		}
		sessions = append(sessions, LiveClaudeSession{
			SessionID: entry.SessionID, PID: pid, Directory: entry.Cwd, Name: sessionName(entry, cache.titles[entry.SessionID]),
			StartedAt: entry.StartedAt, Entrypoint: entry.Entrypoint, BridgeSessionID: entry.BridgeSessionID,
			TranscriptPath: path, LastActivity: lastActivity, State: state, Permission: pendingPermission(root, entry.SessionID, state),
		})
	}
	return sessions, degraded
}

// --- resuming -------------------------------------------------------------------

var sessionIDPattern = regexp.MustCompile(`^\w[\w-]{0,127}$`)

// ResumeArgs are an agent's own arguments for resuming a session, built from
// the id so a request cannot add flags of its own.
func ResumeArgs(kind, sessionID string) ([]string, error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return nil, httpErr(400, "not a session id: %s", sessionID)
	}
	switch kind {
	case "claude":
		return []string{"--resume", sessionID}, nil
	case "opencode":
		return []string{"--session", sessionID}, nil
	}
	return nil, httpErr(400, "cannot resume a %s session", kind)
}

var plainWord = regexp.MustCompile(`^[\w./-]+$`)

func shellQuote(value string) string {
	if plainWord.MatchString(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// ResumeCommand is the shell command that picks a session up in a terminal.
func ResumeCommand(agent, sessionID, directory string) string {
	resume := "claude --resume " + shellQuote(sessionID)
	if agent == "opencode" {
		resume = "opencode --session " + shellQuote(sessionID)
	}
	if directory == "" {
		return resume
	}
	return "cd " + shellQuote(directory) + " && " + resume
}
