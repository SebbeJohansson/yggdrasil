package norn

// The front desk: POST /v1/ask. Every message you type comes here. Norn
// answers it itself (general knowledge, your profile and memories, your
// inbox and calendars), or hands it to Sindri: a question that needs tools,
// a coding task, a reply to an agent. Ported from my-agent's tasks/ask.ts.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"yggdrasil/internal/linear"

	"yggdrasil/internal/llm"
	"yggdrasil/internal/part"
)

// Limits on a message.
const (
	HistoryTurns     = 10
	MaxQuestionChars = 4000
	maxImages        = 5
)

// AskRequest is POST /v1/ask.
type AskRequest struct {
	Question      string   `json:"question"`
	Session       string   `json:"session,omitempty"`
	To            string   `json:"to,omitempty"`
	Images        []string `json:"images,omitempty"`
	Communication string   `json:"communication,omitempty"`
	Mode          string   `json:"mode,omitempty"`
	// Force sends the message where a correction said it belonged; never from a request body.
	Force *Forced `json:"-"`
}

// Forced is a corrected route: the action, and its project (code) or agent id (reply_to_agent, status).
type Forced struct {
	Action, Target string
}

// Delegated is the agent a message went to.
type Delegated struct {
	AgentID string `json:"agentId"`
	Status  string `json:"status"`
	Project string `json:"project,omitempty"`
	Branch  string `json:"branch,omitempty"`
}

// AskResponse is what a message got.
type AskResponse struct {
	Answer    string     `json:"answer"`
	Session   string     `json:"session"`
	Recalled  []Memory   `json:"recalled"`
	UsedInbox bool       `json:"usedInbox"`
	Delegated *Delegated `json:"delegated,omitempty"`
	Via       string     `json:"via"`
}

const askSystem = "You are a personal assistant running locally for one person. Answer briefly and plainly. For general knowledge (facts, definitions, how-tos), answer normally from what you know. For questions about the person themselves -- their colleagues, projects, schedule -- use only what is written below; if it is not there, say you do not know. You have no tools and nothing runs after you answer: never say you will check, look, or do something."

