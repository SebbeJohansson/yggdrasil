package ratatoskr

import (
	"context"
	"fmt"
	"strings"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/env"
	"yggdrasil/internal/ntfy"
	"yggdrasil/internal/part"
	"yggdrasil/internal/where"
)

// DefaultWatch is every other part. A machine that doesn't run one (at work,
// say) leaves it out of RATATOSKR_WATCH.
const DefaultWatch = "norn,sindri,vedrfolnir,gjallar,munin,huginn,dellingr"

// Main runs Ratatoskr until ctx ends. Settings:
//
//	RATATOSKR_ADDR    listen address (127.0.0.1:8775)
//	RATATOSKR_TOKENS  who may read /v1/status: owner=…
//	RATATOSKR_WATCH   the parts to watch (DefaultWatch), each at YGG_<PART>_URL
//	RATATOSKR_EVERY   how often to check (30s)
//	YGG_GJALLAR_URL   Gjallar (http://127.0.0.1:8774); GJALLAR_TOKEN is Ratatoskr's token there
//	NTFY_URL, NTFY_TOKEN  the phone push used only when Gjallar is down
func Main(ctx context.Context) error {
	tokens, err := env.Tokens("RATATOSKR_TOKENS")
	if err != nil {
		return err
	}
	var targets []Target
	watch := env.List("RATATOSKR_WATCH")
	if len(watch) == 0 {
		watch = strings.Split(DefaultWatch, ",")
	}
	for _, name := range watch {
		if _, known := where.Ports[name]; !known || name == Name {
			return fmt.Errorf("RATATOSKR_WATCH: %q is not another part", name)
		}
		targets = append(targets, Target{Name: name, URL: where.URL(name)})
	}
	r := New(targets,
		alert.NewClient(where.URL("gjallar"), env.String("GJALLAR_TOKEN", "")),
		ntfy.New(env.String("NTFY_URL", ""), env.String("NTFY_TOKEN", "")))
	p := part.New(Name, tokens, env.List("YGG_CORS_ORIGINS"))
	r.Mount(p, env.Duration("RATATOSKR_EVERY", 30*time.Second))
	return p.Run(ctx, where.Addr(Name))
}
