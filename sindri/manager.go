package sindri

// The coding agents: Claude Code (stream-json in and out, permissions asked
// over stdio) or GitHub Copilot CLI (one prompt per process, JSON lines
// out). Each runs on the host in your repos, usually in its own worktree.
// Ported from my-agent's runner/src/agents.ts.

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Limits on what an agent may be started with.
const (
	maxToolRules     = 150
	maxToolRuleChars = 200
	maxSystemPrompt  = 8000
	stderrTail       = 2000
	persistedLog     = 50
	closeGrace       = 5 * time.Second
	taskChars        = 8000
	checkOutputKept  = 4000
	rebaseRule       = "Bash(git rebase:*)"
)

// StartRequest is POST /v1/agents.
type StartRequest struct {
	Cwd                string   `json:"cwd"`
	Prompt             string   `json:"prompt"`
	Label              string   `json:"label,omitempty"`
	PermissionMode     string   `json:"permissionMode,omitempty"`
	Model              string   `json:"model,omitempty"`
	AddRoots           bool     `json:"addRoots,omitempty"`
	Worktree           bool     `json:"worktree,omitempty"`
	Slug               string   `json:"slug,omitempty"`
	AllowedTools       []string `json:"allowedTools,omitempty"`
	DisallowedTools    []string `json:"disallowedTools,omitempty"`
	NoTools            bool     `json:"noTools,omitempty"`
	AppendSystemPrompt string   `json:"appendSystemPrompt,omitempty"`
	Engine             string   `json:"engine,omitempty"`
	// Session is the Norn conversation the agent posts what it says into.
	Session string `json:"session,omitempty"`
}

// ManagerOptions configure the manager.
type ManagerOptions struct {
	ClaudeBin, CopilotBin string
	Roots                 []string
	StatePath             string
	MaxRunning            int
	Idle                  time.Duration
}

type live struct {
	command  *exec.Cmd
	stdin    io.WriteCloser
	baseCost float64
	aborted  bool
	detached bool
	done     chan struct{}
}

// Manager holds every agent.
type Manager struct {
	options ManagerOptions
	roots   []string

	mu     sync.Mutex
	agents map[string]*Agent
	live   map[string]*live
	checks map[string]map[string]CheckResult
	saving *time.Timer
	logf   func(string, ...any)
	now    func() time.Time
	// OnChange is told when an agent changes, so its messages reach Norn soon.
	OnChange func(id string)
}

// NewManager loads the agents kept at the state path.
func NewManager(options ManagerOptions, logf func(string, ...any)) (*Manager, error) {
	m := &Manager{options: options, agents: map[string]*Agent{}, live: map[string]*live{}, checks: map[string]map[string]CheckResult{}, logf: logf, now: time.Now}
	for _, root := range options.Roots {
		real, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, fmt.Errorf("root %s: %w", root, err)
		}
		m.roots = append(m.roots, real)
	}
	if len(m.roots) == 0 {
		return nil, fmt.Errorf("no project root")
	}
	m.load()
	return m, nil
}

func (m *Manager) ms() int64 { return m.now().UnixMilli() }

func (m *Manager) worktreesRoot() string { return filepath.Join(m.roots[0], ".agent-worktrees") }

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func toolRules(rules []string, name string) error {
	if len(rules) > maxToolRules {
		return httpErr(400, "`%s` must be at most %d rules of at most %d characters", name, maxToolRules, maxToolRuleChars)
	}
	for _, rule := range rules {
		if rule == "" || len(rule) > maxToolRuleChars {
			return httpErr(400, "`%s` must be at most %d rules of at most %d characters", name, maxToolRules, maxToolRuleChars)
		}
	}
	return nil
}

// List is every agent, the most recently active first.
func (m *Manager) List() []*Agent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listLocked()
}

func (m *Manager) listLocked() []*Agent {
	list := make([]*Agent, 0, len(m.agents))
	for _, a := range m.agents {
		list = append(list, a)
	}
	slices.SortFunc(list, func(a, b *Agent) int { return int(b.LastActivity - a.LastActivity) })
	return list
}

// Get is one agent.
func (m *Manager) Get(id string) (*Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getLocked(id)
}

func (m *Manager) getLocked(id string) (*Agent, error) {
	a, ok := m.agents[id]
	if !ok {
		return nil, httpErr(404, "no agent %s", id)
	}
	return a, nil
}

func (m *Manager) allowedCwd(cwd string) (string, error) {
	if strings.TrimSpace(cwd) == "" {
		return "", httpErr(400, "`cwd` must be a directory path")
	}
	path := cwd
	if !filepath.IsAbs(path) {
		path = filepath.Join(m.roots[0], cwd)
	}
	if !exists(path) {
		for _, project := range FindProjects(m.roots[0]) {
			if project.Name == cwd {
				path = project.Path
			}
		}
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", httpErr(400, "no such directory: %s", cwd)
	}
	for _, root := range m.roots {
		if real == root || strings.HasPrefix(real, root+"/") {
			return real, nil
		}
	}
	return "", httpErr(403, "%s is outside %s", real, strings.Join(m.roots, ", "))
}

