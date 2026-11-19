package norn

// A task sized to the gap you have: when nothing in the work calendar holds
// you right now, the one waiting thing that fits before the next meeting (or
// the end of the working day). What waits: Sindri's goals by their next step
// (answer an agent, pick a race, review a branch, start a draft) and the open
// promises due today or tomorrow. Each kind has a rough size in minutes
// (GapMinutes); the pick prefers what blocks an agent, then what has waited
// longest. A save point still waiting comes first, so then nothing is offered.
// Calendar, goals and promises stay on this machine: nothing here goes to Claude.
//
//	GET /v1/gap   { free, freeUntil?, item? }

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"yggdrasil/internal/part"
)

const (
	// GapShortest is the least free time worth offering anything in.
	GapShortest = 10 * time.Minute
	// GapSlack is the room an item must leave in the gap, so it is not a race against the clock.
	GapSlack = 5 * time.Minute
	// DefaultWorkHours is the working day when WORK_HOURS is unset, Monday to Friday.
	DefaultWorkHours = "09:00-17:00"
)

// GapMinutes is the rough size of each kind of waiting thing, in minutes.
var GapMinutes = map[string]int{
	"answer":  3,  // answer an agent that waits on you
	"pick":    10, // pick the winner of a race
	"review":  20, // review a finished branch
	"start":   2,  // start a draft goal
	"promise": 10, // do what you promised
}

// GapItem is the one thing offered for a gap.
type GapItem struct {
	// Kind is answer, pick, review, start or promise.
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Text    string `json:"text"`
	Minutes int    `json:"minutes"`
	// Link is the dashboard path that does it.
	Link      string `json:"link"`
	GoalID    int64  `json:"goalId,omitempty"`
	PromiseID int64  `json:"promiseId,omitempty"`
	// Since is when it began to wait (epoch ms), for the oldest first.
	Since int64 `json:"-"`
}

// Gap is GET /v1/gap.
type Gap struct {
	// Free is whether nothing in the work calendar holds you now, within the working day.
	Free bool `json:"free"`
	// FreeUntil is when the gap ends: the next meeting's start or the end of the working day (epoch ms).
	FreeUntil int64    `json:"freeUntil,omitempty"`
	Item      *GapItem `json:"item,omitempty"`
}

// WorkDay is the working day, in minutes after midnight, Monday to Friday.
type WorkDay struct{ From, To int }

// ParseWorkHours reads "09:00-17:00".
func ParseWorkHours(text string) (WorkDay, error) {
	clock := func(s string) (int, error) {
		hour, minute, ok := strings.Cut(strings.TrimSpace(s), ":")
		h, errH := strconv.Atoi(hour)
		m, errM := strconv.Atoi(minute)
		if !ok || errH != nil || errM != nil || h < 0 || h > 24 || m < 0 || m > 59 || h*60+m > 24*60 {
			return 0, fmt.Errorf("bad clock %q", s)
		}
		return h*60 + m, nil
	}
	from, to, ok := strings.Cut(text, "-")
	a, errA := clock(from)
	b, errB := clock(to)
	if !ok || errA != nil || errB != nil || a >= b {
		return WorkDay{}, fmt.Errorf("working hours look like 09:00-17:00, not %q", text)
	}
	return WorkDay{From: a, To: b}, nil
}

// GapBusy is whether an event holds you: a meeting not declined, not all
// day, not shown as free; and a break you kept free, since a break is not for work.
func GapBusy(event CalendarEvent) bool {
	if event.Break {
		return true
	}
	return !declined(event) && !event.AllDay && event.ShowAs != "free"
}

// FreeUntil is when the free time you are in at now ends: the next busy
// event's start, or the end of the working day. False when something holds
// you now, or now is outside the working day.
func FreeUntil(events []CalendarEvent, now time.Time, zone *time.Location, day WorkDay) (time.Time, bool) {
	local := now.In(zone)
	if local.Weekday() == time.Saturday || local.Weekday() == time.Sunday {
		return time.Time{}, false
	}
	minutes := local.Hour()*60 + local.Minute()
	if minutes < day.From || minutes >= day.To {
		return time.Time{}, false
	}
	end := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone).Add(time.Duration(day.To) * time.Minute)
	nowMs := now.UnixMilli()
	for _, event := range events {
		if !GapBusy(event) || event.End <= nowMs {
			continue
		}
		if event.Start <= nowMs {
			return time.Time{}, false
		}
		if start := time.UnixMilli(event.Start); start.Before(end) {
			end = start
		}
	}
	return end, true
}

// GapGoal is the little of Sindri's goal a gap needs.
type GapGoal struct {
	ID       int64  `json:"id"`
	At       int64  `json:"at"`
	Title    string `json:"title"`
	State    string `json:"state"`
	ClosedAt *int64 `json:"closedAt"`
	Night    bool   `json:"night"`
	Tasks    []struct {
		Agent        string `json:"agent"`
		LastActivity int64  `json:"lastActivity"`
	} `json:"tasks"`
	Next struct {
		Action string `json:"action"`
		Text   string `json:"text"`
		Repo   string `json:"repo"`
		Agent  string `json:"agent"`
	} `json:"next"`
}

// GapGoals is Sindri's goals; empty when Sindri cannot say.
func (c *SindriClient) GapGoals(ctx context.Context) []GapGoal {
	var answer struct {
		Goals []GapGoal `json:"goals"`
	}
	if c.call(ctx, http.MethodGet, "/v1/goals", nil, &answer) != nil {
		return nil
	}
	return answer.Goals
}

