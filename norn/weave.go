package norn

// The weave: one calm proposal for the working week, made once. At the first
// chance after Sunday's MakeAt (18:00) in TIME_ZONE, or on start and every few
// minutes after it when the machine was off, Norn plans the coming Monday to
// Friday within WORK_HOURS; made on a weekday, it plans what is left of the
// week. Never twice for one week: only "Make it again" (POST /v1/weave/remake)
// makes it anew.
//
// The plan is made here, by Go, from the work calendar (busy as the gap reads
// it, with breaks and focus blocks Yggdrasil booked counting as booked), the
// promises due that week, the goals waiting on you and the drafts that run
// tonight. It holds focus blocks, at most one a day, in the longest free
// stretch of FocusMinutes or more, mornings first, never on a day already
// packed with meetings (WeavePacked, Ratatoskr's packed week, per day); and a
// break after every run of meetings of LongRun or more. Each day gets one line
// in plain words, and the week one more, which the local model may say more
// warmly; without it the plain line stands. Calendar, goals and promises stay
// on this machine: nothing here goes to Claude.
//
// Nothing is booked by itself: POST /v1/weave/book books the blocks you pick
// the way a break is kept (breaks.go), a focus block as "Focus" with its own
// category, so the calendar payload says focus: true.
//
//	GET  /v1/weave                     the plan
//	POST /v1/weave/book                { ids? }
//	POST /v1/weave/remake
//	POST /v1/weave/blocks/{id}/skip
//	GET, PUT /v1/settings/weave        { on, makeAt, focusMinutes }

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"yggdrasil/internal/llm"
	"yggdrasil/internal/part"
)

const (
	// FocusCategory marks a focus block Yggdrasil booked.
	FocusCategory = "Yggdrasil focus"
	// FocusSubject is the focus block's title in the calendar.
	FocusSubject = "Focus"
	// WeaveEvery is how often Norn looks whether the week's plan is due.
	WeaveEvery = 3 * time.Minute
	// MaxFocusBlock is the longest focus block planned.
	MaxFocusBlock = 2 * time.Hour
	// LongRun is a run of meetings long enough to want a break after it.
	LongRun = 3 * time.Hour
	// RunGap is the shortest gap that ends a run of meetings, as Ratatoskr's stretch reads it.
	RunGap = 15 * time.Minute
	// WeaveBreakMin and WeaveBreakMax are the shortest and longest planned break.
	WeaveBreakMin = 15 * time.Minute
	WeaveBreakMax = 30 * time.Minute
	// WeavePacked is the percent of a day's working hours in meetings that makes it packed.
	WeavePacked = 60
	// WeaveLink is the plan on the dashboard.
	WeaveLink = "/dashboard/plan"

	weaveKey         = "weave-plan"
	weaveSettingsKey = "weave-settings"
	// morningEnds and lunchEnds name a block's time of day (minutes after midnight).
	morningEnds = 12 * 60
	lunchEnds   = 14 * 60
)

// focusKind is how a focus block is kept in the calendar.
var focusKind = SlotKind{Subject: FocusSubject, Category: FocusCategory, Body: "Kept for focus by Yggdrasil, from the week's plan.", Focus: true}

// WeaveSettings is GET and PUT /v1/settings/weave.
type WeaveSettings struct {
	On bool `json:"on"`
	// MakeAt is when on Sunday the coming week is planned, "18:00".
	MakeAt string `json:"makeAt"`
	// FocusMinutes is the shortest free stretch worth a focus block.
	FocusMinutes int `json:"focusMinutes"`
}

// DefaultWeaveSettings is the settings when none are saved.
var DefaultWeaveSettings = WeaveSettings{On: true, MakeAt: "18:00", FocusMinutes: 90}

// WeaveBlock is one focus block or break in the plan.
type WeaveBlock struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"` // focus or break
	Start int64  `json:"start"`
	End   int64  `json:"end"`
	// For is a suggested use for a focus block, from what waits on you.
	For  string `json:"for,omitempty"`
	Link string `json:"link,omitempty"`
	// Booked is whether it is in the work calendar now, as EventID.
	Booked  bool   `json:"booked"`
	EventID string `json:"eventId,omitempty"`
	Skipped bool   `json:"skipped"`
}