// ensureCapacityLocked makes room for one more process, closing the agent
// idle longest; 429 when every running agent is at work.
func (m *Manager) ensureCapacityLocked() error {
	if len(m.live) < m.options.MaxRunning {
		return nil
	}
	var oldest *Agent
	for id := range m.live {
		if a := m.agents[id]; a != nil && a.Status == Waiting && (oldest == nil || a.LastActivity < oldest.LastActivity) {
			oldest = a
		}
	}
	if oldest == nil {
		return httpErr(429, "%d agents already running; wait for one to finish or abort one", len(m.live))
	}
	process := m.live[oldest.ID]
	delete(m.live, oldest.ID)
	process.detached = true
	oldest.Status = Done
	oldest.addLog(LogEntry{At: m.ms(), Kind: "exit", Text: "closed while idle to make room for another agent"})
	_ = process.stdin.Close()
	return nil
}

// Start starts an agent.
func (m *Manager) Start(request StartRequest) (*Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startLocked(request)
}

func (m *Manager) startLocked(request StartRequest) (*Agent, error) {
	cwd, err := m.allowedCwd(request.Cwd)
	if err != nil {
		return nil, err
	}
	prompt := strings.TrimSpace(request.Prompt)
	if prompt == "" {
		return nil, httpErr(400, "`prompt` must be a non-empty string")
	}
	mode := request.PermissionMode
	if mode == "" {
		mode = "default"
	}
	if !slices.Contains(PermissionModes, mode) {
		return nil, httpErr(400, "`permissionMode` must be one of %s", strings.Join(PermissionModes, ", "))
	}
	if request.Slug != "" && !Slug.MatchString(request.Slug) {
		return nil, httpErr(400, "`slug` must be lowercase letters, digits and dashes, at most 60")
	}
	if err := toolRules(request.AllowedTools, "allowedTools"); err != nil {
		return nil, err
	}
	if err := toolRules(request.DisallowedTools, "disallowedTools"); err != nil {
		return nil, err
	}
	if len(request.AppendSystemPrompt) > maxSystemPrompt {
		return nil, httpErr(400, "`appendSystemPrompt` must be a string of at most %d characters", maxSystemPrompt)
	}
	engine := request.Engine
	if engine == "" {
		engine = "claude"
	}
	if !slices.Contains(Engines, engine) {
		return nil, httpErr(400, "`engine` must be one of %s", strings.Join(Engines, ", "))
	}
	if engine == "copilot" && request.NoTools {
		return nil, httpErr(400, "`noTools` is for Claude only: Copilot has no way to start without its built-in tools and MCP servers")
	}
	if err := m.ensureCapacityLocked(); err != nil {
		return nil, err
	}
	label := strings.TrimSpace(request.Label)
	if label == "" {
		label = prompt
	}
	label = string([]rune(label)[:min(60, len([]rune(label)))])
	root := m.worktreesRoot()
	allowed := make([]string, len(request.AllowedTools))
	for i, rule := range request.AllowedTools {
		allowed[i] = strings.ReplaceAll(rule, "{worktrees}", root)
	}
	system := request.AppendSystemPrompt
	var worktree *Worktree
	if request.Worktree {
		repo := RepoRoot(cwd)
		if repo == "" {
			return nil, httpErr(400, "%s is not in a git repository, so it cannot have a worktree", cwd)
		}
		worktree, err = CreateWorktree(repo, root, label, request.Slug)
		if err != nil {
			return nil, httpErr(500, "could not create a worktree: %s", firstLine(err))
		}
		cwd = worktree.Path
		_ = os.MkdirAll(filepath.Join(root, ".plans"), 0o755)
		slug := strings.TrimPrefix(worktree.Branch, "agent/")
		for i := range allowed {
			allowed[i] = strings.ReplaceAll(allowed[i], "{slug}", slug)
		}
		system = strings.NewReplacer("{slug}", slug, "{branch}", worktree.Branch,
			"{base}", fmt.Sprintf("%s (%s)", worktree.Base, worktree.BaseCommit), "{path}", worktree.Path).Replace(system)
	}
	system = strings.ReplaceAll(system, "{worktrees}", root)
	now := m.ms()
	a := &Agent{
		ID: newID(), Label: label, Cwd: cwd, PermissionMode: mode, Model: request.Model, AddRoots: request.AddRoots,
		Worktree: worktree, AllowedTools: allowed, DisallowedTools: request.DisallowedTools, NoTools: request.NoTools,
		AppendSystemPrompt: system, Task: prompt[:min(len(prompt), taskChars)], Status: Starting, Pending: []Pending{},
		StartedAt: now, LastActivity: now, Log: []LogEntry{}, Session: request.Session,
	}
	if engine == "copilot" {
		a.Engine = "copilot"
	}
	if len(a.AllowedTools) == 0 {
		a.AllowedTools = nil
	}
	m.agents[a.ID] = a
	if err := m.beginLocked(a, prompt, false, nil, false); err != nil {
		return nil, err
	}
	return a, nil
}

