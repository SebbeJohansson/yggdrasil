package huginn

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
	"yggdrasil/internal/env"
	"yggdrasil/internal/part"
	"yggdrasil/internal/sse"
	"yggdrasil/internal/tell"
	"yggdrasil/internal/where"
)

// Main runs Huginn until ctx ends. Settings:
//
//	HUGINN_ADDR      listen address (127.0.0.1:8777)
//	HUGINN_TOKENS    who may call it: owner=…
//	HUGINN_DB        the SQLite file (data/huginn.db)
//	YGG_NORN_URL     Norn; NORN_TOKEN is Huginn's token there (its events and /v1/ask)
//	YGG_GJALLAR_URL  Gjallar; GJALLAR_TOKEN is Huginn's token there (its alert stream, and stream:<part>)
//	YGG_SINDRI_URL   Sindri; SINDRI_TOKEN is Huginn's token there (reads the agents)
//	YGG_HERMODR_URL  Hermóðr, which holds and carries reactions to you; HERMODR_TOKEN is Huginn's token there
//	TIME_ZONE        yours (Europe/Stockholm)
func Main(ctx context.Context) error {
	tokens, err := env.Tokens("HUGINN_TOKENS")
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		return fmt.Errorf("HUGINN_TOKENS is empty: set at least owner=<token>")
	}
	path := env.String("HUGINN_DB", "data/huginn.db")
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
		zone = time.UTC
	}

	p := part.New(Name, tokens, env.List("YGG_CORS_ORIGINS"))
	nornToken, gjallarToken := env.String("NORN_TOKEN", ""), env.String("GJALLAR_TOKEN", "")
	h, err := New(ctx, store,
		call.New("norn", where.URL("norn"), nornToken, 3*time.Minute),
		call.New("sindri", where.URL("sindri"), env.String("SINDRI_TOKEN", ""), 10*time.Second),
		alert.NewClient(where.URL("gjallar"), gjallarToken),
		tell.NewHermodr(where.URL("hermodr"), env.String("HERMODR_TOKEN", "")), zone, p.Logf)
	if err != nil {
		return err
	}
	now := time.Now()
	h.Streams = []*sse.Stream{
		sse.New("norn", where.URL("norn")+"/v1/events/stream", nornToken, now),
		sse.New("gjallar", where.URL("gjallar")+"/v1/alerts/stream", gjallarToken, now),
	}
	h.Mount(p)
	return p.Run(ctx, where.Addr(Name))
}
