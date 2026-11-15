package sindri

// Question agents in herdr panes. While herdr runs, a new agent that needs
// no worktree and has tools runs as an ordinary interactive Claude Code in
// a herdr workspace, next to your own sessions, where you can watch it or
// take it over. Everything else (the feed to Norn, approvals, the agent
// list) reads the same Agent record as for any other agent: the "herdr"
// loop keeps the record up to date.
//
// The answer is not read off the screen. herdr says whether the agent is
// working, at its prompt or showing a dialog; the transcript Claude Code
// writes under ~/.claude says what it answered. An approval is the dialog
// herdr read off the screen, answered by its key only while that same
// dialog is still showing.

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// HerdrPlace is where a herdr agent runs and what waits to be sent to it.
type HerdrPlace struct {
	// Name is its herdr name: ask- and the start of its id.
	Name string `json:"name"`
	// Args are Claude's own flags, without the session, reused to resume it in a new pane.
	Args []string `json:"args"`
	// PromptedAt is when it was last sent a prompt: a turn that ended before is the previous one.
	PromptedAt int64 `json:"promptedAt,omitempty"`
	// Queued is a prompt waiting for Claude to be ready for it.
	Queued *string `json:"queued,omitempty"`
	// Started is whether Claude has ever started on this session id.
	Started bool `json:"started,omitempty"`
}

const (
	herdrDeliverFor   = 15 * time.Minute
	herdrDeliverRetry = time.Second
	herdrTail         = 256 << 10
)

// UseHerdr runs question agents in herdr while it answers. claudeDir is
// where Claude Code keeps its transcripts (~/.claude).
func (m *Manager) UseHerdr(h *Herdr, claudeDir string) {
	m.mu.Lock()
	m.herdr, m.claudeDir = h, claudeDir
	var queued []string
	for id, a := range m.agents {
		if a.Herdr != nil && a.Herdr.Queued != nil {
			queued = append(queued, id)
		}
	}
	m.mu.Unlock()
	// A prompt still queued when Sindri stopped is sent now.
	for _, id := range queued {
		go m.deliver(id)
	}
}

// herdrTakes is whether a start goes to herdr: a Claude agent with tools and
// no worktree, no engine asked for, while herdr answers.
func (m *Manager) herdrTakes(request StartRequest) bool {
	return m.herdr != nil && m.herdrUp && request.Engine == "" && !request.Worktree && !request.NoTools
}

// ClaudeArgs are Claude's flags for a start, as an interactive session takes
// them; one line each, as herdr refuses control characters.
func ClaudeArgs(a *Agent, roots []string) []string {
	var args []string
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if a.PermissionMode != "" {
		args = append(args, "--permission-mode", a.PermissionMode)
	}
	if a.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", strings.Join(strings.Fields(a.AppendSystemPrompt), " "))
	}
	if a.AddRoots {
		for _, root := range roots {
			args = append(args, "--add-dir", root)
		}
	}
	// Variadic, so last.
	if len(a.DisallowedTools) > 0 {
		args = append(append(args, "--disallowedTools"), a.DisallowedTools...)
	}
	if len(a.AllowedTools) > 0 {
		args = append(append(args, "--allowedTools"), a.AllowedTools...)
	}
	return args
}

// startHerdrLocked makes the record and sends the prompt in the background.
func (m *Manager) startHerdrLocked(a *Agent, prompt string) {
	a.Engine = "herdr"
	a.Herdr = &HerdrPlace{Name: "ask-" + a.ID[:8], Args: ClaudeArgs(a, m.roots), Queued: &prompt}
	a.addLog(LogEntry{At: m.ms(), Kind: "prompt", Text: prompt})
	m.agents[a.ID] = a
	m.touchLocked(a)
	go m.deliver(a.ID)
}

// promptHerdrLocked queues a follow-up; a closed pane opens again on the same session.
func (m *Manager) promptHerdrLocked(a *Agent, text string, images []string) {
	if len(images) > 0 {
		text += "\n\nImages to look at:\n" + strings.Join(images, "\n")
	}
	a.Herdr.Queued = &text
	a.addLog(LogEntry{At: m.ms(), Kind: "prompt", Text: text})
	a.Status = Starting
	m.touchLocked(a)
	go m.deliver(a.ID)
}

// DialogID is a dialog as a request id: a different dialog is a different request.
func DialogID(screen *HerdrDialogScreen) string {
	parts := []string{screen.Question, screen.Detail}
	for _, c := range screen.Choices {
		parts = append(parts, c.Key+"."+c.Label)
	}
	sum := sha1.Sum([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])[:12]
}

// DialogSummary is what the dialog wants: the command or file for a permission, the question and its choices otherwise.
func DialogSummary(pane HerdrAgent) string {
	screen := pane.DialogScreen
	if screen == nil {
		return "an answer to a dialog in its herdr pane"
	}
	if pane.Dialog == "permission" && screen.Detail != "" {
		return strings.Join(strings.Fields(screen.Detail), " ")
	}
	choices := make([]string, len(screen.Choices))
	for i, c := range screen.Choices {
		choices[i] = c.Key + ". " + c.Label
	}
	return screen.Question + " (" + strings.Join(choices, ", ") + ")"
}