func (m *Manager) beginLocked(a *Agent, text string, resume bool, images []any, handoff bool) error {
	if a.Engine == "copilot" {
		if !a.CopilotSession && !handoff && a.AppendSystemPrompt != "" {
			text = "<instructions>\n" + a.AppendSystemPrompt + "\n</instructions>\n\n" + text
		}
		return m.launchCopilotLocked(a, text)
	}
	if _, running := m.live[a.ID]; !running {
		if err := m.launchClaudeLocked(a, resume); err != nil {
			return err
		}
	}
	m.sendLocked(a, text, images)
	return nil
}

func (m *Manager) launchClaudeLocked(a *Agent, resume bool) error {
	a.ClaudeSession = true
	args := []string{"-p", "--output-format", "stream-json", "--input-format", "stream-json", "--verbose",
		"--permission-prompt-tool", "stdio", "--permission-mode", a.PermissionMode, "-n", a.Label}
	if resume {
		args = append(args, "--resume", a.ID)
	} else {
		args = append(args, "--session-id", a.ID)
	}
	if a.AddRoots {
		for _, root := range m.roots {
			args = append(args, "--add-dir", root)
		}
	}
	args = append(append(args, "--allowedTools", UploadsRule()), a.AllowedTools...)
	if len(a.DisallowedTools) > 0 {
		args = append(append(args, "--disallowedTools"), a.DisallowedTools...)
	}
	if a.NoTools {
		args = append(args, "--tools", "", "--strict-mcp-config")
	}
	if a.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", a.AppendSystemPrompt)
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	return m.watchLocked(a, m.options.ClaudeBin, args, false)
}

func (m *Manager) launchCopilotLocked(a *Agent, text string) error {
	var allowed, denied []string
	for _, rule := range a.AllowedTools {
		if r := CopilotRule(rule); r != "" {
			allowed = append(allowed, r)
		}
	}
	for _, rule := range a.DisallowedTools {
		if r := CopilotRule(rule); r != "" {
			denied = append(denied, r)
		}
	}
	args := []string{"-p", text, "--output-format", "json", "--no-ask-user", "--no-color", "--secret-env-vars", "SINDRI_TOKENS,NORN_TOKEN"}
	if a.CopilotSession {
		args = append(args, "--resume", a.ID)
	} else {
		args = append(args, "--session-id", a.ID, "-n", a.Label)
	}
	if a.AddRoots {
		for _, root := range m.roots {
			args = append(args, "--add-dir", root)
		}
	}
	switch a.PermissionMode {
	case "auto":
		args = append(args, "--allow-all-tools")
	case "plan":
		args = append(args, "--mode", "plan")
	default:
		if a.PermissionMode == "acceptEdits" {
			args = append(args, "--allow-tool", "write")
		}
		for _, rule := range allowed {
			args = append(args, "--allow-tool", rule)
		}
	}
	for _, rule := range denied {
		args = append(args, "--deny-tool", rule)
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if err := m.watchLocked(a, m.options.CopilotBin, args, true); err != nil {
		return err
	}
	a.Status = Working
	a.addLog(LogEntry{At: m.ms(), Kind: "prompt", Text: text})
	m.touchLocked(a)
	return nil
}

// watchLocked starts the CLI and reads its JSON lines until it exits.
func (m *Manager) watchLocked(a *Agent, bin string, args []string, copilot bool) error {
	command := exec.Command(bin, args...)
	command.Dir = a.Cwd
	command.Env = append(os.Environ(), "GIT_EDITOR=true")
	stdin, err := command.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return err
	}
	process := &live{command: command, stdin: stdin, baseCost: a.CostUSD, done: make(chan struct{})}
	if err := command.Start(); err != nil {
		a.addLog(LogEntry{At: m.ms(), Kind: "error", Text: fmt.Sprintf("could not start %s in %s: %v", engineOf(a), a.Cwd, err)})
		a.Status = Failed
		m.touchLocked(a)
		return httpErr(500, "could not start %s: %v", engineOf(a), err)
	}
	m.live[a.ID] = process
	a.Status, a.ExitCode = Starting, nil
	if copilot {
		_ = stdin.Close()
	}
	go func() {
		buffer, _ := io.ReadAll(io.LimitReader(stderr, 1<<20))
		m.mu.Lock()
		text := a.StderrTail + string(buffer)
		a.StderrTail = text[max(0, len(text)-stderrTail):]
		m.mu.Unlock()
	}()
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64<<10), 64<<20)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var event map[string]any
			if json.Unmarshal([]byte(line), &event) != nil {
				continue
			}
			m.mu.Lock()
			if m.live[a.ID] == process {
				m.onEventLocked(a, process, event, copilot)
			}
			m.mu.Unlock()
		}
		err := command.Wait()
		m.mu.Lock()
		m.onExitLocked(a, process, err, copilot)
		m.mu.Unlock()
		close(process.done)
	}()
	return nil
}

