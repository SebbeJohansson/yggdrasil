package sindri

// Self-repair: at the start of each night, Sindri drafts goals in the
// yggdrasil repo for what is wrong with Yggdrasil itself, marked to run
// tonight: Gjallar's active alerts about its own parts, and Norn's dispatch
// picks you keep correcting. Each goal's agent reproduces, fixes, adds a test,
// or says it is not sure and changes nothing. The morning report sorts them
// into fixed, not sure and left alone. Nothing merges on its own: every fix
// waits for you to review. Off unless you turn it on.

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"yggdrasil/internal/call"
	"yggdrasil/internal/part"
	"yggdrasil/internal/tell"
)

// SelfRepairSource is the start of every self-repair goal's source: its tag,
// followed by what it repairs (alert:<source>:<key>, dispatch:<picked>><said>).
const SelfRepairSource = "self-repair:"

// Limits on self-repair.
const (
	DefaultRepairsPerNight = 3
	MaxRepairsPerNight     = 10
	// MinCorrections is how many times in a week you must correct the same
	// dispatch pick before it becomes a goal.
	MinCorrections = 3
	correctionDays = 7
	historyEvents  = 10
	notSure        = "NOT SURE"
)

// Repair is where self-repair reads from: Gjallar's alerts and Norn's
// dispatch decisions, each with Sindri's token there.
type Repair struct {
	Gjallar *call.Part
	Norn    *call.Part
	// Default is whether it is on before the owner sets it (SINDRI_SELF_REPAIR=on).
	Default bool
	// Repo is the project the fixes are made in: yggdrasil.
	Repo string
}

// SelfRepairSettings is whether self-repair runs, and how many goals a night drafts at most.
type SelfRepairSettings struct {
	On       bool `json:"on"`
	PerNight int  `json:"perNight"`
}

// SelfRepairNight is what self-repair drafted on one night.
type SelfRepairNight struct {
	Night string  `json:"night"`
	Goals []int64 `json:"goals"`
}

// SelfRepair is the self-repair settings; off unless SINDRI_SELF_REPAIR=on or the owner turned it on.
func (w *Work) SelfRepair() SelfRepairSettings {
	settings := SelfRepairSettings{PerNight: DefaultRepairsPerNight}
	if w.Repair != nil {
		settings.On = w.Repair.Default
	}
	w.Store.Setting("self-repair", &settings)
	return settings
}

// --- alerts -----------------------------------------------------------------------

// RepairAlert is one of Gjallar's alerts, as self-repair reads it.
type RepairAlert struct {
	ID          int64     `json:"id"`
	Source      string    `json:"source"`
	Key         string    `json:"key"`
	Severity    string    `json:"severity"`
	Title       string    `json:"title"`
	Detail      string    `json:"detail"`
	State       string    `json:"state"`
	Count       int       `json:"count"`
	FirstSeenAt time.Time `json:"firstSeenAt"`
	LastSeenAt  time.Time `json:"lastSeenAt"`
	Muted       bool      `json:"muted"`
}

// AlertEvent is one line of an alert's history.
type AlertEvent struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	By   string    `json:"by"`
	Note string    `json:"note"`
}

// repairableSources are the parts whose alerts code in this repo can fix.
// Loki's missed:<part>:<kind> is a watcher that did not notice a trick.
// Munin (tcgpriser, offline, statuspage, XeStrata…) and Veðrfölnir (disks,
// units and containers that are not Yggdrasil's) watch what lies outside it.
var repairableSources = []string{"ratatoskr", "gjallar", "norn", "sindri", "huginn", "dellingr", "hermodr", "loki"}

// outsideKeys are alerts a Yggdrasil part raises about something outside it:
// a sign-in to renew, the local model, a phone channel. A part gone silent
// and a stream down are left out too: they are mostly a part that stopped,
// which Ratatoskr already says, not a bug to fix.
var outsideKeys = map[string][]string{
	"norn":    {"signin:", "model-failing"},
	"hermodr": {"channel:"},
	"gjallar": {"silent:"},
	"huginn":  {"stream:"},
}

// mergedRest is how long a merged fix blocks a new goal for the same
// problem, so it has a day to prove itself before it is tried again.
const mergedRest = 24 * time.Hour