// WeaveDay is one working day of the plan.
type WeaveDay struct {
	Date   string       `json:"date"`
	Words  string       `json:"words"`
	Blocks []WeaveBlock `json:"blocks"`
}

// Weave is the week's plan, GET /v1/weave.
type Weave struct {
	// Week is the week's Monday, YYYY-MM-DD.
	Week   string     `json:"week"`
	MadeAt int64      `json:"madeAt"`
	Days   []WeaveDay `json:"days"`
	Words  string     `json:"words"`
	// Waiting is whether a block is still ahead, neither booked nor skipped.
	Waiting bool `json:"waiting"`
}

// WeaveUse is something waiting on you that a focus block could be for.
type WeaveUse struct {
	// Kind is promise, night (a draft that runs tonight), review, pick, start or answer.
	Kind    string
	For     string
	Link    string
	Minutes int
	// Due is a promise's day, YYYY-MM-DD.
	Due   string
	Since int64
}

// WeaveInput is what a plan is made from.
type WeaveInput struct {
	// Monday is the week's Monday, any time of day, in the zone the plan is made in.
	Monday time.Time
	// From is when the plan starts: nothing before it is planned.
	From     time.Time
	Day      WorkDay
	FocusMin time.Duration
	Events   []CalendarEvent
	Uses     []WeaveUse
}

type span struct{ start, end int64 }

func (s span) length() time.Duration { return time.Duration(s.end-s.start) * time.Millisecond }

// joinSpans sorts spans and joins any two whose gap is shorter than gap
// (touching or overlapping ones always).
func joinSpans(spans []span, gap time.Duration) []span {
	sorted := slices.Clone(spans)
	slices.SortFunc(sorted, func(a, b span) int { return cmp.Compare(a.start, b.start) })
	var joined []span
	for _, s := range sorted {
		if s.end <= s.start {
			continue
		}
		if n := len(joined); n > 0 {
			last := &joined[n-1]
			if d := s.start - last.end; d <= 0 || d < gap.Milliseconds() {
				last.end = max(last.end, s.end)
				continue
			}
		}
		joined = append(joined, s)
	}
	return joined
}

// freeIn is the time in window that busy (joined) does not cover.
func freeIn(window span, busy []span) []span {
	var free []span
	cursor := window.start
	for _, b := range busy {
		if b.end <= cursor || b.start >= window.end {
			continue
		}
		if b.start > cursor {
			free = append(free, span{cursor, min(b.start, window.end)})
		}
		cursor = max(cursor, b.end)
	}
	if cursor < window.end {
		free = append(free, span{cursor, window.end})
	}
	return free
}

// carve takes s out of free.
func carve(free []span, s span) []span {
	var out []span
	for _, f := range free {
		if s.end <= f.start || f.end <= s.start {
			out = append(out, f)
			continue
		}
		if f.start < s.start {
			out = append(out, span{f.start, s.start})
		}
		if s.end < f.end {
			out = append(out, span{s.end, f.end})
		}
	}
	return out
}

// WeaveMeeting is whether an event is a meeting for the day's load, as
// Ratatoskr counts one: not all day, not declined, not shown as free, out of
// office or working elsewhere, and not a break or a focus block.
func WeaveMeeting(event CalendarEvent) bool {
	switch {
	case event.AllDay, declined(event), event.Break, event.Focus:
		return false
	case event.ShowAs == "free", event.ShowAs == "oof", event.ShowAs == "workingElsewhere":
		return false
	}
	return true
}

// roundUp is t moved up to the next quarter hour.
func roundUp(t time.Time) time.Time {
	r := t.Truncate(15 * time.Minute)
	if r.Before(t) {
		r = r.Add(15 * time.Minute)
	}
	return r
}

// dayPlan is what one day's planning found, for the words.
type dayPlan struct {
	date      time.Time
	packed    bool
	load      int // percent of the working day in meetings
	focusKept bool
	blocks    []WeaveBlock
	hadFocus  bool // a focus stretch was long enough
}

