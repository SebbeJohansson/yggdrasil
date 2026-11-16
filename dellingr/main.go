package dellingr

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

// Main runs Dellingr until ctx ends. Settings:
//
//	DELLINGR_ADDR    listen address (127.0.0.1:8778)
//	DELLINGR_TOKENS  who may call it: owner=…
//	DELLINGR_DB      the SQLite file (data/dellingr.db)
//	YGG_NORN_URL     Norn; NORN_TOKEN is Dellingr's token there (ask jobs)
//	YGG_SINDRI_URL   Sindri; SINDRI_TOKEN is Dellingr's token there (goal jobs)
//	YGG_GJALLAR_URL  Gjallar; GJALLAR_TOKEN is Dellingr's token there (failing:<job>)
//	NTFY_URL, NTFY_TOKEN  the phone push topic; unset is widget only
//	TIME_ZONE        for jobs that name none (Europe/Stockholm)
func Main(ctx context.Context) error {
	tokens, err := env.Tokens("DELLINGR_TOKENS")
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		return fmt.Errorf("DELLINGR_TOKENS is empty: set at least owner=<token>")
	}
	path := env.String("DELLINGR_DB", "data/dellingr.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	store, err := OpenStore(path)
	if err != nil {
		return err
	}
	defer store.Close()
	zone, err := time.LoadLocation(env.String("TIME_ZONE", "Europe/Stockholm"))
	if err != nil {
		return fmt.Errorf("TIME_ZONE: %w", err)
	}
	p := part.New(Name, tokens, env.List("YGG_CORS_ORIGINS"))
	d := New(store,
		call.New("norn", where.URL("norn"), env.String("NORN_TOKEN", ""), 3*time.Minute),
		call.New("sindri", where.URL("sindri"), env.String("SINDRI_TOKEN", ""), 30*time.Second),
		alert.NewClient(where.URL("gjallar"), env.String("GJALLAR_TOKEN", "")),
		ntfy.New(env.String("NTFY_URL", ""), env.String("NTFY_TOKEN", "")), zone, p.Logf)
	d.Mount(p)
	return p.Run(ctx, where.Addr(Name))
}
