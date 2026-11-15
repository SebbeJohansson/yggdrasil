package norn

// The conversation as the chat shows it, what is waiting on you across every
// conversation, the dispatcher's decisions, and the route Sindri posts what
// agents say through. Ported from my-agent's tasks/chat.ts, needsYou.ts and
// decisionsView.ts.

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"yggdrasil/internal/part"
)

// Participant is an agent in a conversation, as the chat's "to:" lists it.
type Participant struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Place      string `json:"place"`
	Status     string `json:"status"`
	AsksYou    bool   `json:"asksYou"`
	Tool       string `json:"tool,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
	Pending    any    `json:"pending,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Reviewable bool   `json:"reviewable,omitempty"`
	Stoppable  bool   `json:"stoppable,omitempty"`
}

func participant(a Agent) Participant {
	p := Participant{ID: a.ID, Label: TaskOf(a.Label), Place: placeOf(a), Status: a.Status, AsksYou: a.AsksYou, Cwd: a.Cwd}
	if a.Status == "working" {
		p.Tool = clipText(a.Tool, 120)
	}
	if a.Status == "needs-approval" && len(a.Pending) > 0 {
		p.Pending = map[string]string{"requestId": a.Pending[0].RequestID, "summary": a.Pending[0].Summary}
	}
	if a.Worktree != nil {
		p.Branch = a.Worktree.Branch
		p.Reviewable = a.Resumable && !a.Merged
	}
	p.Stoppable = a.Status == "starting" || a.Status == "working" || a.Status == "needs-approval"
	return p
}

func (n *Norn) chat(w http.ResponseWriter, r *http.Request) {
	session := r.URL.Query().Get("session")
	if !sessionID.MatchString(session) {
		part.Error(w, http.StatusBadRequest, "`session` must be 1-64 letters, digits, - or _")
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	messages, err := n.Store.Chat(r.Context(), session, after, 200)
	if err != nil {
		n.fail(w, err)
		return
	}
	var all []Agent
	if n.Sindri.On() {
		all, _ = n.Sindri.Agents(r.Context())
	}
	own, _ := n.Store.AgentsOf(r.Context(), session)
	participants := []Participant{}
	for _, id := range own {
		for _, a := range all {
			if a.ID == id {
				participants = append(participants, participant(a))
			}
		}
	}
	for _, a := range RecentAgents(all, time.Now()) {
		if a.AsksYou && !slices.Contains(own, a.ID) {
			participants = append(participants, participant(a))
		}
	}
	to := "norn"
	if last, _ := n.Store.RecentTurns(r.Context(), session, 1); len(last) == 1 && last[0].Author != nil {
		for _, p := range participants {
			if p.ID == *last[0].Author && p.AsksYou {
				to = p.ID
			}
		}
	}
	projects := []string{}
	if n.Sindri.On() {
		projects = append(projects, n.Sindri.Projects(r.Context())...)
	}
	part.JSON(w, http.StatusOK, map[string]any{"messages": messages, "participants": participants, "to": to, "projects": projects})
}

// agentTurn is how Sindri posts what an agent said into the conversation it
// works for. Only Sindri may.
func (n *Norn) agentTurn(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != "sindri" {
		part.Error(w, http.StatusForbidden, "only Sindri posts agents' messages")
		return
	}
	session := r.PathValue("session")
	if !sessionID.MatchString(session) {
		part.Error(w, http.StatusBadRequest, "not a conversation id")
		return
	}
	var body struct {
		Agent   string `json:"agent"`
		Label   string `json:"label"`
		Content string `json:"content"`
	}
	if err := part.Decode(r, &body); err != nil || strings.TrimSpace(body.Content) == "" {
		part.Error(w, http.StatusBadRequest, "send `content`, and `agent` and `label` for an agent's message")
		return
	}
	var id int64
	var err error
	if body.Agent == "" {
		id, err = n.Store.AddTurn(r.Context(), session, "assistant", body.Content, nil, nil, nil)
	} else {
		if bound, _ := n.Store.SessionOf(r.Context(), body.Agent); bound != session {
			_ = n.Store.BindAgent(r.Context(), session, body.Agent)
		}
		id, err = n.Store.AddAgentTurn(r.Context(), session, body.Agent, body.Label, body.Content)
	}
	if err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusCreated, map[string]int64{"id": id})
}

// Need is something waiting on you.
type Need struct {
	Kind       string            `json:"kind"` // approval, question, failed, result, stuck, ready
	Agent      string            `json:"agent"`
	Label      string            `json:"label"`
	Session    *string           `json:"session"`
	Text       *string           `json:"text"`
	Pending    map[string]string `json:"pending,omitempty"`
	Reviewable bool              `json:"reviewable,omitempty"`
	Running    bool              `json:"running,omitempty"`
	At         int64             `json:"at"`
	Overdue    bool              `json:"overdue,omitempty"`
	Check      string            `json:"check,omitempty"`
}