// Repairable is whether code in this repo could fix what the alert is about.
// Muted alerts are left alone: you said not to bother you with them.
func Repairable(a RepairAlert) bool {
	if a.Muted || a.State == "resolved" || !slices.Contains(repairableSources, a.Source) {
		return false
	}
	return !slices.ContainsFunc(outsideKeys[a.Source], func(prefix string) bool { return strings.HasPrefix(a.Key, prefix) })
}

// AlertRepairKey is the source of an alert's goal.
func AlertRepairKey(a RepairAlert) string {
	return clip(SelfRepairSource+"alert:"+a.Source+":"+a.Key, 300)
}

func (r *Repair) alerts(ctx context.Context) ([]RepairAlert, error) {
	var answer struct {
		Alerts []RepairAlert `json:"alerts"`
	}
	err := r.Gjallar.Do(ctx, http.MethodGet, "/v1/alerts?state=active&limit=200", nil, &answer)
	return answer.Alerts, err
}

func (r *Repair) history(ctx context.Context, id int64) []AlertEvent {
	var answer struct {
		Events []AlertEvent `json:"events"`
	}
	_ = r.Gjallar.Do(ctx, http.MethodGet, fmt.Sprintf("/v1/alerts/%d", id), nil, &answer)
	return answer.Events
}

func alertContext(a RepairAlert, events []AlertEvent) string {
	lines := []string{
		fmt.Sprintf("Gjallar alert %s from %s (%s): %s", a.Key, a.Source, a.Severity, a.Title),
		fmt.Sprintf("Seen %d times, first %s, last %s.", a.Count, a.FirstSeenAt.UTC().Format(time.RFC3339), a.LastSeenAt.UTC().Format(time.RFC3339)),
	}
	if a.Detail != "" {
		lines = append(lines, "Detail (the error):\n"+a.Detail)
	}
	if len(events) > 0 {
		events = events[max(0, len(events)-historyEvents):]
		history := []string{"History:"}
		for _, e := range events {
			line := fmt.Sprintf("- %s %s by %s", e.At.UTC().Format(time.RFC3339), e.Kind, e.By)
			if e.Note != "" {
				line += ": " + e.Note
			}
			history = append(history, line)
		}
		lines = append(lines, strings.Join(history, "\n"))
	}
	return clip(strings.Join(lines, "\n\n"), MaxGoalContext)
}

// --- dispatch corrections --------------------------------------------------------

// RepairDecision is one of Norn's dispatch decisions, as self-repair reads
// it. It has no field for the message on purpose: the text never leaves Norn
// for a goal, only kinds, agent ids, projects and counts.
type RepairDecision struct {
	ID         int64       `json:"id"`
	At         int64       `json:"at"`
	Live       repairPick  `json:"live"`
	LiveRef    *string     `json:"liveRef"`
	Correction *repairPick `json:"correction"`
}

type repairPick struct {
	Action string  `json:"action"`
	Target *string `json:"target"`
	At     int64   `json:"at"`
}

// DispatchGroup is one kind of correction: the dispatcher picked one thing,
// you said another, this many times.
type DispatchGroup struct {
	Picked string   `json:"picked"`
	Said   string   `json:"said"`
	Count  int      `json:"count"`
	Agents []string `json:"agents"` // ids of the agents either side named
}

// Key is the source of the group's goal.
func (g DispatchGroup) Key() string {
	return clip(SelfRepairSource+"dispatch:"+g.Picked+">"+g.Said, 300)
}

// kindOf is a pick as a group shows it: the kind, with the project for code.
// Agents are named by id beside the group, not in it, so one dispatcher bug
// across several agents is one group.
func kindOf(action string, target *string) string {
	switch action {
	case "reply", "reply_to_agent":
		return "reply_to_agent"
	case "code":
		if target != nil && *target != "" {
			return "code:" + *target
		}
	}
	return action
}

// agentOf is the agent id a pick names; "" when it names none. The live
// pick's own target is the agent's name, so its id comes from liveRef.
func agentOf(ref string) string {
	kind, id, _ := strings.Cut(ref, ":")
	if kind == "reply_to_agent" || kind == "status" {
		return id
	}
	return ""
}

