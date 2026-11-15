package gjallar

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"yggdrasil/internal/env"
	"yggdrasil/internal/ntfy"
	"yggdrasil/internal/part"
	"yggdrasil/internal/where"
)

// Main runs Gjallar until ctx ends. Settings:
//
//	GJALLAR_ADDR        listen address (127.0.0.1:8774)
//	GJALLAR_TOKENS      who may call it: owner=…,ratatoskr=…,munin=…,vedrfolnir=… (32+ characters each)
//	GJALLAR_DB          the SQLite file (data/gjallar.db)
//	GJALLAR_HEARTBEATS  senders that must beat, and their allowance: ratatoskr=3m
//	GJALLAR_LIMITED     senders that may not send critical: glim
//	NTFY_URL, NTFY_TOKEN  the phone push topic for critical alerts; unset is off
//	DASHBOARD_URL       where a push links to
//	YGG_CORS_ORIGINS    browser origins allowed to call it
func Main(ctx context.Context) error {
	tokens, err := env.Tokens("GJALLAR_TOKENS")
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		return fmt.Errorf("GJALLAR_TOKENS is empty: set at least owner=<token>")
	}
	path := env.String("GJALLAR_DB", "data/gjallar.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	store, err := OpenStore(path)
	if err != nil {
		return err
	}
	defer store.Close()

	g := New(store, ntfy.New(env.String("NTFY_URL", ""), env.String("NTFY_TOKEN", "")))
	g.DashboardURL = env.String("DASHBOARD_URL", "")
	for _, pair := range env.Pairs(env.String("GJALLAR_HEARTBEATS", "ratatoskr=3m")) {
		allowance, err := time.ParseDuration(pair.Value)
		if err != nil || allowance < time.Minute {
			return fmt.Errorf("GJALLAR_HEARTBEATS: %s needs a duration of at least 1m, not %q", pair.Name, pair.Value)
		}
		g.Heartbeats[pair.Name] = allowance
	}
	limited := env.List("GJALLAR_LIMITED")
	if len(limited) == 0 {
		limited = []string{"glim"}
	}
	for _, name := range limited {
		g.LimitedSources[name] = true
	}

	p := part.New(Name, tokens, env.List("YGG_CORS_ORIGINS"))
	g.Mount(p)
	return p.Run(ctx, where.Addr(Name))
}