// PlanWeek makes the plan, without its suggested uses' order mattering
// beyond what is given: promises first by day, then the rest as given.
func PlanWeek(in WeaveInput) Weave {
	zone := in.From.Location()
	monday := dayOf(in.Monday.In(zone))
	monday = monday.AddDate(0, 0, -((int(monday.Weekday()) + 6) % 7))
	from := roundUp(in.From)
	var plans []dayPlan
	for i := 0; i < 5; i++ {
		date := monday.AddDate(0, 0, i)
		dayStart := date.Add(time.Duration(in.Day.From) * time.Minute)
		dayEnd := date.Add(time.Duration(in.Day.To) * time.Minute)
		start := dayStart
		if from.After(start) {
			start = from
		}
		if !start.Before(dayEnd) {
			continue
		}
		plans = append(plans, planDay(in, date, span{dayStart.UnixMilli(), dayEnd.UnixMilli()}, start.UnixMilli()))
	}
	suggestUses(plans, in.Uses, zone)
	weave := Weave{Week: monday.Format(dayLayout), MadeAt: in.From.UnixMilli(), Days: []WeaveDay{}}
	for _, p := range plans {
		weave.Days = append(weave.Days, WeaveDay{Date: p.date.Format(dayLayout), Words: dayWords(p, zone), Blocks: p.blocks})
	}
	weave.Words = weekWords(plans, monday, in.From, in.Day)
	return weave
}

func planDay(in WeaveInput, date time.Time, hours span, start int64) dayPlan {
	plan := dayPlan{date: date, blocks: []WeaveBlock{}}
	var busy, meetings, kept []span
	for _, event := range in.Events {
		if event.AllDay || event.End <= hours.start || event.Start >= hours.end {
			continue
		}
		s := span{max(event.Start, hours.start), min(event.End, hours.end)}
		if GapBusy(event) {
			busy = append(busy, s)
		}
		switch {
		case event.Focus:
			plan.focusKept = true
		case event.Break:
			kept = append(kept, s)
		case WeaveMeeting(event):
			meetings = append(meetings, s)
		}
	}
	var load time.Duration
	for _, m := range joinSpans(meetings, 0) {
		load += m.length()
	}
	plan.load = int(load * 100 / hours.length())
	plan.packed = plan.load >= WeavePacked
	free := freeIn(span{start, hours.end}, joinSpans(busy, 0))

	// A break after each long run of meetings, in the first free 15 to 30 minutes.
	for _, run := range joinSpans(meetings, RunGap) {
		if run.length() < LongRun || run.end < start {
			continue
		}
		var gap *span
		for _, f := range free {
			at := max(f.start, run.end)
			if f.end > at && time.Duration(f.end-at)*time.Millisecond >= WeaveBreakMin {
				gap = &span{at, f.end}
				break
			}
		}
		if gap == nil {
			continue
		}
		rested := false
		for _, k := range kept {
			rested = rested || (k.start >= run.end && k.start <= gap.start)
		}
		if rested {
			continue
		}
		b := span{gap.start, gap.start + min(gap.length(), WeaveBreakMax).Milliseconds()}
		plan.blocks = append(plan.blocks, weaveBlock("break", b, in.From.Location()))
		free = carve(free, b)
	}

	// One focus block in the longest free stretch, mornings first.
	if !plan.packed && !plan.focusKept {
		noon := date.Add(morningEnds * time.Minute).UnixMilli()
		var best *span
		bestMorning := false
		for _, f := range free {
			if f.length() < in.FocusMin {
				continue
			}
			morning := f.start < noon
			switch {
			case best == nil, morning && !bestMorning, morning == bestMorning && f.length() > best.length():
				f := f
				best, bestMorning = &f, morning
			}
		}
		if best != nil {
			plan.hadFocus = true
			b := span{best.start, best.start + min(best.length(), MaxFocusBlock).Milliseconds()}
			plan.blocks = append(plan.blocks, weaveBlock("focus", b, in.From.Location()))
		}
	}
	slices.SortFunc(plan.blocks, func(a, b WeaveBlock) int { return cmp.Compare(a.Start, b.Start) })
	return plan
}

func weaveBlock(kind string, s span, zone *time.Location) WeaveBlock {
	return WeaveBlock{ID: kind + "-" + time.UnixMilli(s.start).In(zone).Format("20060102-1504"), Kind: kind, Start: s.start, End: s.end}
}

// usePriority orders what a focus block is for, after promises: what ran
// overnight, then what is closest to done.
var usePriority = map[string]int{"promise": 0, "night": 1, "review": 2, "pick": 3, "start": 4, "answer": 5}

