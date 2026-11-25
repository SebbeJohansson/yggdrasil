package norn

// What each message is: answered here, handed to a question agent, a coding
// task, a reply to an agent, or a status question. One decision by a small
// model with a JSON schema, then guard rails that fix the mistakes it was
// measured making. Ported from my-agent's tasks/dispatch.ts and the routing
// half of tasks/delegate.ts.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"yggdrasil/internal/llm"
)

// Actions the dispatcher picks from.
var Actions = []string{"answer", "delegate", "code", "reply_to_agent", "status"}

// Decision is what a message is.
type Decision struct {
	Action  string  `json:"action"`
	Agent   *string `json:"agent"`
	Project *string `json:"project"`
}

// NamedAgent is an agent with a short name the model can say.
type NamedAgent struct {
	Name  string
	Agent Agent
}

// Line is one line of the conversation as the dispatcher reads it.
type Line struct {
	Who, Text string
}

// DispatchInput is what the dispatcher reads.
type DispatchInput struct {
	Message      string
	HasCalendar  bool
	Conversation []Line
	Agents       []NamedAgent
	Projects     []string
}

var (
	nameWords = regexp.MustCompile(`[a-z0-9]+`)
	nameStop  = map[string]bool{}
)

func init() {
	for _, word := range strings.Fields("the a an to of in on at for with from into and or my it this that is are was be do does did what which how many much there can you please top add make fix change update find check") {
		nameStop[word] = true
	}
}

// TaskOf is an agent's task as a name: its label without the project.
func TaskOf(label string) string {
	if label == "" {
		return "the task"
	}
	return regexp.MustCompile(`^[\w.-]+:\s*`).ReplaceAllString(label, "")
}

// NameAgents gives each agent a short, unique name from its task.
func NameAgents(agents []Agent) []NamedAgent {
	used := map[string]bool{}
	named := make([]NamedAgent, len(agents))
	for i, a := range agents {
		var words []string
		for _, word := range nameWords.FindAllString(strings.ToLower(TaskOf(a.Label)), -1) {
			if !nameStop[word] {
				words = append(words, word)
			}
		}
		base := strings.Join(words[:min(2, len(words))], "-")
		if base == "" {
			base = "agent"
		}
		if len(base) > 24 {
			base = base[:24]
		}
		name := base
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		used[name] = true
		named[i] = NamedAgent{Name: name, Agent: a}
	}
	return named
}

func clipText(text string, limit int) string {
	flat := []rune(strings.TrimSpace(whitespace.ReplaceAllString(text, " ")))
	if len(flat) > limit {
		return string(flat[:limit-1]) + "…"
	}
	return string(flat)
}

func stateOf(a Agent) string {
	if !a.Resumable {
		switch a.Status {
		case "aborted":
			return "stopped, closed for good (takes no follow-ups)"
		case "failed":
			return "failed, closed for good (takes no follow-ups)"
		}
		return "finished, closed for good (takes no follow-ups)"
	}
	if a.AsksYou {
		return "finished, waiting for the person's reply"
	}
	return map[string]string{"starting": "starting", "working": "working", "needs-approval": "waiting for an approval",
		"waiting": "finished", "done": "finished", "failed": "failed", "aborted": "stopped"}[a.Status]
}

var (
	labelProject = regexp.MustCompile(`^([\w.-]+):\s`)
	aPath        = regexp.MustCompile(`(?:/[\w.@-]+){2,}`)
)

// WorkedIn is the project an agent's tool calls touched most: the first
// folder in each path that is a project, so yggdrasil/loki counts as
// yggdrasil. Empty when its tools named no project.
func WorkedIn(a Agent, projects []string) string {
	known := map[string]bool{}
	for _, p := range projects {
		known[p] = true
	}
	counts := map[string]int{}
	best := ""
	for _, entry := range a.Log {
		if entry.Kind != "tool" {
			continue
		}
		for _, path := range aPath.FindAllString(entry.Text, -1) {
			for _, part := range strings.Split(path, "/") {
				if known[part] {
					counts[part]++
					if counts[part] > counts[best] || best == "" {
						best = part
					}
					break
				}
			}
		}
	}
	return best
}

