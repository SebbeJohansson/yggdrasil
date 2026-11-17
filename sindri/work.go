package sindri

// Handing work to agents, and following them: coding tasks (one agent per
// repo, all on one branch name when a task spans several), question agents,
// follow-ups, the parts one agent asks for, the tool calls allowed on your
// behalf, failed tests sent back once, and every change posted into your
// conversation in Norn. Ported from my-agent's tasks/delegate.ts, parts.ts,
// agentFeed.ts, ledger.ts and followThrough.ts.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"yggdrasil/internal/linear"
	"yggdrasil/internal/tell"
)

// MaxParts is the most repositories one task spans.
const MaxParts = 4

// scheduledRetry is how long a task waiting for Claude usage waits between tries.
const scheduledRetry = 30 * time.Minute

// CodingAgentSettings say which CLI new agents run on.
type CodingAgentSettings struct {
	Primary string `json:"primary"` // claude or copilot
}

// AutomationSettings say what Sindri does on its own.
type AutomationSettings struct {
	AutoAllow     bool `json:"autoAllow"`
	FollowThrough bool `json:"followThrough"`
}

// NornClient posts what agents say into your conversations in Norn.
type NornClient struct {
	URL, Token string
	HTTP       *http.Client
}

// Post adds a message to a conversation: an agent's when agent is set, else Sindri's own.
func (c *NornClient) Post(ctx context.Context, session, agent, label, content string) error {
	if c == nil || c.URL == "" {
		return errors.New("norn is not set up")
	}
	body, _ := json.Marshal(map[string]string{"agent": agent, "label": label, "content": content})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+"/v1/conversations/"+url.PathEscape(session)+"/agent-turns", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.Token)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		text, _ := io.ReadAll(io.LimitReader(response.Body, 300))
		return fmt.Errorf("norn answered %d: %s", response.StatusCode, strings.TrimSpace(string(text)))
	}
	return nil
}

// Work hands work to agents and follows them.
type Work struct {
	Agents *Manager
	Store  *Store
	Norn   *NornClient
	// Model is the model coding and question agents run on; empty is the CLI's default.
	Model string
	// AskCwd is where question agents run.
	AskCwd string
	// Transcripts is where your Claude Code transcripts are, which question agents may read without asking.
	Transcripts string
	// AutoAllowDefault is automation.autoAllow before it is set.
	AutoAllowDefault bool
	logf             func(string, ...any)
	// Linear lists issues as goal candidates and moves them as goals move; off without LINEAR_KEY.
	Linear       *linear.Client
	linearFailed map[int64]time.Time
	// Hermodr reaches you through Hermóðr: the night shift's morning report.
	Hermodr tell.Teller
	// Zone is the time zone the night shift's times are in.
	Zone    *time.Location
	feeding sync.Mutex
}

// CodingAgent is the CLI new agents run on.
func (w *Work) CodingAgent() CodingAgentSettings {
	settings := CodingAgentSettings{Primary: "claude"}
	w.Store.Setting("coding-agent", &settings)
	return settings
}

// Automation is what Sindri does on its own.
func (w *Work) Automation() AutomationSettings {
	settings := AutomationSettings{AutoAllow: w.AutoAllowDefault, FollowThrough: true}
	w.Store.Setting("automation", &settings)
	return settings
}

func (w *Work) start(request StartRequest, communication, mode string) (*Agent, error) {
	if request.Engine == "" && !request.NoTools && w.CodingAgent().Primary == "copilot" {
		request.Engine = "copilot"
	}
	a, err := w.Agents.Start(request)
	if err != nil {
		return nil, err
	}
	if request.Session != "" {
		_ = w.Store.Bind(a.ID, request.Session, communication, mode, signatureOf(a))
	}
	return a, nil
}

// --- coding tasks ------------------------------------------------------------------

// CodeRequest is POST /v1/work/code: the hand-over Norn built, and where.
type CodeRequest struct {
	Session string `json:"session"`
	// Prompt is the task with its background, as Norn wrote it.
	Prompt string `json:"prompt"`
	// Name is the task in a few words, for labels and the branch.
	Name          string   `json:"name"`
	Projects      []string `json:"projects"`
	Communication string   `json:"communication,omitempty"`
	Mode          string   `json:"mode,omitempty"`
	GoalID        *int64   `json:"goalId,omitempty"`
	// Goal is what the ledger calls the task, in your words; Name when empty.
	Goal string `json:"goal,omitempty"`
}

