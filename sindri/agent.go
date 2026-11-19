package sindri

// One coding agent, and how its CLI's output changes it: Claude Code's
// stream-json, or GitHub Copilot CLI's JSON lines. Ported from my-agent's
// runner/src/stream.ts and copilot.ts.

import (
	"fmt"
	"regexp"
	"strings"
)

// Agent statuses.
const (
	Starting      = "starting"       // spawned, no init yet
	Working       = "working"        // a turn is running
	NeedsApproval = "needs-approval" // a tool waits on allow or deny
	Waiting       = "waiting"        // the turn ended; the process takes a follow-up
	Done          = "done"           // the process exited cleanly; a follow-up resumes it
	Failed        = "failed"         // the turn errored or the process died
	Aborted       = "aborted"        // stopped on request
)

func running(status string) bool {
	return status == Starting || status == Working || status == NeedsApproval || status == Waiting
}

func busy(status string) bool {
	return status == Starting || status == Working || status == NeedsApproval
}

// Permission modes and engines an agent may run with.
var (
	PermissionModes = []string{"default", "acceptEdits", "plan", "auto"}
	Engines         = []string{"claude", "copilot"}
	engineNames     = map[string]string{"claude": "Claude", "copilot": "Copilot"}
)

// Pending is a tool call waiting on allow or deny.
type Pending struct {
	RequestID string         `json:"requestId"`
	ToolName  string         `json:"toolName"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"toolUseId,omitempty"`
	Summary   string         `json:"summary"`
	At        int64          `json:"at"`
}

// LogEntry is one line of an agent's log.
type LogEntry struct {
	At   int64  `json:"at"`
	Kind string `json:"kind"` // prompt, text, tool, permission, result, error, exit
	Text string `json:"text"`
}

// Worktree is where a coding agent works: its own branch, cut from base.
type Worktree struct {
	Repo       string  `json:"repo"`
	Path       string  `json:"path"`
	Branch     string  `json:"branch"`
	Base       string  `json:"base"`
	BaseCommit string  `json:"baseCommit"`
	Merged     *Merged `json:"merged,omitempty"`
}

// Merged is what a merge brought in, kept for the review once the branch is gone.
type Merged struct {
	From    string   `json:"from"`
	To      string   `json:"to"`
	Commits []Commit `json:"commits"`
}

// Commit is one commit on a branch.
type Commit struct {
	Hash    string `json:"hash"`
	Subject string `json:"subject"`
	At      int64  `json:"at"`
	Merged  bool   `json:"merged"`
}

// Note is something an agent said for another part of its task.
type Note struct {
	N    int    `json:"n"`
	Text string `json:"text"`
}

// CheckSummary is the last test run on an agent's branch.
type CheckSummary struct {
	Status   string `json:"status"`
	Head     string `json:"head"`
	Command  string `json:"command"`
	ExitCode *int   `json:"exitCode,omitempty"`
	At       int64  `json:"at"`
	Output   string `json:"output,omitempty"`
}

// Branch is a branch an agent made outside its worktree.
type Branch struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
}

