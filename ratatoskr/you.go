package ratatoskr

// Watching you: the same idea as watching the parts, pointed at your own
// rhythm. Three patterns, each a pure function over meetings and sessions:
//
//   - stretch: meetings back to back, no gap of Break or more, for Stretch or more.
//   - late:    a session of yours, or a Sindri agent you just answered, still
//     active past Late.
//   - week:    most of the working hours left this week are meetings.
//
// None of these is a fault. They never reach Gjallar. Each may send one quiet,
// low, held message through Hermóðr per day (the week: per week), never during
// quiet hours, worded as an observation with something to do. Times only:
// meeting subjects and people are never read into a message.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/call"
	"yggdrasil/internal/part"
	"yggdrasil/internal/tell"
)

// The patterns, by name.
const (
	PatternStretch = "stretch"
	PatternLate    = "late"
	PatternWeek    = "week"
)

// Fixed parts of the patterns, not worth a setting.
const (
	// Recent is how long ago a session may have changed and still count as you working now.
	Recent = 10 * time.Minute
	// Pause is the shortest gap inside a stretch worth naming as a pause.
	Pause = 5 * time.Minute
	// FreeBlock is the shortest free block in a packed week worth suggesting.
	FreeBlock = 30 * time.Minute
	// Morning is when a late night ends (minutes after midnight).
	Morning = 5 * 60
)

// Rhythm is the settings for watching you.
type Rhythm struct {
	Stretch time.Duration // RATATOSKR_YOU_STRETCH (4h)
	Break   time.Duration // RATATOSKR_YOU_BREAK (15m)
	Late    int           // RATATOSKR_YOU_LATE (23:00), minutes after midnight
	Packed  int           // RATATOSKR_YOU_PACKED (60), percent of the week's working hours
	DayFrom int           // RATATOSKR_YOU_HOURS (09:00-17:00), minutes after midnight
	DayTo   int
	Push    bool // RATATOSKR_YOU_PUSH (off): let Hermóðr push to the phone too
	Zone    *time.Location
}

// DefaultRhythm is the settings when none are given.
func DefaultRhythm() Rhythm {
	return Rhythm{Stretch: 4 * time.Hour, Break: 15 * time.Minute, Late: 23 * 60, Packed: 60, DayFrom: 9 * 60, DayTo: 17 * 60, Zone: time.Local}
}

// ParseClock reads "23:00" as minutes after midnight.
func ParseClock(text string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(text))
	if err != nil {
		return 0, fmt.Errorf("a time looks like 23:00, not %q", text)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// ParseHours reads working hours, "09:00-17:00".
func ParseHours(text string) (int, int, error) {
	from, to, ok := strings.Cut(text, "-")
	a, errA := ParseClock(from)
	b, errB := ParseClock(to)
	if !ok || errA != nil || errB != nil || a >= b {
		return 0, 0, fmt.Errorf("working hours look like 09:00-17:00, not %q", text)
	}
	return a, b, nil
}

// Span is a stretch of time: a meeting, a run of them, or a free block.
type Span struct {
	Start time.Time
	End   time.Time
}

func (s Span) length() time.Duration { return s.End.Sub(s.Start) }

// Join sorts spans and joins any two whose gap is shorter than gap (0 joins
// only overlapping ones).
func Join(spans []Span, gap time.Duration) []Span {
	sorted := append([]Span(nil), spans...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start.Before(sorted[j].Start) })
	var joined []Span
	for _, s := range sorted {
		if !s.End.After(s.Start) {
			continue
		}
		if n := len(joined); n > 0 && s.Start.Before(joined[n-1].End.Add(gap)) {
			if s.End.After(joined[n-1].End) {
				joined[n-1].End = s.End
			}
			continue
		}
		joined = append(joined, s)
	}
	return joined
}

// StretchSeen is what Stretch found.
type StretchSeen struct {
	Active bool
	// Since is when the run of meetings began; NextGap when it ends, the first gap of Break or more.
	Since, NextGap time.Time
	// PauseAt and PauseEnds are the next short gap inside the run, if one is ahead.
	PauseAt, PauseEnds time.Time
}