// suggestUses gives each focus block, in order, the first waiting thing that
// fits it and none before it took: a promise due on or after its day (or
// already late), soonest first; what ran overnight, on the first day only;
// then a review, a race to pick, a draft to start, an agent to answer.
func suggestUses(plans []dayPlan, uses []WeaveUse, zone *time.Location) {
	sorted := slices.Clone(uses)
	slices.SortStableFunc(sorted, func(a, b WeaveUse) int {
		if c := cmp.Compare(usePriority[a.Kind], usePriority[b.Kind]); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Due, b.Due); c != 0 {
			return c
		}
		return cmp.Compare(a.Since, b.Since)
	})
	taken := make([]bool, len(sorted))
	for d := range plans {
		for i := range plans[d].blocks {
			block := &plans[d].blocks[i]
			if block.Kind != "focus" {
				continue
			}
			day := plans[d].date.Format(dayLayout)
			for u, use := range sorted {
				switch {
				case taken[u], time.Duration(use.Minutes)*time.Minute > time.Duration(block.End-block.Start)*time.Millisecond:
					continue
				case use.Kind == "night" && d != 0:
					continue
				case use.Kind == "promise" && use.Due < day && use.Due >= plans[0].date.Format(dayLayout):
					continue // due before this day: it would be late
				}
				taken[u] = true
				block.For, block.Link = use.For, use.Link
				if use.Kind == "promise" {
					block.For = promiseUse(use, zone)
				}
				break
			}
		}
	}
}

// promiseUse names a promise by its day: "the promise to Anna due Thursday: send the deck".
func promiseUse(use WeaveUse, zone *time.Location) string {
	when := ""
	if due, err := time.ParseInLocation(dayLayout, use.Due, zone); err == nil {
		when = " due " + due.Weekday().String()
	}
	return strings.Replace(use.For, "{due}", when, 1)
}

// timeOfDay names when a block starts: in the morning, after lunch, in the afternoon.
func timeOfDay(ms int64, zone *time.Location) string {
	t := time.UnixMilli(ms).In(zone)
	switch m := t.Hour()*60 + t.Minute(); {
	case m < morningEnds:
		return "in the morning"
	case m < lunchEnds:
		return "after lunch"
	}
	return "in the afternoon"
}

var countWords = []string{"no", "one", "two", "three", "four", "five"}

func countWord(n int) string {
	if n < len(countWords) {
		return countWords[n]
	}
	return "several"
}

// dayWords is one day in plain words: "Tuesday is heavy: a break after lunch, no focus block."
func dayWords(p dayPlan, zone *time.Location) string {
	name := p.date.Weekday().String()
	lead := name + " is steady"
	switch {
	case p.packed:
		lead = name + " is heavy"
	case p.load == 0:
		lead = name + " is clear"
	case p.load < 30:
		lead = name + " is light"
	}
	var parts []string
	var breaks []WeaveBlock
	for _, b := range p.blocks {
		if b.Kind == "break" {
			breaks = append(breaks, b)
		}
	}
	switch len(breaks) {
	case 0:
	case 1:
		parts = append(parts, "a break "+timeOfDay(breaks[0].Start, zone))
	default:
		parts = append(parts, countWord(len(breaks))+" breaks after the long runs of meetings")
	}
	switch {
	case p.focusKept:
		parts = append(parts, "your focus block is already booked")
	case p.hadFocus:
		for _, b := range p.blocks {
			if b.Kind == "focus" {
				parts = append(parts, "a focus block "+timeOfDay(b.Start, zone))
			}
		}
	case p.packed:
		parts = append(parts, "no focus block")
	default:
		parts = append(parts, "no stretch long enough for a focus block")
	}
	return lead + ": " + strings.Join(parts, ", ") + "."
}

