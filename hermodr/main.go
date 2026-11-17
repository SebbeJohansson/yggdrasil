package hermodr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
	"yggdrasil/internal/env"
	"yggdrasil/internal/ntfy"
	"yggdrasil/internal/part"
	"yggdrasil/internal/where"
)

// Main runs Hermóðr until ctx ends. Settings:
//
//	HERMODR_ADDR      listen address (127.0.0.1:8779)
//	HERMODR_TOKENS    who may call it: owner=…,huginn=…,dellingr=…,gjallar=…,ratatoskr=…,sindri=…
//	HERMODR_DB        the SQLite file (data/hermodr.db)
//	NTFY_URL, NTFY_TOKEN       your phone's ntfy topic; unset is off
//	NTFY_MIN         the lowest priority ntfy sends (normal)
//	QUIET_HOURS       e.g. 22:00-07:00: only urgent messages go out then; unset is none
//	TIME_ZONE         yours (Europe/Stockholm)
//	YGG_NORN_URL      Norn; NORN_TOKEN is Hermóðr's token there (your calendars, for holding)
//	YGG_GJALLAR_URL   Gjallar; GJALLAR_TOKEN is Hermóðr's token there (channel:<name>)
func Main(ctx context.Context) error {
	tokens, err := env.Tokens("HERMODR_TOKENS")
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		return fmt.Errorf("HERMODR_TOKENS is empty: set at least owner=<token>")
	}
	path := env.String("HERMODR_DB", "data/hermodr.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	zone, err := time.LoadLocation(env.String("TIME_ZONE", "Europe/Stockholm"))
	if err != nil {
		return fmt.Errorf("TIME_ZONE: %w", err)
	}
	quiet, err := ParseQuiet(env.String("QUIET_HOURS", ""))
	if err != nil {
		return err
	}
	// Only ntfy for now: Slack and Discord were left out so nothing from your
	// inbox lands where others in the workspace or server could read it.
	channels := []Channel{
		Ntfy{Topic: ntfy.New(env.String("NTFY_URL", ""), env.String("NTFY_TOKEN", ""))},
	}
	min := map[string]string{"ntfy": env.String("NTFY_MIN", "normal")}
	for name, value := range min {
		if Rank(value) == 1 && value != "normal" {
			return fmt.Errorf("%s_MIN must be low, normal, high or urgent", name)
		}
	}
	p := part.New(Name, tokens, env.List("YGG_CORS_ORIGINS"))
	h, err := New(path, channels, min, quiet, zone,
		call.New("norn", where.URL("norn"), env.String("NORN_TOKEN", ""), 10*time.Second),
		alert.NewClient(where.URL("gjallar"), env.String("GJALLAR_TOKEN", "")), p.Logf)
	if err != nil {
		return err
	}
	defer h.Close()
	h.Mount(p)
	return p.Run(ctx, where.Addr(Name))
}