// GroupCorrections groups the decisions you corrected to something other
// than the live pick, made since since (ms), by what was picked and what you
// said. A decision at or before after[key] (when that group last became a
// goal) no longer counts. Groups under MinCorrections are left out; the rest
// come most corrected first.
func GroupCorrections(decisions []RepairDecision, since int64, after map[string]int64) []DispatchGroup {
	groups := map[string]*DispatchGroup{}
	var order []string
	for _, d := range decisions {
		if d.Correction == nil || d.At < since {
			continue
		}
		group := DispatchGroup{Picked: kindOf(d.Live.Action, d.Live.Target), Said: kindOf(d.Correction.Action, d.Correction.Target)}
		liveAgent, saidAgent := "", ""
		if d.LiveRef != nil {
			liveAgent = agentOf(*d.LiveRef)
		}
		if t := d.Correction.Target; t != nil && agentOf(group.Said+":"+*t) != "" {
			saidAgent = *t
		}
		if group.Picked == group.Said && liveAgent == saidAgent {
			continue // you agreed with the pick
		}
		if d.At <= after[group.Key()] {
			continue
		}
		g, ok := groups[group.Key()]
		if !ok {
			group.Agents = []string{}
			g = &group
			groups[group.Key()] = g
			order = append(order, group.Key())
		}
		g.Count++
		for _, id := range []string{liveAgent, saidAgent} {
			if id != "" && !slices.Contains(g.Agents, id) {
				g.Agents = append(g.Agents, id)
			}
		}
	}
	found := []DispatchGroup{}
	for _, key := range order {
		if g := groups[key]; g.Count >= MinCorrections {
			slices.Sort(g.Agents)
			found = append(found, *g)
		}
	}
	slices.SortStableFunc(found, func(a, b DispatchGroup) int { return b.Count - a.Count })
	return found
}

func (r *Repair) decisions(ctx context.Context) ([]RepairDecision, error) {
	var answer struct {
		Decisions []RepairDecision `json:"decisions"`
	}
	err := r.Norn.Do(ctx, http.MethodGet, "/v1/decisions?limit=1000", nil, &answer)
	return answer.Decisions, err
}

func (g DispatchGroup) title() string {
	return clip(fmt.Sprintf("Self-repair: Norn's dispatcher picked %s where you said %s, %d times this week", g.Picked, g.Said, g.Count), 200)
}

func (g DispatchGroup) context() string {
	lines := []string{
		fmt.Sprintf("Norn's dispatcher picked %s for messages you then corrected to %s, %d times in the last %d days.", g.Picked, g.Said, g.Count, correctionDays),
		"The messages themselves are not given, on purpose: reproduce from the kinds, the dispatcher's prompt, guards and tests (norn/dispatch.go, norn/decisions_view.go), not from anyone's messages.",
	}
	if len(g.Agents) > 0 {
		lines = append(lines, "Agent ids involved: "+strings.Join(g.Agents, ", "))
	}
	return strings.Join(lines, "\n\n")
}

// --- drafting --------------------------------------------------------------------

type repairDraft struct {
	key, title, context string
}

var severityRank = map[string]int{"critical": 0, "warning": 1, "info": 2}

// openRepairs is the self-repair goals that block a new one, by source: open
// ones (not closed, not merged), and ones merged within mergedRest of now,
// counted from their last task activity since a merge has no time of its own.
// last is when each source last became a goal.
func openRepairs(goals []GoalView, now time.Time) (open map[string]bool, last map[string]int64) {
	open, last = map[string]bool{}, map[string]int64{}
	for _, g := range goals {
		if !strings.HasPrefix(g.Source, SelfRepairSource) {
			continue
		}
		last[g.Source] = max(last[g.Source], g.At)
		switch g.State {
		case "closed":
		case "merged":
			latest := g.At
			for _, t := range g.Tasks {
				latest = max(latest, t.LastActivity)
			}
			if now.Sub(time.UnixMilli(latest)) < mergedRest {
				open[g.Source] = true
			}
		default:
			open[g.Source] = true
		}
	}
	return open, last
}