func engineOf(a *Agent) string {
	if a.Engine == "copilot" {
		return "copilot"
	}
	return "claude"
}

func (m *Manager) addBranchesLocked(a *Agent, event map[string]any) {
	for _, command := range bashCommands(event) {
		for _, made := range BranchesMade(command, a.Cwd) {
			if !slices.Contains(a.Branches, made) {
				a.Branches = append(a.Branches, made)
			}
		}
	}
}

func (m *Manager) onEventLocked(a *Agent, process *live, event map[string]any, copilot bool) {
	before := a.Status
	if copilot {
		a.applyCopilot(event, m.ms())
	} else {
		a.applyClaude(event, process.baseCost, m.ms())
	}
	m.addBranchesLocked(a, event)
	if !copilot && a.Status == Waiting && len(a.Queued) > 0 {
		queued := strings.Join(a.Queued, "\n\n")
		a.Queued = nil
		m.sendLocked(a, queued, nil)
	} else if !copilot && before != Waiting && a.Status == Waiting {
		m.photographLocked(a)
		m.followThroughLocked(a)
	}
	m.touchLocked(a)
}

func (m *Manager) onExitLocked(a *Agent, process *live, err error, copilot bool) {
	if process.detached || m.live[a.ID] != process {
		return
	}
	delete(m.live, a.ID)
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		code = -1
	}
	a.ExitCode, a.Pending, a.Tool = &code, nil, ""
	switch {
	case process.aborted:
		a.Status = Aborted
	case a.Status != Failed:
		a.Status = Done
		if code != 0 {
			a.Status = Failed
		}
	}
	text := fmt.Sprintf("exited (%d)", code)
	if process.aborted {
		text = "aborted"
	}
	a.addLog(LogEntry{At: m.ms(), Kind: "exit", Text: text})
	if copilot && a.Status == Done && len(a.Queued) > 0 && a.Engine == "copilot" {
		queued := strings.Join(a.Queued, "\n\n")
		a.Queued = nil
		_ = m.launchCopilotLocked(a, queued)
		return
	}
	if copilot && a.Status == Done {
		m.followThroughLocked(a)
	}
	if len(a.Queued) > 0 {
		a.addLog(LogEntry{At: m.ms(), Kind: "error", Text: fmt.Sprintf("dropped %d queued message%s: the process ended first", len(a.Queued), plural(len(a.Queued)))})
		a.Queued = nil
	}
	m.touchLocked(a)
}

func (m *Manager) sendLocked(a *Agent, text string, images []any) {
	process := m.live[a.ID]
	var content any = text
	if len(images) > 0 {
		content = append(append([]any{}, images...), map[string]any{"type": "text", "text": text})
	}
	m.writeLocked(process, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}})
	if a.Status != Starting {
		a.Status = Working
	}
	logged := text
	if len(images) > 0 {
		logged = fmt.Sprintf("%s\n(+%d image%s)", text, len(images), plural(len(images)))
	}
	a.addLog(LogEntry{At: m.ms(), Kind: "prompt", Text: logged})
	m.touchLocked(a)
}

func (m *Manager) writeLocked(process *live, message any) {
	if process == nil {
		return
	}
	line, _ := json.Marshal(message)
	_, _ = process.stdin.Write(append(line, '\n'))
}

func (m *Manager) touchLocked(a *Agent) {
	a.LastActivity = m.ms()
	if m.saving == nil {
		m.saving = time.AfterFunc(500*time.Millisecond, func() {
			m.mu.Lock()
			m.saving = nil
			m.mu.Unlock()
			m.Save()
		})
	}
	if m.OnChange != nil {
		go m.OnChange(a.ID)
	}
}

// Prompt sends a follow-up; a finished session resumes. With queue, a busy
// turn is waited out instead of answered with a 409.
func (m *Manager) Prompt(id, text string, images []string, queue bool) (*Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, err := m.getLocked(id)
	if err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, httpErr(400, "`text` must be a non-empty string")
	}
	_, running := m.live[id]
	if busy(a.Status) || (a.Engine == "copilot" && running) {
		if !queue {
			return nil, httpErr(409, "agent is %s; wait for the turn to end", a.Status)
		}
		a.Queued = append(a.Queued, text)
		a.addLog(LogEntry{At: m.ms(), Kind: "prompt", Text: "queued: " + text})
		m.touchLocked(a)
		return a, nil
	}
	if !running {
		if !exists(a.Cwd) {
			return nil, httpErr(410, "its folder %s is gone (the worktree was removed), so it cannot be resumed", a.Cwd)
		}
		if err := m.ensureCapacityLocked(); err != nil {
			return nil, err
		}
	}
	var blocks []any
	if len(images) > 0 {
		if blocks, err = ImageBlocks(images, a.Cwd, a.ID); err != nil {
			return nil, err
		}
	}
	if err := m.beginLocked(a, text, true, blocks, false); err != nil {
		return nil, err
	}
	return a, nil
}

