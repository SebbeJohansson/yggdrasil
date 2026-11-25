package sindri

// herdr (https://herdr.dev), the terminal multiplexer coding agents run in,
// ported from agent-conduit (herdr/client.ts, herdr/agents.ts). Because herdr
// owns the terminal it can say what the agent in a pane is doing (it watches
// the screen, so a permission dialog shows as blocked) and type into the
// pane as you would.
//
// The API is newline-delimited JSON over a unix socket, one connection per
// request:
//
//	→ {"id":"1","method":"agent.list","params":{}}
//	← {"id":"1","result":{"type":"agent_list","agents":[...]}}
//
// A target is an agent's name or its pane's id (w1:p2).

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
)

// HerdrAgent is a coding agent in one of herdr's panes.
type HerdrAgent struct {
	PaneID      string `json:"paneId"`
	WorkspaceID string `json:"workspaceId"`
	TabID       string `json:"tabId"`
	Name        string `json:"name,omitempty"`
	// Kind is which agent: claude, opencode, codex…
	Kind string `json:"kind,omitempty"`
	// Status is idle, working, blocked (a dialog waits for you), done or unknown.
	Status string `json:"status"`
	Cwd    string `json:"cwd,omitempty"`
	Title  string `json:"title,omitempty"`
	// SessionID is the agent's own session id, when herdr was told it.
	SessionID string `json:"sessionId,omitempty"`
	Focused   bool   `json:"focused"`
	// PIDs are the pane's foreground processes; Claude Code's registry uses the same ids.
	PIDs []int `json:"pids,omitempty"`
	// Dialog is, when blocked, permission (a tool prompt) or other (for a person only).
	Dialog       string             `json:"dialog,omitempty"`
	DialogScreen *HerdrDialogScreen `json:"dialogScreen,omitempty"`
}

// HerdrDialogScreen is a dialog's question and numbered choices, read off the screen.
type HerdrDialogScreen struct {
	Question string             `json:"question"`
	Detail   string             `json:"detail,omitempty"`
	Choices  []HerdrDialogChoice `json:"choices"`
}

// HerdrDialogChoice is one choice, picked by pressing its key.
type HerdrDialogChoice struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Selected bool   `json:"selected,omitempty"`
}

// HerdrError is a request herdr answered with an error: not_found, agent_blocked, timeout…
type HerdrError struct {
	Code, Message string
}

func (e *HerdrError) Error() string { return e.Message }

// HerdrDown is herdr not answering at all: not running, or no socket.
type HerdrDown struct{ Err error }

func (e *HerdrDown) Error() string { return "herdr is not running: " + e.Err.Error() }

// Herdr talks to one herdr server.
type Herdr struct {
	// Socket is the server's socket; DefaultHerdrSocket when empty.
	Socket  string
	Timeout time.Duration
}