// RepairDrafts is what tonight could draft, most pressing first: alerts by
// severity, then dispatch groups by count, without any that has a goal open,
// at most limit.
func RepairDrafts(alerts []RepairAlert, groups []DispatchGroup, open map[string]bool, limit int, history func(RepairAlert) []AlertEvent) []repairDraft {
	alerts = slices.Clone(alerts)
	slices.SortStableFunc(alerts, func(a, b RepairAlert) int {
		return cmp.Or(cmp.Compare(rankOf(a.Severity), rankOf(b.Severity)), b.LastSeenAt.Compare(a.LastSeenAt))
	})
	var drafts []repairDraft
	seen := map[string]bool{}
	for _, a := range alerts {
		key := AlertRepairKey(a)
		if len(drafts) >= limit || !Repairable(a) || open[key] || seen[key] {
			continue
		}
		seen[key] = true
		var events []AlertEvent
		if history != nil {
			events = history(a)
		}
		drafts = append(drafts, repairDraft{key: key, title: clip("Self-repair: "+a.Title, 200), context: alertContext(a, events)})
	}
	for _, g := range groups {
		if len(drafts) >= limit || open[g.Key()] {
			continue
		}
		drafts = append(drafts, repairDraft{key: g.Key(), title: g.title(), context: g.context()})
	}
	return drafts
}

func rankOf(severity string) int {
	if rank, ok := severityRank[severity]; ok {
		return rank
	}
	return len(severityRank)
}

// DraftSelfRepair drafts tonight's self-repair goals, marked to run tonight,
// once per night. A self-repair draft still waiting for the night whose alert
// has since resolved is unmarked and closed.
func (w *Work) DraftSelfRepair(ctx context.Context, night string) ([]int64, error) {
	settings := w.SelfRepair()
	var done SelfRepairNight
	w.Store.Setting("self-repair-night", &done)
	if !settings.On || w.Repair == nil || done.Night == night {
		return nil, nil
	}
	goals, err := w.GoalViews()
	if err != nil {
		return nil, err
	}
	var alerts []RepairAlert
	if w.Repair.Gjallar.On() {
		if alerts, err = w.Repair.alerts(ctx); err != nil {
			return nil, fmt.Errorf("could not read Gjallar's alerts: %w", err)
		}
	}
	active := map[string]bool{}
	for _, a := range alerts {
		active[AlertRepairKey(a)] = true
	}
	for _, g := range goals {
		if strings.HasPrefix(g.Source, SelfRepairSource+"alert:") && g.State == "draft" && g.Night && !active[g.Source] {
			_ = w.Store.SetGoalNight(g.ID, false)
			_ = w.Store.SetGoalClosed(g.ID, true)
			w.logf("self-repair: closed goal %d, its alert resolved", g.ID)
		}
	}
	open, last := openRepairs(goals, w.Store.now())
	var groups []DispatchGroup
	if w.Repair.Norn.On() {
		decisions, err := w.Repair.decisions(ctx)
		if err != nil {
			w.logf("self-repair: could not read Norn's decisions: %v", err)
		}
		since := w.Store.now().AddDate(0, 0, -correctionDays).UnixMilli()
		groups = GroupCorrections(decisions, since, last)
	}
	history := func(a RepairAlert) []AlertEvent { return w.Repair.history(ctx, a.ID) }
	drafted := []int64{}
	for _, draft := range RepairDrafts(alerts, groups, open, settings.PerNight, history) {
		goal, err := w.Store.AddGoal(draft.title, []string{cmp.Or(w.Repair.Repo, "yggdrasil")}, draft.key, draft.context, newSession())
		if err != nil {
			return drafted, err
		}
		if err := w.Store.SetGoalNight(goal.ID, true); err != nil {
			return drafted, err
		}
		drafted = append(drafted, goal.ID)
		w.logf("self-repair: drafted goal %d for %s", goal.ID, draft.key)
	}
	return drafted, w.Store.SetSetting("self-repair-night", SelfRepairNight{Night: night, Goals: drafted})
}

// selfRepairTonight drafts tonight's goals, best effort: the night shift goes on without them.
func (w *Work) selfRepairTonight(ctx context.Context, night string) {
	if _, err := w.DraftSelfRepair(ctx, night); err != nil {
		w.logf("self-repair: %v", err)
	}
}

// SelfRepairBrief is what a self-repair goal's agent is told on top of the
// goal; "" for any other goal.
func SelfRepairBrief(goal *Goal) string {
	if !strings.HasPrefix(goal.Source, SelfRepairSource) {
		return ""
	}
	return "This is a self-repair goal: Yggdrasil found this problem in itself overnight, and nobody is watching while you work.\n" +
		"1. Reproduce it from the error and history above, with a failing test where you can.\n" +
		"2. Fix the cause, not the symptom: no retries, longer timeouts or silenced alerts just to make it go away.\n" +
		"3. Add a test that fails without the fix and passes with it.\n" +
		"4. If you cannot reproduce it, or you are not confident about the cause or the fix, change nothing: start your final answer with `" + notSure + ":` and say what you found.\n" +
		"Do not merge, push or open a pull request. The owner reviews every fix in the morning."
}