// listWords is "Monday", "Monday and Friday", "Monday, Wednesday and Friday".
func listWords(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// weekWords is the week in one plain line.
func weekWords(plans []dayPlan, monday, from time.Time, day WorkDay) string {
	lead := "This week"
	switch {
	case from.Before(monday):
		lead = "The week ahead"
	case from.After(monday.Add(time.Duration(day.From) * time.Minute)):
		lead = "The rest of the week"
	}
	if len(plans) == 0 {
		return lead + ": nothing left to plan."
	}
	var focus, heavy []string
	breaks := false
	for _, p := range plans {
		name := p.date.Weekday().String()
		if p.packed {
			heavy = append(heavy, name)
		}
		for _, b := range p.blocks {
			if b.Kind == "focus" {
				focus = append(focus, name)
			} else {
				breaks = true
			}
		}
	}
	var parts []string
	if len(focus) > 0 {
		parts = append(parts, "focus time on "+listWords(focus))
	}
	switch {
	case len(heavy) == len(plans) && len(plans) > 1:
		parts = append(parts, "every day is heavy")
	case len(heavy) == 1:
		parts = append(parts, heavy[0]+" is heavy")
	case len(heavy) > 1:
		parts = append(parts, listWords(heavy)+" are heavy")
	}
	if breaks {
		parts = append(parts, "a break after each long run of meetings")
	}
	if len(parts) == 0 {
		return lead + ": nothing to add, the calendar already has room."
	}
	return lead + ": " + strings.Join(parts, "; ") + "."
}

// WeaveDue is the Monday of the week to plan at now, and whether its time
// has come: from Sunday's makeAt (minutes after midnight) for the week after,
// and on any weekday for the rest of the current week while working time is left.
func WeaveDue(now time.Time, zone *time.Location, makeAt int, day WorkDay) (time.Time, bool) {
	local := now.In(zone)
	today := dayOf(local)
	switch local.Weekday() {
	case time.Saturday:
		return today.AddDate(0, 0, 2), false
	case time.Sunday:
		monday := today.AddDate(0, 0, 1)
		return monday, !local.Before(today.Add(time.Duration(makeAt) * time.Minute))
	}
	monday := mondayOf(today)
	fridayEnd := monday.AddDate(0, 0, 4).Add(time.Duration(day.To) * time.Minute)
	return monday, local.Before(fridayEnd)
}

// refresh sets Waiting: a block still ahead, neither booked nor skipped.
func (w *Weave) refresh(now time.Time) {
	w.Waiting = false
	for _, d := range w.Days {
		for _, b := range d.Blocks {
			if !b.Booked && !b.Skipped && b.End > now.UnixMilli() {
				w.Waiting = true
			}
		}
	}
}

// block is the plan's block by id.
func (w *Weave) block(id string) *WeaveBlock {
	for d := range w.Days {
		for b := range w.Days[d].Blocks {
			if w.Days[d].Blocks[b].ID == id {
				return &w.Days[d].Blocks[b]
			}
		}
	}
	return nil
}

// keepBooked carries the blocks booked in old into w, the same week made again.
func (w *Weave) keepBooked(old *Weave) {
	if old == nil || old.Week != w.Week {
		return
	}
	for _, d := range old.Days {
		for _, b := range d.Blocks {
			if !b.Booked || w.block(b.ID) != nil {
				continue
			}
			for i := range w.Days {
				if w.Days[i].Date == d.Date {
					w.Days[i].Blocks = append(w.Days[i].Blocks, b)
					slices.SortFunc(w.Days[i].Blocks, func(a, b WeaveBlock) int { return cmp.Compare(a.Start, b.Start) })
				}
			}
		}
	}
}

// --- running it -------------------------------------------------------------------

// WeaveState is the plan's running state.
type WeaveState struct {
	mu  sync.Mutex
	now func() time.Time
	// Rephrase says the week's line more warmly on the local model.
	Rephrase Chatter
}

// errNoCalendar is no fresh work calendar to plan from: the plan waits for one.
var errNoCalendar = errors.New("the work calendar has not been read yet")

// WeaveSettingsNow is the saved settings, or the defaults.
func (n *Norn) WeaveSettingsNow(ctx context.Context) WeaveSettings {
	settings := DefaultWeaveSettings
	if ok, err := n.Store.Setting(ctx, weaveSettingsKey, &settings); err != nil || !ok {
		return DefaultWeaveSettings
	}
	return settings
}

func (n *Norn) loadWeave(ctx context.Context) (*Weave, error) {
	var weave Weave
	ok, err := n.Store.Setting(ctx, weaveKey, &weave)
	if err != nil || !ok {
		return nil, err
	}
	return &weave, nil
}

// weaveUses is what waits on you: the promises due by the week's Friday (or
// late already), the goals waiting on you and the drafts that run tonight.
func (n *Norn) weaveUses(ctx context.Context, monday time.Time) []WeaveUse {
	var uses []WeaveUse
	for _, goal := range n.Gap.Goals(ctx) {
		if goal.ClosedAt == nil && goal.Night && goal.State == "draft" && goal.Next.Action == "start" {
			uses = append(uses, WeaveUse{Kind: "night", For: "what ran overnight on " + goal.Title, Link: fmt.Sprintf("/dashboard/goals/%d", goal.ID),
				Minutes: GapMinutes["review"], Since: goal.At})
			continue
		}
		item, ok := GoalGapItem(goal)
		if !ok {
			continue
		}
		words := map[string]string{"review": "the review of ", "pick": "the race to pick on ", "start": "the draft ", "answer": "the agent waiting on "}[item.Kind]
		uses = append(uses, WeaveUse{Kind: item.Kind, For: words + item.Title, Link: item.Link, Minutes: item.Minutes, Since: item.Since})
	}
	if n.Promises.On(ctx) {
		friday := monday.AddDate(0, 0, 4).Format(dayLayout)
		open, _ := n.Promises.List(ctx, "open")
		for _, promise := range open {
			if promise.Due == "" || promise.Due > friday {
				continue
			}
			words := "the promise{due}: " + promise.What
			if to := strings.TrimSpace(promise.To); to != "" {
				words = "the promise to " + to + "{due}: " + promise.What
			}
			uses = append(uses, WeaveUse{Kind: "promise", For: words, Link: PromisesLink, Minutes: GapMinutes["promise"], Due: promise.Due, Since: promise.CreatedAt})
		}
	}
	return uses
}

// WeaveSystem is what the local model is told when it says the week's line more warmly.
const WeaveSystem = `You rephrase one line about someone's working week in a warmer, calm voice, as a kind friend would say it. Keep every day named and every fact; add nothing, leave nothing out. No numbers, no exclamation marks, no lists, no advice. One or two short sentences. Answer with the line only.`

// warmer is the model's line when it keeps to the plain one: every day it
// names, no new numbers, no exclamation, not much longer. Else "".
func warmer(plain, reply string) string {
	reply = strings.TrimSpace(strings.Trim(strings.TrimSpace(reply), `"`))
	if reply == "" || strings.Contains(reply, "!") || strings.Contains(reply, "\n") || len(reply) > 2*len(plain)+60 {
		return ""
	}
	for _, day := range []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday"} {
		if strings.Contains(plain, day) && !strings.Contains(reply, day) {
			return ""
		}
	}
	if !strings.ContainsFunc(plain, unicode.IsDigit) && strings.ContainsFunc(reply, unicode.IsDigit) {
		return ""
	}
	return reply
}