// ResumeScheduled resumes an agent parked after a Claude usage limit.
func (m *Manager) ResumeScheduled(id, text string) (*Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, err := m.getLocked(id)
	if err != nil {
		return nil, err
	}
	if !a.UsageLimited || a.Worktree == nil || a.Status != Failed {
		return nil, httpErr(409, "agent is not parked for a Claude usage limit")
	}
	if strings.TrimSpace(text) == "" {
		return nil, httpErr(400, "`text` must be non-empty")
	}
	if err := m.ensureCapacityLocked(); err != nil {
		return nil, err
	}
	a.UsageLimited = false
	return a, m.beginLocked(a, strings.TrimSpace(text), true, nil, false)
}

// Handoff carries a stopped agent on in the other CLI, same id and folder.
func (m *Manager) Handoff(id, note, to string) (*Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, err := m.getLocked(id)
	if err != nil {
		return nil, err
	}
	from := engineOf(a)
	if to == "" {
		to = map[string]string{"claude": "copilot", "copilot": "claude"}[from]
	}
	switch {
	case !slices.Contains(Engines, to):
		return nil, httpErr(400, "`to` must be one of %s", strings.Join(Engines, ", "))
	case to == from:
		return nil, httpErr(409, "this agent already runs on %s", engineNames[from])
	case a.NoTools:
		return nil, httpErr(409, "an agent without tools stays on Claude")
	case running(a.Status) && a.Status != Waiting:
		return nil, httpErr(409, "agent is %s; stop it or wait for the turn to end", a.Status)
	case !exists(a.Cwd):
		return nil, httpErr(410, "its folder %s is gone (the worktree was removed), so it cannot be handed over", a.Cwd)
	}
	text := HandoffPrompt(a, from, to, note)
	if process, ok := m.live[id]; ok {
		process.detached = true
		delete(m.live, id)
		_ = process.stdin.Close()
	}
	if err := m.ensureCapacityLocked(); err != nil {
		return nil, err
	}
	a.Engine = ""
	if to == "copilot" {
		a.Engine = "copilot"
	}
	a.UsageLimited, a.Model = false, ""
	a.addLog(LogEntry{At: m.ms(), Kind: "exit", Text: "handed over from " + engineNames[from] + " to " + engineNames[to]})
	return a, m.beginLocked(a, text, a.ClaudeSession, nil, true)
}

// Rename relabels an agent.
func (m *Manager) Rename(id, label string) (*Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, err := m.getLocked(id)
	if err != nil {
		return nil, err
	}
	label = strings.TrimSpace(label)
	if label == "" {
		return nil, httpErr(400, "`label` must be a non-empty string")
	}
	a.Label = string([]rune(label)[:min(60, len([]rune(label)))])
	m.touchLocked(a)
	return a, nil
}

// Answer allows or denies a pending tool call.
func (m *Manager) Answer(id, requestID string, allow bool, message string) (*Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, err := m.getLocked(id)
	if err != nil {
		return nil, err
	}
	process := m.live[id]
	index := slices.IndexFunc(a.Pending, func(p Pending) bool { return p.RequestID == requestID })
	if index < 0 || process == nil {
		return nil, httpErr(404, "no pending permission %s", requestID)
	}
	pending := a.Pending[index]
	var response map[string]any
	if allow {
		response = map[string]any{"behavior": "allow", "updatedInput": pending.Input, "toolUseID": pending.ToolUseID}
	} else {
		if message == "" {
			message = "The user denied this."
		}
		response = map[string]any{"behavior": "deny", "message": message, "toolUseID": pending.ToolUseID}
	}
	m.writeLocked(process, map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": requestID, "response": response}})
	a.Pending = slices.Delete(a.Pending, index, index+1)
	if len(a.Pending) == 0 {
		a.Status = Working
	}
	verb := "denied"
	if allow {
		verb = "allowed"
	}
	a.addLog(LogEntry{At: m.ms(), Kind: "permission", Text: verb + ": " + pending.Summary})
	m.touchLocked(a)
	return a, nil
}

// Abort stops an agent's process.
func (m *Manager) Abort(id string) (*Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, err := m.getLocked(id)
	if err != nil {
		return nil, err
	}
	if process, ok := m.live[id]; ok {
		process.aborted = true
		_ = process.command.Process.Signal(syscall.SIGTERM)
	}
	return a, nil
}

// ReapIdle closes processes idle past the idle time; a follow-up resumes them.
func (m *Manager) ReapIdle() {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := m.now().Add(-m.options.Idle).UnixMilli()
	for id, process := range m.live {
		if a := m.agents[id]; a != nil && a.Status == Waiting && a.LastActivity < cutoff {
			_ = process.stdin.Close()
		}
	}
}

// close ends an agent's process gracefully: stdin closed, SIGTERM after 5 s.
func (m *Manager) close(id string) {
	m.mu.Lock()
	process := m.live[id]
	m.mu.Unlock()
	if process == nil {
		return
	}
	_ = process.stdin.Close()
	select {
	case <-process.done:
	case <-time.After(closeGrace):
		_ = process.command.Process.Signal(syscall.SIGTERM)
		<-process.done
	}
}

// Shutdown stops every process and saves.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	for _, process := range m.live {
		_ = process.command.Process.Signal(syscall.SIGTERM)
	}
	m.mu.Unlock()
	m.Save()
}