// AskSystemPrompt is the local answer's system prompt.
func AskSystemPrompt(profile string, recalled []Memory, inbox, agent, overview, calendar []string) string {
	parts := []string{askSystem}
	if calendar != nil {
		parts = append(parts, "",
			"Their calendar: work from Outlook, and personal from Google under \"Personal calendar\" when it is there. Answer meeting questions from it: times are already in their time zone.",
			`Reply as JSON {"answer": "...", "enough": true|false}. enough is false when the answer needs anything`,
			"that is not below: further ahead than it covers, who is invited, what a meeting is about, or changing a meeting.",
			`Examples: "am I free at 15 on Friday?" when Friday is below -> {"answer": "No, Design review 14:30-15:30.", "enough": true}.`,
			`"who is coming to the design review?" -> {"answer": "I can not see the invitees.", "enough": false}.`)
		for _, line := range calendar {
			parts = append(parts, "- "+line)
		}
	}
	if len(overview) > 0 {
		parts = append(parts, "", "Their agents from the last day (task (where): state):")
		for _, line := range overview {
			parts = append(parts, "- "+line)
		}
	}
	if agent != nil {
		parts = append(parts, "", "An agent is working for them in this conversation. Its live status, which is what to", "answer from when they ask how it is going:")
		for _, line := range agent {
			parts = append(parts, "- "+line)
		}
	}
	if profile != "" {
		parts = append(parts, "", "About the reader, in their own words:", profile)
	}
	if len(recalled) > 0 {
		parts = append(parts, "", "Things the reader told you before. They may be out of date; use them only where they help:")
		for _, m := range recalled {
			parts = append(parts, "- "+m.Text)
		}
	}
	if inbox != nil {
		if len(inbox) > 0 {
			parts = append(parts, "", "Their unread inbox right now, most urgent first (sender in subject (source, age, signals): preview):")
		} else {
			parts = append(parts, "", "Their inbox has nothing unread right now.")
		}
		for _, line := range inbox {
			parts = append(parts, "- "+line)
		}
	}
	return strings.Join(parts, "\n")
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// asker is one message being handled.
type asker struct {
	n        *Norn
	ctx      context.Context
	request  AskRequest
	session  string
	question string
	recalled []Memory
	history  []llm.Message
	// yours is your own Claude Code sessions from the last day.
	yours      []OwnSession
	decisionID *int64
}

func (a *asker) answered(answer string, delegated *Delegated, via string) AskResponse {
	label := "Norn"
	_, _ = a.n.Store.AddTurn(a.ctx, a.session, "user", a.question, &label, a.decisionID, &via)
	_, _ = a.n.Store.AddTurn(a.ctx, a.session, "assistant", answer, nil, nil, nil)
	return AskResponse{Answer: answer, Session: a.session, Recalled: orNone(a.recalled), Via: via, Delegated: delegated}
}

func orNone(memories []Memory) []Memory {
	if memories == nil {
		return []Memory{}
	}
	return memories
}

func (a *asker) rules() (string, string) {
	communication, _ := a.n.Store.SessionSetting(a.ctx, "communication", a.session)
	mode, _ := a.n.Store.SessionSetting(a.ctx, "mode", a.session)
	return communication, mode
}

// handOver gives a question that needs tools to Sindri's question agent.
func (a *asker) handOver(via string, inbox []string) AskResponse {
	communication, mode := a.rules()
	agents, _ := a.n.Store.AgentsOf(a.ctx, a.session)
	request := map[string]any{"session": a.session, "name": a.name(), "communication": communication, "mode": mode, "goal": a.question,
		"prompt": HandoverPrompt(a.question, LoadProfile(a.n.Config.ProfilePath), a.recalled, inbox, a.yours, a.history, ""), "followUp": a.question}
	if len(agents) > 0 {
		request["agent"] = agents[0]
	}
	agent, err := a.n.Sindri.Ask(a.ctx, request)
	if err != nil {
		var refused *SindriError
		if errors.As(err, &refused) && refused.Status == 409 {
			return a.answered(refused.Message+" Ask again when it has answered.", nil, via)
		}
		return a.answered("This needs tools, and Sindri is not reachable. Start the question with /local to get my best guess instead.", nil, via)
	}
	_ = a.n.Store.BindAgent(a.ctx, a.session, agent.ID)
	return a.answered("This needs tools, so I handed it to an agent. It will answer here.", &Delegated{AgentID: agent.ID, Status: agent.Status}, via)
}

// code hands a coding task to Sindri: one agent per project.
func (a *asker) code(projects []string, via string) AskResponse {
	communication, mode := a.rules()
	handed, err := a.n.Sindri.Code(a.ctx, map[string]any{"session": a.session, "name": a.name(), "projects": projects, "goal": a.question,
		"communication": communication, "mode": mode, "prompt": HandoverPrompt(a.question, LoadProfile(a.n.Config.ProfilePath), a.recalled, nil, nil, ownWords(a.history), "")})
	if err != nil {
		return a.answered("Could not hand the coding over: "+err.Error(), nil, via)
	}
	var delegated *Delegated
	for _, p := range handed.Parts {
		_ = a.n.Store.BindAgent(a.ctx, a.session, p.AgentID)
		delegated = &Delegated{AgentID: p.AgentID, Status: p.Status, Project: p.Project, Branch: p.Branch}
	}
	return a.answered(handed.Answer, delegated, via)
}

func ownWords(history []llm.Message) []llm.Message {
	var own []llm.Message
	for _, m := range history {
		if m.Role == "user" {
			own = append(own, m)
		}
	}
	return own
}

// reply sends your words to an agent.
func (a *asker) reply(agent *Agent, text, via string) AskResponse {
	communication, mode := a.rules()
	prompted, err := a.n.Sindri.Reply(a.ctx, map[string]any{"session": a.session, "agent": agent.ID, "text": text, "images": a.request.Images, "communication": communication, "mode": mode})
	task := TaskOf(agent.Label)
	if err != nil {
		var refused *SindriError
		if errors.As(err, &refused) && refused.Status == 409 {
			return a.answered(refused.Message+" Try again when it has finished.", nil, via)
		}
		return a.answered("Could not reach that agent: "+err.Error(), nil, via)
	}
	_ = a.n.Store.BindAgent(a.ctx, a.session, agent.ID)
	_, _ = a.n.Store.AddTurn(a.ctx, a.session, "user", a.question, &task, a.decisionID, &via)
	delegated := &Delegated{AgentID: prompted.ID, Status: prompted.Status}
	if prompted.Worktree != nil {
		delegated.Branch = prompted.Worktree.Branch
	}
	return AskResponse{Answer: `Sent to the agent on "` + task + `".`, Session: a.session, Recalled: orNone(a.recalled), Via: via, Delegated: delegated}
}

// ShortName is a task name from a message: its first line, cut at a word.
// nameSystem names a task for the list of running jobs.
const nameSystem = "Name the task in the newest message for a list of running jobs, using the conversation to see what it refers to. " +
	"Say what the work is, starting with a verb; leave out greetings and \"I would like to\". " +
	"Reply with the name only: 3 to 8 words, in the person's language, no quotes, no full stop. " +
	"Example: conversation about a monitor setup display bug, newest message \"check it again\" -> Check the monitor setup display issue"

// name is the task's name from the dispatcher's model (kept loaded, so a
// second or two), else the question's first line.
func (a *asker) name() string {
	if a.n.Config.DispatchModel == "off" {
		return ShortName(a.question)
	}
	var lines []string
	for _, m := range a.history[max(0, len(a.history)-6):] {
		who := "Assistant"
		if m.Role == "user" {
			who = "Person"
		}
		lines = append(lines, who+": "+clipRunes(m.Content, 300))
	}
	think := false
	reply, err := a.n.LLM.Chat(a.ctx, []llm.Message{
		{Role: "system", Content: nameSystem},
		{Role: "user", Content: strings.Join(lines, "\n") + "\n\nNewest message: " + clipRunes(a.question, 1500)},
	}, llm.Options{Purpose: "name", Model: a.n.Config.DispatchModel, Temperature: 0, TopP: 1, MaxTokens: 24, Think: &think,
		Timeout: 5 * time.Second, NoFallback: true})
	name := strings.Trim(strings.TrimSpace(strings.SplitN(strings.TrimSpace(reply), "\n", 2)[0]), "\"'.")
	if err != nil || name == "" || len([]rune(name)) > 80 {
		return ShortName(a.question)
	}
	return name
}

func clipRunes(text string, n int) string {
	if runes := []rune(text); len(runes) > n {
		return string(runes[:n])
	}
	return text
}

func ShortName(text string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(text), "\n", 2)[0])
	if runes := []rune(line); len(runes) > 50 {
		cut := string(runes[:50])
		if space := strings.LastIndex(cut, " "); space > 25 {
			cut = cut[:space]
		}
		return strings.TrimRight(cut, " ,.;:") + "…"
	}
	return line
}