// Started is one agent a request started.
type Started struct {
	AgentID string `json:"agentId"`
	Status  string `json:"status"`
	Project string `json:"project,omitempty"`
	Branch  string `json:"branch,omitempty"`
}

// Handed is what handing work over did.
type Handed struct {
	Answer  string    `json:"answer"`
	Parts   []Started `json:"parts"`
	Busy    []string  `json:"busy"`
	Failed  []string  `json:"failed"`
	partOf  int64
	goalID  *int64
	pending []StartRequest
}

type joining struct {
	slug     string
	projects []string
}

func (w *Work) codeRequest(request CodeRequest, project string, slug string, others []string, lead string) StartRequest {
	system := CodingPrompt
	if len(others) > 0 {
		system += "\n\n" + PartsPrompt(slug, project, others, lead)
	}
	if rules := Rules(request.Communication, request.Mode); rules != "" {
		system += "\n\n" + rules
	}
	return StartRequest{
		Cwd: project, Prompt: request.Prompt, Label: project + ": " + request.Name, Model: w.Model, Worktree: true,
		PermissionMode: "auto", AllowedTools: append(append([]string{}, CodingAllowed...), PlanRules(slug)...),
		DisallowedTools: CodingDenied, AppendSystemPrompt: system, Slug: slug, Session: request.Session,
	}
}

// Code starts one coding agent per project, all on one branch name when
// there are several. A project the host has no room for waits, and starts
// when an agent finishes.
func (w *Work) Code(request CodeRequest, join *joining) Handed {
	projects := slices.Compact(slices.Clone(request.Projects))
	several := len(projects) > 1 || join != nil
	slug, lead, all := "", "", projects
	if several {
		slug, lead = PartsSlug(request.Name), projects[0]
		if join != nil {
			slug, lead = join.slug, ""
			all = slices.Compact(append(slices.Clone(join.projects), projects...))
		}
	}
	handed := Handed{Parts: []Started{}, Busy: []string{}, Failed: []string{}, goalID: request.GoalID}
	for _, project := range projects {
		var others []string
		for _, other := range all {
			if other != project {
				others = append(others, other)
			}
		}
		start := w.codeRequest(request, project, slug, others, lead)
		a, err := w.start(start, request.Communication, request.Mode)
		if err != nil {
			var refused *HTTPError
			if errors.As(err, &refused) && refused.Status == 429 {
				handed.Busy = append(handed.Busy, project)
				handed.pending = append(handed.pending, start)
			}
			handed.Failed = append(handed.Failed, fmt.Sprintf("Could not start a coding agent in %s: %v", project, err))
			continue
		}
		started := Started{AgentID: a.ID, Status: a.Status, Project: project}
		if a.Worktree != nil {
			started.Branch = a.Worktree.Branch
		}
		handed.Parts = append(handed.Parts, started)
	}
	if len(handed.Parts) == 0 {
		handed.Answer = strings.Join(handed.Failed, "\n")
		return handed
	}
	branch := handed.Parts[0].Branch
	if branch == "" {
		branch = "agent/" + slug
	}
	var names []string
	for _, part := range handed.Parts {
		names = append(names, part.Project)
	}
	switch {
	case !several:
		on := ""
		if handed.Parts[0].Branch != "" {
			on = " on branch " + handed.Parts[0].Branch
		}
		handed.Answer = fmt.Sprintf("Claude is coding this in %s%s. Your own checkout is untouched; the summary will appear here.", handed.Parts[0].Project, on)
	case join != nil:
		handed.Answer = fmt.Sprintf("Started the %s part%s on %s, beside %s. Your own checkouts are untouched.", strings.Join(names, " and "), plural(len(names)), branch, strings.Join(join.projects, " and "))
	default:
		handed.Answer = fmt.Sprintf("Claude is coding this in %d parts, one agent each: %s, all on %s. Your own checkouts are untouched; each summary will appear here.", len(names), strings.Join(names, ", "), branch)
	}
	if len(handed.Failed) > 0 {
		handed.Answer += "\n" + strings.Join(handed.Failed, "\n")
	}
	return handed
}

