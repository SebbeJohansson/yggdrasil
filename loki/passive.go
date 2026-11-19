package loki

// Passive Loki: with `passive` on, at most `perDay` strikes a day, at random
// times inside your working hours (WORK_HOURS, Monday to Friday), never at
// night, and never while you are in a meeting, on a break or in focus
// (Hermóðr). Each picks a blind spot that has a trick, or else a random part
// and trick, and afterwards one quiet Hermóðr message tells the tale.

import (
	"context"
	"fmt"
	mathrand "math/rand/v2"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

// Hours is your working day, Monday to Friday, in minutes after midnight.
type Hours struct {
	From, To int
	Zone     *time.Location
}

// DefaultHours is 09:00-17:00 in this machine's zone.
func DefaultHours() Hours {
	return Hours{From: 9 * 60, To: 17 * 60, Zone: time.Local}
}

// ParseHours reads working hours, "09:00-17:00".
func ParseHours(text string) (int, int, error) {
	from, to, ok := strings.Cut(text, "-")
	a, errA := time.Parse("15:04", strings.TrimSpace(from))
	b, errB := time.Parse("15:04", strings.TrimSpace(to))
	if !ok || errA != nil || errB != nil {
		return 0, 0, fmt.Errorf("working hours look like 09:00-17:00, not %q", text)
	}
	fromMin, toMin := a.Hour()*60+a.Minute(), b.Hour()*60+b.Minute()
	if fromMin >= toMin {
		return 0, 0, fmt.Errorf("working hours look like 09:00-17:00, not %q", text)
	}
	return fromMin, toMin, nil
}

// Night is never a time for a strike, whatever the working hours say.
const (
	nightEnds   = 7 * 60
	nightStarts = 22 * 60
)

// Busy is whether you are somewhere a strike must not disturb.
type Busy struct {
	InMeeting bool
	InBreak   bool
	Focus     bool
}

// Allowed is whether a passive strike may start now, and why not.
func Allowed(now time.Time, hours Hours, busy Busy) (bool, string) {
	local := now.In(hours.Zone)
	minutes := local.Hour()*60 + local.Minute()
	switch {
	case local.Weekday() == time.Saturday || local.Weekday() == time.Sunday:
		return false, "the weekend"
	case minutes < nightEnds || minutes >= nightStarts:
		return false, "night"
	case minutes < hours.From || minutes >= hours.To:
		return false, "outside working hours"
	case busy.InMeeting:
		return false, "in a meeting"
	case busy.InBreak:
		return false, "on a break"
	case busy.Focus:
		return false, "in focus"
	}
	return true, ""
}

// PlanDay is the times of a day's passive strikes: perDay random times in
// working hours, leaving room at the end for a strike to finish. None on a
// weekend.
func PlanDay(day time.Time, hours Hours, perDay int, random *mathrand.Rand) []time.Time {
	local := day.In(hours.Zone)
	if local.Weekday() == time.Saturday || local.Weekday() == time.Sunday || perDay < 1 {
		return nil
	}
	from, to := max(hours.From, nightEnds), min(hours.To, nightStarts)-20
	if to <= from {
		to = from + 1
	}
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, hours.Zone)
	var slots []time.Time
	for range perDay {
		minute := from + random.IntN(to-from)
		slots = append(slots, midnight.Add(time.Duration(minute)*time.Minute))
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].Before(slots[j]) })
	return slots
}

// Postpone is how long a passive strike waits when you are busy.
const Postpone = 15 * time.Minute

func (l *Loki) busy(ctx context.Context) (Busy, error) {
	hermodr := l.Clients["hermodr"]
	var channels Channels
	if err := hermodr.Do(ctx, http.MethodGet, "/v1/channels", nil, &channels); err != nil {
		return Busy{}, err
	}
	var focus struct {
		On bool `json:"on"`
	}
	if err := hermodr.Do(ctx, http.MethodGet, "/v1/focus", nil, &focus); err != nil {
		return Busy{}, err
	}
	return Busy{InMeeting: channels.InMeeting, InBreak: channels.InBreak, Focus: focus.On || channels.InFocusBlock}, nil
}

func sameDay(a, b time.Time, zone *time.Location) bool {
	return a.In(zone).Format(time.DateOnly) == b.In(zone).Format(time.DateOnly)
}