var (
	yesLabel    = regexp.MustCompile(`(?i)^yes\b`)
	noLabel     = regexp.MustCompile(`(?i)^no\b`)
	broaderYes  = regexp.MustCompile(`(?i)don'?t ask|always|session|all\b|keep allowing|from now on`)
)

// ChoiceFor is the choice a yes or a no picks: a plain "Yes" over "Yes, and don't ask again".
func ChoiceFor(choices []HerdrDialogChoice, allow bool) *HerdrDialogChoice {
	if len(choices) == 0 {
		return nil
	}
	find := func(ok func(HerdrDialogChoice) bool) *HerdrDialogChoice {
		if i := slices.IndexFunc(choices, ok); i >= 0 {
			return &choices[i]
		}
		return nil
	}
	if allow {
		if c := find(func(c HerdrDialogChoice) bool { return yesLabel.MatchString(c.Label) && !broaderYes.MatchString(c.Label) }); c != nil {
			return c
		}
		if c := find(func(c HerdrDialogChoice) bool { return yesLabel.MatchString(c.Label) }); c != nil {
			return c
		}
		return &choices[0]
	}
	if c := find(func(c HerdrDialogChoice) bool { return noLabel.MatchString(c.Label) }); c != nil {
		return c
	}
	return &choices[len(choices)-1]
}

func paneOf(a *Agent, panes []HerdrAgent) *HerdrAgent {
	for i := range panes {
		if panes[i].Name == a.Herdr.Name {
			return &panes[i]
		}
	}
	for i := range panes {
		if panes[i].SessionID == a.ID {
			return &panes[i]
		}
	}
	return nil
}

// answerHerdr picks the dialog's choice for allow or deny; a "no" with a
// reason sends the reason as the next prompt.
func (m *Manager) answerHerdr(id, requestID string, allow bool, message string) (*Agent, error) {
	panes, err := m.herdr.List(context.Background())
	if err != nil {
		return nil, httpErr(503, "herdr is not answering: %s", err)
	}
	m.mu.Lock()
	a := m.agents[id]
	pane := paneOf(a, panes)
	m.mu.Unlock()
	if pane == nil || pane.Status != "blocked" || pane.DialogScreen == nil || DialogID(pane.DialogScreen) != requestID {
		return nil, httpErr(404, "that question is no longer on screen in its herdr pane")
	}
	choice := ChoiceFor(pane.DialogScreen.Choices, allow)
	if err := m.herdr.Choose(context.Background(), pane.PaneID, DialogAnswer{Question: pane.DialogScreen.Question, Key: choice.Key, Label: choice.Label}); err != nil {
		return nil, herdrHTTPError(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	verb := "denied"
	if allow {
		verb = "allowed"
	}
	a.addLog(LogEntry{At: m.ms(), Kind: "permission", Text: verb + ": " + choice.Label})
	a.Pending, a.Status = []Pending{}, Working
	if !allow && strings.TrimSpace(message) != "" {
		m.promptHerdrLocked(a, strings.TrimSpace(message), nil)
	}
	m.touchLocked(a)
	return a, nil
}

// herdrHTTPError is a herdr failure as an answer: not running is 503, its own codes keep their meaning.
func herdrHTTPError(err error) error {
	var down *HerdrDown
	var herdrErr *HerdrError
	switch {
	case errors.As(err, &down):
		return &HTTPError{Status: 503, Message: "herdr is not running", Code: "not_running"}
	case errors.As(err, &herdrErr):
		status := 409
		switch herdrErr.Code {
		case "not_found":
			status = 404
		case "timeout":
			status = 504
		}
		return &HTTPError{Status: status, Message: herdrErr.Message, Code: herdrErr.Code}
	}
	return err
}

// deliver sends a queued prompt once Claude takes it, opening a pane first
// when there is none. While Claude starts herdr says it is not ready, and
// while a dialog shows (folder trust, an approval) it refuses, so this waits
// for you to answer that first.
func (m *Manager) deliver(id string) {
	m.mu.Lock()
	if m.delivering == nil {
		m.delivering = map[string]bool{}
	}
	if m.delivering[id] {
		m.mu.Unlock()
		return
	}
	m.delivering[id] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.delivering, id)
		m.mu.Unlock()
	}()
	ctx := context.Background()
	deadline := m.now().Add(herdrDeliverFor)
	opened := false
	fail := func(err error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if a := m.agents[id]; a != nil {
			a.Herdr.Queued = nil
			a.Status = Failed
			a.addLog(LogEntry{At: m.ms(), Kind: "error", Text: "could not send it to Claude in herdr: " + err.Error()})
			m.touchLocked(a)
		}
	}
	for {
		m.mu.Lock()
		a := m.agents[id]
		if a == nil || a.Herdr == nil || a.Herdr.Queued == nil {
			m.mu.Unlock()
			return
		}
		text, place, cwd := *a.Herdr.Queued, *a.Herdr, a.Cwd
		m.mu.Unlock()

		panes, err := m.herdr.List(ctx)
		var pane *HerdrAgent
		if err == nil {
			m.mu.Lock()
			pane = paneOf(a, panes)
			m.mu.Unlock()
		}
		switch {
		case err != nil:
		case pane == nil && !opened:
			// No pane: open one, on this session id or resuming it.
			session := []string{"--session-id", id}
			name := place.Name
			if place.Started {
				session = []string{"--resume", id}
				name = place.Name + "-" + randomSuffix()
			}
			if _, err = m.herdr.Start(ctx, HerdrStart{Kind: "claude", Cwd: cwd, Name: name, Args: append(session, place.Args...)}); err != nil {
				fail(err)
				return
			}
			opened = true
			m.mu.Lock()
			a.Herdr.Name, a.Herdr.Started = name, true
			m.touchLocked(a)
			m.mu.Unlock()
		case pane == nil:
			err = &HerdrError{Code: "agent_not_ready", Message: "its herdr pane is gone"}
		default:
			promptedAt := m.ms()
			if _, err = m.herdr.Prompt(ctx, pane.PaneID, text); err == nil {
				m.mu.Lock()
				a.Herdr.Queued, a.Herdr.PromptedAt, a.Herdr.Started = nil, promptedAt, true
				a.Status = Working
				m.touchLocked(a)
				m.mu.Unlock()
				return
			}
		}
		var herdrErr *HerdrError
		waiting := err == nil || (errors.As(err, &herdrErr) && slices.Contains([]string{"agent_not_ready", "agent_blocked", "agent_pane_busy"}, herdrErr.Code))
		var down *HerdrDown
		if errors.As(err, &down) {
			waiting = true
		}
		if !waiting || m.now().After(deadline) {
			if err == nil {
				err = errors.New("Claude never became ready")
			}
			fail(err)
			return
		}
		time.Sleep(herdrDeliverRetry)
	}
}