// record writes what was handed over into the ledger: one task, or one per
// part, and a waiting task for each project that had no room.
func (w *Work) record(session, goal string, handed *Handed) {
	several := len(handed.Parts) > 1 || handed.partOf != 0
	group := handed.partOf
	for _, part := range handed.Parts {
		partOf := int64(0)
		if several {
			partOf = group
			if partOf == 0 {
				partOf = -1
			}
		}
		id, err := w.Store.AddTask(session, goal, "code", part.AgentID, part.Project, part.Branch, partOf, handed.goalID)
		if err != nil {
			w.logf("could not record a task: %v", err)
			continue
		}
		if several && group == 0 {
			group = id
		}
	}
	for _, start := range handed.pending {
		if group == 0 && len(handed.Parts) > 0 {
			group = -1
		}
		id, err := w.Store.AddTask(session, goal, "code", "", start.Cwd, "", group, handed.goalID)
		if err != nil {
			continue
		}
		// It starts when an agent finishes: the next feed pass after that.
		waiting := Scheduled{Prompt: mustJSON(start), NextTryAt: w.Store.ms()}
		_, _ = w.Store.db.Exec(`UPDATE tasks SET outcome = 'scheduled', result = 'Waiting for room for another agent.', scheduled = ? WHERE id = ?`, mustJSON(waiting), id)
	}
}

func mustJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// --- questions, follow-ups ---------------------------------------------------------

// AskRequest is POST /v1/work/ask: a question that needs tools.
type AskRequest struct {
	Session       string `json:"session"`
	Prompt        string `json:"prompt"`
	Name          string `json:"name"`
	Communication string `json:"communication,omitempty"`
	Mode          string `json:"mode,omitempty"`
	// Agent is the conversation's question agent, followed up when it is settled.
	Agent string `json:"agent,omitempty"`
	// FollowUp is what the agent is sent when it is followed up instead of started.
	FollowUp string `json:"followUp,omitempty"`
	Goal     string `json:"goal,omitempty"`
}

func settled(status string) bool {
	return status == Waiting || status == Done || status == Failed || status == Aborted
}

// Ask hands a question to a question agent: the conversation's own when it
// is settled, else a new one, read-only, across every project.
func (w *Work) Ask(request AskRequest) (*Agent, error) {
	rules := Rules(request.Communication, request.Mode)
	if request.Agent != "" {
		if existing, err := w.Agents.Get(request.Agent); err == nil && settled(existing.Status) && exists(existing.Cwd) {
			text := request.FollowUp
			if text == "" {
				text = request.Prompt
			}
			if rules != "" {
				text += "\n\n(Norn: " + rules + ")"
			}
			a, err := w.Agents.Prompt(existing.ID, text, nil, false)
			if err == nil {
				_ = w.Store.Bind(a.ID, request.Session, request.Communication, request.Mode, "")
				_ = w.Store.ReopenTask(a.ID)
			}
			return a, err
		}
	}
	prompt := request.Prompt
	if rules != "" {
		prompt += "\n\n" + rules
	}
	a, err := w.start(StartRequest{
		Cwd: w.AskCwd, Prompt: prompt, Label: "ask: " + request.Name, Model: w.Model, AddRoots: true,
		AllowedTools: w.askAllowed(), DisallowedTools: AskDenied, Session: request.Session,
	}, request.Communication, request.Mode)
	if err != nil {
		return nil, err
	}
	goal := request.Goal
	if goal == "" {
		goal = request.Name
	}
	_, _ = w.Store.AddTask(request.Session, goal, "ask", a.ID, "", "", 0, nil)
	return a, nil
}

// Reply sends your words to an agent that has stopped, and puts it in the
// conversation you wrote from.
func (w *Work) Reply(session, agentID, text string, images []string, communication, mode string) (*Agent, error) {
	a, err := w.Agents.Get(agentID)
	if err != nil {
		return nil, err
	}
	if !settled(a.Status) {
		if a.Status == NeedsApproval {
			return nil, httpErr(409, "That agent is waiting for your yes or no on a tool; reply yes or no.")
		}
		return nil, httpErr(409, "That agent is still working.")
	}
	if rules := Rules(communication, mode); rules != "" {
		text += "\n\n(Norn: " + rules + ")"
	}
	prompted, err := w.Agents.Prompt(agentID, text, images, false)
	if err != nil {
		return nil, err
	}
	if session != "" {
		_ = w.Store.Bind(agentID, session, communication, mode, signatureOf(prompted))
	}
	_ = w.Store.ReopenTask(agentID)
	return prompted, nil
}