// makeWeave plans the week starting monday at now, keeping what old booked
// when it is the same week, and keeps it.
func (n *Norn) makeWeave(ctx context.Context, now, monday time.Time, old *Weave) (*Weave, error) {
	snapshot := n.Accounts.Work.Current(now)
	if snapshot == nil {
		return nil, errNoCalendar
	}
	settings := n.WeaveSettingsNow(ctx)
	weave := PlanWeek(WeaveInput{Monday: monday, From: now.In(n.Zone), Day: n.Gap.Day, FocusMin: time.Duration(settings.FocusMinutes) * time.Minute,
		Events: append(slices.Clone(snapshot.Events), snapshot.Later...), Uses: n.weaveUses(ctx, monday)})
	weave.keepBooked(old)
	if n.Weave.Rephrase != nil {
		reply, err := n.Weave.Rephrase(ctx, []llm.Message{{Role: "system", Content: WeaveSystem}, {Role: "user", Content: weave.Words}},
			llm.Options{Purpose: "weave", Model: n.Config.SummaryModel, Temperature: 0.4, TopP: 0.9, MaxTokens: 120, Timeout: time.Minute})
		if line := warmer(weave.Words, reply); err == nil && line != "" {
			weave.Words = line
		}
	}
	weave.refresh(now)
	if err := n.Store.SetSetting(ctx, weaveKey, weave); err != nil {
		return nil, err
	}
	return &weave, nil
}