// How long before an approval or question is overdue, and a silent agent stuck.
const (
	overdueAfter = 20 * time.Minute
	stuckAfter   = 15 * time.Minute
	needTextMax  = 600
)

func clipNeed(text string) *string {
	if runes := []rune(text); len(runes) > needTextMax {
		text = strings.TrimRight(string(runes[:needTextMax-1]), " ") + "…"
	}
	return &text
}

// NeedsYou is everything waiting on you, across every conversation, oldest first.
func (n *Norn) NeedsYou(ctx context.Context, now time.Time) ([]Need, error) {
	var agents []Agent
	if n.Sindri.On() {
		agents, _ = n.Sindri.Agents(ctx)
	}
	unread, err := n.Store.UnreadAgentTurns(ctx)
	if err != nil {
		return nil, err
	}
	latest := map[string]UnreadTurn{}
	for _, t := range unread {
		latest[t.Agent] = t
	}
	needs := []Need{}
	live := map[string]bool{}
	for _, a := range agents {
		turn, hasTurn := latest[a.ID]
		need := Need{Agent: a.ID, Label: TaskOf(a.Label), At: a.LastActivity, Reviewable: a.Worktree != nil && a.Resumable,
			Running: a.Status == "starting" || a.Status == "working" || a.Status == "needs-approval"}
		if session, _ := n.Store.SessionOf(ctx, a.ID); session != "" {
			need.Session = &session
		}
		if hasTurn {
			need.At = turn.At
			if need.Session == nil {
				need.Session = &turn.Session
			}
		}
		if a.Worktree != nil && a.Check != nil {
			need.Check = a.Check.Status
		}
		said := func() *string {
			if hasTurn {
				return clipNeed(turn.Content)
			}
			if a.LastText != "" {
				return clipNeed(a.LastText)
			}
			return nil
		}
		overdue := now.UnixMilli()-need.At >= overdueAfter.Milliseconds()
		switch {
		case a.Status == "needs-approval" && len(a.Pending) > 0:
			need.Kind, need.Overdue = "approval", overdue
			need.Pending = map[string]string{"requestId": a.Pending[0].RequestID, "summary": a.Pending[0].Summary}
			live[a.ID] = true
		case a.AsksYou && (a.Status == "waiting" || a.Status == "done"):
			need.Kind, need.Overdue, need.Text = "question", overdue, said()
			live[a.ID] = true
		case (a.Status == "waiting" || a.Status == "done") && a.Worktree != nil && !a.Merged && a.Resumable && a.Check != nil && a.Check.Status == "passed":
			need.Kind, need.At, need.Text = "ready", a.Check.At, said()
			live[a.ID] = true
		case a.Status == "starting" || a.Status == "working":
			live[a.ID] = true
			if silent := now.UnixMilli() - a.LastActivity; silent >= stuckAfter.Milliseconds() {
				doing := ""
				if a.Tool != "" {
					doing = " while running " + a.Tool
				}
				need.Kind, need.At = "stuck", a.LastActivity
				need.Text = clipNeed("No output for " + strconv.Itoa(int(silent/60_000)) + " min" + doing + ". Open it to see where it stopped, or stop it.")
			} else {
				continue
			}
		default:
			continue
		}
		needs = append(needs, need)
	}
	byID := map[string]Agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	for _, t := range unread {
		if live[t.Agent] {
			continue
		}
		known, isKnown := byID[t.Agent]
		if isKnown && known.Merged && known.Status != "failed" {
			continue
		}
		kind := "result"
		if (isKnown && known.Status == "failed") || strings.HasPrefix(t.Content, "Failed") {
			kind = "failed"
		}
		label := "agent"
		if t.Label != nil {
			label = *t.Label
		}
		session := t.Session
		need := Need{Kind: kind, Agent: t.Agent, Label: label, Session: &session, Text: clipNeed(t.Content), At: t.At}
		if isKnown && known.Worktree != nil {
			need.Reviewable = known.Resumable
			if known.Check != nil {
				need.Check = known.Check.Status
			}
		}
		needs = append(needs, need)
	}
	slices.SortStableFunc(needs, func(a, b Need) int { return int(a.At - b.At) })
	return needs, nil
}

// --- decisions -------------------------------------------------------------------