// DefaultHerdrSocket is where herdr's socket is, by the rules its CLI uses.
func DefaultHerdrSocket() string {
	if path := os.Getenv("HERDR_SOCKET_PATH"); path != "" {
		return path
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if config == "" {
		home, _ := os.UserHomeDir()
		config = filepath.Join(home, ".config")
	}
	if session := os.Getenv("HERDR_SESSION"); session != "" {
		return filepath.Join(config, "herdr", "sessions", session, "herdr.sock")
	}
	return filepath.Join(config, "herdr", "herdr.sock")
}

var herdrIDs atomic.Int64

// Request sends one request and decodes its result into into.
func (h *Herdr) Request(ctx context.Context, method string, params map[string]any, timeout time.Duration, into any) error {
	socket := h.Socket
	if socket == "" {
		socket = DefaultHerdrSocket()
	}
	if timeout == 0 {
		timeout = h.Timeout
	}
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	if params == nil {
		params = map[string]any{}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return &HerdrDown{err}
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	id := fmt.Sprintf("yggdrasil:%d:%d", os.Getpid(), herdrIDs.Add(1))
	payload, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return err
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64<<10), 32<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var message struct {
			ID     *string         `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(line), &message) != nil || message.ID == nil {
			continue
		}
		// Errors for unparseable requests come back with an empty id.
		if *message.ID != id && *message.ID != "" {
			continue
		}
		if message.Error != nil {
			code, text := message.Error.Code, message.Error.Message
			if code == "" {
				code = "error"
			}
			if text == "" {
				text = method + " failed"
			}
			return &HerdrError{Code: code, Message: text}
		}
		if into == nil || len(message.Result) == 0 {
			return nil
		}
		return json.Unmarshal(message.Result, into)
	}
	var timedOut net.Error
	if err := scanner.Err(); errors.As(err, &timedOut) && timedOut.Timeout() {
		return &HerdrError{Code: "timeout", Message: "herdr did not answer " + method + " in time"}
	}
	return &HerdrError{Code: "closed", Message: "herdr closed the connection before answering " + method}
}

type herdrAgentInfo struct {
	PaneID        string  `json:"pane_id"`
	WorkspaceID   string  `json:"workspace_id"`
	TabID         string  `json:"tab_id"`
	Name          *string `json:"name"`
	Agent         *string `json:"agent"`
	AgentStatus   string  `json:"agent_status"`
	Cwd           *string `json:"cwd"`
	ForegroundCwd *string `json:"foreground_cwd"`
	TitleStripped *string `json:"terminal_title_stripped"`
	Title         *string `json:"title"`
	Focused       bool    `json:"focused"`
	AgentSession  *struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	} `json:"agent_session"`
}

func firstSet(values ...*string) string {
	for _, v := range values {
		if v != nil && *v != "" {
			return *v
		}
	}
	return ""
}

var herdrStatuses = []string{"idle", "working", "blocked", "done", "unknown"}

func (info herdrAgentInfo) agent() HerdrAgent {
	status := info.AgentStatus
	if !slices.Contains(herdrStatuses, status) {
		status = "unknown"
	}
	a := HerdrAgent{PaneID: info.PaneID, WorkspaceID: info.WorkspaceID, TabID: info.TabID, Status: status, Focused: info.Focused,
		Name: firstSet(info.Name), Kind: firstSet(info.Agent), Cwd: firstSet(info.ForegroundCwd, info.Cwd), Title: firstSet(info.TitleStripped, info.Title)}
	if info.AgentSession != nil && info.AgentSession.Kind == "id" {
		a.SessionID = info.AgentSession.Value
	}
	return a
}

// --- dialogs ---------------------------------------------------------------------

var (
	permissionAsk = regexp.MustCompile(`(?i)Do you want to (proceed|make this edit|create|allow|delete|run|fetch)`)
	yesOne        = regexp.MustCompile(`\b1\.\s+Yes\b`)
	wordNo        = regexp.MustCompile(`\bNo\b`)
	trustFolder   = regexp.MustCompile(`(?i)trust this folder`)
	choiceLine    = regexp.MustCompile(`^\s*(❯\s*)?(\d)\.\s+(.*\S)\s*$`)
	ruleLine      = regexp.MustCompile(`^\s*─{10,}\s*$`)
	dottedLine    = regexp.MustCompile(`^\s*╌{10,}\s*$`)
	dialogFooter  = regexp.MustCompile(`(?i)Esc to cancel|Enter to confirm|Tab to amend`)
	farRight      = regexp.MustCompile(`^\s{30,}`)
	pinnedRight   = regexp.MustCompile(`(\S)\s{8,}\S.*$`)
	leadingBlank  = regexp.MustCompile(`^\s*\n`)
)

const dialogDetailLines = 16

// DialogOf says which dialog a blocked agent shows: a Claude Code tool
// permission prompt, recognised by its wording, or other, for a person. The
// same key that allows a tool on one quits the session on another.
func DialogOf(kind, screen string) string {
	if kind == "claude" && permissionAsk.MatchString(screen) && yesOne.MatchString(screen) && wordNo.MatchString(screen) && !trustFolder.MatchString(screen) {
		return "permission"
	}
	return "other"
}

func dialogLines(screen string) []string {
	lines := strings.Split(screen, "\n")
	for i, line := range lines {
		if farRight.MatchString(line) {
			lines[i] = ""
		} else {
			lines[i] = strings.TrimRight(pinnedRight.ReplaceAllString(line, "$1"), " \t")
		}
	}
	return lines
}

// DialogScreenOf reads a Claude Code dialog off the screen: the question, the
// detail above it, and its numbered choices; nil without a numbered list.
func DialogScreenOf(screen string) *HerdrDialogScreen {
	lines := dialogLines(screen)
	type numbered struct {
		index, number int
		selected      bool
		label         string
	}
	var found []numbered
	for i, line := range lines {
		if m := choiceLine.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[2])
			found = append(found, numbered{i, n, m[1] != "", strings.TrimSpace(m[3])})
		}
	}
	if len(found) == 0 {
		return nil
	}
	// The last run numbered 1, 2, 3… on screen.
	run := []numbered{found[len(found)-1]}
	for i := len(found) - 2; i >= 0; i-- {
		if run[0].number == 1 || found[i].number != run[0].number-1 {
			break
		}
		run = append([]numbered{found[i]}, run...)
	}
	if len(run) < 2 || run[0].number != 1 {
		return nil
	}
	choices := make([]HerdrDialogChoice, len(run))
	for at, c := range run {
		end := len(lines)
		if at+1 < len(run) {
			end = run[at+1].index
		}
		label := []string{c.label}
		for i := c.index + 1; i < end; i++ {
			if strings.TrimSpace(lines[i]) == "" || dialogFooter.MatchString(lines[i]) {
				break
			}
			label = append(label, strings.TrimSpace(lines[i]))
		}
		choices[at] = HerdrDialogChoice{Key: strconv.Itoa(c.number), Label: strings.Join(label, " "), Selected: c.selected}
	}
	questionAt := run[0].index - 1
	for questionAt >= 0 && strings.TrimSpace(lines[questionAt]) == "" {
		questionAt--
	}
	if questionAt < 0 || ruleLine.MatchString(lines[questionAt]) {
		return nil
	}
	top := questionAt
	for top > 0 && questionAt-top < dialogDetailLines && !ruleLine.MatchString(lines[top-1]) {
		top--
	}
	var detail []string
	for _, line := range lines[top:questionAt] {
		if !dottedLine.MatchString(line) {
			detail = append(detail, line)
		}
	}
	return &HerdrDialogScreen{Question: strings.TrimSpace(lines[questionAt]), Detail: strings.TrimRightFunc(leadingBlank.ReplaceAllString(strings.Join(detail, "\n"), ""), unicode.IsSpace), Choices: choices}
}

// --- what can be done -------------------------------------------------------------

func (h *Herdr) screen(ctx context.Context, target string) (string, error) {
	var result struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	err := h.Request(ctx, "agent.read", map[string]any{"target": target, "source": "detection", "strip_ansi": true}, 0, &result)
	return result.Read.Text, err
}

func (h *Herdr) foregroundPIDs(ctx context.Context, pane string) []int {
	var info struct {
		ProcessInfo struct {
			Foreground []struct {
				PID *int `json:"pid"`
			} `json:"foreground_processes"`
		} `json:"process_info"`
	}
	if h.Request(ctx, "pane.process_info", map[string]any{"pane_id": pane}, 0, &info) != nil {
		return nil
	}
	var pids []int
	for _, p := range info.ProcessInfo.Foreground {
		if p.PID != nil {
			pids = append(pids, *p.PID)
		}
	}
	return pids
}

// List is every pane herdr sees a coding agent in, with its foreground
// processes and, when blocked, its dialog.
func (h *Herdr) List(ctx context.Context) ([]HerdrAgent, error) {
	var result struct {
		Agents []herdrAgentInfo `json:"agents"`
	}
	if err := h.Request(ctx, "agent.list", nil, 0, &result); err != nil {
		return nil, err
	}
	agents := make([]HerdrAgent, len(result.Agents))
	for i, info := range result.Agents {
		a := info.agent()
		a.PIDs = h.foregroundPIDs(ctx, a.PaneID)
		if a.Status == "blocked" {
			a.Dialog = "other"
			if screen, err := h.screen(ctx, a.PaneID); err == nil {
				a.Dialog = DialogOf(a.Kind, screen)
				if a.Kind == "claude" {
					a.DialogScreen = DialogScreenOf(screen)
				}
			}
		}
		agents[i] = a
	}
	return agents, nil
}

// Prompt types text into the agent's pane and presses Enter. herdr refuses
// (agent_blocked) while a dialog shows, so a prompt never lands in one.
func (h *Herdr) Prompt(ctx context.Context, target, text string) (HerdrAgent, error) {
	var result struct {
		Agent herdrAgentInfo `json:"agent"`
	}
	err := h.Request(ctx, "agent.prompt", map[string]any{"target": target, "text": text}, 0, &result)
	return result.Agent.agent(), err
}

// Keys presses keys in the pane: enter, esc, 1, ctrl+c…
func (h *Herdr) Keys(ctx context.Context, target string, keys []string) error {
	return h.Request(ctx, "agent.send_keys", map[string]any{"target": target, "keys": keys}, 0, nil)
}

// Read is the agent's recent output as plain text.
func (h *Herdr) Read(ctx context.Context, target string, lines int) (string, error) {
	var result struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	err := h.Request(ctx, "agent.read", map[string]any{"target": target, "source": "recent_unwrapped", "lines": lines, "strip_ansi": true}, 0, &result)
	return result.Read.Text, err
}

// DialogAnswer is one of a dialog's choices, as the person picking it saw it.
type DialogAnswer struct {
	Question string `json:"question"`
	Key      string `json:"key"`
	Label    string `json:"label"`
}

// Choose picks a choice by its key, but only while that same dialog is on
// screen: the same digit means something else in the next dialog.
func (h *Herdr) Choose(ctx context.Context, target string, answer DialogAnswer) error {
	screen, err := h.screen(ctx, target)
	if err != nil {
		return err
	}
	if shown := DialogScreenOf(screen); shown != nil && shown.Question == answer.Question {
		for _, c := range shown.Choices {
			if c.Key == answer.Key && c.Label == answer.Label {
				return h.Keys(ctx, target, []string{c.Key})
			}
		}
	}
	return &HerdrError{Code: "dialog_changed", Message: fmt.Sprintf("the dialog in %s is no longer %q with %s. %s", target, answer.Question, answer.Key, answer.Label)}
}

var (
	nameUnsafe  = regexp.MustCompile(`[^a-z0-9_-]+`)
	nameLeading = regexp.MustCompile(`^[^a-z]+`)
)

// HerdrAgentName is a herdr name from a folder: lowercase, starting with a letter, at most 32 characters.
func HerdrAgentName(directory string) string {
	return PrefixedHerdrName("", directory)
}

// PrefixedHerdrName is HerdrAgentName after prefix, still within herdr's
// 32 characters; prefix must itself be a valid name start, such as "cloud-".
func PrefixedHerdrName(prefix, directory string) string {
	suffix := make([]byte, 2)
	_, _ = rand.Read(suffix)
	base := nameLeading.ReplaceAllString(nameUnsafe.ReplaceAllString(strings.ToLower(DirectoryName(directory)), "-"), "")
	if limit := 26 - len(prefix); len(base) > limit {
		base = base[:limit]
	}
	if base == "" {
		base = "agent"
	}
	return prefix + base + "-" + hex.EncodeToString(suffix)
}

// HerdrStart is an agent to start in a new herdr workspace.
type HerdrStart struct {
	Kind, Cwd, Name string
	Args            []string
}

const (
	shellReady = 15 * time.Second
	shellRetry = 300 * time.Millisecond
)

// Start starts an agent in a new workspace without taking focus, and
// returns once herdr sees it ready. A new workspace's shell is still running
// its startup files at first (agent_pane_busy), so the start is retried; if
// it never takes, the workspace is closed again.
func (h *Herdr) Start(ctx context.Context, start HerdrStart) (HerdrAgent, error) {
	if start.Name == "" {
		start.Name = HerdrAgentName(start.Cwd)
	}
	if start.Args == nil {
		start.Args = []string{}
	}
	var created struct {
		Workspace *struct {
			ID string `json:"workspace_id"`
		} `json:"workspace"`
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	if err := h.Request(ctx, "workspace.create", map[string]any{"cwd": start.Cwd, "label": start.Name, "focus": false}, 0, &created); err != nil {
		return HerdrAgent{}, err
	}
	params := map[string]any{"name": start.Name, "kind": start.Kind, "pane_id": created.RootPane.PaneID, "args": start.Args, "timeout_ms": 30_000}
	deadline := time.Now().Add(shellReady)
	for {
		var result struct {
			Agent herdrAgentInfo `json:"agent"`
		}
		err := h.Request(ctx, "agent.start", params, 35*time.Second, &result)
		if err == nil {
			return result.Agent.agent(), nil
		}
		var herdrErr *HerdrError
		if !errors.As(err, &herdrErr) || herdrErr.Code != "agent_pane_busy" || time.Now().After(deadline) {
			workspace := strings.SplitN(created.RootPane.PaneID, ":", 2)[0]
			if created.Workspace != nil && created.Workspace.ID != "" {
				workspace = created.Workspace.ID
			}
			if workspace != "" {
				_ = h.Request(ctx, "workspace.close", map[string]any{"workspace_id": workspace}, 0, nil)
			}
			return HerdrAgent{}, err
		}
		select {
		case <-ctx.Done():
			return HerdrAgent{}, ctx.Err()
		case <-time.After(shellRetry):
		}
	}
}