// --- after a turn: screenshots and checks ------------------------------------------

func (m *Manager) shotDir(id string) string {
	return filepath.Join(filepath.Dir(m.options.StatePath), "screenshots", id)
}

// photographLocked starts an agent that photographs a branch's visible
// changes, once per new commit.
func (m *Manager) photographLocked(a *Agent) {
	w := a.Worktree
	if w == nil || a.ScreenshotFor != "" || a.Engine == "copilot" || !exists(w.Path) {
		return
	}
	head := w.Head()
	if head == a.ScreenshotHead || !w.HasUnmergedCommits() {
		return
	}
	dir := m.shotDir(a.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	a.ScreenshotHead = head
	started, err := m.startLocked(StartRequest{Cwd: w.Path, Label: Clip("screenshots: "+a.Label, 60), Prompt: ScreenshotPrompt(w.Branch, w.Base, dir),
		PermissionMode: "auto", AllowedTools: []string{"Write(/" + dir + "/**)"},
		DisallowedTools: []string{"Edit", "Bash(git commit:*)", "Bash(git add:*)", "Bash(git push:*)", "Bash(git checkout:*)", "Bash(git reset:*)"}})
	if err != nil {
		m.logf("no screenshot agent for %s: %v", a.ID, err)
		return
	}
	started.ScreenshotFor = a.ID
}

// followThroughLocked runs the branch's tests when a turn ends with a new commit.
func (m *Manager) followThroughLocked(a *Agent) {
	w := a.Worktree
	if w == nil || a.ScreenshotFor != "" || !exists(w.Path) {
		return
	}
	if head := w.Head(); (a.Check != nil && a.Check.Head == head) || !w.HasUnmergedCommits() {
		return
	}
	m.startChecksLocked(a, "script", "", false)
}

// Checks is the last result of each kind of check run for an agent.
func (m *Manager) Checks(id string) (map[string]CheckResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.getLocked(id); err != nil {
		return nil, err
	}
	results := map[string]CheckResult{}
	for kind, result := range m.checks[id] {
		results[kind] = result
	}
	return results, nil
}

// StartChecks runs a check, or returns this commit's result.
func (m *Manager) StartChecks(id, kind, script string, force bool) (CheckResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, err := m.getLocked(id)
	if err != nil {
		return CheckResult{}, err
	}
	if a.Worktree == nil {
		return CheckResult{}, httpErr(400, "this agent did not work in a worktree, so there is no branch to review")
	}
	if busy(a.Status) {
		return CheckResult{}, httpErr(409, "agent is %s; wait for it to finish", a.Status)
	}
	return m.startChecksLocked(a, kind, script, force), nil
}

func (m *Manager) startChecksLocked(a *Agent, kind, script string, force bool) CheckResult {
	if kind == "script" && script == "" {
		script = "test"
	}
	results := m.checks[a.ID]
	if results == nil {
		results = map[string]CheckResult{}
		m.checks[a.ID] = results
	}
	if last, ok := results[kind]; ok && !force && (last.Status == "running" || (last.Head == a.Worktree.Head() && last.Script == script && last.Status != "unavailable")) {
		return last
	}
	id := a.ID
	started := RunCheck(a.Worktree, kind, script, func(result CheckResult) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.checks[id][kind] = result
		m.noteCheckLocked(id, result)
	})
	results[kind] = started
	m.noteCheckLocked(id, started)
	return started
}

func (m *Manager) noteCheckLocked(id string, result CheckResult) {
	a := m.agents[id]
	if a == nil || result.Kind != "script" || result.Script != "test" || result.Status == "unavailable" {
		return
	}
	output := result.Output
	a.Check = &CheckSummary{Status: result.Status, Head: result.Head, Command: result.Command, ExitCode: result.ExitCode, At: m.ms(), Output: output[max(0, len(output)-checkOutputKept):]}
	m.touchLocked(a)
}

// --- review, merge, publish ----------------------------------------------------------