// Stretch is active while now is inside a run of meetings with no gap of
// Break or more that lasts Stretch or more.
func Stretch(meetings []Span, now time.Time, rhythm Rhythm) StretchSeen {
	busy := Join(meetings, 0)
	for _, run := range Join(busy, rhythm.Break) {
		if now.Before(run.Start) || !now.Before(run.End) || run.length() < rhythm.Stretch {
			continue
		}
		seen := StretchSeen{Active: true, Since: run.Start, NextGap: run.End}
		for i := 1; i < len(busy); i++ {
			gap := Span{busy[i-1].End, busy[i].Start}
			if gap.Start.Before(now) || gap.Start.Before(run.Start) || gap.End.After(run.End) || gap.length() < Pause {
				continue
			}
			seen.PauseAt, seen.PauseEnds = gap.Start, gap.End
			break
		}
		return seen
	}
	return StretchSeen{}
}

func at(day time.Time, minutes int) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), minutes/60, minutes%60, 0, 0, day.Location())
}

func clockOf(t time.Time) int { return t.Hour()*60 + t.Minute() }

// LateStart is when the late evening now belongs to began: today's or
// yesterday's Late.
func LateStart(now time.Time, rhythm Rhythm) time.Time {
	now = now.In(rhythm.Zone)
	start := at(now, rhythm.Late)
	if start.After(now) {
		start = start.AddDate(0, 0, -1)
	}
	return start
}

// IsLate reports whether now is between Late and Morning.
func IsLate(now time.Time, rhythm Rhythm) bool {
	m := clockOf(now.In(rhythm.Zone))
	if rhythm.Late > Morning {
		return m >= rhythm.Late || m < Morning
	}
	return m >= rhythm.Late && m < Morning
}

// LateSeen is what Late found.
type LateSeen struct {
	Active bool
	// Since is the earliest activity seen after Late.
	Since time.Time
}

// Late is active when it is late and one of the times given (your sessions'
// last activity, an agent you answered) is past Late and within Recent.
func Late(activity []time.Time, now time.Time, rhythm Rhythm) LateSeen {
	if !IsLate(now, rhythm) {
		return LateSeen{}
	}
	start := LateStart(now, rhythm)
	var seen LateSeen
	for _, t := range activity {
		if t.Before(start) || now.Sub(t) > Recent {
			continue
		}
		if !seen.Active || t.Before(seen.Since) {
			seen = LateSeen{Active: true, Since: t}
		}
	}
	return seen
}

// WeekSeen is what Week found.
type WeekSeen struct {
	Active bool
	// Free is the longest free block of working hours still ahead this week, when there is one.
	Free *Span
}

// Week is active when, from the start of today to Friday's end, meetings
// fill Packed percent or more of the working hours. Weekends are never packed.
func Week(meetings []Span, now time.Time, rhythm Rhythm) WeekSeen {
	now = now.In(rhythm.Zone)
	if now.Weekday() == time.Saturday || now.Weekday() == time.Sunday {
		return WeekSeen{}
	}
	busy := Join(meetings, 0)
	var working, booked time.Duration
	var free *Span
	for day := now; day.Weekday() != time.Saturday; day = day.AddDate(0, 0, 1) {
		hours := Span{at(day, rhythm.DayFrom), at(day, rhythm.DayTo)}
		working += hours.length()
		cursor := hours.Start
		for _, m := range busy {
			start, end := maxTime(m.Start, hours.Start), minTime(m.End, hours.End)
			if !end.After(start) {
				continue
			}
			booked += end.Sub(start)
			free = longer(free, Span{maxTime(cursor, now), start})
			cursor = maxTime(cursor, end)
		}
		free = longer(free, Span{maxTime(cursor, now), hours.End})
	}
	if working <= 0 || booked*100 < working*time.Duration(rhythm.Packed) {
		return WeekSeen{}
	}
	if free != nil && free.length() < FreeBlock {
		free = nil
	}
	return WeekSeen{Active: true, Free: free}
}