// WeaveReady is weave.ready: the week's plan is made and waits for you.
type WeaveReady struct {
	Week   string `json:"week"`
	Words  string `json:"words"`
	MadeAt int64  `json:"madeAt"`
	Link   string `json:"link"`
}

// weaveTick makes the week's plan when its time has come and it has none
// yet, and says so with weave.ready when there is something in it.
func (n *Norn) weaveTick(ctx context.Context) error {
	settings := n.WeaveSettingsNow(ctx)
	if !settings.On {
		return nil
	}
	makeAt, err := ParseClock(settings.MakeAt)
	if err != nil {
		makeAt = 18 * 60
	}
	n.Weave.mu.Lock()
	defer n.Weave.mu.Unlock()
	now := n.Weave.now()
	monday, due := WeaveDue(now, n.Zone, makeAt, n.Gap.Day)
	if !due {
		return nil
	}
	old, err := n.loadWeave(ctx)
	if err != nil {
		return err
	}
	if old != nil && old.Week == monday.Format(dayLayout) {
		return nil
	}
	weave, err := n.makeWeave(ctx, now, monday, nil)
	if errors.Is(err, errNoCalendar) {
		return nil // planned once the calendar is read
	}
	if err != nil {
		return err
	}
	n.logf("weave: planned the week of %s", weave.Week)
	if weave.Waiting {
		n.Events.Publish("weave.ready", WeaveReady{Week: weave.Week, Words: weave.Words, MadeAt: weave.MadeAt, Link: WeaveLink})
	}
	return nil
}

