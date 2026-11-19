package loki

// The plan: the blind spots, rebuilt every hour from what is really there,
// and the tales of the real alerts of the last 14 days.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/part"
)

// BlindSpot is something that could break without anyone noticing.
type BlindSpot struct {
	ID       string `json:"id"`
	Part     string `json:"part"`
	Kind     string `json:"kind"`
	Text     string `json:"text"`
	Severity string `json:"severity"`
	// Trick is the strike that would test it, when there is one.
	Trick *Trick `json:"trick,omitempty"`
}

// Tale is a short story of one real alert.
type Tale struct {
	ID             string    `json:"id"`
	AlertID        int64     `json:"alertId"`
	Part           string    `json:"part"`
	Title          string    `json:"title"`
	Text           string    `json:"text"`
	OpenedAt       time.Time `json:"openedAt"`
	NoticedAfterMs *int64    `json:"noticedAfterMs,omitempty"`
	HealedAfterMs  *int64    `json:"healedAfterMs,omitempty"`
}

// Plan is GET /v1/plan.
type Plan struct {
	CheckedAt  time.Time   `json:"checkedAt"`
	BlindSpots []BlindSpot `json:"blindSpots"`
	Tales      []Tale      `json:"tales"`
}

// Channel is one of Hermóðr's channels, as Loki reads it.
type Channel struct {
	Name      string `json:"name"`
	On        bool   `json:"on"`
	LastError string `json:"lastError,omitempty"`
	Failures  int    `json:"failures"`
}

// Channels is the part of Hermóðr's GET /v1/channels Loki reads.
type Channels struct {
	Channels  []Channel `json:"channels"`
	InMeeting bool      `json:"inMeeting"`
	InBreak   bool      `json:"inBreak"`
	// InFocusBlock is a focus block you booked from the week's plan.
	InFocusBlock bool `json:"inFocusBlock"`
}

// Seen is everything the blind spots are built from.
type Seen struct {
	Parts  []string
	Probes map[string]Probe
	// Watched is the parts Ratatoskr watches.
	Watched []string
	// Heartbeats are the senders Gjallar expects to beat.
	Heartbeats []string
	// Alerts is Gjallar's alert history; nil when Gjallar didn't answer.
	Alerts []Alert
	// Channels is Hermóðr's channels; nil when Hermóðr didn't answer.
	Channels *Channels
	// Strikes is every strike kept.
	Strikes []Strike
	// Proxied is the parts behind Loki's proxy (LOKI_PROXY).
	Proxied []string
}

var severityRank = map[string]int{"high": 0, "medium": 1, "low": 2}

// caughtBefore is whether a strike of this trick on the part was ever caught.
func (s Seen) caughtBefore(name string, trick Trick) bool {
	// A part going down counts however it was made to: switch, proxy or freeze.
	down := func(kind string) bool {
		return kind == part.TrickHealth || kind == KindDrop || kind == KindHang || kind == KindFreeze
	}
	return slices.ContainsFunc(s.Strikes, func(strike Strike) bool {
		same := strike.Trick.Kind == trick.Kind || (down(strike.Trick.Kind) && down(trick.Kind))
		return strike.State == Caught && strike.Part == name && same && strike.Trick.Loop == trick.Loop
	})
}

func (s Seen) everRaised(source string, keys ...string) bool {
	return slices.ContainsFunc(s.Alerts, func(a Alert) bool {
		return a.Source == source && slices.Contains(keys, a.Key)
	})
}