// Tell passes a message to an agent, after its current turn if it is busy.
func (w *Work) Tell(agentID, text string) (*Agent, error) {
	return w.Agents.Prompt(agentID, text, nil, true)
}

// --- parts one agent asks for ------------------------------------------------------

// actOnNotes starts the parts an agent asked for and passes on what one
// part tells another. False when a part had to wait for room, so the note
// is acted on again next time.
func (w *Work) actOnNotes(ctx context.Context, a *Agent, notes string) bool {
	task, err := w.Store.TaskOfAgent(a.ID)
	if err != nil || task == nil {
		return true
	}
	projects := projectNames(FindProjects(w.Agents.roots[0]))
	if wanted := NeededParts(notes, projects); len(wanted) > 0 && (task.Kind != "code" || task.Branch != nil) {
		var busy []string
		for _, part := range wanted {
			handed := w.startPart(ctx, task, part)
			busy = append(busy, handed.Busy...)
		}
		if len(busy) > 0 {
			w.say(ctx, task.Session, fmt.Sprintf("The %s part%s to start: the host already runs as many agents as it may. It starts when one finishes.", strings.Join(busy, " and "), map[bool]string{true: "s wait", false: " waits"}[len(busy) > 1]))
			return false
		}
	}
	if task.PartOf == nil {
		return true
	}
	parts, _ := w.Store.PartsOf(*task.PartOf)
	from := "other"
	if task.Project != nil {
		from = *task.Project
	}
	for _, message := range PartMessages(notes, projects) {
		var to *Task
		for i := len(parts) - 1; i >= 0; i-- {
			if parts[i].Project != nil && *parts[i].Project == message.Project && parts[i].Agent != a.ID && parts[i].Agent != "" {
				to = &parts[i]
				break
			}
		}
		if to == nil {
			continue
		}
		if _, err := w.Tell(to.Agent, fmt.Sprintf("The %s part says: %s", from, message.Task)); err != nil {
			w.say(ctx, task.Session, fmt.Sprintf("Could not tell the %s part \"%s\": %v", message.Project, message.Task, err))
			continue
		}
		w.say(ctx, task.Session, fmt.Sprintf("The %s part told the %s part: %s", from, message.Project, message.Task))
	}
	return true
}

func projectNames(projects []Project) []string {
	names := make([]string, len(projects))
	for i, p := range projects {
		names[i] = p.Name
	}
	return names
}

// startPart adds a project to a task, on the task's branch name.
func (w *Work) startPart(ctx context.Context, of *Task, part ProjectTask) Handed {
	binding, _ := w.Store.BindingOf(of.Agent)
	communication, mode := "", ""
	if binding != nil {
		communication, mode = binding.Communication, binding.Mode
	}
	if of.Kind == "ask" || of.Branch == nil {
		// A question agent found code to change: a coding task of its own.
		request := CodeRequest{Session: of.Session, Prompt: of.Goal + "\n\nThe agent that looked into it says " + part.Project + " needs: " + part.Task,
			Name: ShortTask(of.Goal, 50), Projects: []string{part.Project}, Communication: communication, Mode: mode, Goal: of.Goal}
		handed := w.Code(request, nil)
		w.record(of.Session, of.Goal, &handed)
		if len(handed.Parts) > 0 {
			w.say(ctx, of.Session, handed.Answer)
		}
		return handed
	}
	group, err := w.Store.MakeParts(of.ID)
	if err != nil {
		return Handed{}
	}
	have, _ := w.Store.PartsOf(group)
	var projects []string
	for _, p := range have {
		if p.Project != nil {
			if *p.Project == part.Project {
				return Handed{}
			}
			projects = append(projects, *p.Project)
		}
	}
	if len(have) >= MaxParts {
		w.say(ctx, of.Session, fmt.Sprintf("That task already has %d parts; start %s as a task of its own.", MaxParts, part.Project))
		return Handed{}
	}
	from := "other"
	if of.Project != nil {
		from = *of.Project
	}
	request := CodeRequest{Session: of.Session, Prompt: fmt.Sprintf("%s\n\nThe %s part's agent says this part needs: %s", of.Goal, from, part.Task),
		Name: ShortTask(of.Goal, 50), Projects: []string{part.Project}, Communication: communication, Mode: mode, Goal: of.Goal}
	handed := w.Code(request, &joining{slug: strings.TrimPrefix(*of.Branch, "agent/"), projects: projects})
	handed.partOf, handed.goalID = group, of.GoalID
	w.record(of.Session, of.Goal, &handed)
	if len(handed.Parts) > 0 {
		w.say(ctx, of.Session, handed.Answer)
		if of.GoalID != nil {
			if goal, _ := w.Store.Goal(*of.GoalID); goal != nil && !slices.Contains(goal.Repos, part.Project) {
				_ = w.Store.UpdateGoal(goal.ID, nil, append(goal.Repos, part.Project), nil)
			}
		}
	}
	return handed
}