// --- the morning ------------------------------------------------------------------

// RepairOutcome sorts a self-repair goal for the morning: fixed (ready to
// review), not-sure, running (still at it when the report goes out), or
// left-alone.
func RepairOutcome(g GoalView) string {
	if slices.ContainsFunc(g.Tasks, func(t GoalTask) bool {
		return t.Result != nil && strings.HasPrefix(strings.ToUpper(strings.TrimLeft(*t.Result, " *_`#\n")), notSure)
	}) {
		return "not-sure"
	}
	switch {
	case g.State == "done" && g.Next.Action == "review":
		return "fixed"
	case g.State == "needs-you":
		return "not-sure"
	case g.State == "running":
		return "running"
	}
	return "left-alone"
}

// SelfRepairSection is the morning report's part on self-repair goals.
func SelfRepairSection(goals []GoalView) string {
	groups := []struct{ outcome, label string }{
		{"fixed", "Fixed, ready to review"},
		{"not-sure", "Not sure"},
		{"running", "Still running"},
		{"left-alone", "Left alone"},
	}
	lines := []string{"Self-repair (nothing was merged; each fix waits for your review):"}
	for _, group := range groups {
		var in []string
		for _, g := range goals {
			if RepairOutcome(g) == group.outcome {
				line := "- " + strings.TrimPrefix(g.Title, "Self-repair: ")
				if group.outcome != "fixed" && g.Next.Text != "" {
					line += ": " + g.Next.Text
				}
				in = append(in, line)
			}
		}
		if len(in) > 0 {
			lines = append(lines, group.label+":\n"+strings.Join(in, "\n"))
		}
	}
	return strings.Join(lines, "\n\n")
}

// NightReportWithRepairs is the morning message, with self-repair goals in a
// section of their own.
func NightReportWithRepairs(goals []GoalView) tell.Message {
	var others, repairs []GoalView
	for _, g := range goals {
		if strings.HasPrefix(g.Source, SelfRepairSource) {
			repairs = append(repairs, g)
		} else {
			others = append(others, g)
		}
	}
	message := NightReport(others)
	if len(repairs) == 0 {
		return message
	}
	message.Text = strings.TrimSpace(message.Text + "\n\n" + SelfRepairSection(repairs))
	if message.Title == "Night shift is done" && slices.ContainsFunc(repairs, func(g GoalView) bool { return RepairOutcome(g) == "fixed" }) {
		message.Title = "Night shift: work is ready to review"
	}
	return message
}

// --- the settings route -----------------------------------------------------------

func (w *Work) mountSelfRepair(p *part.Part) {
	p.Handle("GET /v1/settings/self-repair", func(rw http.ResponseWriter, _ *http.Request) {
		part.JSON(rw, http.StatusOK, w.SelfRepair())
	})
	p.Handle("PATCH /v1/settings/self-repair", func(rw http.ResponseWriter, r *http.Request) {
		if part.Caller(r) != part.Owner {
			part.Error(rw, http.StatusForbidden, "only the owner changes self-repair")
			return
		}
		var body struct {
			On       *bool `json:"on"`
			PerNight *int  `json:"perNight"`
		}
		if err := part.Decode(r, &body); err != nil || (body.On == nil && body.PerNight == nil) || (body.PerNight != nil && (*body.PerNight < 0 || *body.PerNight > MaxRepairsPerNight)) {
			part.Error(rw, http.StatusBadRequest, fmt.Sprintf("send `on` and/or `perNight` (0 to %d)", MaxRepairsPerNight))
			return
		}
		settings := w.SelfRepair()
		if body.On != nil {
			settings.On = *body.On
		}
		if body.PerNight != nil {
			settings.PerNight = *body.PerNight
		}
		if err := w.Store.SetSetting("self-repair", settings); err != nil {
			fail(rw, err)
			return
		}
		part.JSON(rw, http.StatusOK, settings)
	})
}
