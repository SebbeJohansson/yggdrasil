package vedrfolnir

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/env"
	"yggdrasil/internal/part"
	"yggdrasil/internal/where"
)

// DefaultProbes are the local servers listed on the services page, with what
// they use.
var DefaultProbes = strings.Join([]string{
	"xestrata=http://127.0.0.1:8095/api/health",
	"ollama=http://127.0.0.1:11434",
	"laya=http://127.0.0.1:8000",
	"languagetool=http://127.0.0.1:8010",
	"my-agent=http://127.0.0.1:8787/health",
	"runner=http://127.0.0.1:8788/health",
}, ",")

// Main runs Veðrfölnir until ctx ends, saving the hour of samples on the way
// out. Settings:
//
//	VEDRFOLNIR_ADDR      listen address (127.0.0.1:8773)
//	VEDRFOLNIR_TOKENS    who may call it: owner=…,glim=…
//	VEDRFOLNIR_HISTORY   where the hour of samples is kept (~/.local/state/yggdrasil/vedrfolnir-history.json)
//	VEDRFOLNIR_UNIT_DIR  your systemd user units (~/.config/systemd/user)
//	VEDRFOLNIR_PROBES    name=url of the local servers to list (DefaultProbes)
//	YGG_GJALLAR_URL      Gjallar (http://127.0.0.1:8774); GJALLAR_TOKEN is Veðrfölnir's token there
func Main(ctx context.Context) error {
	tokens, err := env.Tokens("VEDRFOLNIR_TOKENS")
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	machine := NewMachine(env.String("VEDRFOLNIR_HISTORY", filepath.Join(home, ".local/state/yggdrasil/vedrfolnir-history.json")))
	machine.Load()
	defer machine.Save()

	v := New(machine,
		env.String("VEDRFOLNIR_UNIT_DIR", filepath.Join(home, ".config/systemd/user")),
		ParseProbes(env.String("VEDRFOLNIR_PROBES", DefaultProbes)),
		alert.NewClient(where.URL("gjallar"), env.String("GJALLAR_TOKEN", "")))
	p := part.New(Name, tokens, env.List("YGG_CORS_ORIGINS"))
	v.Mount(p)
	return p.Run(ctx, where.Addr(Name))
}