func longer(best *Span, s Span) *Span {
	if s.length() <= 0 || (best != nil && best.length() >= s.length()) {
		return best
	}
	return &s
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// CalendarEvent is the part of Norn's calendar event watching you needs: no subject, no people.
type CalendarEvent struct {
	Start    int64  `json:"start"`
	End      int64  `json:"end"`
	AllDay   bool   `json:"allDay"`
	Response string `json:"response"`
	ShowAs   string `json:"showAs"`
}

// Meetings are the events that take your time: not all-day, not declined,
// not shown as free, out of office or working elsewhere.
func Meetings(events []CalendarEvent) []Span {
	var spans []Span
	for _, e := range events {
		switch {
		case e.AllDay, e.Response == "declined":
			continue
		}
		switch e.ShowAs {
		case "free", "oof", "workingElsewhere":
			continue
		}
		spans = append(spans, Span{time.UnixMilli(e.Start), time.UnixMilli(e.End)})
	}
	return spans
}

// AgentSeen is the part of a Sindri agent watching you needs.
type AgentSeen struct {
	ID           string `json:"id"`
	AsksYou      bool   `json:"asksYou"`
	LastActivity int64  `json:"lastActivity"`
}

// Answered is when you answered an agent: one that asked you something last
// round has moved on since. An agent working on its own does not count.
func Answered(before map[string]AgentSeen, agents []AgentSeen) []time.Time {
	var times []time.Time
	for _, a := range agents {
		prev, ok := before[a.ID]
		if ok && prev.AsksYou && !a.AsksYou && a.LastActivity > prev.LastActivity {
			times = append(times, time.UnixMilli(a.LastActivity))
		}
	}
	return times
}

// Pattern is one pattern as GET /v1/you shows it.
type Pattern struct {
	Name   string     `json:"name"`
	Active bool       `json:"active"`
	Since  *time.Time `json:"since,omitempty"`
	// NextGap is the next free time to step away; NextGapEnds when it ends.
	NextGap     *time.Time `json:"nextGap,omitempty"`
	NextGapEnds *time.Time `json:"nextGapEnds,omitempty"`
	// ToldAt is when you were last told of it, in the current day (week).
	ToldAt *time.Time `json:"toldAt,omitempty"`
}

// YouStatus is GET /v1/you's answer.
type YouStatus struct {
	On        bool       `json:"on"`
	Patterns  []Pattern  `json:"patterns"`
	CheckedAt *time.Time `json:"checkedAt,omitempty"`
	Errors    []string   `json:"errors,omitempty"`
}

// You watches your rhythm.
type You struct {
	Rhythm  Rhythm
	Norn    *call.Part
	Sindri  *call.Part
	Hermodr *call.Part
	// Every is how often it looks (RATATOSKR_YOU_EVERY, 5m).
	Every time.Duration

	mu     sync.Mutex
	now    func() time.Time
	seeded bool
	since  map[string]time.Time
	told   map[string]time.Time
	agents map[string]AgentSeen
	status YouStatus
}

// NewYou watches you with the parts given; each may be off.
func NewYou(rhythm Rhythm, norn, sindri, hermodr *call.Part) *You {
	if rhythm.Zone == nil {
		rhythm.Zone = time.Local
	}
	return &You{Rhythm: rhythm, Norn: norn, Sindri: sindri, Hermodr: hermodr, Every: 5 * time.Minute, now: time.Now,
		since: map[string]time.Time{}, told: map[string]time.Time{}, status: YouStatus{On: true, Patterns: []Pattern{}}}
}

// Titles are fixed per pattern, so what was told today can be read back
// from Hermóðr after a restart.
func (y *You) title(pattern string) string {
	switch pattern {
	case PatternStretch:
		return "A long run of meetings"
	case PatternLate:
		return "It's past " + hhmm(y.Rhythm.Late)
	case PatternWeek:
		return "A full week"
	}
	return ""
}

func hhmm(minutes int) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}