// Ask handles one message.
func (n *Norn) Ask(ctx context.Context, request AskRequest) (AskResponse, error) {
	now := time.Now()
	session := request.Session
	if session == "" {
		session = newSessionID()
	}
	ctx = llm.WithExchange(ctx, session, now)
	if request.Communication != "" {
		_ = n.Store.SetSessionSetting(ctx, "communication", session, request.Communication)
	}
	if request.Mode != "" {
		_ = n.Store.SetSessionSetting(ctx, "mode", session, request.Mode)
	}
	question := request.Question
	if retry.MatchString(question) {
		turns, _ := n.Store.Chat(ctx, session, 0, 10_000)
		for i := len(turns) - 1; i >= 0; i-- {
			if turns[i].Role == "user" && turns[i].Author == nil && !retry.MatchString(turns[i].Content) {
				question = turns[i].Content
				break
			}
		}
	}
	route := ""
	if match := forced.FindStringSubmatch(question); match != nil {
		route, question = match[1], strings.TrimSpace(match[2])
	}
	a := &asker{n: n, ctx: ctx, request: request, session: session, question: question}
	a.recalled, _ = n.Brain.RecallFor(ctx, question)
	turns, _ := n.Store.RecentTurns(ctx, session, HistoryTurns)
	for _, t := range turns {
		a.history = append(a.history, llm.Message{Role: t.Role, Content: t.Content})
	}
	sindri := n.Sindri.On()
	if request.Force != nil && request.Force.Action == "answer" {
		route = "local"
	}

	// A corrected message, re-sent where you said it belonged.
	if f := request.Force; f != nil && f.Action != "answer" && sindri {
		via := "correction: " + f.Action
		switch f.Action {
		case "reply_to_agent", "status":
			target, err := n.Sindri.Agent(ctx, f.Target)
			if err != nil {
				return a.answered("That agent is gone.", nil, via), nil
			}
			if f.Action == "status" {
				return a.answered(StatusAnswer(target), nil, via), nil
			}
			return a.reply(target, question, via), nil
		case "code":
			return a.code([]string{f.Target}, via), nil
		}
		return a.handOver(via, n.Inbox.InboxForQuestion(question, now)), nil
	}

	// "yes" or "no" to an agent waiting on a tool.
	if allow, message, ok := ApprovalReply(question); sindri && ok {
		ids, _ := n.Store.AgentsOf(ctx, session)
		if request.To != "" && request.To != "norn" {
			ids = []string{request.To}
		}
		var waiting *Agent
		for _, id := range ids {
			if agent, err := n.Sindri.Agent(ctx, id); err == nil && agent.Status == "needs-approval" && len(agent.Pending) > 0 && (waiting == nil || agent.LastActivity > waiting.LastActivity) {
				waiting = agent
			}
		}
		if waiting != nil {
			if err := n.Sindri.Answer(ctx, waiting.ID, waiting.Pending[0].RequestID, allow, message); err != nil {
				return a.answered("Could not pass that on: "+err.Error(), nil, "rule: approval"), nil
			}
			task := TaskOf(waiting.Label)
			answer := `Allowed. The agent on "` + task + `" goes on.`
			if !allow {
				told := ""
				if message != "" {
					told = `, and told it: "` + message + `"`
				}
				answer = "Denied" + told + `. The agent on "` + task + `" goes on without it.`
			}
			via := "rule: approval"
			_, _ = n.Store.AddTurn(ctx, session, "user", question, &task, nil, &via)
			_, _ = n.Store.AddTurn(ctx, session, "assistant", answer, nil, nil, nil)
			return AskResponse{Answer: answer, Session: session, Recalled: orNone(a.recalled), Via: via}, nil
		}
	}

	// A message to one agent, picked by hand.
	if request.To != "" && request.To != "norn" && request.To != "my-agent" {
		if !sindri {
			return a.answered("Agents are off here (no Sindri).", nil, "to: picked"), nil
		}
		target, err := n.Sindri.Agent(ctx, request.To)
		if err != nil {
			return a.answered(`That agent is gone; pick another in "to:".`, nil, "to: picked"), nil
		}
		decision := map[string]any{"action": "reply_to_agent", "agent": NameAgents([]Agent{*target})[0].Name, "project": nil, "agentId": target.ID}
		id, _ := n.Store.LogDecision(ctx, session, request.Question, "", "person", nil, decision, nil, 0)
		a.decisionID = &id
		return a.reply(target, request.Question, "to: picked"), nil
	}

	if match := agentCmd.FindStringSubmatch(question); match != nil && sindri {
		a.question = strings.TrimSpace(match[2])
		return a.code(strings.Split(match[1], ","), "/agent"), nil
	} else if strings.HasPrefix(question, "/agent") {
		return a.answered("Usage: /agent <project> <task>, e.g. /agent my-widget fix the DM titles", nil, "/agent"), nil
	}

	// /part <project> <task>: another repo's part of this conversation's
	// code task, on the same branch name.
	if match := partCmd.FindStringSubmatch(question); match != nil {
		if !sindri {
			return a.answered("Agents are off here (no Sindri).", nil, "/part"), nil
		}
		request := map[string]any{"session": session, "project": match[1], "task": strings.TrimSpace(match[2])}
		if agents, _ := n.Store.AgentsOf(ctx, session); len(agents) > 0 {
			request["agent"] = agents[0]
		}
		handed, err := n.Sindri.Part(ctx, request)
		if err != nil {
			return a.answered(err.Error(), nil, "/part"), nil
		}
		for _, p := range handed.Parts {
			_ = n.Store.BindAgent(ctx, session, p.AgentID)
		}
		return a.answered(handed.Answer, nil, "/part"), nil
	} else if strings.HasPrefix(question, "/part") {
		return a.answered("Usage: /part <project> <task>, e.g. /part norn add the pull-request route", nil, "/part"), nil
	}

	var everyone []Agent
	if sindri {
		everyone, _ = n.Sindri.Agents(ctx)
		a.yours = n.Sindri.Sessions(ctx)
	}
	if sindri && route != "claude" && IsOverviewQuestion(question) && !Change.MatchString(question) {
		return a.answered(OverviewAnswer(everyone, a.yours, now), nil, "rule: overview"), nil
	}
	// A status question that names one of your own sessions better than any
	// agent is about that session. Before the dispatcher, which is not shown
	// your sessions and would pick the agent sharing one word with it.
	if route != "claude" && statusAsk.MatchString(question) {
		if own := LikelySession(question, a.yours, everyone, now); own != nil {
			return a.answered(SessionStatus(*own, now)+ReadLink(n.Config.DashboardURL, own.ID, own.Cwd, own.Title, ""), nil, "rule: your session"), nil
		}
	}
	if match := replyCmd.FindStringSubmatch(question); match != nil && sindri && route != "local" {
		if target := LikelyAgent(match[1], everyone, now); target != nil {
			return a.reply(target, strings.TrimSpace(match[2]), "reply to"), nil
		}
		return a.answered(`No agent from the last day matches that. Ask "which agents are there?" to see them.`, nil, "reply to"), nil
	}

	inbox := n.Inbox.InboxForQuestion(question, now)
	calendar := n.calendarLines(question, now)
	hasCalendar := n.Accounts.Work.Current(now) != nil || n.Accounts.Personal.Current(now) != nil
	decidedLocal := false
	if sindri && route == "" && n.Config.DispatchModel != "off" {
		own, _ := n.Store.AgentsOf(ctx, session)
		var candidates []Agent
		for _, id := range own {
			for _, agent := range everyone {
				if agent.ID == id {
					candidates = append(candidates, agent)
				}
			}
		}
		for _, agent := range RecentAgents(everyone, now) {
			if !slices.Contains(own, agent.ID) {
				candidates = append(candidates, agent)
			}
		}
		named := NameAgents(candidates[:min(8, len(candidates))])
		names := map[string]string{}
		for _, entry := range named {
			names[entry.Agent.ID] = entry.Name
		}
		chat, _ := n.Store.Chat(ctx, session, 0, 10_000)
		var conversation []Line
		for _, t := range chat[max(0, len(chat)-6):] {
			who := "Norn"
			switch {
			case t.Author != nil:
				who = names[*t.Author]
				if who == "" {
					who = "an agent"
				}
			case t.Role == "user":
				who = "person"
			}
			conversation = append(conversation, Line{who, t.Content})
		}
		input := DispatchInput{Message: question, HasCalendar: hasCalendar, Projects: n.Sindri.Projects(ctx), Agents: named, Conversation: conversation}
		result := Dispatch(ctx, input, func(ctx context.Context, messages []llm.Message, format map[string]any) (string, error) {
			think := false
			return n.LLM.Chat(ctx, messages, llm.Options{Purpose: "dispatch", Model: n.Config.DispatchModel, Temperature: 0, TopP: 1, MaxTokens: 60,
				Format: format, Think: &think, KeepAlive: n.Config.DispatchKeepAlive, Timeout: 20 * time.Second, NoFallback: true})
		})
		target := agentNamed(input, result.Decision.Agent)
		source := "fallback"
		if result.Model != nil {
			source = "model"
		}
		decision := map[string]any{"action": result.Decision.Action, "agent": result.Decision.Agent, "project": result.Decision.Project}
		if target != nil {
			decision["agentId"] = target.ID
		}
		id, _ := n.Store.LogDecision(ctx, session, question, DispatchPrompt(input), source, result.Model, decision, result.Guards, result.MS)
		a.decisionID = &id
		shadowInput := ShadowInput{Message: question, Agents: candidates, Projects: input.Projects}
		if len(chat) > 0 {
			t := chat[len(chat)-1]
			who := "Norn"
			switch {
			case t.Author != nil:
				who = *t.Author
			case t.Role == "user":
				who = "person"
			}
			shadowInput.Last = &[2]string{who, t.Content}
		}
		n.shadow(id, shadowInput)
		// "Is the Linear task marked as done?": read from Linear, not from an
		// agent's last message. The key is in the message, in the task of the
		// agent it is about, or earlier in the conversation.
		if n.Linear.On() && linear.AsksState(question) {
			key := linear.Ticket.FindString(question)
			if key == "" && target != nil {
				key = linear.Ticket.FindString(target.Task)
			}
			for i := len(conversation) - 1; key == "" && i >= 0; i-- {
				key = linear.Ticket.FindString(conversation[i].Text)
			}
			if key != "" {
				issue, err := n.Linear.Issue(ctx, key)
				switch {
				case err == nil:
					return a.answered(linear.Answer(issue, n.Zone), nil, "rule: linear"), nil
				case !errors.Is(err, linear.ErrNotFound):
					n.logf("could not read %s from Linear: %v", key, err)
				}
			}
		}
		if result.Model != nil {
			switch d := result.Decision; {
			case d.Action == "status" && target != nil:
				detail, err := n.Sindri.Agent(ctx, target.ID)
				if err != nil {
					detail = target
				}
				return a.answered(StatusAnswer(detail), nil, "dispatcher: status"), nil
			case d.Action == "reply_to_agent" && target != nil:
				return a.reply(target, question, "dispatcher: reply"), nil
			case d.Action == "code" && d.Project != nil:
				n.learnLater(question)
				projects := append([]string{*d.Project}, without(MentionedProjects(question, input.Projects), *d.Project)...)
				return a.code(projects[:min(4, len(projects))], "dispatcher: code"), nil
			case d.Action == "delegate":
				if local := a.lookedUp(); local != nil {
					return *local, nil
				}
				n.learnLater(question)
				response := a.handOver("dispatcher: delegate", inbox)
				response.UsedInbox = inbox != nil
				return response, nil
			default:
				decidedLocal = true
			}
		}
	}

	// The dispatcher gave nothing usable: the older rules decide.
	var agent *Agent
	if sindri {
		if own, _ := n.Store.AgentsOf(ctx, session); len(own) > 0 {
			agent, _ = n.Sindri.Agent(ctx, own[0])
		}
	}
	asksStatus := !decidedLocal && route != "claude" && statusAsk.MatchString(question)
	if agent == nil && asksStatus {
		agent = LikelyAgent(question, everyone, now)
	}
	if agent != nil && asksStatus {
		return a.answered(StatusAnswer(agent), nil, "rule: status"), nil
	}
	if !decidedLocal && agent != nil && agent.AsksYou && route != "local" && request.To != "norn" {
		return a.reply(agent, question, "rule: it asked you"), nil
	}
	if sindri && route != "local" && !decidedLocal {
		projects := n.Sindri.Projects(ctx)
		project := MentionedProject(question, projects)
		tools, change := n.Classify(ctx, question)
		if route == "claude" || project != "" || tools {
			n.learnLater(question)
			if change && project != "" {
				return a.code([]string{project}, "rule: code change"), nil
			}
			if !(change && project == "" && route != "claude") {
				via := map[bool]string{true: "/claude", false: "rule: needs tools"}[route == "claude"]
				if project != "" && route != "claude" {
					via = "rule: names a project"
				}
				if route != "claude" && project == "" {
					if local := a.lookedUp(); local != nil {
						return *local, nil
					}
				}
				response := a.handOver(via, inbox)
				response.UsedInbox = inbox != nil
				return response, nil
			}
		}
	}
	if calendar != nil && sindri && BeyondCalendar(question) {
		return a.handOver("rule: beyond the calendar", inbox), nil
	}

	var agentLines []string
	if agent != nil {
		agentLines = []string{"Task: " + agent.Label, "Status: " + agent.Status}
		if agent.LastText != "" {
			agentLines = append(agentLines, "Its last message: "+clipText(agent.LastText, 400))
		}
	}
	messages := append([]llm.Message{{Role: "system", Content: AskSystemPrompt(LoadProfile(n.Config.ProfilePath), a.recalled, inbox, agentLines, append(OverviewLines(everyone, now), SessionLines(a.yours)...), calendar)}}, a.history...)
	messages = append(messages, llm.Message{Role: "user", Content: question})
	options := llm.Options{Purpose: "answer", Model: n.Config.AskModel, Temperature: 0.5, TopP: 0.9, MaxTokens: 400}
	if calendar != nil {
		options.Temperature = 0
		options.Format = map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}, "enough": map[string]any{"type": "boolean"}}, "required": []string{"answer", "enough"}}
	}
	raw, err := n.LLM.Chat(ctx, messages, options)
	if err != nil && options.Format != nil {
		// XeStrata's JSON mode gives up on some questions (structured_output_failed).
		// A plain answer is better than none: asked again without it, and read as
		// enough unless it says it does not know.
		n.logf("answer: JSON mode failed (%v); asking without it", err)
		plain := options
		plain.Format = nil
		// Its own purpose, so the health view tells a failed answer from a recovered one.
		plain.Purpose = "answer without JSON"
		raw, err = n.LLM.Chat(ctx, messages, plain)
	}
	if err != nil {
		return AskResponse{}, fmt.Errorf("the model did not answer: %w", err)
	}
	answer, enough := strings.TrimSpace(raw), true
	if calendar != nil {
		var parsed struct {
			Answer string `json:"answer"`
			Enough *bool  `json:"enough"`
		}
		if json.Unmarshal([]byte(answer), &parsed) == nil {
			if strings.TrimSpace(parsed.Answer) != "" {
				answer = strings.TrimSpace(parsed.Answer)
			}
			enough = (parsed.Enough == nil || *parsed.Enough) && !notKnown.MatchString(answer)
		}
	}
	if calendar != nil && sindri && !enough {
		n.learnLater(question)
		return a.handOver("calendar: not enough", inbox), nil
	}
	if sindri && route != "local" && promise.MatchString(answer) {
		n.learnLater(question)
		return a.handOver("local model: promised to look", inbox), nil
	}
	answer += NamedReadLinks(answer, a.yours, everyone, n.Config.DashboardURL)
	via := "local model"
	switch {
	case a.decisionID != nil && decidedLocal:
		via = "dispatcher: answer"
	case route == "local":
		via = "/local"
	case calendar != nil:
		via = "calendar"
	}
	response := a.answered(answer, nil, via)
	response.UsedInbox = inbox != nil
	n.learnLater(question)
	return response, nil
}