func projectOf(a Agent, projects []string) string {
	if a.Worktree != nil {
		return a.Worktree.Repo[strings.LastIndex(a.Worktree.Repo, "/")+1:]
	}
	if a.WorkedIn != "" {
		return a.WorkedIn
	}
	if match := labelProject.FindStringSubmatch(a.Label); match != nil {
		return match[1]
	}
	if project := MentionedProject(a.Label, projects); project != "" {
		return project
	}
	return "no project"
}

const dispatchSystem = `You dispatch messages for a personal assistant. Decide what to do with the person's newest message. Actions:
- answer: the assistant answers it itself. It knows general knowledge, the person's profile and memories, and their unread inbox. It has no tools.
- delegate: a question that needs tools: reading files or code in a project, the web or current information, calendars, tickets, documents.
- code: the person wants code or files changed in one of their projects. Set project.
- reply_to_agent: the message is meant for one of the agents: an answer to its question, or more instructions or a question about its task. Set agent.
- status: the person asks how an agent's own work is going: whether it is still working or has finished. Set agent. Not for the state of something outside the assistant, such as a ticket, issue or pull request: ask the agent who worked on it (reply_to_agent), or delegate.
Rules:
1. If the last line of the conversation is an agent asking the person something, and the newest message answers it or picks an option, it is reply_to_agent for that agent -- even when it is short or says "it".
2. A request that names a project and asks for something to be fixed, added, changed, renamed, bumped or made is code, never delegate.
3. Looking something up in a project, on the web, or in calendars and tickets is delegate.
4. Questions about the person's own colleagues, team or inbox are answer: the assistant knows those.
5. A ticket to work on, or a change, that names no project is delegate: that agent finds the repository and hands the coding on.
6. A follow-up on what an agent already did -- it refers to that agent's task or result, often with "also", "and", "those" or "that change" -- is reply_to_agent for that agent, not delegate: the agent has the context.
7. Committing, pushing or adding a remote for changes an agent made is reply_to_agent for the agent that worked in that project, not code: a new coding agent starts from a clean copy without those changes.
Examples (other agents and projects than the real ones):
- Agent "migrate" last asked "Should I also run it against staging?"; message "yes, staging too" -> {"action":"reply_to_agent","agent":"migrate","project":null}
- Agent "migrate" last asked "Keep the old column or drop it?"; message "drop it" -> {"action":"reply_to_agent","agent":"migrate","project":null}
- Agent "docs" finished updating the API docs; message "also mention the rate limits there" -> {"action":"reply_to_agent","agent":"docs","project":null}
- Agent "invoices" finished listing unpaid invoices; message "which of those are older than 30 days?" -> {"action":"reply_to_agent","agent":"invoices","project":null}
- Agent "search" built a search page in shop-app; message "commit it and push to origin" -> {"action":"reply_to_agent","agent":"search","project":null}
- Message "is the migration finished yet?" -> {"action":"status","agent":"migrate","project":null}
- Agent "billing" finished work on ticket SHOP-12; message "is the ticket marked as done in Jira?" -> {"action":"reply_to_agent","agent":"billing","project":null}
- Message "change the button colour to green in shop-app" -> {"action":"code","agent":null,"project":"shop-app"}
- Message "what does the deploy script in shop-app do?" -> {"action":"delegate","agent":null,"project":null}
- Message "how tall is Mount Everest?" -> {"action":"answer","agent":null,"project":null}
- Message "who is my manager?" -> {"action":"answer","agent":null,"project":null}
Reply with JSON only.`