// period is the day (or week, or late evening) a message at t belongs to:
// one message per pattern per period.
func (y *You) period(pattern string, t time.Time) string {
	t = t.In(y.Rhythm.Zone)
	switch pattern {
	case PatternLate:
		return LateStart(t, y.Rhythm).Format("2006-01-02")
	case PatternWeek:
		year, week := t.ISOWeek()
		return fmt.Sprintf("%d-W%02d", year, week)
	}
	return t.Format("2006-01-02")
}

func (y *You) toldNow(pattern string, now time.Time) *time.Time {
	t, ok := y.told[pattern]
	if !ok || y.period(pattern, t) != y.period(pattern, now) {
		return nil
	}
	return &t
}

// seed reads back from Hermóðr what was already said, so a restart repeats nothing.
func (y *You) seed(ctx context.Context) error {
	if y.seeded {
		return nil
	}
	if !y.Hermodr.On() {
		return nil
	}
	var list struct {
		Messages []struct {
			Source string    `json:"source"`
			Title  string    `json:"title"`
			At     time.Time `json:"at"`
		} `json:"messages"`
	}
	if err := y.Hermodr.Do(ctx, http.MethodGet, "/v1/messages", nil, &list); err != nil {
		return err
	}
	for _, m := range list.Messages {
		if m.Source != Name {
			continue
		}
		for _, pattern := range []string{PatternStretch, PatternLate, PatternWeek} {
			if m.Title == y.title(pattern) && m.At.After(y.told[pattern]) {
				y.told[pattern] = m.At
			}
		}
	}
	y.seeded = true
	return nil
}

// Message is what you are told when a pattern starts. Times only, no subjects.
func (y *You) Message(pattern string, stretch StretchSeen, week WeekSeen) tell.Message {
	clock := func(t time.Time) string { return t.In(y.Rhythm.Zone).Format("15:04") }
	m := tell.Message{Title: y.title(pattern), Priority: "low", Hold: true}
	switch pattern {
	case PatternStretch:
		if !stretch.PauseAt.IsZero() {
			m.Text = fmt.Sprintf("Meetings run back to back until %s. The pause at %s is enough to stand up and get some water, and the gap at %s is a good moment to step away.",
				clock(stretch.NextGap), clock(stretch.PauseAt), clock(stretch.NextGap))
		} else {
			m.Text = fmt.Sprintf("Meetings run back to back until %s. The next gap is at %s, a good moment to step away from the screen.", clock(stretch.NextGap), clock(stretch.NextGap))
		}
	case PatternLate:
		m.Text = "A session of yours is still going. Writing down the next step now makes tomorrow's start easy, and then it's a good moment to stop for tonight."
	case PatternWeek:
		if week.Free != nil {
			start := week.Free.Start.In(y.Rhythm.Zone)
			m.Text = fmt.Sprintf("Most of this week's working hours are meetings. %s %s–%s is still open, a good block to keep for yourself.",
				start.Format("Monday"), clock(week.Free.Start), clock(week.Free.End))
		} else {
			m.Text = "Most of this week's working hours are meetings. A short walk between two of them is a good way to keep some air in the week."
		}
	}
	push := y.Rhythm.Push
	m.Push = &push
	return m
}