func without(list []string, drop string) []string {
	var kept []string
	for _, item := range list {
		if item != drop {
			kept = append(kept, item)
		}
	}
	return kept
}

func (n *Norn) calendarLines(question string, now time.Time) []string {
	work := n.Accounts.Work.ForQuestion(question, now, n.Zone)
	personal := n.Accounts.Personal.ForQuestion(question, now, n.Zone)
	if work == nil && personal == nil {
		return nil
	}
	lines := append([]string{}, work...)
	if personal != nil {
		lines = append(lines, "Personal calendar:")
		lines = append(lines, personal...)
	}
	return lines
}

// learnLater learns from your message in the background, when learning is on.
func (n *Norn) learnLater(question string) {
	if !n.Config.Learn {
		return
	}
	go n.Brain.LearnFrom(context.Background(), question)
}

func (n *Norn) askRoute(w http.ResponseWriter, r *http.Request) {
	var request AskRequest
	if err := part.Decode(r, &request); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	request.Question = strings.TrimSpace(request.Question)
	switch {
	case request.Question == "":
		part.Error(w, http.StatusBadRequest, "`question` must be a non-empty string")
		return
	case len([]rune(request.Question)) > MaxQuestionChars:
		part.Error(w, http.StatusBadRequest, fmt.Sprintf("`question` is longer than %d characters", MaxQuestionChars))
		return
	case request.Session != "" && !sessionID.MatchString(request.Session):
		part.Error(w, http.StatusBadRequest, "`session` must be 1-64 letters, digits, - or _")
		return
	case request.To != "" && !sessionID.MatchString(request.To):
		part.Error(w, http.StatusBadRequest, "`to` must be `norn` or an agent id")
		return
	case len(request.Images) > maxImages:
		part.Error(w, http.StatusBadRequest, fmt.Sprintf("`images` must be up to %d paths from /v1/uploads", maxImages))
		return
	case request.Communication != "" && !slices.Contains([]string{"concise", "detailed", "verbose"}, request.Communication):
		part.Error(w, http.StatusBadRequest, "`communication` must be one of concise, detailed, verbose")
		return
	case request.Mode != "" && !slices.Contains([]string{"auto", "manual"}, request.Mode):
		part.Error(w, http.StatusBadRequest, "`mode` must be one of auto, manual")
		return
	}
	response, err := n.Ask(r.Context(), request)
	if err != nil {
		part.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, response)
}