// say posts Sindri's own line into a conversation.
func (w *Work) say(ctx context.Context, session, text string) {
	if err := w.Norn.Post(ctx, session, "", "", text); err != nil {
		w.logf("could not post to the conversation: %v", err)
	}
}

// --- the feed --------------------------------------------------------------------------

// signatureOf is what the conversation was last told about an agent: a
// change in it is worth a message.
func signatureOf(a *Agent) string {
	status := a.Status
	if status == Done {
		status = Waiting
	}
	var pending []string
	for _, p := range a.Pending {
		pending = append(pending, p.RequestID)
	}
	return fmt.Sprintf("%s|%s|%d", status, strings.Join(pending, ","), len(a.LastText))
}

var regexpParagraph = regexp.MustCompile(`\n\s*\n`)

func lastParagraph(text string) string {
	paragraphs := regexpParagraph.Split(strings.TrimSpace(text), -1)
	return strings.TrimSpace(paragraphs[len(paragraphs)-1])
}

// PostFor is what the conversation is told about an agent now; empty when nothing.
func PostFor(a *Agent, communication, lastError string) string {
	concise := communication == "concise"
	switch a.Status {
	case NeedsApproval:
		wants := "a tool"
		if len(a.Pending) > 0 {
			wants = a.Pending[0].Summary
		}
		if concise {
			return "Run " + wants + "? yes or no."
		}
		return "Wants to run " + wants + ". Reply yes to allow it, or no (and why) to deny it."
	case Waiting, Done:
		text := strings.TrimSpace(a.LastText)
		if concise && a.AsksForReply() {
			text = lastParagraph(a.LastText)
		}
		if text == "" {
			return "Finished, without a message."
		}
		return text
	case Failed:
		switch {
		case lastError != "":
			return "Failed: " + lastError
		case a.LastText != "":
			return "Failed: " + a.LastText
		}
		return "Failed."
	case Aborted:
		return "Stopped."
	}
	return ""
}

func outcomeOf(a *Agent) string {
	switch a.Status {
	case Waiting, Done:
		if a.AsksForReply() {
			return "asked"
		}
		return "done"
	case Failed:
		return "failed"
	case Aborted:
		return "stopped"
	}
	return ""
}

type snapshot struct {
	agent     Agent
	lastError string
	merged    bool
}

func (w *Work) snapshots() []snapshot {
	w.Agents.mu.Lock()
	defer w.Agents.mu.Unlock()
	var list []snapshot
	for _, a := range w.Agents.listLocked() {
		s := snapshot{agent: *a}
		s.agent.Pending = slices.Clone(a.Pending)
		s.agent.Log = nil
		for i := len(a.Log) - 1; i >= 0; i-- {
			if a.Log[i].Kind == "error" {
				s.lastError = a.Log[i].Text
				break
			}
		}
		list = append(list, s)
	}
	return list
}