// Round reads the calendar and the sessions, works out each pattern, and
// tells you of one that just started, unless it is quiet hours or you were
// already told today.
func (y *You) Round(ctx context.Context) error {
	var errs []string
	var meetings []Span
	calendarOK := false
	if y.Norn.On() {
		var calendar struct {
			Events []CalendarEvent `json:"events"`
			At     *int64          `json:"at"`
		}
		if err := y.Norn.Do(ctx, http.MethodGet, "/v1/calendar", nil, &calendar); err != nil {
			errs = append(errs, err.Error())
		} else if calendar.At == nil {
			errs = append(errs, "norn: no fresh calendar")
		} else {
			meetings, calendarOK = Meetings(calendar.Events), true
		}
	}
	var activity []time.Time
	var agents []AgentSeen
	agentsOK := false
	if y.Sindri.On() {
		var sessions []struct {
			LastActivity int64 `json:"lastActivity"`
		}
		if err := y.Sindri.Do(ctx, http.MethodGet, "/v1/sessions/recent", nil, &sessions); err != nil {
			errs = append(errs, err.Error())
		}
		for _, s := range sessions {
			activity = append(activity, time.UnixMilli(s.LastActivity))
		}
		if err := y.Sindri.Do(ctx, http.MethodGet, "/v1/agents", nil, &agents); err != nil {
			errs = append(errs, err.Error())
		} else {
			agentsOK = true
		}
	}

	// Only this loop touches the state below; mu guards status alone.
	now := y.now()
	if agentsOK {
		activity = append(activity, Answered(y.agents, agents)...)
		y.agents = map[string]AgentSeen{}
		for _, a := range agents {
			y.agents[a.ID] = a
		}
	}
	var stretch StretchSeen
	var week WeekSeen
	if calendarOK {
		stretch = Stretch(meetings, now, y.Rhythm)
		week = Week(meetings, now, y.Rhythm)
	}
	late := Late(activity, now, y.Rhythm)

	patterns := []Pattern{
		{Name: PatternStretch, Active: stretch.Active},
		{Name: PatternLate, Active: late.Active},
		{Name: PatternWeek, Active: week.Active},
	}
	if stretch.Active {
		patterns[0].Since, patterns[0].NextGap = ptr(stretch.Since), ptr(stretch.NextGap)
	}
	if late.Active {
		y.keepSince(PatternLate, late.Since)
	}
	if week.Active {
		y.keepSince(PatternWeek, now)
		if week.Free != nil {
			patterns[2].NextGap, patterns[2].NextGapEnds = ptr(week.Free.Start), ptr(week.Free.End)
		}
	}
	for i := range patterns {
		if !patterns[i].Active {
			delete(y.since, patterns[i].Name)
		} else if since, ok := y.since[patterns[i].Name]; ok {
			patterns[i].Since = ptr(since)
		}
	}

	if err := y.seed(ctx); err != nil {
		errs = append(errs, err.Error())
	}
	quiet, checkedQuiet := false, false
	for i := range patterns {
		p := &patterns[i]
		if p.Active && y.seeded && y.toldNow(p.Name, now) == nil && y.Hermodr.On() {
			if !checkedQuiet {
				checkedQuiet = true
				var err error
				if quiet, err = y.quietNow(ctx); err != nil {
					errs = append(errs, err.Error())
					quiet = true
				}
			}
			if !quiet {
				if err := (&tell.Hermodr{Part: y.Hermodr}).Tell(ctx, y.Message(p.Name, stretch, week)); err != nil {
					errs = append(errs, err.Error())
				} else {
					y.told[p.Name] = now
				}
			}
		}
		p.ToldAt = y.toldNow(p.Name, now)
	}
	y.mu.Lock()
	y.status = YouStatus{On: true, Patterns: patterns, CheckedAt: ptr(now), Errors: errs}
	y.mu.Unlock()
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (y *You) keepSince(pattern string, t time.Time) {
	if since, ok := y.since[pattern]; !ok || t.Before(since) {
		y.since[pattern] = t
	}
}

// quietNow asks Hermóðr whether it is quiet hours.
func (y *You) quietNow(ctx context.Context) (bool, error) {
	var channels struct {
		Quiet struct {
			Now bool `json:"now"`
		} `json:"quiet"`
	}
	err := y.Hermodr.Do(ctx, http.MethodGet, "/v1/channels", nil, &channels)
	return channels.Quiet.Now, err
}

// Status is what it sees now.
func (y *You) Status() YouStatus {
	if y == nil {
		return YouStatus{Patterns: []Pattern{}}
	}
	y.mu.Lock()
	defer y.mu.Unlock()
	return y.status
}

func (y *You) serve(w http.ResponseWriter, _ *http.Request) {
	part.JSON(w, http.StatusOK, y.Status())
}

func ptr(t time.Time) *time.Time { return &t }