// LogDecision keeps one dispatch decision; its id.
func (s *Store) LogDecision(ctx context.Context, session, message, prompt, source string, model *Decision, decision any, guards []string, ms int64) (int64, error) {
	var modelJSON, guardsJSON any
	if model != nil {
		encoded, _ := json.Marshal(model)
		modelJSON = string(encoded)
	}
	if guards != nil {
		encoded, _ := json.Marshal(guards)
		guardsJSON = string(encoded)
	}
	decisionJSON, _ := json.Marshal(decision)
	result, err := s.db.ExecContext(ctx, `INSERT INTO decisions (at, session, message, prompt, source, model, decision, guards, ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ms(), session, message, prompt, source, modelJSON, string(decisionJSON), guardsJSON, ms)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// DecisionRow is one decision as the decisions view lists it.
type DecisionRow struct {
	ID         int64           `json:"id"`
	At         int64           `json:"at"`
	Session    string          `json:"session"`
	Message    string          `json:"message"`
	Source     string          `json:"source"`
	Model      json.RawMessage `json:"model"`
	Decision   json.RawMessage `json:"decision"`
	Guards     json.RawMessage `json:"guards"`
	MS         *int64          `json:"ms"`
	Correction json.RawMessage `json:"correction"`
	// Shadow is what the shadow Laya dispatcher would have done; null without one.
	Shadow json.RawMessage `json:"shadow"`
}

// Decisions is the latest decisions, newest first.
func (s *Store) Decisions(ctx context.Context, limit int) ([]DecisionRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, session, message, source, coalesce(model, 'null'), decision, coalesce(guards, '[]'), ms, coalesce(correction, 'null'), coalesce(shadow, 'null') FROM decisions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []DecisionRow{}
	for rows.Next() {
		var d DecisionRow
		var model, decision, guards, correction, shadow string
		if err := rows.Scan(&d.ID, &d.At, &d.Session, &d.Message, &d.Source, &model, &decision, &guards, &d.MS, &correction, &shadow); err != nil {
			return nil, err
		}
		d.Model, d.Decision, d.Guards, d.Correction = json.RawMessage(model), json.RawMessage(decision), json.RawMessage(guards), json.RawMessage(correction)
		d.Shadow = json.RawMessage(shadow)
		list = append(list, d)
	}
	return list, rows.Err()
}

// Correct keeps what a decision should have been.
func (s *Store) Correct(ctx context.Context, id int64, decision any) (bool, error) {
	encoded, _ := json.Marshal(map[string]any{"decision": decision, "at": s.ms()})
	result, err := s.db.ExecContext(ctx, `UPDATE decisions SET correction = ? WHERE id = ?`, string(encoded), id)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

func (n *Norn) mountChat(p *part.Part) {
	p.Handle("POST /v1/ask", n.askRoute)
	p.Handle("GET /v1/chat", n.chat)
	p.Handle("POST /v1/conversations/{session}/agent-turns", n.agentTurn)
	p.Handle("GET /v1/needs-you", func(w http.ResponseWriter, r *http.Request) {
		needs, err := n.NeedsYou(r.Context(), time.Now())
		if err != nil {
			n.fail(w, err)
			return
		}
		part.JSON(w, http.StatusOK, map[string]any{"needs": needs})
	})
	p.Handle("POST /v1/needs-you/read", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Agent string `json:"agent"`
		}
		if err := part.Decode(r, &body); err != nil || body.Agent == "" {
			part.Error(w, http.StatusBadRequest, "`agent` must be an agent id")
			return
		}
		read, err := n.Store.MarkAgentRead(r.Context(), body.Agent)
		if err != nil {
			n.fail(w, err)
			return
		}
		part.JSON(w, http.StatusOK, map[string]bool{"read": read})
	})
	p.Handle("GET /v1/decisions", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 200
		}
		decisions, err := n.Store.Decisions(r.Context(), min(limit, 1000))
		if err != nil {
			n.fail(w, err)
			return
		}
		part.JSON(w, http.StatusOK, map[string]any{"decisions": decisions})
	})
	p.Handle("POST /v1/decisions/{id}/correction", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		var body struct {
			Decision map[string]any `json:"decision"`
		}
		if err := part.Decode(r, &body); err != nil || !slices.Contains(Actions, stringOf(body.Decision["action"])) {
			part.Error(w, http.StatusBadRequest, "`decision` must be { action, agent?, project? } with one of the dispatcher's actions")
			return
		}
		corrected, err := n.Store.Correct(r.Context(), id, body.Decision)
		if err != nil {
			n.fail(w, err)
			return
		}
		if !corrected {
			part.Error(w, http.StatusNotFound, "no decision "+r.PathValue("id"))
			return
		}
		part.JSON(w, http.StatusOK, map[string]any{"corrected": id})
	})
}

func stringOf(value any) string {
	text, _ := value.(string)
	return text
}