func (m *Manager) withWorktree(id string, changes bool, step func(a *Agent) (any, error)) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, err := m.getLocked(id)
	if err != nil {
		return nil, err
	}
	if a.Worktree == nil {
		return nil, httpErr(400, "this agent did not work in a worktree, so there is no branch to review")
	}
	if changes && busy(a.Status) {
		return nil, httpErr(409, "agent is %s; wait for it to finish", a.Status)
	}
	result, err := step(a)
	if review, ok := err.(*ReviewError); ok {
		return nil, httpErr(409, "%s", review.Message)
	}
	return result, err
}

// Review is an agent's branch, with its screenshots.
func (m *Manager) Review(id, commit string) (any, error) {
	return m.withWorktree(id, false, func(a *Agent) (any, error) {
		review, err := a.Worktree.ReviewOf(commit)
		if err != nil {
			return nil, err
		}
		review.Screenshots = ListScreenshots(m.shotDir(id), id)
		return review, nil
	})
}

// Screenshot is one of an agent's pictures.
func (m *Manager) Screenshot(id, name string) ([]byte, string, error) {
	if _, err := m.Get(id); err != nil {
		return nil, "", err
	}
	data, kind, ok := ReadScreenshot(m.shotDir(id), name)
	if !ok {
		return nil, "", httpErr(404, "no screenshot %s", name)
	}
	return data, kind, nil
}

// Merge merges an agent's branch into the branch it was cut from.
func (m *Manager) Merge(id string) (string, error) {
	done, err := m.withWorktree(id, true, func(a *Agent) (any, error) {
		done, err := a.Worktree.Merge()
		if err == nil {
			a.MergedAt = m.ms()
			m.touchLocked(a)
		}
		return done, err
	})
	if err != nil {
		return "", err
	}
	return done.(string), nil
}

// PullRequest pushes an agent's branch and opens a pull request.
func (m *Manager) PullRequest(id string) (map[string]string, error) {
	result, err := m.withWorktree(id, true, func(a *Agent) (any, error) {
		url, done, err := a.Worktree.PullRequest()
		return map[string]string{"url": url, "done": done}, err
	})
	if err != nil {
		return nil, err
	}
	return result.(map[string]string), nil
}

// PushBase pushes the branch an agent's work merged into.
func (m *Manager) PushBase(id string) (string, error) {
	done, err := m.withWorktree(id, false, func(a *Agent) (any, error) { return a.Worktree.PushBase() })
	if err != nil {
		return "", err
	}
	return done.(string), nil
}

// RemoveWorktree removes an agent's worktree, and its branch once merged.
func (m *Manager) RemoveWorktree(id string, force bool) (string, error) {
	done, err := m.withWorktree(id, true, func(a *Agent) (any, error) { return a.Worktree.Remove(force) })
	if err != nil {
		return "", err
	}
	return done.(string), nil
}

// FixConflicts asks an agent to rebase its branch and resolve the conflicts.
func (m *Manager) FixConflicts(id string) (*Agent, error) {
	text, err := m.withWorktree(id, true, func(a *Agent) (any, error) {
		conflicts, err := a.Worktree.ConflictsToFix()
		if err != nil {
			return nil, err
		}
		var denied []string
		for _, rule := range a.DisallowedTools {
			if !strings.HasPrefix(rule, "Bash(git rebase") {
				denied = append(denied, rule)
			}
		}
		a.DisallowedTools = denied
		if !slices.Contains(a.AllowedTools, rebaseRule) {
			a.AllowedTools = append(a.AllowedTools, rebaseRule)
		}
		return RebasePrompt(a.Worktree, conflicts), nil
	})
	if err != nil {
		return nil, err
	}
	m.close(id)
	return m.Prompt(id, text.(string), nil, false)
}

// --- worktrees no agent owns -------------------------------------------------------

// Worktrees is every worktree, with the agent working in it.
func (m *Manager) Worktrees() []ListedWorktree {
	m.mu.Lock()
	owners := map[string]*Agent{}
	for _, a := range m.agents {
		if a.Worktree != nil {
			owners[a.Worktree.Path] = a
		}
	}
	m.mu.Unlock()
	list := ListWorktrees(m.roots[0])
	for i := range list {
		if owner := owners[list[i].Path]; owner != nil {
			list[i].Agent = &struct {
				ID     string `json:"id"`
				Label  string `json:"label"`
				Status string `json:"status"`
			}{owner.ID, owner.Label, owner.Status}
		}
	}
	return list
}