// PassiveTick starts a passive strike when one is due and nothing stands in
// the way. It runs every minute.
func (l *Loki) PassiveTick(ctx context.Context) error {
	settings := l.Settings()
	if !settings.Passive {
		return nil
	}
	now := l.now()
	if ok, _ := Allowed(now, l.Hours, Busy{}); !ok {
		return nil
	}
	today := now.In(l.Hours.Zone).Format(time.DateOnly)
	l.mu.Lock()
	if l.plannedDay != today {
		l.plannedDay = today
		l.slots = PlanDay(now, l.Hours, settings.PerDay, l.rand)
	}
	done, running := 0, false
	for _, s := range l.strikes {
		if s.By == "passive" && sameDay(s.StartedAt, now, l.Hours.Zone) {
			done++
		}
		running = running || s.running()
	}
	due := len(l.slots) > 0 && !l.slots[0].After(now)
	l.mu.Unlock()
	if done >= settings.PerDay {
		l.mu.Lock()
		l.slots = nil
		l.mu.Unlock()
		return nil
	}
	if !due || running {
		return nil
	}
	busy, err := l.busy(ctx)
	if err != nil {
		// Not knowing whether you are in a meeting is a reason to wait.
		l.postpone(now)
		return fmt.Errorf("passive: asking Hermóðr whether you are free: %w", err)
	}
	if ok, why := Allowed(now, l.Hours, busy); !ok {
		l.Logf("passive strike waits: %s", why)
		l.postpone(now)
		return nil
	}
	name, trick, ok := l.pickPassive(ctx)
	if !ok {
		l.postpone(now)
		return fmt.Errorf("passive: no part to trick (is LOKI=on set, or LOKI_PROXY?)")
	}
	l.mu.Lock()
	if len(l.slots) > 0 {
		l.slots = l.slots[1:]
	}
	l.mu.Unlock()
	start := l.Start
	if l.start != nil {
		start = l.start
	}
	if _, err := start(ctx, name, trick, DefaultMinutes, "passive"); err != nil {
		return fmt.Errorf("passive strike on %s: %w", name, err)
	}
	return nil
}

// postpone moves the next slot on by Postpone, or drops it when that is past
// the working day.
func (l *Loki) postpone(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.slots) == 0 {
		return
	}
	next := now.Add(Postpone)
	local := next.In(l.Hours.Zone)
	if local.Hour()*60+local.Minute() >= l.Hours.To-20 || !sameDay(next, now, l.Hours.Zone) {
		l.slots = l.slots[1:]
		return
	}
	l.slots[0] = next
	sort.Slice(l.slots, func(i, j int) bool { return l.slots[i].Before(l.slots[j]) })
}

// passiveTricks is the tricks a passive strike may play on a part: by its
// switch when that answers, by proxy when it is proxied, by freeze only with
// allowFreeze on, and only on a part that is up. Never Loki or Gjallar;
// Ratatoskr only with allowWatchers on.
func passiveTricks(t Target, settings Settings) []TargetTrick {
	if !t.Up || t.Name == Name || t.Name == Gjallar || (t.Name == Ratatoskr && !settings.AllowWatchers) {
		return nil
	}
	var tricks []TargetTrick
	for _, trick := range t.Tricks {
		via := trick.Via
		if via == "" {
			via = ViaSwitch
		}
		if !t.available(via) || (via == ViaFreeze && !settings.AllowFreeze) {
			continue
		}
		tricks = append(tricks, trick)
	}
	return tricks
}

// pickPassive picks what to strike: a blind spot with a trick, else a random
// part and trick, among the tricks passiveTricks allows.
func (l *Loki) pickPassive(ctx context.Context) (string, Trick, bool) {
	targets := l.Targets(ctx)
	l.mu.Lock()
	spots := l.plan.BlindSpots
	settings := l.settings
	l.mu.Unlock()
	open := map[string][]TargetTrick{}
	for _, t := range targets {
		if tricks := passiveTricks(t, settings); len(tricks) > 0 {
			open[t.Name] = tricks
		}
	}
	var candidates []BlindSpot
	for _, spot := range spots {
		if spot.Trick == nil {
			continue
		}
		if slices.ContainsFunc(open[spot.Part], func(t TargetTrick) bool {
			return t.Via == spot.Trick.via() && t.Kind == spot.Trick.Kind && t.Loop == spot.Trick.Loop
		}) {
			candidates = append(candidates, spot)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(candidates) > 0 {
		spot := candidates[l.rand.IntN(len(candidates))]
		return spot.Part, *spot.Trick, true
	}
	var names []string
	for name := range open {
		names = append(names, name)
	}
	if len(names) == 0 {
		return "", Trick{}, false
	}
	slices.Sort(names)
	name := names[l.rand.IntN(len(names))]
	tricks := open[name]
	return name, tricks[l.rand.IntN(len(tricks))].Trick(), true
}