// DispatchSystem is the system prompt; with a calendar, meeting questions are answer.
func DispatchSystem(hasCalendar bool) string {
	if !hasCalendar {
		return dispatchSystem
	}
	return strings.NewReplacer(
		"their unread inbox. It has no tools.", "their unread inbox and their calendar for the next seven days. It has no tools.",
		"Reply with JSON only.", "5b. Questions about their own meetings in the next seven days (next meeting, am I free, what do I have tomorrow) are answer. Changing a meeting, or looking further ahead, is delegate.\nReply with JSON only.",
	).Replace(dispatchSystem)
}

// DispatchPrompt is what the dispatcher reads about this message.
func DispatchPrompt(input DispatchInput) string {
	projects := strings.Join(input.Projects, ", ")
	if projects == "" {
		projects = "none"
	}
	lines := []string{"Projects: " + projects, "Agents:"}
	if len(input.Agents) == 0 {
		lines = append(lines, "- none")
	}
	for _, named := range input.Agents {
		a := named.Agent
		lines = append(lines, fmt.Sprintf(`- %s: task "%s" in %s; %s. Last message: "%s"`, named.Name, clipText(TaskOf(a.Label), 80), projectOf(a, input.Projects), stateOf(a), clipText(a.LastText, 240)))
	}
	lines = append(lines, "Conversation, oldest first:")
	if len(input.Conversation) == 0 {
		lines = append(lines, "(empty)")
	}
	for _, line := range input.Conversation {
		lines = append(lines, line.Who+": "+clipText(line.Text, 300))
	}
	return strings.Join(append(lines, "", "Newest message from the person: "+input.Message), "\n")
}

func dispatchSchema(input DispatchInput) map[string]any {
	agents := []any{}
	for _, named := range input.Agents {
		agents = append(agents, named.Name)
	}
	projects := []any{}
	for _, project := range input.Projects {
		projects = append(projects, project)
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":  map[string]any{"type": "string", "enum": Actions},
			"agent":   map[string]any{"type": []string{"string", "null"}, "enum": append(agents, nil)},
			"project": map[string]any{"type": []string{"string", "null"}, "enum": append(projects, nil)},
		},
		"required": []string{"action", "agent", "project"},
	}
}

// ReadDecision reads the model's reply; false when it is not a decision.
func ReadDecision(raw string, input DispatchInput) (Decision, bool) {
	var parsed Decision
	if json.Unmarshal([]byte(raw), &parsed) != nil || !slices.Contains(Actions, parsed.Action) {
		return Decision{}, false
	}
	decision := Decision{Action: parsed.Action}
	if parsed.Agent != nil && slices.ContainsFunc(input.Agents, func(n NamedAgent) bool { return n.Name == *parsed.Agent }) {
		decision.Agent = parsed.Agent
	}
	if parsed.Project != nil && slices.Contains(input.Projects, *parsed.Project) {
		decision.Project = parsed.Project
	}
	return decision, true
}