func randomSuffix() string {
	return strings.TrimPrefix(HerdrAgentName(""), "agent-")
}

// RefreshHerdr reads herdr's panes and each herdr agent's transcript, and
// brings the agents' records up to date. Run every couple of seconds.
func (m *Manager) RefreshHerdr(ctx context.Context) error {
	if m.herdr == nil {
		return nil
	}
	panes, err := m.herdr.List(ctx)
	m.mu.Lock()
	m.herdrUp = err == nil
	m.mu.Unlock()
	if err != nil {
		var down *HerdrDown
		if errors.As(err, &down) {
			return nil // herdr not running is a state, not a failure
		}
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.agents {
		if a.Herdr == nil {
			continue
		}
		m.refreshHerdrLocked(a, paneOf(a, panes))
	}
	return nil
}

func (m *Manager) refreshHerdrLocked(a *Agent, pane *HerdrAgent) {
	transcript := filepath.Join(m.claudeDir, "projects", EncodeProjectDir(a.Cwd), a.ID+".jsonl")
	state := ReadTranscriptState(readTail(transcript, herdrTail))
	// The turn the transcript ended is the one before the last prompt until it catches up.
	var current *TranscriptState
	if state != nil && state.At >= a.Herdr.PromptedAt {
		current = state
	}
	status, pending := a.Status, []Pending{}
	switch {
	case pane != nil && pane.Status == "blocked":
		status = NeedsApproval
		if pane.DialogScreen != nil {
			pending = append(pending, Pending{RequestID: DialogID(pane.DialogScreen), Summary: DialogSummary(*pane), At: m.ms()})
		}
	case a.Herdr.Queued != nil:
		status = Starting
	case a.Status == Failed && current == nil:
	case pane == nil:
		status = Done
	case pane.Status == "working" || pane.Status == "unknown" || current == nil || current.Phase == "busy":
		status = Working
		if current != nil && current.Tool != "" {
			a.Tool = current.Tool
		}
	default:
		status = Waiting
	}
	changed := status != a.Status || len(pending) != len(a.Pending) || (len(pending) > 0 && pending[0].RequestID != a.Pending[0].RequestID)
	if len(pending) > 0 && len(a.Pending) > 0 && pending[0].RequestID == a.Pending[0].RequestID {
		pending[0].At = a.Pending[0].At
	}
	a.Status, a.Pending = status, pending
	if current != nil && current.Phase == "turn-ended" && current.LastText != "" && current.LastText != a.Result {
		a.Result = current.LastText
		a.LastText = string([]rune(current.LastText)[:min(TextLimit, len([]rune(current.LastText)))])
		a.addLog(LogEntry{At: current.At, Kind: "text", Text: a.LastText})
		a.addNotes(current.LastText)
		a.NumTurns++
		changed = true
	}
	if changed {
		m.touchLocked(a)
	}
}
