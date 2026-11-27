package sindri

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"yggdrasil/internal/linear"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
	"yggdrasil/internal/env"
	"yggdrasil/internal/part"
	"yggdrasil/internal/tell"
	"yggdrasil/internal/where"
)

// Main runs Sindri until ctx ends, stopping its agents on the way out. Settings:
//
//	SINDRI_ADDR         listen address (127.0.0.1:8772)
//	SINDRI_TOKENS       who may call it: owner=…,norn=…
//	SINDRI_ROOTS        where your projects are, colon separated (~/projects)
//	SINDRI_STATE        where the agents are kept (~/.local/state/yggdrasil/sindri-agents.json)
//	SINDRI_MAX_RUNNING  how many agent processes at once (4)
//	SINDRI_IDLE         how long an idle agent's process is kept (30m)
//	SINDRI_CLEAN_AFTER_DAYS  a project quiet this long loses its git-ignored node_modules, build output and caches, daily (90; 0: only list, at GET /v1/cleanup)
//	SINDRI_REMOTE_CONTROL  coding agents, and Claude started in herdr (a start, a resume, one brought home), join Remote Control, to follow them on claude.ai/code; screenshot and question agents only on POST .../remote (true)
//	CLAUDE_BIN          Claude Code (~/.local/bin/claude); off turns it off
//	CLAUDE_DIR          Claude Code's state (~/.claude); CLAUDE_SOCKET_DIR its live sockets
//	OPENCODE_URL        a running `opencode serve` (http://127.0.0.1:4096); off turns it off
//	HERDR_SOCKET        herdr's socket (herdr's own default); off turns herdr off
//	DELEGATE_IN         herdr: question agents run in herdr panes while it answers; sindri: here
//	COPILOT_BIN         GitHub Copilot CLI (~/.local/bin/copilot); off turns it off
//	SINDRI_DB           tasks, goals, approvals (~/.local/state/yggdrasil/sindri.db)
//	DELEGATE_MODEL      the model agents run on (sonnet)
//	SINDRI_ASK_CWD      where question agents run (<root>/yggdrasil/workspace)
//	AUTO_ALLOW          allow everyday reads, tests and git reads by default (true)
//	YGG_NORN_URL        Norn, where agents' messages are posted; NORN_TOKEN is Sindri's token there
//	YGG_HERMODR_URL     Hermóðr, for the night shift's morning report; HERMODR_TOKEN is Sindri's token there
//	TIME_ZONE           the night shift's times are in this zone (Europe/Stockholm)
//	SINDRI_SELF_REPAIR  on: draft goals for Yggdrasil's own alerts and dispatch corrections each night (off)
//	YGG_GJALLAR_URL     Gjallar, whose alerts self-repair reads; GJALLAR_TOKEN is Sindri's token there
//	SINDRI_USAGE_BUDGET tokens the plan allows per window; the usage ledger raises claude-usage at 80% of it (0: no alert)
//	SINDRI_USAGE_WINDOW the plan's window (5h)
//	SINDRI_WHY_TIMEOUT  how long a goal's start waits for Norn's why before going without it (30s)
func Main(ctx context.Context) error {
	tokens, err := env.Tokens("SINDRI_TOKENS")
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		return fmt.Errorf("SINDRI_TOKENS is empty: set at least owner=<token>")
	}
	p := part.New(Name, tokens, env.List("YGG_CORS_ORIGINS"))
	UseShellPath(p.Logf)
	home, _ := os.UserHomeDir()
	roots := strings.Split(env.String("SINDRI_ROOTS", filepath.Join(home, "projects")), ":")
	agents, err := NewManager(ManagerOptions{
		ClaudeBin:     env.String("CLAUDE_BIN", filepath.Join(home, ".local/bin/claude")),
		CopilotBin:    env.String("COPILOT_BIN", filepath.Join(home, ".local/bin/copilot")),
		Roots:         roots,
		StatePath:     env.String("SINDRI_STATE", filepath.Join(home, ".local/state/yggdrasil/sindri-agents.json")),
		MaxRunning:    env.Int("SINDRI_MAX_RUNNING", 4),
		Idle:          env.Duration("SINDRI_IDLE", 30*time.Minute),
		RemoteControl: env.String("SINDRI_REMOTE_CONTROL", "true") == "true",
	}, p.Logf)
	if err != nil {
		return err
	}
	defer agents.Shutdown()
	store, err := OpenStore(env.String("SINDRI_DB", filepath.Join(home, ".local/state/yggdrasil/sindri.db")))
	if err != nil {
		return err
	}
	defer store.Close()
	zone, err := time.LoadLocation(env.String("TIME_ZONE", "Europe/Stockholm"))
	if err != nil {
		return fmt.Errorf("TIME_ZONE: %w", err)
	}
	work := &Work{
		Hermodr: tell.NewHermodr(where.URL("hermodr"), env.String("HERMODR_TOKEN", "")), Zone: zone,
		Linear: &linear.Client{Key: env.String("LINEAR_KEY", "")},
		Agents: agents, Store: store, logf: p.Logf,
		Norn:             &NornClient{URL: where.URL("norn"), Token: env.String("NORN_TOKEN", ""), HTTP: &http.Client{Timeout: 10 * time.Second}},
		Model:            env.String("DELEGATE_MODEL", "sonnet"),
		AskCwd:           env.String("SINDRI_ASK_CWD", filepath.Join(roots[0], "yggdrasil", "workspace")),
		AutoAllowDefault: env.String("AUTO_ALLOW", "true") == "true",
		Repair: &Repair{
			Gjallar: call.New("gjallar", where.URL("gjallar"), env.String("GJALLAR_TOKEN", ""), 10*time.Second),
			Norn:    call.New("norn", where.URL("norn"), env.String("NORN_TOKEN", ""), 10*time.Second),
			Default: env.String("SINDRI_SELF_REPAIR", "off") == "on", Repo: "yggdrasil",
		},
		Kinds: NornKinds{Norn: call.New("norn", where.URL("norn"), env.String("NORN_TOKEN", ""), 3*time.Minute)},
	}
	// A goal's why comes from Norn (why.go); without NORN_TOKEN it is not asked.
	WhyTimeout = env.Duration("SINDRI_WHY_TIMEOUT", WhyTimeout)
	if norn := call.New("norn", where.URL("norn"), env.String("NORN_TOKEN", ""), WhyTimeout+5*time.Second); norn.On() {
		work.Why = NornWhy{Norn: norn}
	}
	claudeDir := env.String("CLAUDE_DIR", filepath.Join(home, ".claude"))
	socketDir := env.String("CLAUDE_SOCKET_DIR", fmt.Sprintf("/run/user/%d/cc-socks", os.Getuid()))
	if _, err := os.Stat(socketDir); err != nil {
		socketDir = "" // no sockets: liveness is a guess from recent activity
	}
	var opencode *OpencodeClient
	if u := env.String("OPENCODE_URL", "http://127.0.0.1:4096"); u != "off" {
		opencode = &OpencodeClient{BaseURL: u, Authorization: env.String("OPENCODE_AUTHORIZATION", ""), HTTP: &http.Client{Timeout: 10 * time.Second}}
	}
	// Coding agents may run in OpenCode too: the "opencode" conduit of a race.
	agents.UseOpencode(opencode)
	var herdr *Herdr
	if socket := env.String("HERDR_SOCKET", DefaultHerdrSocket()); socket != "off" {
		herdr = &Herdr{Socket: socket}
		// Question agents run in herdr panes while herdr answers; DELEGATE_IN=sindri keeps them here.
		if env.String("DELEGATE_IN", "herdr") == "herdr" {
			agents.UseHerdr(herdr, claudeDir)
		}
	}
	agents.RetakeScreenshots()
	work.Transcripts = filepath.Join(claudeDir, "projects")
	s := &Sindri{Agents: agents, Root: agents.roots[0], Work: work,
		ClaudeProjects: env.String("CLAUDE_PROJECTS", filepath.Join(claudeDir, "projects")),
		PromptsKeep:    env.Duration("SINDRI_PROMPTS_KEEP", 0),
		Sessions:       NewAgentSessions(ClaudeRoot{ClaudeDir: claudeDir, SocketDir: socketDir}, opencode, herdr)}
	s.Sessions.Agents = agents
	s.Upgrader = &Upgrader{Dir: filepath.Join(home, ".local/state/yggdrasil"), Run: systemdRun}
	s.Cleaner = &Cleaner{Root: roots[0], After: time.Duration(env.Int("SINDRI_CLEAN_AFTER_DAYS", 90)) * 24 * time.Hour,
		StatePath: filepath.Join(home, ".local/state/yggdrasil/sindri-cleanup.json")}
	cli := &ClaudeCloud{Bin: env.String("CLAUDE_BIN", filepath.Join(home, ".local/bin/claude")), StartWait: env.Duration("SINDRI_CLOUD_START_WAIT", 90*time.Second),
		RemoteControl: env.String("SINDRI_REMOTE_CONTROL", "true") == "true"}
	if herdr != nil {
		cli.Herdr = herdr
	}
	if s.Cloud, err = NewCloud(store, cli); err != nil {
		return err
	}
	s.Usage = &Usage{Store: store, Budget: int64(env.Int("SINDRI_USAGE_BUDGET", 0)), Window: env.Duration("SINDRI_USAGE_WINDOW", 5*time.Hour), Logf: p.Logf}
	if url, token := where.URL("gjallar"), env.String("GJALLAR_TOKEN", ""); url != "" && token != "" {
		s.Usage.Gjallar = alert.NewClient(url, token)
	}
	agents.OnUsage = s.Usage.Record
	s.Mount(p)
	return p.Run(ctx, where.Addr(Name))
}
