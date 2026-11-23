package heimdall

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
	"yggdrasil/internal/env"
	"yggdrasil/internal/part"
	"yggdrasil/internal/where"
)

// Main runs Heimdall until ctx ends. It is a host unit: it reads the host's
// /proc/net/tcp and processes, your repos and ~/.config/yggdrasil. Settings:
//
//	HEIMDALL_ADDR    listen address (127.0.0.1:8781)
//	HEIMDALL_TOKENS  who may call it: owner=…
//	HEIMDALL_STATE   where findings and settings are kept (~/.local/state/yggdrasil/heimdall.json)
//	HEIMDALL_SINDRI_REPOS  on: also every repo Sindri knows (off)
//	HEIMDALL_REPOS   the repos, comma separated (~/projects/yggdrasil/yggdrasil-desktop,
//	                 ~/projects/yggdrasil/mimir,~/projects/my-widget),
//	                 until PATCH /v1/settings sets them; Sindri's projects are added
//	HEIMDALL_FILES   more files to check the mode and age of, globs (none)
//	HEIMDALL_OSV_URL osv.dev's API (https://api.osv.dev)
//	HEIMDALL_PROC    /proc
//	TIME_ZONE        yours (Europe/Stockholm), for the nightly run
//	YGG_GJALLAR_URL  Gjallar; GJALLAR_TOKEN is Heimdall's token there
//	YGG_NORN_URL     Norn; NORN_TOKEN is Heimdall's token there (the scopes check)
//	YGG_SINDRI_URL   Sindri; SINDRI_TOKEN is Heimdall's token there (its projects)
func Main(ctx context.Context) error {
	tokens, err := env.Tokens("HEIMDALL_TOKENS")
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		return fmt.Errorf("HEIMDALL_TOKENS is empty: set at least owner=<token>")
	}
	home, _ := os.UserHomeDir()
	var repos []string
	for _, repo := range env.List("HEIMDALL_REPOS") {
		repos = append(repos, expandHome(repo, home))
	}
	if len(repos) == 0 {
		repos = []string{filepath.Join(home, "projects/yggdrasil/yggdrasil-desktop"), filepath.Join(home, "projects/yggdrasil/mimir"), filepath.Join(home, "projects/my-widget")}
	}
	zone, err := time.LoadLocation(env.String("TIME_ZONE", "Europe/Stockholm"))
	if err != nil {
		return fmt.Errorf("TIME_ZONE: %w", err)
	}
	checks := []Check{
		NewDeps(env.String("HEIMDALL_OSV_URL", OSVURL)),
		NewListening(env.String("HEIMDALL_PROC", "/proc")),
		NewSecrets(),
		&Files{Dirs: []string{filepath.Join(home, ".config/yggdrasil")}, Globs: env.List("HEIMDALL_FILES"), Home: home},
		&Scopes{Norn: call.New("norn", where.URL("norn"), env.String("NORN_TOKEN", ""), 15*time.Second)},
	}
	h := New(checks, alert.NewClient(where.URL("gjallar"), env.String("GJALLAR_TOKEN", "")), DefaultSettings(repos))
	h.Zone = zone
	h.StatePath = env.String("HEIMDALL_STATE", filepath.Join(home, ".local/state/yggdrasil/heimdall.json"))
	// Sindri's other repos only on request: a first run over every project's
	// history is a lot to read at once (HEIMDALL_SINDRI_REPOS=on).
	if sindri := call.New("sindri", where.URL("sindri"), env.String("SINDRI_TOKEN", ""), 15*time.Second); sindri.On() && env.String("HEIMDALL_SINDRI_REPOS", "off") == "on" {
		h.MoreRepos = func(ctx context.Context) ([]Repo, error) {
			var projects []struct {
				Name string `json:"name"`
				Path string `json:"path"`
			}
			if err := sindri.Do(ctx, http.MethodGet, "/v1/projects", nil, &projects); err != nil {
				return nil, err
			}
			var found []Repo
			for _, p := range projects {
				if filepath.IsAbs(p.Path) {
					found = append(found, Repo{Name: p.Name, Path: p.Path})
				}
			}
			return found, nil
		}
	}
	if err := h.Load(); err != nil {
		return fmt.Errorf("HEIMDALL_STATE: %w", err)
	}
	p := part.New(Name, tokens, env.List("YGG_CORS_ORIGINS"))
	h.Logf = p.Logf
	h.Mount(ctx, p)
	defer h.Wait()
	return p.Run(ctx, where.Addr(Name))
}

func expandHome(path, home string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		return filepath.Join(home, rest)
	}
	return path
}