// Agent is one coding agent, as kept and as listed.
type Agent struct {
	ID                 string        `json:"id"`
	Label              string        `json:"label"`
	Cwd                string        `json:"cwd"`
	PermissionMode     string        `json:"permissionMode"`
	Model              string        `json:"model,omitempty"`
	AddRoots           bool          `json:"addRoots,omitempty"`
	ScreenshotFor      string        `json:"screenshotFor,omitempty"`
	ScreenshotHead     string        `json:"screenshotHead,omitempty"`
	ShotTries          int           `json:"shotTries,omitempty"`
	Check              *CheckSummary `json:"check,omitempty"`
	Branches           []Branch      `json:"branches,omitempty"`
	Worktree           *Worktree     `json:"worktree,omitempty"`
	AllowedTools       []string      `json:"allowedTools,omitempty"`
	DisallowedTools    []string      `json:"disallowedTools,omitempty"`
	NoTools            bool          `json:"noTools,omitempty"`
	AppendSystemPrompt string        `json:"appendSystemPrompt,omitempty"`
	Status             string        `json:"status"`
	Tool               string        `json:"tool,omitempty"`
	LastText           string        `json:"lastText,omitempty"`
	Result             string        `json:"result,omitempty"`
	Pending            []Pending     `json:"pending"`
	StartedAt          int64         `json:"startedAt"`
	LastActivity       int64         `json:"lastActivity"`
	CostUSD            float64       `json:"costUsd"`
	NumTurns           int           `json:"numTurns"`
	ExitCode           *int          `json:"exitCode,omitempty"`
	StderrTail         string        `json:"stderrTail,omitempty"`
	Log                []LogEntry    `json:"log"`
	MergedAt           int64         `json:"mergedAt,omitempty"`
	Notes              []Note        `json:"notes,omitempty"`
	Queued             []string      `json:"queued,omitempty"`
	UsageLimited       bool          `json:"usageLimited,omitempty"`
	Engine             string        `json:"engine,omitempty"`
	ClaudeSession      bool          `json:"claudeSession,omitempty"`
	CopilotSession     bool          `json:"copilotSession,omitempty"`
	PremiumRequests    float64       `json:"premiumRequests,omitempty"`
	Task               string        `json:"task,omitempty"`
	// Session is the Norn conversation the agent posts into; empty when none.
	Session string `json:"session,omitempty"`
	// Herdr is set for an agent running in a herdr pane (Engine "herdr").
	Herdr *HerdrPlace `json:"herdr,omitempty"`
}

// Limits on what an agent keeps.
const (
	LogLimit    = 200
	TextLimit   = 500
	ResultLimit = 20_000
	notesKept   = 20
)

var note = regexp.MustCompile("(?im)^[\\s*>-]*((?:needs a part in `?[\\w.-]+`?|tell the `?[\\w.-]+`? part)\\s*:.+)$")

func (a *Agent) addNotes(text string) {
	for _, match := range note.FindAllStringSubmatch(text, -1) {
		n := 1
		if len(a.Notes) > 0 {
			n = a.Notes[len(a.Notes)-1].N + 1
		}
		a.Notes = append(a.Notes, Note{N: n, Text: strings.TrimSpace(match[1])})
		if len(a.Notes) > notesKept {
			a.Notes = a.Notes[len(a.Notes)-notesKept:]
		}
	}
}

func (a *Agent) addLog(entry LogEntry) {
	a.Log = append(a.Log, entry)
	if len(a.Log) > LogLimit {
		a.Log = a.Log[len(a.Log)-LogLimit:]
	}
}

var spaces = regexp.MustCompile(`\s+`)

// Clip collapses whitespace and cuts at limit characters.
func Clip(text string, limit int) string {
	flat := []rune(strings.TrimSpace(spaces.ReplaceAllString(text, " ")))
	if len(flat) > limit {
		return string(flat[:limit-1]) + "…"
	}
	return string(flat)
}

// ToolSummary is a tool call as a line: the tool and its most telling argument.
func ToolSummary(name string, input map[string]any) string {
	for _, key := range []string{"command", "file_path", "path", "pattern", "url", "query", "description", "prompt"} {
		if value, ok := input[key].(string); ok && value != "" {
			return name + " " + Clip(value, 120)
		}
	}
	return name
}

var usageLimit = regexp.MustCompile(`(?i)(?:usage limit|rate limit|weekly limit|resets? (?:at|in)|out of (?:extra )?usage|capacity until)`)