// goalPart is a goal's page with one of its parts open, on its review when asked.
func goalPart(id int64, agent string, review bool) string {
	path := fmt.Sprintf("/dashboard/goals/%d", id)
	if agent == "" {
		return path
	}
	path += "?part=" + url.QueryEscape(agent)
	if review {
		path += "&view=review"
	}
	return path
}

// GoalGapItem is what a goal waits on you for; false when it waits on nothing you can do in a gap.
func GoalGapItem(goal GapGoal) (GapItem, bool) {
	if goal.ClosedAt != nil || goal.State == "closed" || goal.State == "merged" {
		return GapItem{}, false
	}
	item := GapItem{Kind: goal.Next.Action, Title: goal.Title, Text: goal.Next.Text, GoalID: goal.ID, Since: goal.At}
	for _, task := range goal.Tasks {
		item.Since = max(item.Since, task.LastActivity)
	}
	switch goal.Next.Action {
	case "answer":
		item.Link = goalPart(goal.ID, goal.Next.Agent, false)
	case "review":
		item.Link = goalPart(goal.ID, goal.Next.Agent, true)
	case "pick":
		item.Link = fmt.Sprintf("/dashboard/races/%d", goal.ID)
	case "start":
		if goal.Night || goal.State != "draft" {
			return GapItem{}, false // it starts on its own tonight
		}
		item.Link, item.Text = fmt.Sprintf("/dashboard/goals/%d", goal.ID), "start the draft"
	default:
		return GapItem{}, false
	}
	if item.Text == "" {
		item.Text = goal.Next.Action
	}
	item.Minutes = GapMinutes[item.Kind]
	return item, true
}

// PromisesLink is the dashboard's promises, at the foot of the Inbox page.
const PromisesLink = "/dashboard/inbox#promises"

// PromiseGapItem is an open promise due by tomorrow (today, tomorrow, or
// already past); false for one without a day or due later. Days are YYYY-MM-DD.
func PromiseGapItem(promise Promise, tomorrow string) (GapItem, bool) {
	if promise.State != "open" || promise.Due == "" || promise.Due > tomorrow {
		return GapItem{}, false
	}
	text := "a promise"
	if promise.To != "" {
		text = "promised to " + promise.To
	}
	return GapItem{Kind: "promise", Title: promise.What, Text: text, Minutes: GapMinutes["promise"], Link: PromisesLink,
		PromiseID: promise.ID, Since: promise.CreatedAt}, true
}

// PickGapItem is the one item for a gap of free: what blocks an agent
// (answer) first, then the oldest waiting, among those that fit with
// GapSlack to spare. Nil when the gap is under GapShortest or nothing fits.
func PickGapItem(items []GapItem, free time.Duration) *GapItem {
	if free < GapShortest {
		return nil
	}
	sorted := slices.Clone(items)
	slices.SortStableFunc(sorted, func(a, b GapItem) int {
		if blocksA, blocksB := a.Kind == "answer", b.Kind == "answer"; blocksA != blocksB {
			if blocksA {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.Since, b.Since)
	})
	for _, item := range sorted {
		if time.Duration(item.Minutes)*time.Minute+GapSlack <= free {
			return &item
		}
	}
	return nil
}

// GapSources is where a gap's waiting things come from; Sindri's goals by default, swapped in tests.
type GapSources struct {
	Goals func(context.Context) []GapGoal
	Day   WorkDay
}

// savePointWaiting is whether a save point is handed back (or its meeting
// over) and not yet put away: that comes before anything else.
func (n *Norn) savePointWaiting(ctx context.Context, now time.Time) bool {
	points, err := n.Store.SavePoints(ctx, 5)
	if err != nil {
		return false
	}
	nowMs, late := now.UnixMilli(), SavePointLate.Milliseconds()
	for _, sp := range points {
		ended := sp.MeetingEnd
		if sp.ReadyAt != nil {
			ended = *sp.ReadyAt
		}
		if sp.DoneAt == nil && ended <= nowMs && nowMs-ended <= late {
			return true
		}
	}
	return false
}

// GapAt is the gap you are in at now, and the one thing that fits it.
func (n *Norn) GapAt(ctx context.Context, now time.Time) Gap {
	snapshot := n.Accounts.Work.Current(now)
	if snapshot == nil {
		return Gap{}
	}
	until, free := FreeUntil(snapshot.Events, now, n.Zone, n.Gap.Day)
	if !free {
		return Gap{}
	}
	gap := Gap{Free: true, FreeUntil: until.UnixMilli()}
	if until.Sub(now) < GapShortest || n.savePointWaiting(ctx, now) {
		return gap
	}
	var items []GapItem
	for _, goal := range n.Gap.Goals(ctx) {
		if item, ok := GoalGapItem(goal); ok {
			items = append(items, item)
		}
	}
	if n.Promises.On(ctx) {
		tomorrow := now.In(n.Zone).AddDate(0, 0, 1).Format(dayLayout)
		open, _ := n.Promises.List(ctx, "open")
		for _, promise := range open {
			if item, ok := PromiseGapItem(promise, tomorrow); ok {
				items = append(items, item)
			}
		}
	}
	gap.Item = PickGapItem(items, until.Sub(now))
	return gap
}

func (n *Norn) mountGap(p *part.Part) {
	day, err := ParseWorkHours(n.Config.WorkHours)
	if err != nil {
		if n.Config.WorkHours != "" {
			n.logf("WORK_HOURS: %v; using %s", err, DefaultWorkHours)
		}
		day, _ = ParseWorkHours(DefaultWorkHours)
	}
	n.Gap = &GapSources{Goals: n.Sindri.GapGoals, Day: day}
	p.Handle("GET /v1/gap", func(w http.ResponseWriter, r *http.Request) {
		part.JSON(w, http.StatusOK, n.GapAt(r.Context(), time.Now()))
	})
}
