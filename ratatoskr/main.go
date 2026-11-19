package ratatoskr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
	"yggdrasil/internal/env"
	"yggdrasil/internal/ntfy"
	"yggdrasil/internal/part"
	"yggdrasil/internal/tell"
	"yggdrasil/internal/where"
)

// DefaultWatch is every other part. A machine that doesn't run one (at work,
// say) leaves it out of RATATOSKR_WATCH.
const DefaultWatch = "norn,sindri,vedrfolnir,gjallar,munin,huginn,dellingr,hermodr"

// Main runs Ratatoskr until ctx ends. Settings:
//
//	RATATOSKR_ADDR    listen address (127.0.0.1:8775)
//	RATATOSKR_TOKENS  who may read /v1/status: owner=…
//	RATATOSKR_WATCH   the parts to watch (DefaultWatch), each at YGG_<PART>_URL
//	RATATOSKR_EVERY   how often to check (30s)
//	YGG_GJALLAR_URL   Gjallar (http://127.0.0.1:8774); GJALLAR_TOKEN is Ratatoskr's token there
//	YGG_HERMODR_URL   Hermóðr, which tells you when Gjallar is down; HERMODR_TOKEN is Ratatoskr's token there
//	NTFY_URL, NTFY_TOKEN  a phone push used when Gjallar is down and Hermóðr can't take it
//
// Watching you (you.go):
//
//	RATATOSKR_YOU          on (default) or off: off sends nothing and reads nothing;
//	                       the default until PATCH /v1/settings/you sets it
//	RATATOSKR_STATE        where that is kept (~/.local/state/yggdrasil/ratatoskr.json)
//	RATATOSKR_YOU_EVERY    how often to look (5m)
//	RATATOSKR_YOU_STRETCH  a run of meetings this long with no real gap (4h)
//	RATATOSKR_YOU_BREAK    the shortest gap that counts as a real one (15m)
//	RATATOSKR_YOU_LATE     a session of yours still active after this is working late (23:00)
//	RATATOSKR_YOU_PACKED   percent of the week's working hours in meetings that makes it packed (60)
//	RATATOSKR_YOU_HOURS    your working hours, Monday to Friday (09:00-17:00)
//	RATATOSKR_YOU_PUSH     off (default): listed in the widget only; on: Hermóðr may push it too
//	TIME_ZONE              yours (Europe/Stockholm)
//	YGG_NORN_URL           Norn; NORN_TOKEN is Ratatoskr's token there (the work calendar)
//	YGG_SINDRI_URL         Sindri; SINDRI_TOKEN is Ratatoskr's token there (your sessions and agents)
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
		tell.Chain{
			tell.NewHermodr(where.URL("hermodr"), env.String("HERMODR_TOKEN", "")),
			tell.Direct{Push: ntfy.New(env.String("NTFY_URL", ""), env.String("NTFY_TOKEN", ""))},
		})
	if r.You, err = you(); err != nil {
		return err
	}
	p := part.New(Name, tokens, env.List("YGG_CORS_ORIGINS"))
	r.Mount(p, env.Duration("RATATOSKR_EVERY", 30*time.Second))
	return p.Run(ctx, where.Addr(Name))
}

// you reads the settings for watching you. RATATOSKR_YOU is whether it is on
// until you turn it on or off from the dashboard, which is kept in
// RATATOSKR_STATE.
func you() (*You, error) {
	on := true
	switch mode := env.String("RATATOSKR_YOU", "on"); mode {
	case "off":
		on = false
	case "on":
	default:
		return nil, fmt.Errorf("RATATOSKR_YOU is on or off, not %q", mode)
	}
	home, _ := os.UserHomeDir()
	statePath := env.String("RATATOSKR_STATE", filepath.Join(home, ".local/state/yggdrasil/ratatoskr.json"))
	state, err := LoadState(statePath)
	if err != nil {
		return nil, fmt.Errorf("RATATOSKR_STATE: %w", err)
	}
	if state.You != nil {
		on = state.You.On
	}
	rhythm := DefaultRhythm()
	zone, err := time.LoadLocation(env.String("TIME_ZONE", "Europe/Stockholm"))
	if err != nil {
		return nil, fmt.Errorf("TIME_ZONE: %w", err)
	}
	rhythm.Zone = zone
	rhythm.Stretch = env.Duration("RATATOSKR_YOU_STRETCH", rhythm.Stretch)
	rhythm.Break = env.Duration("RATATOSKR_YOU_BREAK", rhythm.Break)
	if rhythm.Late, err = ParseClock(env.String("RATATOSKR_YOU_LATE", "23:00")); err != nil {
		return nil, fmt.Errorf("RATATOSKR_YOU_LATE: %w", err)
	}
	if rhythm.DayFrom, rhythm.DayTo, err = ParseHours(env.String("RATATOSKR_YOU_HOURS", "09:00-17:00")); err != nil {
		return nil, fmt.Errorf("RATATOSKR_YOU_HOURS: %w", err)
	}
	if rhythm.Packed = env.Int("RATATOSKR_YOU_PACKED", rhythm.Packed); rhythm.Packed < 1 || rhythm.Packed > 100 {
		return nil, fmt.Errorf("RATATOSKR_YOU_PACKED is a percent, 1 to 100, not %d", rhythm.Packed)
	}
	rhythm.Push = env.String("RATATOSKR_YOU_PUSH", "off") == "on"
	y := NewYou(rhythm,
		call.New("norn", where.URL("norn"), env.String("NORN_TOKEN", ""), 10*time.Second),
		call.New("sindri", where.URL("sindri"), env.String("SINDRI_TOKEN", ""), 10*time.Second),
		call.New("hermodr", where.URL("hermodr"), env.String("HERMODR_TOKEN", ""), 30*time.Second))
	y.Every = env.Duration("RATATOSKR_YOU_EVERY", y.Every)
	y.StatePath, y.on = statePath, on
	if !on {
		y.status = YouStatus{On: false, Patterns: []Pattern{}}
	}
	return y, nil
}