// applyClaude updates an agent from one Claude Code stream-json event.
// baseCost is what the agent cost before this process started.
func (a *Agent) applyClaude(event map[string]any, baseCost float64, now int64) {
	a.LastActivity = now
	switch event["type"] {
	case "system":
		if event["subtype"] == "init" {
			if model, ok := event["model"].(string); ok {
				a.Model = model
			}
			if a.Status == Starting {
				a.Status = Working
			}
		}
	case "assistant":
		message, _ := event["message"].(map[string]any)
		content, ok := message["content"].([]any)
		if !ok {
			return
		}
		for _, raw := range content {
			block, _ := raw.(map[string]any)
			switch block["type"] {
			case "text":
				if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" {
					a.addNotes(text)
					a.LastText = Clip(text, TextLimit)
					a.addLog(LogEntry{At: now, Kind: "text", Text: a.LastText})
				}
			case "tool_use":
				if name, ok := block["name"].(string); ok {
					input, _ := block["input"].(map[string]any)
					a.Tool = name
					a.addLog(LogEntry{At: now, Kind: "tool", Text: ToolSummary(name, input)})
				}
			}
		}
		if len(a.Pending) == 0 {
			a.Status = Working
		}
	case "user":
		if len(a.Pending) == 0 {
			a.Status = Working
		}
	case "control_request":
		request, _ := event["request"].(map[string]any)
		requestID, ok := event["request_id"].(string)
		if request["subtype"] != "can_use_tool" || !ok {
			return
		}
		toolName, _ := request["tool_name"].(string)
		if toolName == "" {
			toolName = "a tool"
		}
		input, _ := request["input"].(map[string]any)
		toolUseID, _ := request["tool_use_id"].(string)
		pending := Pending{RequestID: requestID, ToolName: toolName, Input: input, ToolUseID: toolUseID, Summary: ToolSummary(toolName, input), At: now}
		a.Pending = append(a.Pending, pending)
		a.Status = NeedsApproval
		a.addLog(LogEntry{At: now, Kind: "permission", Text: "wants to run: " + pending.Summary})
	case "result":
		if cost, ok := event["total_cost_usd"].(float64); ok {
			a.CostUSD = baseCost + cost
		}
		if turns, ok := event["num_turns"].(float64); ok {
			a.NumTurns += int(turns)
		}
		result, _ := event["result"].(string)
		if strings.TrimSpace(result) != "" {
			a.LastText = Clip(result, TextLimit)
			a.Result = result[:min(len(result), ResultLimit)]
		}
		a.Tool, a.Pending = "", nil
		subtype, _ := event["subtype"].(string)
		failed := event["is_error"] == true || (subtype != "" && subtype != "success")
		a.UsageLimited = failed && usageLimit.MatchString(subtype+" "+result)
		if failed {
			a.Status = Failed
			why := fmt.Sprintf("turn failed (%s)", subtype)
			if a.UsageLimited {
				why += ": Claude usage limit reached"
			}
			a.addLog(LogEntry{At: now, Kind: "error", Text: why})
		} else {
			a.Status = Waiting
			a.addLog(LogEntry{At: now, Kind: "result", Text: fmt.Sprintf("turn done (≈$%.2f API-equivalent so far)", a.CostUSD)})
		}
	}
}

// applyCopilot updates an agent from one Copilot CLI JSON event.
func (a *Agent) applyCopilot(event map[string]any, now int64) {
	a.LastActivity = now
	data, _ := event["data"].(map[string]any)
	switch kind, _ := event["type"].(string); kind {
	case "session.start", "session.resume":
		a.CopilotSession = true
		if a.Status == Starting {
			a.Status = Working
		}
	case "session.model_change":
		if model, ok := data["newModel"].(string); ok {
			a.Model = model
		}
	case "assistant.message":
		if content, ok := data["content"].(string); ok && strings.TrimSpace(content) != "" {
			a.addNotes(content)
			a.LastText = Clip(content, TextLimit)
			a.Result = content[:min(len(content), ResultLimit)]
			a.addLog(LogEntry{At: now, Kind: "text", Text: a.LastText})
		}
		requests, _ := data["toolRequests"].([]any)
		for _, raw := range requests {
			request, _ := raw.(map[string]any)
			if name, ok := request["name"].(string); ok {
				arguments, _ := request["arguments"].(map[string]any)
				a.Tool = name
				a.addLog(LogEntry{At: now, Kind: "tool", Text: ToolSummary(name, arguments)})
			}
		}
		a.Status = Working
	case "tool.execution_start":
		if name, ok := data["toolName"].(string); ok {
			a.Tool = name
		}
	case "result":
		if usage, ok := event["usage"].(map[string]any); ok {
			if premium, ok := usage["premiumRequests"].(float64); ok {
				a.PremiumRequests += premium
			}
		}
		a.NumTurns++
		a.Tool = ""
		code, hasCode := event["exitCode"].(float64)
		if hasCode && code != 0 {
			a.Status = Failed
			a.addLog(LogEntry{At: now, Kind: "error", Text: fmt.Sprintf("copilot turn failed (exit %v)", code)})
		} else {
			a.Status = Waiting
			a.addLog(LogEntry{At: now, Kind: "result", Text: fmt.Sprintf("turn done (%v Copilot premium requests so far)", a.PremiumRequests)})
		}
	default:
		if strings.HasSuffix(kind, "error") {
			if message, ok := data["message"].(string); ok {
				a.addLog(LogEntry{At: now, Kind: "error", Text: Clip(message, TextLimit)})
			}
		}
	}
}

