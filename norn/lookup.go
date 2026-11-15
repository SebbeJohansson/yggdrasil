package norn

// Norn's own read-only lookups: a question its MCP servers can answer
// ("what's in the current cycle?") is answered by XeStrata calling their
// tools, in seconds, before a Claude agent is started for it. The model can
// hand the question on at any point, and does when the tools can't answer.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/llm"
	"yggdrasil/internal/mcp"
)

// LookupServer is an MCP server Norn may read from.
type LookupServer struct {
	Name   string
	Client *mcp.Client
}

// CoreTools are the tools offered per server: all read-only, and few, so a
// small model picks well.
var CoreTools = map[string][]string{
	"linear": {"get_issue", "list_issues", "list_comments", "list_issue_statuses", "list_projects", "get_project", "list_cycles"},
}

// Topics say which questions a server can answer.
var Topics = map[string]*regexp.Regexp{
	"linear": regexp.MustCompile(`(?i)\b([A-Z][A-Z0-9]{1,9}-\d+|linear|issues?|tickets?|cycles?|sprints?|backlog|triage|milestones?|roadmap)\b`),
}

var lookupOrder = regexp.MustCompile(`(?i)^\s*(please\s+|can you\s+|could you\s+|kan du\s+)?(mark|set|move|close|reopen|assign|unassign|cancel|update|change|start|begin|create|add|fix|implement|comment|write|make|work on|plan)\b`)

// AsksForChange is whether the question asks for something to change: an agent's work.
func AsksForChange(question string) bool { return lookupOrder.MatchString(question) }

// ServersFor is the servers whose topic the question is about.
func ServersFor(question string, servers []LookupServer) []LookupServer {
	var out []LookupServer
	for _, s := range servers {
		if topic := Topics[s.Name]; topic != nil && topic.MatchString(question) {
			out = append(out, s)
		}
	}
	return out
}

const handOff = "hand_to_claude"
const maxResult = 8000

// LookupSystem is the lookup model's instructions.
func LookupSystem(person, today string, servers []string) string {
	if person == "" {
		person = "the person"
	}
	return strings.Join([]string{
		fmt.Sprintf("You answer questions for %s by looking things up with read-only tools (%s). Today is %s.", person, strings.Join(servers, ", "), today),
		"Call the tools you need, then answer the newest question in one to three plain sentences, the answer first. Give the link a tool returned, when there is one.",
		"If the tools cannot answer it -- it needs code or files, the web, email, a calendar, or something changed -- call " + handOff + " at once instead of answering.",
		"Filter Linear states by type, which every team has: triage, backlog, unstarted (todo), started (in progress, in review), completed (done), canceled.",
		"An empty result is an answer: say plainly that there is none (no current cycle, no comments) instead of searching on.",
		"Never guess or answer from memory, and never say you will do something later.",
	}, "\n")
}

// Looked is a lookup's answer and the tools it used, or why it handed on.
type Looked struct {
	Answer  string
	Tools   []string
	HandOff string
}

// ToolChatter is one turn of a tool conversation.
type ToolChatter func(ctx context.Context, messages []llm.ToolMessage, tools []llm.Tool, options llm.Options) (llm.ToolTurn, error)

// Lookup answers a question with the servers' tools, or hands it on.
type Lookup struct {
	Chat    ToolChatter
	Model   string
	Servers []LookupServer
	Zone    *time.Location
	Budget  time.Duration
	Steps   int

	mu    sync.Mutex
	tools map[string][]mcp.Tool
}

func (l *Lookup) offered(ctx context.Context, s LookupServer) ([]mcp.Tool, error) {
	l.mu.Lock()
	cached, ok := l.tools[s.Name]
	l.mu.Unlock()
	if ok {
		return cached, nil
	}
	all, err := s.Client.Tools(ctx)
	if err != nil {
		return nil, err
	}
	var kept []mcp.Tool
	for _, t := range all {
		if slices.Contains(CoreTools[s.Name], t.Name) {
			kept = append(kept, t)
		}
	}
	l.mu.Lock()
	if l.tools == nil {
		l.tools = map[string][]mcp.Tool{}
	}
	l.tools[s.Name] = kept
	l.mu.Unlock()
	return kept, nil
}

func strPtr(s string) *string { return &s }