// BlindSpots is what could break with nobody noticing, worst first.
func BlindSpots(s Seen) []BlindSpot {
	spots := []BlindSpot{}
	add := func(id, name, kind, severity, text string, trick *Trick) {
		spots = append(spots, BlindSpot{ID: id, Part: name, Kind: kind, Severity: severity, Text: text, Trick: trick})
	}
	health := &Trick{Via: ViaSwitch, Kind: part.TrickHealth}

	// A part that runs but that Ratatoskr doesn't watch.
	for _, name := range s.Parts {
		probe := s.Probes[name]
		if name == Ratatoskr || probe.Health == nil || slices.Contains(s.Watched, name) {
			continue
		}
		text := fmt.Sprintf("If %s stopped answering, nobody would know: Ratatoskr doesn't watch it. Add %s to RATATOSKR_WATCH in parts.env.", Title(name), name)
		add("unwatched:"+name, name, "unwatched", "high", text, health)
	}

	// The watcher's own watcher: Gjallar's heartbeat from Ratatoskr.
	if !slices.Contains(s.Heartbeats, Ratatoskr) {
		var trick *Trick
		if probe := s.Probes[Ratatoskr]; probe.Health != nil {
			if _, beats := probe.Health.Loops["heartbeat"]; beats {
				trick = &Trick{Via: ViaSwitch, Kind: part.TrickLoop, Loop: "heartbeat"}
			}
		}
		add("heartbeat:ratatoskr", Ratatoskr, "heartbeat", "high",
			"If Ratatoskr itself stopped, nobody would know: Gjallar expects no heartbeat from it. Set GJALLAR_HEARTBEATS=ratatoskr=3m in gjallar.env.", trick)
	}
	for _, name := range s.Parts {
		probe := s.Probes[name]
		if name == Ratatoskr || probe.Health == nil || slices.Contains(s.Watched, name) || slices.Contains(s.Heartbeats, name) {
			continue
		}
		add("heartbeat:"+name, name, "heartbeat", "medium",
			fmt.Sprintf("%s sends Gjallar no heartbeat either, so nothing at all would miss it.", Title(name)), nil)
	}

	// Critical alerts with no phone to reach.
	switch {
	case s.Channels == nil:
		add("phone:hermodr", "hermodr", "phone", "medium",
			"Hermóðr didn't answer Loki, so nobody can say whether a critical alert would reach your phone.", nil)
	default:
		ntfy := Channel{}
		for _, c := range s.Channels.Channels {
			if c.Name == "ntfy" {
				ntfy = c
			}
		}
		switch {
		case !ntfy.On:
			add("phone:hermodr", "hermodr", "phone", "high",
				"Critical alerts reach only the widget: Hermóðr has no phone channel. If something broke while you were away from the screen, nobody would know. Set NTFY_URL in hermodr.env.", nil)
		case ntfy.Failures > 0:
			add("phone:hermodr", "hermodr", "phone", "high",
				fmt.Sprintf("Hermóðr's ntfy channel failed its last %d sends (%s). A critical alert now would not reach your phone.", ntfy.Failures, ntfy.LastError), nil)
		default:
			add("phone:hermodr", "hermodr", "phone", "low",
				"If Hermóðr's ntfy channel failed at night, nobody would know: the alert about it is a warning, and warnings stay on the widget.", nil)
		}
	}

	// Watchers never proven: a part going down, and each loop failing.
	for _, name := range s.Parts {
		probe := s.Probes[name]
		if name == Ratatoskr || probe.Health == nil || !slices.Contains(s.Watched, name) {
			continue
		}
		if !s.caughtBefore(name, *health) {
			add("unproven:"+name, name, "unproven", "low",
				fmt.Sprintf("Ratatoskr has never been seen catching %s going down. One strike would show it does.", Title(name)), health)
		}
		var loops []string
		for loop := range probe.Health.Loops {
			loops = append(loops, loop)
		}
		slices.Sort(loops)
		for _, loop := range loops {
			if s.Alerts == nil {
				// Without Gjallar's history, "never seen failing" can't be said.
				break
			}
			trick := &Trick{Via: ViaSwitch, Kind: part.TrickLoop, Loop: loop}
			if s.caughtBefore(name, *trick) || s.everRaised(Ratatoskr, "failing:"+name+":"+loop, "stalled:"+name+":"+loop) {
				continue
			}
			add("loop:"+name+":"+loop, name, "loop", "low",
				fmt.Sprintf("Nobody has ever seen %s's %s loop fail. If it broke, Ratatoskr should say so, but that has never been tested.", Title(name), loop), trick)
		}
	}

	// Slowness: nobody times a part's routes.
	add("slow", Ratatoskr, "slow", "medium",
		"If a part answered every call ten seconds late, nobody would know: Ratatoskr reads only /health, which stays quick.", nil)

	// Errors: nobody reads a part's answers but /health. Loki's proxy can show it.
	errorsPart, errorsTrick := Ratatoskr, (*Trick)(nil)
	for _, name := range s.Proxied {
		if name != Ratatoskr && name != Gjallar {
			errorsPart, errorsTrick = name, &Trick{Via: ViaProxy, Kind: KindError, Status: 500}
			break
		}
	}
	add("errors", errorsPart, "errors", "medium",
		"If a part answered errors on its routes while /health stayed fine, nobody would know: Ratatoskr reads only /health, and the callers keep their failures to themselves.", errorsTrick)

	sort.SliceStable(spots, func(i, j int) bool {
		return severityRank[spots[i].Severity] < severityRank[spots[j].Severity]
	})
	return spots
}