var (
	// Change is a request to change something.
	Change  = regexp.MustCompile(`(?i)\b(fix|add|change|rename|bump|update|upgrade|remove|delete|refactor|implement|make|move|replace|write|create|set up|log|work(ing)? on|pick up|tackle|next (task|item|todo))\b`)
	asksHow = regexp.MustCompile(`(?i)^\s*(how|why|where|when|which|what does|does|do|is|are|should)\b`)
	live    = regexp.MustCompile(`(?i)\b(weather|news|latest|newest|current (version|price)|today'?s?|calendar|meetings?|linear|jira|asana|tickets?|issues? assigned|pull requests?|prs?)\b`)
	ticket  = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,9}-\d+\b`)
	gitAsk  = regexp.MustCompile(`(?i)\b(commit|push|remote|stage)\b`)
	outside = regexp.MustCompile(`(?i)\b(linear|jira|asana|github|tickets?|issues?|pull requests?|prs?|marked|merged|deployed|released)\b`)
)

func str(text string) *string { return &text }

type guard struct {
	name  string
	apply func(Decision, DispatchInput) *Decision
}

func agentNamed(input DispatchInput, name *string) *Agent {
	if name == nil {
		return nil
	}
	for _, named := range input.Agents {
		if named.Name == *name {
			return &named.Agent
		}
	}
	return nil
}

var guards = []guard{
	{"a named project is not answered locally", func(d Decision, in DispatchInput) *Decision {
		project := MentionedProject(in.Message, in.Projects)
		if d.Action != "answer" || project == "" {
			return nil
		}
		before := ""
		for i := len(in.Conversation) - 1; i >= 0; i-- {
			if in.Conversation[i].Who == "person" {
				before = in.Conversation[i].Text
				break
			}
		}
		followUp := len(strings.Fields(in.Message)) <= 6 && !Change.MatchString(in.Message) && Change.MatchString(before)
		if (Change.MatchString(in.Message) && !asksHow.MatchString(in.Message)) || followUp {
			return &Decision{Action: "code", Project: str(project)}
		}
		return &Decision{Action: "delegate"}
	}},
	{"named project + change verb is code", func(d Decision, in DispatchInput) *Decision {
		project := MentionedProject(in.Message, in.Projects)
		if d.Action == "delegate" && project != "" && Change.MatchString(in.Message) && !asksHow.MatchString(in.Message) {
			return &Decision{Action: "code", Project: str(project)}
		}
		return nil
	}},
	{"committing an agent's work goes to that agent", func(d Decision, in DispatchInput) *Decision {
		if d.Action != "code" || d.Project == nil || !gitAsk.MatchString(in.Message) {
			return nil
		}
		for _, named := range in.Agents {
			if named.Agent.Resumable && projectOf(named.Agent, in.Projects) == *d.Project {
				return &Decision{Action: "reply_to_agent", Agent: str(named.Name)}
			}
		}
		return nil
	}},
	{"code needs a project", func(d Decision, in DispatchInput) *Decision {
		if d.Action != "code" || d.Project != nil {
			return nil
		}
		if project := MentionedProject(in.Message, in.Projects); project != "" {
			d.Project = str(project)
			return &d
		}
		return &Decision{Action: "delegate"}
	}},
	{"live information is never answered locally", func(d Decision, in DispatchInput) *Decision {
		if d.Action == "answer" && (live.MatchString(in.Message) || ticket.MatchString(in.Message)) && !(in.HasCalendar && AsksAboutCalendar(in.Message)) {
			return &Decision{Action: "delegate"}
		}
		return nil
	}},
	{"status of a ticket is not the agent's status", func(d Decision, in DispatchInput) *Decision {
		if d.Action != "status" || !(outside.MatchString(in.Message) || ticket.MatchString(in.Message)) {
			return nil
		}
		if a := agentNamed(in, d.Agent); a != nil && !a.Resumable {
			return &Decision{Action: "delegate"}
		}
		d.Action = "reply_to_agent"
		return &d
	}},
	{"a reply with no agent goes to the agent last spoken with", func(d Decision, in DispatchInput) *Decision {
		if d.Action != "reply_to_agent" || d.Agent != nil {
			return nil
		}
		names := map[string]bool{}
		for _, named := range in.Agents {
			if named.Agent.Resumable {
				names[named.Name] = true
			}
		}
		for i := len(in.Conversation) - 1; i >= 0; i-- {
			if names[in.Conversation[i].Who] {
				d.Agent = str(in.Conversation[i].Who)
				return &d
			}
		}
		return nil
	}},
	{"a closed agent takes no replies", func(d Decision, in DispatchInput) *Decision {
		if a := agentNamed(in, d.Agent); d.Action == "reply_to_agent" && a != nil && !a.Resumable {
			d.Action = "status"
			return &d
		}
		return nil
	}},
	{"agent actions need an agent", func(d Decision, _ DispatchInput) *Decision {
		if (d.Action == "reply_to_agent" || d.Action == "status") && d.Agent == nil {
			return &Decision{Action: "answer"}
		}
		return nil
	}},
}

// Dispatched is the decision, the model's before the guards, and which guards changed it.
type Dispatched struct {
	Model    *Decision
	Decision Decision
	Guards   []string
	MS       int64
}

// Dispatch decides what a message is.
func Dispatch(ctx context.Context, input DispatchInput, complete func(context.Context, []llm.Message, map[string]any) (string, error)) Dispatched {
	start := time.Now()
	var result Dispatched
	raw, err := complete(ctx, []llm.Message{{Role: "system", Content: DispatchSystem(input.HasCalendar)}, {Role: "user", Content: DispatchPrompt(input)}}, dispatchSchema(input))
	if err == nil {
		if decision, ok := ReadDecision(raw, input); ok {
			result.Model = &decision
		}
	}
	decision := Decision{Action: "answer"}
	if result.Model != nil {
		decision = *result.Model
	}
	result.Guards = []string{}
	for _, g := range guards {
		if fixed := g.apply(decision, input); fixed != nil {
			decision = *fixed
			result.Guards = append(result.Guards, g.name)
		}
	}
	result.Decision, result.MS = decision, time.Since(start).Milliseconds()
	return result
}

// --- rules from before the dispatcher, still the fallback -------------------------

var (
	approvalWords = regexp.MustCompile(`(?i)^(yes|y|ja|japp|ok|okay|sure|allow( it)?|approve( it)?|go( ahead)?|do it|no|n|nej|nope|deny( it)?|don'?t)\b[\s,.:!-]*([\s\S]*)$`)
	denial        = regexp.MustCompile(`(?i)^(no|n|nej|nope|deny|don'?t)`)
	retry         = regexp.MustCompile(`(?i)^\s*(try (it |that )?again|retry|again|do it again|försök igen|igen)[.!]?\s*$`)
	forced        = regexp.MustCompile(`^/(claude|local)\s+([\s\S]+)$`)
	agentCmd      = regexp.MustCompile(`^/agent\s+(\S+)\s+([\s\S]+)$`)
	partCmd       = regexp.MustCompile(`^/part\s+([\w.-]+)\s+([\s\S]+)$`)
	statusAsk     = regexp.MustCompile(`(?i)\b(status|progress|done yet|finished yet|any news|how('?s| is| are) (it|that|things)( going)?|what('?s| is) (it|claude|the agent) doing|still (working|running)|hur går det|är (den|det) klar)\b`)
	replyCmd      = regexp.MustCompile(`(?i)^(?:reply|answer|respond|tell|svara)\s+(?:to\s+|till\s+)?(?:the\s+)?([^:]+?)(?:\s+agent)?\s*:\s*([\s\S]+)$`)
	promise       = regexp.MustCompile(`(?i)\b(let me|i'?ll|i will|i'?m going to|going to)\s+(go\s+)?(check|look|find|verify|search|investigate|re-?check|dig|see|take a look|get back)\b`)
	notKnown      = regexp.MustCompile(`(?i)\b(i )?(don'?t|do not|can'?t|cannot) (know|see|tell)\b|\bnot (listed|shown|included|available|in (the|your) calendar)\b|\bno (information|details)\b`)
	overviewAsk   = regexp.MustCompile(`(?i)\b(agents|sessions|what('?s| is) running|anything running|waiting for (me|my (reply|answer))|need(s)? (me|my reply|an answer)|agenterna|vad körs)\b`)
	asksForWork   = regexp.MustCompile(`(?i)\b(please|pls|plz|can you|could you|would you|do (that|it|this)|go ahead|(spawn|launch|kick off) (a|an|two|three|\d+|new|some|more)|kan du|snälla|gör (det|detta))\b`)
	discusses     = regexp.MustCompile(`(?i)\b(improve|better|ideas?|ways? (to|of)|consider|think(ing)? about|i feel|feel like|suggest|should (i|we)|how (do|can|should) (i|we))\b`)
)