// Feed is one pass over every agent: approvals, notes, follow-through, and
// what changed posted into its conversation. A pass still running when the
// next is due is skipped.
func (w *Work) Feed(ctx context.Context) error {
	if !w.feeding.TryLock() {
		return nil
	}
	defer w.feeding.Unlock()
	automation := w.Automation()
	for _, snap := range w.snapshots() {
		a := &snap.agent
		if automation.AutoAllow {
			w.autoAllow(a)
		}
		binding, err := w.Store.BindingOf(a.ID)
		if err != nil || binding == nil {
			continue
		}
		var fresh []string
		lastNote := binding.NotesSeen
		for _, n := range a.Notes {
			if n.N > binding.NotesSeen {
				fresh = append(fresh, n.Text)
				lastNote = n.N
			}
		}
		if len(fresh) > 0 && w.actOnNotes(ctx, a, strings.Join(fresh, "\n")) {
			_ = w.Store.SetNotesSeen(a.ID, lastNote)
		}
		if automation.FollowThrough && w.retryFailedCheck(ctx, a, binding.Session) {
			continue
		}
		signature := signatureOf(a)
		if signature == binding.Seen {
			continue
		}
		_ = w.Store.SetSeen(a.ID, signature)
		if a.Status == Starting || a.Status == Working {
			_ = w.Store.ReopenTask(a.ID)
		}
		if picked := PickedTask(a.Result + "\n" + a.LastText); picked != "" && Relabel(a.Label, picked) != a.Label {
			if renamed, err := w.Agents.Rename(a.ID, Relabel(a.Label, picked)); err == nil {
				a.Label = renamed.Label
			}
		}
		if outcome := outcomeOf(a); outcome != "" {
			result := snap.lastError
			if result == "" {
				result = a.LastText
			}
			if outcome == "failed" && a.UsageLimited && a.Worktree != nil {
				_ = w.Store.Schedule(a.ID, Scheduled{AgentID: a.ID, NextTryAt: w.Store.now().Add(scheduledRetry).UnixMilli()},
					strings.TrimSpace("Waiting for Claude usage before continuing. "+result))
			} else {
				if outcome == "stopped" {
					result = ""
				}
				_ = w.Store.EndTask(a.ID, outcome, result)
			}
		}
		said := *a
		if (a.Status == Waiting || a.Status == Done) && a.Result != "" {
			said.LastText = a.Result
		}
		if message := PostFor(&said, binding.Communication, snap.lastError); message != "" {
			if err := w.Norn.Post(ctx, binding.Session, a.ID, TaskOf(a.Label), message); err != nil {
				w.logf("could not post an agent's message: %v", err)
				_ = w.Store.SetSeen(a.ID, binding.Seen)
			}
		}
	}
	w.retryScheduled(ctx)
	return nil
}

// autoAllow answers the pending calls the approval rules allow, and writes each down.
func (w *Work) autoAllow(a *Agent) {
	if a.Status != NeedsApproval {
		return
	}
	folders := []string{a.Cwd}
	if a.Worktree != nil {
		folders = []string{a.Worktree.Path, a.Cwd}
	}
	for _, call := range a.Pending {
		rule := AutoAllowRule(call.ToolName, call.Input, folders)
		if rule == "" {
			continue
		}
		if _, err := w.Agents.Answer(a.ID, call.RequestID, true, ""); err == nil {
			_ = w.Store.AddAutoAllowed(a.ID, call.ToolName, call.Summary, rule)
		}
	}
}

// retryFailedCheck sends failed tests back to the agent, once per task.
func (w *Work) retryFailedCheck(ctx context.Context, a *Agent, session string) bool {
	if a.Worktree == nil || a.Check == nil || a.Check.Status != "failed" || a.AsksForReply() || (a.Status != Waiting && a.Status != Done) || !exists(a.Cwd) || a.Worktree.IsLanded() {
		return false
	}
	key := "follow-through-retried:" + a.ID
	if task, _ := w.Store.TaskOfAgent(a.ID); task != nil {
		key = fmt.Sprintf("follow-through-retried:%d", task.ID)
	}
	var head string
	if w.Store.Setting(key, &head) {
		return false
	}
	_ = w.Store.SetSetting(key, a.Check.Head)
	if _, err := w.Tell(a.ID, RetryPrompt(a.Check)); err != nil {
		w.logf("could not send the failed tests back: %v", err)
		return false
	}
	if err := w.Norn.Post(ctx, session, a.ID, TaskOf(a.Label), fmt.Sprintf("Tests failed (`%s`); sent the failure back to fix, once.", a.Check.Command)); err != nil {
		w.logf("could not post to the conversation: %v", err)
	}
	return true
}