// OnUnowned reviews, merges, removes, publishes or moves a worktree no agent owns.
func (m *Manager) OnUnowned(path, step, commit string, force bool) (any, error) {
	var listed *ListedWorktree
	for _, w := range m.Worktrees() {
		if w.Path == path {
			listed = &w
			break
		}
	}
	switch {
	case listed == nil:
		return nil, httpErr(404, "no worktree at %s", path)
	case listed.Agent != nil:
		return nil, httpErr(409, "agent %s works there; use its own routes", listed.Agent.ID)
	case listed.Branch == "":
		return nil, httpErr(400, "that worktree has no branch checked out")
	}
	if (listed.Checkout && (step == "merge" || step == "remove")) || (!listed.Checkout && step == "move") {
		if listed.Checkout {
			return nil, httpErr(409, "%s is checked out in %s itself; move it to a worktree first", listed.Branch, listed.Repo)
		}
		return nil, httpErr(409, "that is a worktree already")
	}
	w := &Worktree{Repo: listed.Repo, Path: listed.Path, Branch: listed.Branch, Base: listed.Base, BaseCommit: CutFrom(listed.Repo, listed.Branch, listed.Base)}
	var result any
	var err error
	switch step {
	case "review":
		result, err = w.ReviewOf(commit)
	case "merge":
		result, err = w.Merge()
	case "remove":
		result, err = w.Remove(force)
	case "pull-request":
		var url, done string
		url, done, err = w.PullRequest()
		result = map[string]string{"url": url, "done": done}
	case "push-base":
		result, err = w.PushBase()
	case "move":
		var moved string
		if moved, err = MoveToWorktree(w.Repo, w.Branch, w.Base, m.worktreesRoot()); err != nil {
			err = &ReviewError{Message: firstLine(err)}
		} else {
			result = fmt.Sprintf("moved %s to %s; %s is back on %s", w.Branch, moved, w.Repo, w.Base)
		}
	}
	if review, ok := err.(*ReviewError); ok {
		return nil, httpErr(409, "%s", review.Message)
	}
	return result, err
}

// --- keeping state ---------------------------------------------------------------------

func (m *Manager) load() {
	data, err := os.ReadFile(m.options.StatePath)
	if err != nil {
		return
	}
	var saved []*Agent
	if json.Unmarshal(data, &saved) != nil {
		return
	}
	for _, a := range saved {
		if running(a.Status) {
			a.Status, a.Pending, a.Tool = Done, nil, ""
			a.addLog(LogEntry{At: m.ms(), Kind: "exit", Text: "stopped when Sindri restarted"})
		}
		if a.Pending == nil {
			a.Pending = []Pending{}
		}
		m.agents[a.ID] = a
	}
}

// Save writes every agent aside and renames it over the state file.
func (m *Manager) Save() {
	m.mu.Lock()
	list := m.listLocked()
	trimmed := make([]Agent, len(list))
	for i, a := range list {
		trimmed[i] = *a
		trimmed[i].Log = a.Log[max(0, len(a.Log)-persistedLog):]
	}
	data, err := json.MarshalIndent(trimmed, "", " ")
	m.mu.Unlock()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.options.StatePath), 0o700); err != nil {
		m.logf("could not save the agents: %v", err)
		return
	}
	temp := m.options.StatePath + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err == nil {
		_ = os.Rename(temp, m.options.StatePath)
	}
}

// --- what the API shows ------------------------------------------------------------------

// PlacedBranch is a branch an agent made, with where it is and whether merged.
type PlacedBranch struct {
	Repo    string `json:"repo"`
	Project string `json:"project"`
	Branch  string `json:"branch"`
	Path    string `json:"path,omitempty"`
	Merged  bool   `json:"merged"`
}

// Summarise is an agent as GET /v1/agents lists it: without its log, task
// and instructions, with whether it asks you something, its last error, and
// where the branches it made are.
func Summarise(a *Agent) map[string]any {
	var fields map[string]any
	data, _ := json.Marshal(a)
	_ = json.Unmarshal(data, &fields)
	for _, hidden := range []string{"log", "appendSystemPrompt", "result", "task", "branches"} {
		delete(fields, hidden)
	}
	if a.Check != nil {
		check, _ := fields["check"].(map[string]any)
		delete(check, "output")
	}
	fields["asksYou"] = a.AsksForReply()
	fields["resumable"] = busy(a.Status) || exists(a.Cwd)
	for i := len(a.Log) - 1; i >= 0; i-- {
		if a.Log[i].Kind == "error" {
			fields["lastError"] = a.Log[i].Text
			break
		}
	}
	if a.Worktree != nil && a.Worktree.IsLanded() {
		fields["merged"] = true
	}
	var placed []PlacedBranch
	for _, b := range a.Branches {
		if path, merged, ok := BranchPlace(b.Repo, b.Branch); ok {
			placed = append(placed, PlacedBranch{Repo: b.Repo, Project: filepath.Base(b.Repo), Branch: b.Branch, Path: path, Merged: merged})
		}
	}
	if len(placed) > 0 {
		fields["branches"] = placed
	}
	pending := []map[string]any{}
	for _, p := range a.Pending {
		pending = append(pending, map[string]any{"requestId": p.RequestID, "toolName": p.ToolName, "summary": p.Summary, "at": p.At})
	}
	fields["pending"] = pending
	return fields
}