// ApprovalReply reads "yes", "no, because …" as an answer to a tool call; nil when it is not one.
func ApprovalReply(text string) (allow bool, message string, ok bool) {
	match := approvalWords.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil {
		return false, "", false
	}
	allow = !denial.MatchString(match[1])
	rest := strings.TrimSpace(match[len(match)-1])
	if allow && len([]rune(rest)) > 40 {
		return false, "", false // "ok so what about …" is a new message, not a yes
	}
	if allow {
		return true, "", true
	}
	return false, rest, true
}

// MentionedProjects is the projects a text names, longest names first.
func MentionedProjects(text string, projects []string) []string {
	lower := strings.ToLower(text)
	sorted := slices.Clone(projects)
	slices.SortFunc(sorted, func(a, b string) int { return len(b) - len(a) })
	var found []string
	for _, project := range sorted {
		parts := regexp.MustCompile(`[-_ ]+`).Split(strings.ToLower(project), -1)
		for i, part := range parts {
			parts[i] = regexp.QuoteMeta(part)
		}
		if regexp.MustCompile(`(^|[^a-z0-9-])` + strings.Join(parts, "[-_ ]?") + `($|[^a-z0-9-])`).MatchString(lower) {
			found = append(found, project)
		}
	}
	return found
}

// MentionedProject is the first project a text names; empty when none.
func MentionedProject(text string, projects []string) string {
	if found := MentionedProjects(text, projects); len(found) > 0 {
		return found[0]
	}
	return ""
}

