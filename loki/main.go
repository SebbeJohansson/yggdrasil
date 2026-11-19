package loki

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
	"yggdrasil/internal/env"
	"yggdrasil/internal/part"
	"yggdrasil/internal/where"
)

// DefaultWatch is Ratatoskr's watch list when RATATOSKR_WATCH is unset; Loki
// reads it only when Ratatoskr doesn't answer.
const DefaultWatch = "norn,sindri,vedrfolnir,gjallar,munin,huginn,dellingr,hermodr,loki"

// Main runs Loki until ctx ends. Settings:
//
//	LOKI_ADDR          listen address (127.0.0.1:8780)
//	LOKI_TOKENS        who may call it: owner=…
//	LOKI_STATE         where settings and strikes are kept (data/loki.json)
//	LOKI_PARTS         the parts it may trick (every part but Loki)
//	<PART>_TOKEN       Loki's token at each part (NORN_TOKEN, GJALLAR_TOKEN…), at YGG_<PART>_URL:
//	                   its trick switch, Ratatoskr's status, Gjallar's alerts, Hermóðr's channels
//	GJALLAR_HEARTBEATS the senders Gjallar expects to beat (ratatoskr=3m), as in gjallar.env
//	RATATOSKR_WATCH    the parts Ratatoskr watches, read when Ratatoskr doesn't answer
//	WORK_HOURS         your working day, Monday to Friday (09:00-17:00), for passive strikes
//	TIME_ZONE          yours (Europe/Stockholm)
//
// A part answers Loki's tricks only with LOKI=on in its own environment.
func Main(ctx context.Context) error {
	tokens, err := env.Tokens("LOKI_TOKENS")
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		return fmt.Errorf("LOKI_TOKENS is empty: set at least owner=<token>")
	}
	parts := env.List("LOKI_PARTS")
	if len(parts) == 0 {
		parts = KnownParts()
	}
	urls := map[string]string{}
	clients := map[string]*call.Part{}
	for _, name := range parts {
		if _, known := where.Ports[name]; !known || name == Name {
			return fmt.Errorf("LOKI_PARTS: %q is not another part", name)
		}
	}
	for _, name := range KnownParts() {
		urls[name] = where.URL(name)
		clients[name] = call.New(name, urls[name], env.String(strings.ToUpper(name)+"_TOKEN", ""), 15*time.Second)
	}
	l := New(parts, urls, clients, alert.NewClient(where.URL("gjallar"), env.String("GJALLAR_TOKEN", "")))
	for _, pair := range env.Pairs(env.String("GJALLAR_HEARTBEATS", "ratatoskr=3m")) {
		l.Heartbeats = append(l.Heartbeats, pair.Name)
	}
	l.Watch = env.List("RATATOSKR_WATCH")
	if len(l.Watch) == 0 {
		l.Watch = strings.Split(DefaultWatch, ",")
	}
	zone, err := time.LoadLocation(env.String("TIME_ZONE", "Europe/Stockholm"))
	if err != nil {
		return fmt.Errorf("TIME_ZONE: %w", err)
	}
	l.Hours.Zone = zone
	if l.Hours.From, l.Hours.To, err = ParseHours(env.String("WORK_HOURS", "09:00-17:00")); err != nil {
		return fmt.Errorf("WORK_HOURS: %w", err)
	}
	l.StatePath = env.String("LOKI_STATE", "data/loki.json")
	if err := l.Load(); err != nil {
		return fmt.Errorf("LOKI_STATE: %w", err)
	}
	p := part.New(Name, tokens, env.List("YGG_CORS_ORIGINS"))
	l.Logf = p.Logf
	var missing []string
	for _, name := range parts {
		if !clients[name].On() {
			missing = append(missing, strings.ToUpper(name)+"_TOKEN")
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		p.Logf("no token at some parts, so they can't be tricked or read: %s", strings.Join(missing, ", "))
	}
	l.Mount(ctx, p)
	return p.Run(ctx, where.Addr(Name))
}
