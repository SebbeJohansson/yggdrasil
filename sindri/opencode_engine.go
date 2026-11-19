package sindri

// Coding agents in OpenCode. With a running `opencode serve` (OPENCODE_URL),
// an agent started with engine "opencode" is an OpenCode session in its own
// worktree: Sindri makes the worktree as for any coding agent, opens a session
// there and sends the prompt; the "opencode" loop reads the server's snapshot
// and keeps the agent's record up to date (working, waiting on an approval,
// done with its turn). Review, checks and merge read the worktree, so they
// work the same as for Claude. Races use it as the "opencode" conduit.
//
// OpenCode decides its own tool permissions (its config); Sindri's allow and
// deny lists are Claude's and do not reach it. An approval it asks for shows
// as the agent's pending request and is answered through the server.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OpencodePlace is where an OpenCode agent's session is.
type OpencodePlace struct {
	SessionID string `json:"sessionId,omitempty"`
	Directory string `json:"directory"`
	// PromptedAt is when it was last sent a prompt.
	PromptedAt int64 `json:"promptedAt,omitempty"`
	// SeenBusy is whether the server has said it is working since the last prompt.
	SeenBusy bool `json:"seenBusy,omitempty"`
}

// opencodeQuiet is how long a session may stay idle after a prompt before
// its turn counts as over without ever having been seen working.
const opencodeQuiet = 30 * time.Second

// UseOpencode lets agents run in OpenCode through its server; nil turns it off.
func (m *Manager) UseOpencode(c *OpencodeClient) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opencode = c
}

// OpencodeSetUp is whether OpenCode is set up here at all (OPENCODE_URL is not
// off), whether or not its server answers.
func (m *Manager) OpencodeSetUp() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opencode != nil
}

// OpencodeOn is whether OpenCode agents can start: a server is set up and
// its last read worked.
func (m *Manager) OpencodeOn() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opencode != nil && m.opencodeUp
}

func (m *Manager) startOpencodeLocked(a *Agent, prompt string) {
	a.Engine = "opencode"
	a.Opencode = &OpencodePlace{Directory: a.Cwd, PromptedAt: m.ms()}
	a.addLog(LogEntry{At: m.ms(), Kind: "prompt", Text: prompt})
	m.agents[a.ID] = a
	m.touchLocked(a)
	go m.openOpencode(a.ID, a.Label, a.AppendSystemPrompt, prompt)
}

type opencodeMessage struct {
	Parts  []map[string]string `json:"parts"`
	System string              `json:"system,omitempty"`
}

func textMessage(text, system string) opencodeMessage {
	return opencodeMessage{Parts: []map[string]string{{"type": "text", "text": text}}, System: system}
}

// openOpencode opens the session and sends the first prompt.
func (m *Manager) openOpencode(id, title, system, prompt string) {
	m.mu.Lock()
	client, a := m.opencode, m.agents[id]
	directory := a.Opencode.Directory
	m.mu.Unlock()
	ctx := context.Background()
	var session struct {
		ID string `json:"id"`
	}
	err := errors.New("OpenCode is off here (no OPENCODE_URL)")
	if client != nil {
		err = client.do(ctx, http.MethodPost, "/session", directory, map[string]string{"title": title}, &session)
	}
	if err == nil && session.ID == "" {
		err = errors.New("OpenCode made no session")
	}
	if err == nil {
		err = client.do(ctx, http.MethodPost, "/session/"+url.PathEscape(session.ID)+"/prompt_async", directory, textMessage(prompt, system), nil)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		a.Status = Failed
		a.addLog(LogEntry{At: m.ms(), Kind: "error", Text: "could not start OpenCode in " + directory + ": " + err.Error()})
	} else {
		a.Opencode.SessionID, a.Status = session.ID, Working
	}
	m.touchLocked(a)
}

// promptOpencodeLocked sends a follow-up to the agent's session.
func (m *Manager) promptOpencodeLocked(a *Agent, text string) error {
	if m.opencode == nil {
		return httpErr(503, "this agent runs in OpenCode, and OpenCode is off here")
	}
	if a.Opencode.SessionID == "" {
		return httpErr(409, "its OpenCode session has not opened yet")
	}
	client, place := m.opencode, *a.Opencode
	a.Opencode.PromptedAt, a.Opencode.SeenBusy = m.ms(), false
	a.Status = Working
	a.addLog(LogEntry{At: m.ms(), Kind: "prompt", Text: text})
	m.touchLocked(a)
	id := a.ID
	go func() {
		err := client.do(context.Background(), http.MethodPost, "/session/"+url.PathEscape(place.SessionID)+"/prompt_async", place.Directory, textMessage(text, ""), nil)
		if err != nil {
			m.mu.Lock()
			defer m.mu.Unlock()
			if a := m.agents[id]; a != nil {
				a.Status = Failed
				a.addLog(LogEntry{At: m.ms(), Kind: "error", Text: "OpenCode did not take the prompt: " + err.Error()})
				m.touchLocked(a)
			}
		}
	}()
	return nil
}