// RecentAgents is the agents active in the last day, newest first.
func RecentAgents(agents []Agent, now time.Time) []Agent {
	var recent []Agent
	for _, a := range agents {
		if now.UnixMilli()-a.LastActivity < 24*3_600_000 {
			recent = append(recent, a)
		}
	}
	slices.SortStableFunc(recent, func(a, b Agent) int { return int(b.LastActivity - a.LastActivity) })
	return recent
}

var commonWords = map[string]bool{"what": true, "that": true, "this": true, "with": true, "from": true, "have": true, "status": true, "asked": true, "about": true, "there": true, "done": true, "does": true, "into": true, "your": true, "when": true, "which": true}

func wordsOf(text string) map[string]bool {
	words := map[string]bool{}
	for _, word := range regexp.MustCompile(`\p{L}{4,}`).FindAllString(strings.ToLower(text), -1) {
		if !commonWords[word] {
			words[word] = true
		}
	}
	return words
}

// LikelyAgent is the recent agent a question is about: the one whose task
// shares the most words with it, else the one asking you, else the newest.
func LikelyAgent(question string, agents []Agent, now time.Time) *Agent {
	recent := RecentAgents(agents, now)
	asked := wordsOf(question)
	var best *Agent
	bestScore := 0.0
	for i, a := range recent {
		score := 0.0
		for word := range wordsOf(a.Label) {
			if asked[word] {
				score++
			}
		}
		if a.AsksYou {
			score += 0.5
		}
		if score >= 1 && score > bestScore {
			best, bestScore = &recent[i], score
		}
	}
	if best != nil {
		return best
	}
	for i := range recent {
		if recent[i].AsksYou {
			return &recent[i]
		}
	}
	if len(recent) > 0 {
		return &recent[0]
	}
	return nil
}

func placeOf(a Agent) string {
	switch {
	case a.Worktree != nil:
		return a.Worktree.Repo[strings.LastIndex(a.Worktree.Repo, "/")+1:]
	case strings.HasPrefix(a.Label, "ask:"):
		return "a question"
	}
	if match := labelProject.FindStringSubmatch(a.Label); match != nil {
		return match[1]
	}
	return a.Cwd[strings.LastIndex(a.Cwd, "/")+1:]
}

func questionTail(text string, limit int) string {
	flat := strings.TrimSpace(whitespace.ReplaceAllString(text, " "))
	if len([]rune(flat)) <= limit {
		return flat
	}
	tail := string([]rune(flat)[len([]rune(flat))-limit:])
	return "…" + tail[strings.Index(tail, " ")+1:]
}

