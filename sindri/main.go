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

	"yggdrasil/internal/env"
	"yggdrasil/internal/part"
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
//	CLAUDE_BIN          Claude Code (~/.local/bin/claude)
//	COPILOT_BIN         GitHub Copilot CLI (~/.local/bin/copilot)
//	SINDRI_DB           tasks, goals, approvals (~/.local/state/yggdrasil/sindri.db)
//	DELEGATE_MODEL      the model agents run on (sonnet)
//	SINDRI_ASK_CWD      where question agents run (<root>/yggdrasil/workspace)
//	AUTO_ALLOW          allow everyday reads, tests and git reads by default (true)
//	YGG_NORN_URL        Norn, where agents' messages are posted; NORN_TOKEN is Sindri's token there
func Main(ctx context.Context) error {
	tokens, err := env.Tokens("SINDRI_TOKENS")
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		return fmt.Errorf("SINDRI_TOKENS is empty: set at least owner=<token>")
	}
	home, _ := os.UserHomeDir()
	roots := strings.Split(env.String("SINDRI_ROOTS", filepath.Join(home, "projects")), ":")
	p := part.New(Name, tokens, env.List("YGG_CORS_ORIGINS"))
	agents, err := NewManager(ManagerOptions{
		ClaudeBin:  env.String("CLAUDE_BIN", filepath.Join(home, ".local/bin/claude")),
		CopilotBin: env.String("COPILOT_BIN", filepath.Join(home, ".local/bin/copilot")),
		Roots:      roots,
		StatePath:  env.String("SINDRI_STATE", filepath.Join(home, ".local/state/yggdrasil/sindri-agents.json")),
		MaxRunning: env.Int("SINDRI_MAX_RUNNING", 4),
		Idle:       env.Duration("SINDRI_IDLE", 30*time.Minute),
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
	work := &Work{
		Linear: &linear.Client{Key: env.String("LINEAR_KEY", "")},
		Agents: agents, Store: store, logf: p.Logf,
		Norn:             &NornClient{URL: where.URL("norn"), Token: env.String("NORN_TOKEN", ""), HTTP: &http.Client{Timeout: 10 * time.Second}},
		Model:            env.String("DELEGATE_MODEL", "sonnet"),
		AskCwd:           env.String("SINDRI_ASK_CWD", filepath.Join(roots[0], "yggdrasil", "workspace")),
		AutoAllowDefault: env.String("AUTO_ALLOW", "true") == "true",
	}
	s := &Sindri{Agents: agents, Root: agents.roots[0], Work: work}
	s.Mount(p)
	return p.Run(ctx, where.Addr(Name))
}