// BuildPlan reads what is there and rebuilds the plan.
func (l *Loki) BuildPlan(ctx context.Context) error {
	seen := Seen{Parts: l.Parts, Heartbeats: l.Heartbeats, Watched: l.Watch}
	for _, name := range l.Parts {
		if l.Proxies[name] != nil {
			seen.Proxied = append(seen.Proxied, name)
		}
	}
	var wg sync.WaitGroup
	var statusErr, alertsErr error
	wg.Add(4)
	go func() {
		defer wg.Done()
		seen.Probes = l.probeAll(ctx, false)
	}()
	go func() {
		defer wg.Done()
		var status *RatatoskrStatus
		if status, statusErr = l.ratatoskrStatus(ctx); statusErr == nil {
			seen.Watched = []string{}
			for _, p := range status.Parts {
				seen.Watched = append(seen.Watched, p.Name)
			}
		}
	}()
	go func() {
		defer wg.Done()
		seen.Alerts, alertsErr = l.alerts(ctx, 1000)
	}()
	go func() {
		defer wg.Done()
		var channels Channels
		if l.Clients["hermodr"].Do(ctx, http.MethodGet, "/v1/channels", nil, &channels) == nil {
			seen.Channels = &channels
		}
	}()
	wg.Wait()
	seen.Strikes = l.Strikes(KeepStrikes)

	plan := Plan{CheckedAt: l.now(), BlindSpots: BlindSpots(seen), Tales: []Tale{}}
	if alertsErr == nil {
		plan.Tales = l.tales(ctx, seen.Alerts, seen.Strikes)
	}
	l.mu.Lock()
	if alertsErr != nil {
		// Keep the tales already told while Gjallar doesn't answer.
		plan.Tales = l.plan.Tales
	}
	l.plan = plan
	l.mu.Unlock()
	var problems []string
	if statusErr != nil {
		problems = append(problems, "Ratatoskr: "+statusErr.Error())
	}
	if alertsErr != nil {
		problems = append(problems, "Gjallar: "+alertsErr.Error())
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// Event is one step in an alert's history.
type Event struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	By   string    `json:"by"`
}

// AlertPart is the part an alert is about: down:norn and failing:norn:inbox
// from Ratatoskr are Norn's; any other alert is its source's.
func AlertPart(a Alert) string {
	if a.Source == Ratatoskr {
		fields := strings.Split(a.Key, ":")
		if len(fields) >= 2 {
			return fields[1]
		}
	}
	return a.Source
}

// MaxTales is how many tales the plan tells.
const MaxTales = 20

// tales tells the story of each real alert that opened and resolved in the
// last TaleDays: not Loki's own, and not one a strike caused.
func (l *Loki) tales(ctx context.Context, alerts []Alert, strikes []Strike) []Tale {
	since := l.now().Add(-TaleDays * 24 * time.Hour)
	var picked []Alert
	for _, a := range alerts {
		if a.ResolvedAt == nil || a.ResolvedAt.Before(since) || a.Source == Name || causedByStrike(a, strikes, l.HealWait) {
			continue
		}
		picked = append(picked, a)
	}
	sort.Slice(picked, func(i, j int) bool { return picked[i].ResolvedAt.After(*picked[j].ResolvedAt) })
	if len(picked) > MaxTales {
		picked = picked[:MaxTales]
	}
	tales := []Tale{}
	for _, a := range picked {
		var history struct {
			Events []Event `json:"events"`
		}
		_ = l.Clients[Gjallar].Do(ctx, http.MethodGet, fmt.Sprintf("/v1/alerts/%d", a.ID), nil, &history)
		opened, noticed, resolved := Timeline(a, history.Events)
		if opened.Before(since) {
			continue
		}
		tales = append(tales, AlertTale(a, opened, noticed, resolved, l.Hours.Zone))
	}
	return tales
}

// causedByStrike is whether the alert opened while a strike on its part ran.
func causedByStrike(a Alert, strikes []Strike, healWait time.Duration) bool {
	name := AlertPart(a)
	for _, s := range strikes {
		end := s.EndsAt.Add(healWait)
		if s.HealedAt != nil {
			end = *s.HealedAt
		}
		if s.Part == name && !a.FirstSeenAt.Before(s.StartedAt) && !a.FirstSeenAt.After(end) {
			return true
		}
		if s.Part == name && a.ResolvedAt != nil && !a.ResolvedAt.Before(s.StartedAt) && !a.ResolvedAt.After(end) {
			return true
		}
	}
	return false
}

// Timeline is when the alert last opened before it resolved, when you first
// acknowledged it after that (nil if you didn't), and when it resolved.
func Timeline(a Alert, events []Event) (opened time.Time, noticed *time.Time, resolved time.Time) {
	opened, resolved = a.FirstSeenAt, *a.ResolvedAt
	sort.SliceStable(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
	for _, e := range events {
		if e.At.After(resolved) {
			break
		}
		switch e.Kind {
		case "opened", "reopened":
			opened, noticed = e.At, nil
		case "acknowledged":
			if noticed == nil && !e.At.Before(opened) {
				at := e.At
				noticed = &at
			}
		}
	}
	return opened, noticed, resolved
}