// StatusAnswer is how an agent is doing, said without a model.
func StatusAnswer(a *Agent) string {
	task := `"` + TaskOf(a.Label) + `"`
	branch := ""
	if a.Worktree != nil {
		branch = " Review branch " + a.Worktree.Branch + "."
	}
	lastStep := ""
	for _, entry := range a.Log {
		if entry.Kind == "tool" {
			lastStep = entry.Text
		}
	}
	closed := ""
	if !a.Resumable {
		closed = " Its worktree is gone, so it cannot take follow-ups; ask again to start a new agent."
	}
	switch a.Status {
	case "starting":
		return "Claude is starting on " + task + "."
	case "working":
		if lastStep != "" {
			return "Claude is still working on " + task + ". Last step: " + lastStep + "."
		}
		return "Claude is still working on " + task + "."
	case "needs-approval":
		wants := ""
		if len(a.Pending) > 0 {
			wants = ": it wants to run " + a.Pending[0].Summary
		}
		return "Claude is waiting for your approval" + wants + ". Reply yes to allow it, or no to deny it."
	case "waiting", "done":
		said := ""
		if a.LastText != "" {
			said = " " + a.LastText
		}
		return "Done with " + task + "." + said + branch
	case "failed":
		text := "Claude failed on " + task + "."
		if a.LastError != "" {
			text += " " + a.LastError + "."
		}
		if a.LastText != "" {
			text += " Its last message: " + a.LastText
		}
		return text + closed
	}
	return "Claude was stopped before finishing " + task + "." + closed
}

// OverviewLines is the recent agents, those asking you first.
func OverviewLines(agents []Agent, now time.Time) []string {
	phrase := map[string]string{"starting": "starting", "working": "working", "needs-approval": "waiting for your yes or no on a tool",
		"waiting": "finished", "done": "finished", "failed": "failed", "aborted": "stopped"}
	ordered := RecentAgents(agents, now)
	slices.SortStableFunc(ordered, func(a, b Agent) int {
		if a.AsksYou == b.AsksYou {
			return 0
		}
		if a.AsksYou {
			return -1
		}
		return 1
	})
	var lines []string
	for _, a := range ordered[:min(8, len(ordered))] {
		state := phrase[a.Status]
		if a.AsksYou {
			state = `asks you: "` + questionTail(a.LastText, 140) + `"`
		}
		lines = append(lines, fmt.Sprintf(`"%s" (%s): %s`, TaskOf(a.Label), placeOf(a), state))
	}
	return lines
}

func isShort(question string) bool {
	text := strings.TrimSpace(question)
	return len(regexp.MustCompile(`[.!?]+\s+\S`).FindAllString(text, -1)) == 0 && len(strings.Fields(text)) <= 16
}

// IsOverviewQuestion asks which agents there are.
func IsOverviewQuestion(question string) bool {
	return overviewAsk.MatchString(question) && !asksForWork.MatchString(question) && !discusses.MatchString(question) && isShort(question)
}

// OverviewAnswer lists the recent agents, said without a model.
func OverviewAnswer(agents []Agent, sessions []OwnSession, now time.Time) string {
	lines := OverviewLines(agents, now)
	own := OwnSessionsOverview(sessions)
	if len(lines) == 0 && len(own) == 0 {
		return "No agents or Claude Code sessions in the last day."
	}
	asking := 0
	for _, a := range RecentAgents(agents, now) {
		if a.AsksYou {
			asking++
		}
	}
	lead := fmt.Sprintf("%d agent%s in the last day.", len(lines), map[bool]string{true: "", false: "s"}[len(lines) == 1])
	if asking > 0 {
		verb := "are"
		if asking == 1 {
			verb = "is"
		}
		lead = fmt.Sprintf(`%d of %d recent agents %s waiting for your reply. Answer with "reply to <task>: <message>".`, asking, len(lines), verb)
	}
	for i := range lines {
		lines[i] = "• " + lines[i]
	}
	return strings.Join(append(append([]string{lead}, lines...), own...), "\n")
}

