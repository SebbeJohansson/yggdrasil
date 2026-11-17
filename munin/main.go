package munin

import (
	"context"
	"os"
	"strings"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/env"
	"yggdrasil/internal/part"
	"yggdrasil/internal/where"
)

// DefaultTargets are what the parts depend on today.
var DefaultTargets = strings.Join([]string{
	"xestrata=openai:http://127.0.0.1:8095/v1/models",
	"embeddings=openai:http://127.0.0.1:8096/v1/models",
	"laya=http:http://127.0.0.1:8000/",
	"languagetool=http:http://127.0.0.1:8010/",
	"microsoft-graph=http:https://graph.microsoft.com/v1.0/",
	"slack=http:https://slack.com/api/api.test",
	"google=http:https://www.googleapis.com/",
	"linear=http:https://api.linear.app/",
	"github=http:https://api.github.com/",
	"anthropic-status=statuspage:https://status.anthropic.com/api/v2/status.json",
	"github-status=statuspage:https://www.githubstatus.com/api/v2/status.json",
}, ",")

// Main runs Munin until ctx ends. Settings:
//
//	MUNIN_ADDR           listen address (127.0.0.1:8776)
//	MUNIN_TOKENS         who may read /v1/status: owner=…
//	MUNIN_TARGETS        name=kind:url,… (DefaultTargets)
//	MUNIN_OLLAMA_MODELS  models Ollama must have, for an ollama target (none: Yggdrasil runs without Ollama)
//	MUNIN_ONLINE_URL     fetched first to tell "offline" from "that service is down"
//	MUNIN_SOURCES        the sources this machine runs: tcgpriser,… (none). Each reads
//	                     its own MUNIN_<SOURCE>_* settings; see its package.
//	MUNIN_STATE          where feed cursors are kept (data/munin.json)
//	YGG_GJALLAR_URL      Gjallar (http://127.0.0.1:8774); GJALLAR_TOKEN is Munin's token there
func Main(ctx context.Context) error {
	tokens, err := env.Tokens("MUNIN_TOKENS")
	if err != nil {
		return err
	}
	var targets []Target
	for _, pair := range env.Pairs(env.String("MUNIN_TARGETS", DefaultTargets)) {
		target, err := ParseTarget(pair.Name, pair.Value)
		if err != nil {
			return err
		}
		targets = append(targets, target)
	}
	models := env.List("MUNIN_OLLAMA_MODELS")
	m := New(targets, models, env.String("MUNIN_ONLINE_URL", "https://1.1.1.1/cdn-cgi/trace"),
		alert.NewClient(where.URL("gjallar"), env.String("GJALLAR_TOKEN", "")))
	sources, err := MakeSources(env.List("MUNIN_SOURCES"), os.Getenv)
	if err != nil {
		return err
	}
	statePath := env.String("MUNIN_STATE", "data/munin.json")
	state, err := LoadState(statePath)
	if err != nil {
		return err
	}
	m.UseSources(sources, statePath, state)
	p := part.New(Name, tokens, env.List("YGG_CORS_ORIGINS"))
	m.Mount(p)
	return p.Run(ctx, where.Addr(Name))
}