// Run looks the question up with servers (a subset of l.Servers).
func (l *Lookup) Run(ctx context.Context, question, person string, history []llm.Message, servers []LookupServer, logf func(string, ...any)) Looked {
	start := time.Now()
	budget := l.Budget
	if budget == 0 {
		budget = 40 * time.Second
	}
	owner := map[string]LookupServer{}
	var specs []llm.Tool
	var names []string
	for _, s := range servers {
		tools, err := l.offered(ctx, s)
		if err != nil {
			logf("lookup: %s is unreachable: %v", s.Name, err)
			continue
		}
		names = append(names, s.Name)
		for _, t := range tools {
			var spec llm.Tool
			spec.Type = "function"
			spec.Function.Name, spec.Function.Description, spec.Function.Parameters = s.Name+"__"+t.Name, t.Description, t.InputSchema
			owner[spec.Function.Name] = s
			specs = append(specs, spec)
		}
	}
	if len(specs) == 0 {
		return Looked{HandOff: "no tools"}
	}
	var hand llm.Tool
	hand.Type = "function"
	hand.Function.Name = handOff
	hand.Function.Description = "Hand the question to a Claude agent with more tools. Use when these tools cannot answer it."
	hand.Function.Parameters = map[string]any{"type": "object", "properties": map[string]any{"reason": map[string]any{"type": "string"}}, "required": []string{"reason"}}
	specs = append(specs, hand)

	zone := l.Zone
	if zone == nil {
		zone = time.Local
	}
	messages := []llm.ToolMessage{{Role: "system", Content: strPtr(LookupSystem(person, time.Now().In(zone).Format("Monday 2 January 2006"), names))}}
	for _, m := range history[max(0, len(history)-6):] {
		messages = append(messages, llm.ToolMessage{Role: m.Role, Content: strPtr(m.Content)})
	}
	messages = append(messages, llm.ToolMessage{Role: "user", Content: strPtr(question)})

	steps := l.Steps
	if steps == 0 {
		steps = 5
	}
	var used []string
	think := false
	for step := 0; step < steps; step++ {
		left := budget - time.Since(start)
		if left <= 0 {
			return Looked{HandOff: "took too long"}
		}
		turn, err := l.Chat(ctx, messages, specs, llm.Options{Purpose: "lookup", Model: l.Model, Temperature: 0, TopP: 1, MaxTokens: 500, Think: &think, Timeout: max(5*time.Second, left)})
		if err != nil {
			return Looked{HandOff: "the model failed: " + err.Error()}
		}
		if len(turn.ToolCalls) == 0 {
			if answer := strings.TrimSpace(turn.Content); answer != "" {
				return Looked{Answer: answer, Tools: used}
			}
			return Looked{HandOff: "no answer"}
		}
		assistant := llm.ToolMessage{Role: "assistant", ToolCalls: turn.ToolCalls}
		if turn.Content != "" {
			assistant.Content = strPtr(turn.Content)
		}
		messages = append(messages, assistant)
		for _, call := range turn.ToolCalls {
			args := map[string]any{}
			_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
			if call.Function.Name == handOff {
				reason, _ := args["reason"].(string)
				if reason == "" {
					reason = "the model handed it on"
				}
				return Looked{HandOff: reason}
			}
			messages = append(messages, llm.ToolMessage{Role: "tool", ToolCallID: call.ID, Content: strPtr(l.runTool(ctx, owner, call.Function.Name, args, &used))})
		}
	}
	return Looked{HandOff: "too many steps"}
}

func (l *Lookup) runTool(ctx context.Context, owner map[string]LookupServer, name string, args map[string]any, used *[]string) string {
	s, ok := owner[name]
	if !ok {
		return "Error: there is no tool named " + name + "."
	}
	*used = append(*used, name)
	result, err := s.Client.Call(ctx, strings.TrimPrefix(name, s.Name+"__"), args)
	if err != nil {
		return "Error: " + err.Error()
	}
	if len(result) > maxResult {
		return fmt.Sprintf("%s\n[cut: %d more characters]", result[:maxResult], len(result)-maxResult)
	}
	return result
}

// lookedUp answers the question with Norn's own tools, or nil when they
// don't cover it or the model hands it on.
func (a *asker) lookedUp() *AskResponse {
	l := a.n.Lookup
	if l == nil || AsksForChange(a.question) {
		return nil
	}
	servers := ServersFor(a.question, l.Servers)
	if len(servers) == 0 {
		return nil
	}
	found := l.Run(a.ctx, a.question, "", a.history, servers, a.n.logf)
	if found.HandOff != "" {
		a.n.logf("lookup handed on (%s): %s", found.HandOff, a.question)
		return nil
	}
	tools := slices.Compact(slices.Sorted(slices.Values(found.Tools)))
	via := "lookup: no tools"
	if len(tools) > 0 {
		via = "lookup: " + strings.Join(tools, ", ")
	}
	response := a.answered(found.Answer, nil, via)
	return &response
}