// HandoverPrompt is a question with the background an agent needs.
func HandoverPrompt(question, profile string, recalled []Memory, inbox []string, sessions []OwnSession, history []llm.Message, rules string) string {
	parts := []string{question, "", "---", "Context from Norn, their assistant (background; may be stale):"}
	if profile != "" {
		parts = append(parts, "", "About the person, in their words:", profile)
	}
	if len(recalled) > 0 {
		parts = append(parts, "", "Things they told Norn before:")
		for _, m := range recalled {
			parts = append(parts, "- "+m.Text)
		}
	}
	if inbox != nil {
		parts = append(parts, "", "Their unread inbox right now, most urgent first:")
		if len(inbox) == 0 {
			parts = append(parts, "- nothing unread")
		}
		for _, line := range inbox {
			parts = append(parts, "- "+line)
		}
	}
	parts = append(parts, SessionsSection(sessions)...)
	if rules != "" {
		parts = append(parts, "", rules)
	}
	if len(history) > 0 {
		parts = append(parts, "", "The conversation with Norn so far:")
		for _, m := range history {
			who := "Norn"
			if m.Role == "user" {
				who = "Them"
			}
			parts = append(parts, who+": "+m.Content)
		}
	}
	return strings.Join(parts, "\n")
}

const routerSystem = "You route questions for a personal assistant that has no tools. It can explain, advise, write code snippets and answer from general knowledge, and it knows the person's profile, what they told it before, and their unread inbox. Answer tools=true ONLY when the question needs to SEE or DO something specific: read or change files or code in their projects, check live or recent information (news, weather, latest versions), run commands, or use their calendar, tickets, documents or other accounts. Explaining a concept, a how-to, or writing a snippet from scratch is tools=false, even about code or git. Answer change=true only when they ask for code or files in one of their projects to be changed: fixed, added, refactored, updated. Looking, explaining or summarising is change=false."

var routerExamples = []struct {
	question      string
	tools, change bool
}{
	{"What is the capital of France?", false, false}, {"What is waiting on me in my inbox?", false, false},
	{"Who is my team lead?", false, false}, {"How do I rebase a branch onto main?", false, false},
	{"Write a TypeScript function that debounces a callback.", false, false}, {"What is the difference between TCP and UDP?", false, false},
	{"What meetings do I have tomorrow?", true, false}, {"Summarise the README in my-widget.", true, false},
	{"What is the latest version of Node.js?", true, false}, {"Why does the build fail in my-agent?", true, false},
	{"Open a Linear ticket for the DM title bug.", true, false}, {"Fix the typo in the my-agent README.", true, true},
	{"Add a dark mode toggle to my-widget.", true, true}, {"Refactor the router in my-agent into its own file.", true, true},
}

// Classify is whether a question needs tools, and whether it asks for a change.
func (n *Norn) Classify(ctx context.Context, question string) (tools, change bool) {
	messages := []llm.Message{{Role: "system", Content: routerSystem}}
	for _, example := range routerExamples {
		answer, _ := json.Marshal(map[string]bool{"tools": example.tools, "change": example.change})
		messages = append(messages, llm.Message{Role: "user", Content: example.question}, llm.Message{Role: "assistant", Content: string(answer)})
	}
	messages = append(messages, llm.Message{Role: "user", Content: question})
	raw, err := n.LLM.Chat(ctx, messages, llm.Options{Purpose: "route", Model: n.Config.AskModel, Temperature: 0, TopP: 1, MaxTokens: 30, Timeout: 15 * time.Second, NoFallback: true,
		Format: map[string]any{"type": "object", "properties": map[string]any{"tools": map[string]any{"type": "boolean"}, "change": map[string]any{"type": "boolean"}}, "required": []string{"tools", "change"}}})
	if err != nil {
		n.logf("the router did not answer; answering locally: %v", err)
		return false, false
	}
	var parsed struct{ Tools, Change bool }
	_ = json.Unmarshal([]byte(raw), &parsed)
	return parsed.Tools, parsed.Tools && parsed.Change
}