// abortOpencodeLocked stops the session's turn.
func (m *Manager) abortOpencodeLocked(a *Agent) {
	if m.opencode != nil && a.Opencode.SessionID != "" {
		client, place := m.opencode, *a.Opencode
		go func() {
			_ = client.do(context.Background(), http.MethodPost, "/session/"+url.PathEscape(place.SessionID)+"/abort", place.Directory, nil, nil)
		}()
	}
	a.Status, a.Pending = Aborted, []Pending{}
	a.addLog(LogEntry{At: m.ms(), Kind: "exit", Text: "aborted"})
	m.touchLocked(a)
}

// answerOpencode answers the approval the session asked for.
func (m *Manager) answerOpencode(id, requestID string, allow bool) (*Agent, error) {
	m.mu.Lock()
	a, err := m.getLocked(id)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	client, directory := m.opencode, a.Opencode.Directory
	found := false
	for _, p := range a.Pending {
		found = found || p.RequestID == requestID
	}
	m.mu.Unlock()
	if client == nil {
		return nil, httpErr(503, "this agent runs in OpenCode, and OpenCode is off here")
	}
	if !found {
		return nil, httpErr(404, "no pending permission %s", requestID)
	}
	reply := "reject"
	if allow {
		reply = "once"
	}
	if err := client.ReplyPermission(context.Background(), requestID, directory, reply); err != nil {
		return nil, httpErr(502, "OpenCode did not take the answer: %s", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	verb := "denied"
	if allow {
		verb = "allowed"
	}
	a.addLog(LogEntry{At: m.ms(), Kind: "permission", Text: verb + ": " + requestID})
	a.Pending, a.Status = []Pending{}, Working
	m.touchLocked(a)
	return a, nil
}

// lastOpencodeText is the last thing the session's assistant wrote.
func lastOpencodeText(ctx context.Context, client *OpencodeClient, place OpencodePlace) string {
	var messages []struct {
		Info  map[string]any   `json:"info"`
		Parts []map[string]any `json:"parts"`
	}
	if client.do(ctx, http.MethodGet, "/session/"+url.PathEscape(place.SessionID)+"/message", place.Directory, nil, &messages) != nil {
		return ""
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if asText(messages[i].Info["role"]) != "assistant" {
			continue
		}
		var texts []string
		for _, part := range messages[i].Parts {
			if asText(part["type"]) == "text" && strings.TrimSpace(asText(part["text"])) != "" {
				texts = append(texts, strings.TrimSpace(asText(part["text"])))
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n\n")
		}
	}
	return ""
}

// RefreshOpencode reads the server and brings each OpenCode agent's record
// up to date. Run by the "opencode" loop.
func (m *Manager) RefreshOpencode(ctx context.Context) error {
	m.mu.Lock()
	client := m.opencode
	m.mu.Unlock()
	if client == nil {
		return nil
	}
	sessions, err := client.Sessions(ctx)
	m.applyOpencode(ctx, client, sessions, err)
	return nil
}

// applyOpencode brings the OpenCode agents up to date from one read of the server.
func (m *Manager) applyOpencode(ctx context.Context, client *OpencodeClient, sessions []OpencodeSession, err error) {
	m.mu.Lock()
	m.opencodeUp = err == nil
	if err != nil {
		m.mu.Unlock()
		return // a server not running is a state; the sessions view shows the error
	}
	byID := map[string]OpencodeSession{}
	for _, s := range sessions {
		byID[s.SessionID] = s
	}
	var finished []string
	now := m.ms()
	for id, a := range m.agents {
		if a.Opencode == nil || a.Opencode.SessionID == "" || !busy(a.Status) {
			continue
		}
		session, ok := byID[a.Opencode.SessionID]
		switch {
		case ok && session.Activity == "approval":
			a.Opencode.SeenBusy = true
			pending := []Pending{}
			for _, p := range session.Pending {
				pending = append(pending, Pending{RequestID: p.RequestID, ToolName: p.Kind, Summary: p.Summary, At: now})
			}
			if a.Status != NeedsApproval || len(a.Pending) != len(pending) {
				a.Status, a.Pending = NeedsApproval, pending
				m.touchLocked(a)
			}
		case ok && session.Activity == "working":
			a.Opencode.SeenBusy = true
			if a.Status != Working {
				a.Status, a.Pending = Working, []Pending{}
				m.touchLocked(a)
			}
		case a.Opencode.SeenBusy || now-a.Opencode.PromptedAt > opencodeQuiet.Milliseconds():
			finished = append(finished, id)
		}
	}
	m.mu.Unlock()
	for _, id := range finished {
		m.mu.Lock()
		place := *m.agents[id].Opencode
		m.mu.Unlock()
		text := lastOpencodeText(ctx, client, place)
		m.mu.Lock()
		a := m.agents[id]
		if a != nil && busy(a.Status) {
			a.Status, a.Pending, a.Tool = Waiting, []Pending{}, ""
			if text != "" {
				a.Result = text
				a.LastText = string([]rune(text)[:min(TextLimit, len([]rune(text)))])
				a.addLog(LogEntry{At: m.ms(), Kind: "text", Text: a.LastText})
				a.addNotes(text)
			}
			a.NumTurns++
			m.photographLocked(a)
			m.followThroughLocked(a)
			m.touchLocked(a)
		}
		m.mu.Unlock()
	}
}