// ParseClock reads "18:00" as minutes after midnight.
func ParseClock(text string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(text))
	if err != nil {
		return 0, fmt.Errorf("a time looks like 18:00, not %q", text)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// --- routes ------------------------------------------------------------------------

func (n *Norn) mountWeave(p *part.Part) {
	n.Weave = &WeaveState{now: time.Now, Rephrase: func(ctx context.Context, messages []llm.Message, options llm.Options) (string, error) {
		return n.LLM.Chat(ctx, messages, options)
	}}
	p.Handle("GET /v1/weave", n.getWeave)
	p.Handle("POST /v1/weave/book", n.bookWeave)
	p.Handle("POST /v1/weave/remake", n.remakeWeave)
	p.Handle("POST /v1/weave/blocks/{id}/skip", n.skipWeaveBlock)
	p.Handle("GET /v1/settings/weave", func(w http.ResponseWriter, r *http.Request) {
		part.JSON(w, http.StatusOK, n.WeaveSettingsNow(r.Context()))
	})
	p.Handle("PUT /v1/settings/weave", n.setWeaveSettings)
	p.Loop("weave", WeaveEvery, n.weaveTick)
}

func (n *Norn) getWeave(w http.ResponseWriter, r *http.Request) {
	weave, err := n.loadWeave(r.Context())
	if err != nil {
		n.fail(w, err)
		return
	}
	if weave == nil {
		// Before the first plan: an empty one, week "".
		weave = &Weave{Days: []WeaveDay{}}
	}
	weave.refresh(n.Weave.now())
	part.JSON(w, http.StatusOK, weave)
}

// WeaveBooked is one block's answer from POST /v1/weave/book.
type WeaveBooked struct {
	ID      string `json:"id"`
	Booked  bool   `json:"booked"`
	EventID string `json:"eventId,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (n *Norn) bookWeave(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only you can change your calendar")
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if r.ContentLength != 0 {
		if err := part.Decode(r, &body); err != nil {
			part.Error(w, http.StatusBadRequest, "send `ids`, the blocks to book, or none for all")
			return
		}
	}
	ctx := r.Context()
	n.Weave.mu.Lock()
	defer n.Weave.mu.Unlock()
	weave, err := n.loadWeave(ctx)
	if err != nil {
		n.fail(w, err)
		return
	}
	if weave == nil {
		part.Error(w, http.StatusNotFound, "no plan for the week yet")
		return
	}
	now := n.Weave.now()
	ids := body.IDs
	if len(ids) == 0 {
		for _, d := range weave.Days {
			for _, b := range d.Blocks {
				if !b.Booked && !b.Skipped && b.End > now.UnixMilli() {
					ids = append(ids, b.ID)
				}
			}
		}
	}
	results := []WeaveBooked{}
	var token string
	var events []CalendarEvent
	created := false
	for _, id := range ids {
		block := weave.block(id)
		switch {
		case block == nil:
			results = append(results, WeaveBooked{ID: id, Error: "no such block in the plan"})
			continue
		case block.Booked:
			results = append(results, WeaveBooked{ID: id, Booked: true, EventID: block.EventID})
			continue
		case block.End <= now.UnixMilli():
			results = append(results, WeaveBooked{ID: id, Error: "it is over already"})
			continue
		}
		if events == nil {
			var status int
			var problem string
			if token, events, status, problem = n.calendarToWrite(ctx); problem != "" {
				part.Error(w, status, problem)
				return
			}
		}
		kind := breakKind
		if block.Kind == "focus" {
			kind = focusKind
		}
		event, status, problem := n.keepSlot(ctx, token, events, kind, BreakRequest{Start: block.Start, End: block.End})
		if problem != "" {
			results = append(results, WeaveBooked{ID: id, Error: problem})
			continue
		}
		created = created || status == http.StatusCreated
		events = append(events, event)
		block.Booked, block.EventID, block.Skipped = true, event.ID, false
		results = append(results, WeaveBooked{ID: id, Booked: true, EventID: event.ID})
	}
	if err := n.Store.SetSetting(ctx, weaveKey, weave); err != nil {
		n.fail(w, err)
		return
	}
	if created {
		// Read again, so the blocks show everywhere Norn shows the calendar.
		if err := n.readWorkCalendar(ctx); err != nil {
			n.logf("reading the calendar after booking the plan: %v", err)
		}
	}
	weave.refresh(now)
	part.JSON(w, http.StatusOK, map[string]any{"results": results, "weave": weave})
}

func (n *Norn) remakeWeave(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only you can make the plan again")
		return
	}
	ctx := r.Context()
	n.Weave.mu.Lock()
	defer n.Weave.mu.Unlock()
	now := n.Weave.now()
	monday, _ := WeaveDue(now, n.Zone, 0, n.Gap.Day)
	old, err := n.loadWeave(ctx)
	if err != nil {
		n.fail(w, err)
		return
	}
	weave, err := n.makeWeave(ctx, now, monday, old)
	if errors.Is(err, errNoCalendar) {
		part.Error(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, weave)
}

func (n *Norn) skipWeaveBlock(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only you can change the plan")
		return
	}
	ctx := r.Context()
	n.Weave.mu.Lock()
	defer n.Weave.mu.Unlock()
	weave, err := n.loadWeave(ctx)
	if err != nil {
		n.fail(w, err)
		return
	}
	var block *WeaveBlock
	if weave != nil {
		block = weave.block(r.PathValue("id"))
	}
	switch {
	case block == nil:
		part.Error(w, http.StatusNotFound, "no such block in the plan")
		return
	case block.Booked:
		part.Error(w, http.StatusConflict, "it is in your calendar already: remove it there")
		return
	}
	block.Skipped = true
	if err := n.Store.SetSetting(ctx, weaveKey, weave); err != nil {
		n.fail(w, err)
		return
	}
	weave.refresh(n.Weave.now())
	part.JSON(w, http.StatusOK, weave)
}

func (n *Norn) setWeaveSettings(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only you change the week's plan")
		return
	}
	var body struct {
		On           *bool   `json:"on"`
		MakeAt       *string `json:"makeAt"`
		FocusMinutes *int    `json:"focusMinutes"`
	}
	if err := part.Decode(r, &body); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	settings := n.WeaveSettingsNow(r.Context())
	if body.On != nil {
		settings.On = *body.On
	}
	if body.MakeAt != nil {
		if _, err := ParseClock(*body.MakeAt); err != nil {
			part.Error(w, http.StatusBadRequest, "`makeAt` is a time on Sunday, like 18:00")
			return
		}
		settings.MakeAt = strings.TrimSpace(*body.MakeAt)
	}
	if body.FocusMinutes != nil {
		if *body.FocusMinutes < 30 || *body.FocusMinutes > 240 {
			part.Error(w, http.StatusBadRequest, "`focusMinutes` is 30 to 240")
			return
		}
		settings.FocusMinutes = *body.FocusMinutes
	}
	if err := n.Store.SetSetting(r.Context(), weaveSettingsKey, settings); err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, settings)
}
