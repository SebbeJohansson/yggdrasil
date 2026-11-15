package ratatoskr

import (
	"context"
	"fmt"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/env"
	"yggdrasil/internal/ntfy"
	"yggdrasil/internal/part"
)

// DefaultParts are the parts that exist so far. Add norn and sindri as they
// are built.
const DefaultParts = "gjallar=http://127.0.0.1:8774,munin=http://127.0.0.1:8776,vedrfolnir=http://127.0.0.1:8773"

// Main runs Ratatoskr until ctx ends. Settings:
//
//	RATATOSKR_ADDR    listen address (127.0.0.1:8775)
//	RATATOSKR_TOKENS  who may read /v1/status: owner=…
//	RATATOSKR_PARTS   name=url of every part to watch (DefaultParts)
//	RATATOSKR_EVERY   how often to check (30s)
//	GJALLAR_URL       Gjallar (http://127.0.0.1:8774); GJALLAR_TOKEN is Ratatoskr's token there
//	NTFY_URL, NTFY_TOKEN  the phone push used only when Gjallar is down
func Main(ctx context.Context) error {
	tokens, err := env.Tokens("RATATOSKR_TOKENS")
	if err != nil {
		return err
	}
	var targets []Target
	for _, pair := range env.Pairs(env.String("RATATOSKR_PARTS", DefaultParts)) {
		if pair.Name == Name {
			continue
		}
		targets = append(targets, Target{Name: pair.Name, URL: pair.Value})
	}
	if len(targets) == 0 {
		return fmt.Errorf("RATATOSKR_PARTS names no parts")
	}
	r := New(targets,
		alert.NewClient(env.String("GJALLAR_URL", "http://127.0.0.1:8774"), env.String("GJALLAR_TOKEN", "")),
		ntfy.New(env.String("NTFY_URL", ""), env.String("NTFY_TOKEN", "")))
	p := part.New(Name, tokens, env.List("YGG_CORS_ORIGINS"))
	r.Mount(p, env.Duration("RATATOSKR_EVERY", 30*time.Second))
	return p.Run(ctx, env.String("RATATOSKR_ADDR", "127.0.0.1:8775"))
}