// bashCommands are the shell commands an event ran, to find branches made.
func bashCommands(event map[string]any) []string {
	var commands []string
	if event["type"] == "assistant" {
		message, _ := event["message"].(map[string]any)
		content, _ := message["content"].([]any)
		for _, raw := range content {
			block, _ := raw.(map[string]any)
			input, _ := block["input"].(map[string]any)
			if command, ok := input["command"].(string); ok && block["type"] == "tool_use" && block["name"] == "Bash" {
				commands = append(commands, command)
			}
		}
	}
	if event["type"] == "assistant.message" {
		data, _ := event["data"].(map[string]any)
		requests, _ := data["toolRequests"].([]any)
		for _, raw := range requests {
			request, _ := raw.(map[string]any)
			arguments, _ := request["arguments"].(map[string]any)
			if command, ok := arguments["command"].(string); ok && (request["name"] == "bash" || request["name"] == "shell") {
				commands = append(commands, command)
			}
		}
	}
	return commands
}

var asks = regexp.MustCompile(`(?i)(\?\s*$)|\b(tell me (which|what|how|whether|if)|let me know|which (one|option|do you|would you)|do you want|would you like|should i|want me to|please confirm|confirm (that|whether)|reply with)\b`)

// AsksForReply is whether a stopped agent's last words ask you something.
func (a *Agent) AsksForReply() bool {
	return (a.Status == Waiting || a.Status == Done) && a.LastText != "" && asks.MatchString(a.LastText)
}

var (
	bashRule      = regexp.MustCompile(`^Bash(?:\((.+)\))?$`)
	mcpToolRule   = regexp.MustCompile(`^mcp__([\w-]+?)__([\w-]+)$`)
	mcpServerRule = regexp.MustCompile(`^mcp__([\w-]+)$`)
)

// CopilotRule is a Claude Code tool rule as Copilot CLI writes it; empty
// when Copilot has no such rule.
func CopilotRule(rule string) string {
	if match := bashRule.FindStringSubmatch(rule); match != nil {
		if match[1] != "" {
			return "shell(" + match[1] + ")"
		}
		return "shell"
	}
	if match := mcpToolRule.FindStringSubmatch(rule); match != nil {
		return match[1] + "(" + match[2] + ")"
	}
	if match := mcpServerRule.FindStringSubmatch(rule); match != nil {
		return match[1]
	}
	return ""
}

// HandoffPrompt is what the other CLI reads when it takes a task over.
func HandoffPrompt(a *Agent, from, to, note string) string {
	task := a.Task
	if task == "" {
		for _, entry := range a.Log {
			if entry.Kind == "prompt" {
				task = entry.Text
				break
			}
		}
	}
	if task == "" {
		task = a.Label
	}
	said := ""
	for i := len(a.Log) - 1; i >= 0; i-- {
		if a.Log[i].Kind == "text" {
			said = a.Log[i].Text
			break
		}
	}
	fromName := "Claude Code"
	if from == "copilot" {
		fromName = "GitHub Copilot CLI"
	}
	parts := []string{
		"You are taking over this task from another coding agent (" + fromName + "); the person running it moved it to you.",
		"Its conversation since it last worked with you is not available, but its work is in this folder: read `git status`, `git diff` and `git log` first to see what it already did, then carry on from there. Do not start over.",
	}
	if to == "copilot" && a.AppendSystemPrompt != "" {
		parts = append(parts, "\nThe instructions it worked under, which now apply to you:\n<instructions>\n"+a.AppendSystemPrompt+"\n</instructions>")
	}
	parts = append(parts, "\nThe task as it was given:\n<task>\n"+task+"\n</task>")
	if said != "" {
		parts = append(parts, "\nIts last message before it stopped:\n<last-message>\n"+said+"\n</last-message>")
	}
	if strings.TrimSpace(note) != "" {
		parts = append(parts, "\nFrom the person handing it over: "+strings.TrimSpace(note))
	}
	return strings.Join(parts, "\n")
}