// retryScheduled starts the tasks waiting for room or for Claude usage, when due.
func (w *Work) retryScheduled(ctx context.Context) {
	tasks, err := w.Store.ScheduledTasks()
	if err != nil {
		return
	}
	now := w.Store.ms()
	for _, task := range tasks {
		if task.Scheduled == nil || task.Scheduled.NextTryAt > now {
			continue
		}
		var a *Agent
		if task.Scheduled.AgentID != "" {
			a, err = w.Agents.ResumeScheduled(task.Scheduled.AgentID, "Claude usage is available again. Continue the task from where you stopped: "+task.Goal)
		} else {
			var start StartRequest
			if json.Unmarshal([]byte(task.Scheduled.Prompt), &start) != nil {
				continue
			}
			binding := &Binding{}
			if b, _ := w.Store.BindingOf(task.Agent); b != nil {
				binding = b
			}
			a, err = w.start(start, binding.Communication, binding.Mode)
		}
		if err != nil {
			next := *task.Scheduled
			next.NextTryAt, next.Retries = w.Store.now().Add(scheduledRetry).UnixMilli(), next.Retries+1
			_, _ = w.Store.db.Exec(`UPDATE tasks SET scheduled = ? WHERE id = ?`, mustJSON(next), task.ID)
			continue
		}
		_ = w.Store.Unschedule(task.ID, a.ID)
		if a.Worktree != nil {
			_, _ = w.Store.db.Exec(`UPDATE tasks SET branch = ? WHERE id = ?`, a.Worktree.Branch, task.ID)
		}
		w.say(ctx, task.Session, fmt.Sprintf("Started the waiting %s part.", nullString(task.Project, "coding")))
	}
}

func nullString(text *string, fallback string) string {
	if text == nil {
		return fallback
	}
	return *text
}

// --- the ledger ---------------------------------------------------------------------------

// TaskView is a task with its live status.
type TaskView struct {
	Task
	// Status is needs-you, running, done, failed, stopped, gone or scheduled.
	Status       string  `json:"status"`
	Label        *string `json:"label"`
	LastActivity int64   `json:"lastActivity"`
	Merged       bool    `json:"merged"`
}

func liveStatus(a *Agent) string {
	switch a.Status {
	case NeedsApproval:
		return "needs-you"
	case Starting, Working:
		return "running"
	case Waiting, Done:
		if a.AsksForReply() {
			return "needs-you"
		}
		return "done"
	case Failed:
		return "failed"
	}
	return "stopped"
}

// TaskViews is tasks with their agents' live state.
func (w *Work) TaskViews(tasks []Task) []TaskView {
	byID := map[string]snapshot{}
	for _, s := range w.snapshots() {
		byID[s.agent.ID] = s
	}
	latest := map[string]int64{}
	for _, t := range tasks {
		latest[t.Agent] = max(latest[t.Agent], t.ID)
	}
	views := make([]TaskView, len(tasks))
	for i, t := range tasks {
		s, known := byID[t.Agent]
		current := known && latest[t.Agent] == t.ID
		view := TaskView{Task: t, LastActivity: max(t.At, deref(t.EndedAt))}
		switch {
		case current && (t.Outcome == nil || *t.Outcome == "asked"):
			view.Status = liveStatus(&s.agent)
		case t.Outcome != nil && *t.Outcome != "asked":
			view.Status = *t.Outcome
		case known:
			view.Status = "done"
		default:
			view.Status = "gone"
		}
		if known {
			label := s.agent.Label
			view.Label = &label
			view.Merged = s.agent.Worktree != nil && s.agent.Worktree.IsLanded()
			if current {
				view.LastActivity = max(view.LastActivity, s.agent.LastActivity)
			}
		}
		views[i] = view
	}
	return views
}

func deref(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}

// askAllowed are the tools a question agent uses without asking: reading,
// your services read-only, and your Claude Code transcripts.
func (w *Work) askAllowed() []string {
	allowed := append(append([]string{}, AskAllowed...), ReadOnlyMCPTools()...)
	if w.Transcripts != "" {
		allowed = append(allowed, "Read("+w.Transcripts+"/**)")
	}
	return allowed
}
