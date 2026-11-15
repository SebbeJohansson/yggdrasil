package munin

import (
	"context"
	"strings"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/env"
	"yggdrasil/internal/part"
	"yggdrasil/internal/where"
)

// DefaultTargets are what the parts depend on today.
var DefaultTargets = strings.Join([]string{
	"ollama=ollama:http://127.0.0.1:11434/api/tags",
	"xestrata=http:http://127.0.0.1:8095/api/health",
	"laya=http:http://127.0.0.1:8000/",
	"languagetool=http:http://127.0.0.1:8010/",
	"runner=http:http://127.0.0.1:8788/health",
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
//	MUNIN_OLLAMA_MODELS  models Ollama must have (qwen2.5:7b,llama3.2:1b,nomic-embed-text)
//	MUNIN_ONLINE_URL     fetched first to tell "offline" from "that service is down"
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
	if len(models) == 0 {
		models = []string{"qwen2.5:7b", "llama3.2:1b", "nomic-embed-text"}
	}
	m := New(targets, models, env.String("MUNIN_ONLINE_URL", "https://1.1.1.1/cdn-cgi/trace"),
		alert.NewClient(where.URL("gjallar"), env.String("GJALLAR_TOKEN", "")))
	p := part.New(Name, tokens, env.List("YGG_CORS_ORIGINS"))
	m.Mount(p)
	return p.Run(ctx, where.Addr(Name))
}
