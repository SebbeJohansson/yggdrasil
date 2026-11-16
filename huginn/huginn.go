// Package huginn reacts to what just happened, so you hear about it without
// asking: an important message, a meeting that just landed in the next hour,
// an alert that stops your agents. It follows Norn's and Gjallar's event
// streams and runs each rule whose trigger matches. It suggests and tells,
// and never acts for you. Time-based jobs belong to Dellingr.
//
//	GET    /v1/status       each stream, connected or not
//	GET    /v1/rules        every rule, the built-in ones first
//	POST   /v1/rules        a new rule; disabled unless the owner made it
//	PATCH  /v1/rules/{id}   change it; enabling is owner only
//	DELETE /v1/rules/{id}   not a built-in one
//	GET    /v1/reactions    what Huginn did lately, held ones included
package huginn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
	"yggdrasil/internal/ntfy"
	"yggdrasil/internal/part"
	"yggdrasil/internal/sse"
)

// Name is this part's name.
const Name = "huginn"

const (
	// TooOld is when an event is recorded but no longer delivered.
	TooOld = time.Hour
	// KeepReactions is how long reactions are listed.
	KeepReactions = 7 * 24 * time.Hour
	// StreamDownAlert is how long a stream may be down before it is raised.
	StreamDownAlert = 5 * time.Minute
	// AskSession is the Norn conversation Huginn's questions go into.
	AskSession = "huginn"
)

// Trigger is which event, and which of them. Every filter given must match.
type Trigger struct {
	Event               string   `json:"event"`
	Relation            []string `json:"relation,omitempty"`
	Band                []string `json:"band,omitempty"`
	Direct              *bool    `json:"direct,omitempty"`
	MentionsMe          *bool    `json:"mentionsMe,omitempty"`
	StartsWithinMinutes int      `json:"startsWithinMinutes,omitempty"`
	Calendar            string   `json:"calendar,omitempty"`
	Severity            []string `json:"severity,omitempty"`
	Source              string   `json:"source,omitempty"`
}

// Action is what a rule does: notify, ask or alert-impact.
type Action struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
}

// Rule is a trigger and what to do about it.
type Rule struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	On          Trigger    `json:"on"`
	Action      Action     `json:"action"`
	Deliver     []string   `json:"deliver"`
	Hold        bool       `json:"hold"`
	Enabled     bool       `json:"enabled"`
	BuiltIn     bool       `json:"builtIn"`
	CreatedBy   string     `json:"createdBy"`
	LastFiredAt *time.Time `json:"lastFiredAt,omitempty"`
	LastResult  string     `json:"lastResult,omitempty"`
}

// Reaction is one rule firing on one event.
type Reaction struct {
	ID          string     `json:"id"`
	RuleID      string     `json:"ruleId"`
	RuleName    string     `json:"ruleName"`
	Event       string     `json:"event"`
	EventAt     time.Time  `json:"eventAt"`
	State       string     `json:"state"`
	Title       string     `json:"title,omitempty"`
	Text        string     `json:"text,omitempty"`
	Link        string     `json:"link,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	Deliver     []string   `json:"deliver"`
	Urgent      bool       `json:"urgent,omitempty"`
	At          time.Time  `json:"at"`
	DeliveredAt *time.Time `json:"deliveredAt,omitempty"`
}

// Events are the events a trigger can name.
var Events = []string{"inbox.arrived", "calendar.added", "calendar.moved", "calendar.removed", "calendar.ended", "alert.opened", "alert.escalated", "alert.resolved"}

var (
	yes      = true
	briefAsk = `Brief me for my meeting "{subject}" at {start}{organizerWith}: who will be there, what is open with them in my inbox and memory, and what I should bring. At most five short lines.`
)

// BuiltIn is the rules Huginn ships with. They can be switched off, never
// changed or removed.
func BuiltIn() []Rule {
	both := []string{"phone", "widget"}
	return []Rule{
		{ID: "important-message", Name: "Message from someone above you", On: Trigger{Event: "inbox.arrived", Relation: []string{"manager", "above", "leadership"}},
			Action: Action{Kind: "notify", Text: "{from} on {source}: {summary}"}, Deliver: both},
		{ID: "urgent-message", Name: "Urgent message", On: Trigger{Event: "inbox.arrived", Band: []string{"urgent"}, Direct: &yes},
			Action: Action{Kind: "notify", Text: "{from} on {source}: {summary}"}, Deliver: both},
		{ID: "meeting-prep", Name: "Meeting in the next hour", On: Trigger{Event: "calendar.added", StartsWithinMinutes: 60},
			Action: Action{Kind: "ask", Text: briefAsk}, Deliver: both, Hold: true},
		{ID: "meeting-moved", Name: "Meeting moved into the next hour", On: Trigger{Event: "calendar.moved", StartsWithinMinutes: 60},
			Action: Action{Kind: "ask", Text: briefAsk}, Deliver: both, Hold: true},
		{ID: "alert-impact", Name: "Critical alert", On: Trigger{Event: "alert.opened", Severity: []string{"critical"}},
			Action: Action{Kind: "alert-impact"}, Deliver: both},
	}
}

// Huginn is the running service.
type Huginn struct {
	Store   *Store
	Norn    *call.Part
	Sindri  *call.Part
	Gjallar *alert.Client
	Push    *ntfy.Topic
	Zone    *time.Location
	// Streams are Norn's and Gjallar's; nil in tests.
	Streams []*sse.Stream

	now     func() time.Time
	logf    func(string, ...any)
	mu      sync.Mutex
	started bool
	// meetings are your calendars as last read from Norn.
	meetings []meeting
	alerts   map[int64]alertSeen
	told     map[string]string
	raised   map[string]bool
}

type meeting struct {
	ID         string
	Start, End int64
	Busy       bool
}

type alertSeen struct {
	State    string
	Severity string
}

// New makes Huginn and writes the built-in rules into the store.
func New(ctx context.Context, store *Store, norn, sindri *call.Part, gjallar *alert.Client, push *ntfy.Topic, zone *time.Location, logf func(string, ...any)) (*Huginn, error) {
	h := &Huginn{Store: store, Norn: norn, Sindri: sindri, Gjallar: gjallar, Push: push, Zone: zone, now: time.Now, logf: logf,
		alerts: map[int64]alertSeen{}, told: map[string]string{}, raised: map[string]bool{}}
	kept, err := store.Rules(ctx)
	if err != nil {
		return nil, err
	}
	for _, rule := range BuiltIn() {
		rule.BuiltIn, rule.CreatedBy, rule.Enabled = true, part.Owner, true
		for _, old := range kept {
			if old.ID == rule.ID {
				rule.Enabled, rule.LastFiredAt, rule.LastResult = old.Enabled, old.LastFiredAt, old.LastResult
			}
		}
		if err := store.PutRule(ctx, rule); err != nil {
			return nil, err
		}
	}
	return h, nil
}

// Mount adds the routes and loops.
func (h *Huginn) Mount(p *part.Part) {
	p.Handle("GET /v1/status", h.status)
	p.Handle("GET /v1/rules", h.listRules)
	p.Handle("POST /v1/rules", h.createRule)
	p.Handle("PATCH /v1/rules/{id}", h.changeRule)
	p.Handle("DELETE /v1/rules/{id}", h.deleteRule)
	p.Handle("GET /v1/reactions", h.reactions)
	p.Loop("streams", 30*time.Second, h.watchStreams)
	p.Loop("meetings", 2*time.Minute, h.readMeetings)
	p.Loop("deliver", 30*time.Second, h.releaseHeld)
}

// --- routes ---

func (h *Huginn) status(w http.ResponseWriter, _ *http.Request) {
	streams := []sse.Status{}
	for _, stream := range h.Streams {
		streams = append(streams, stream.Status())
	}
	part.JSON(w, http.StatusOK, map[string]any{"streams": streams})
}

func (h *Huginn) sortedRules(ctx context.Context) ([]Rule, error) {
	rules, err := h.Store.Rules(ctx)
	if err != nil {
		return nil, err
	}
	order := map[string]int{}
	for i, rule := range BuiltIn() {
		order[rule.ID] = i
	}
	sort.SliceStable(rules, func(i, j int) bool {
		a, b := rules[i], rules[j]
		if a.BuiltIn != b.BuiltIn {
			return a.BuiltIn
		}
		if a.BuiltIn {
			return order[a.ID] < order[b.ID]
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	if rules == nil {
		rules = []Rule{}
	}
	return rules, nil
}

func (h *Huginn) listRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.sortedRules(r.Context())
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"rules": rules})
}

type ruleInput struct {
	Name    *string   `json:"name"`
	On      *Trigger  `json:"on"`
	Action  *Action   `json:"action"`
	Deliver *[]string `json:"deliver"`
	Hold    *bool     `json:"hold"`
	Enabled *bool     `json:"enabled"`
}

func (in ruleInput) apply(rule *Rule) {
	if in.Name != nil {
		rule.Name = strings.TrimSpace(*in.Name)
	}
	if in.On != nil {
		rule.On = *in.On
	}
	if in.Action != nil {
		rule.Action = *in.Action
	}
	if in.Deliver != nil {
		rule.Deliver = *in.Deliver
	}
	if in.Hold != nil {
		rule.Hold = *in.Hold
	}
}

// Validate says what is wrong with a rule, or nil.
func Validate(rule Rule) error {
	switch {
	case rule.Name == "" || len([]rune(rule.Name)) > 100:
		return errors.New("name must be 1 to 100 characters")
	case !slices.Contains(Events, rule.On.Event):
		return fmt.Errorf("on.event must be one of %s", strings.Join(Events, ", "))
	case rule.On.StartsWithinMinutes < 0 || rule.On.StartsWithinMinutes > 1440:
		return errors.New("on.startsWithinMinutes must be 1 to 1440")
	case len(rule.Action.Text) > 2000:
		return errors.New("action.text must be at most 2000 characters")
	}
	switch rule.Action.Kind {
	case "notify", "ask":
		if strings.TrimSpace(rule.Action.Text) == "" {
			return fmt.Errorf("a %s action needs text", rule.Action.Kind)
		}
	case "alert-impact":
		if !strings.HasPrefix(rule.On.Event, "alert.") {
			return errors.New("alert-impact is for alert events only")
		}
	default:
		return errors.New("action.kind must be notify, ask or alert-impact")
	}
	for _, where := range rule.Deliver {
		if where != "phone" && where != "widget" {
			return errors.New("deliver takes phone and widget")
		}
	}
	return nil
}

func (h *Huginn) createRule(w http.ResponseWriter, r *http.Request) {
	var in ruleInput
	if err := part.Decode(r, &in); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	caller := part.Caller(r)
	rule := Rule{ID: newID(), Deliver: []string{"phone", "widget"}, Hold: true, CreatedBy: caller, Enabled: caller == part.Owner}
	in.apply(&rule)
	if err := Validate(rule); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.Store.PutRule(r.Context(), rule); err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusCreated, rule)
}

func (h *Huginn) rule(ctx context.Context, id string) (Rule, error) {
	rules, err := h.Store.Rules(ctx)
	if err != nil {
		return Rule{}, err
	}
	for _, rule := range rules {
		if rule.ID == id {
			return rule, nil
		}
	}
	return Rule{}, ErrNotFound
}

func (h *Huginn) mayChange(w http.ResponseWriter, r *http.Request) (Rule, bool) {
	rule, err := h.rule(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, ErrNotFound):
		part.Error(w, http.StatusNotFound, "no such rule")
		return rule, false
	case err != nil:
		part.Error(w, http.StatusInternalServerError, err.Error())
		return rule, false
	}
	if caller := part.Caller(r); caller != part.Owner && caller != rule.CreatedBy {
		part.Error(w, http.StatusForbidden, "only the owner may change a rule another made")
		return rule, false
	}
	return rule, true
}

func (h *Huginn) changeRule(w http.ResponseWriter, r *http.Request) {
	rule, ok := h.mayChange(w, r)
	if !ok {
		return
	}
	var in ruleInput
	if err := part.Decode(r, &in); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if rule.BuiltIn && (in.Name != nil || in.On != nil || in.Action != nil || in.Deliver != nil || in.Hold != nil) {
		part.Error(w, http.StatusForbidden, "a built-in rule can only be switched on or off")
		return
	}
	if in.Enabled != nil && *in.Enabled && part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only the owner may enable a rule")
		return
	}
	in.apply(&rule)
	if in.Enabled != nil {
		rule.Enabled = *in.Enabled
	}
	if err := Validate(rule); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.Store.PutRule(r.Context(), rule); err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, rule)
}

func (h *Huginn) deleteRule(w http.ResponseWriter, r *http.Request) {
	rule, ok := h.mayChange(w, r)
	if !ok {
		return
	}
	if rule.BuiltIn {
		part.Error(w, http.StatusForbidden, "a built-in rule can't be removed, only switched off")
		return
	}
	if err := h.Store.DeleteRule(r.Context(), rule.ID); err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Huginn) reactions(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state != "" && !slices.Contains([]string{"held", "delivered", "failed", "skipped"}, state) {
		part.Error(w, http.StatusBadRequest, "state must be held, delivered, failed or skipped")
		return
	}
	reactions, err := h.Store.Reactions(r.Context(), state, 200)
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"reactions": reactions})
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// --- events ---

// Fired is one event as the rules see it.
type Fired struct {
	Kind   string
	Key    string
	At     time.Time
	Title  string
	Link   string
	Urgent bool
	Fields map[string]string
	Inbox  *InboxArrived
	Change *CalendarChange
	Alert  *Alert
}

// InboxArrived is Norn's inbox.arrived.
type InboxArrived struct {
	ID         string   `json:"id"`
	Source     string   `json:"source"`
	From       string   `json:"from"`
	Relation   string   `json:"relation"`
	Band       string   `json:"band"`
	Reasons    []string `json:"reasons"`
	Direct     bool     `json:"direct"`
	MentionsMe bool     `json:"mentionsMe"`
	Summary    string   `json:"summary"`
	Link       string   `json:"link"`
	At         int64    `json:"at"`
}

// CalendarEvent is a meeting as Norn sends it.
type CalendarEvent struct {
	ID        string `json:"id"`
	Subject   string `json:"subject"`
	Start     int64  `json:"start"`
	End       int64  `json:"end"`
	AllDay    bool   `json:"allDay"`
	Location  string `json:"location"`
	Organizer string `json:"organizer"`
	Response  string `json:"response"`
	ShowAs    string `json:"showAs"`
}

// CalendarChange is Norn's calendar.* event.
type CalendarChange struct {
	Calendar      string        `json:"calendar"`
	Event         CalendarEvent `json:"event"`
	PreviousStart int64         `json:"previousStart"`
}

// Alert is Gjallar's alert, as its stream sends it.
type Alert struct {
	ID          int64     `json:"id"`
	Source      string    `json:"source"`
	Key         string    `json:"key"`
	Severity    string    `json:"severity"`
	Title       string    `json:"title"`
	Detail      string    `json:"detail"`
	Link        string    `json:"link"`
	State       string    `json:"state"`
	FirstSeenAt time.Time `json:"firstSeenAt"`
	LastSeenAt  time.Time `json:"lastSeenAt"`
	Muted       bool      `json:"muted"`
}

func (h *Huginn) clock(ms int64) string {
	return time.UnixMilli(ms).In(h.Zone).Format("15:04")
}

// FromNorn reads one of Norn's events; ok is false for one it doesn't know.
func (h *Huginn) FromNorn(event sse.Event) (Fired, bool) {
	now := h.now()
	switch {
	case event.Kind == "inbox.arrived":
		var item InboxArrived
		if json.Unmarshal([]byte(event.Data), &item) != nil {
			return Fired{}, false
		}
		relation := ""
		if item.Relation != "" {
			relation = " (" + item.Relation + ")"
		}
		return Fired{Kind: event.Kind, Key: "inbox:" + item.ID, At: time.UnixMilli(item.At), Title: item.From + relation,
			Link: item.Link, Urgent: item.Band == "urgent", Inbox: &item,
			Fields: map[string]string{"from": item.From, "source": item.Source, "summary": item.Summary, "relation": item.Relation, "band": item.Band, "link": item.Link}}, true
	case strings.HasPrefix(event.Kind, "calendar."):
		var change CalendarChange
		if json.Unmarshal([]byte(event.Data), &change) != nil {
			return Fired{}, false
		}
		meeting := change.Event
		at := now
		if event.Kind == "calendar.ended" {
			at = time.UnixMilli(meeting.End)
		}
		with := ""
		if meeting.Organizer != "" {
			with = ", organized by " + meeting.Organizer
		}
		fields := map[string]string{"subject": meeting.Subject, "start": h.clock(meeting.Start), "end": h.clock(meeting.End),
			"organizer": meeting.Organizer, "organizerWith": with, "location": meeting.Location, "calendar": change.Calendar}
		if change.PreviousStart != 0 {
			fields["previousStart"] = h.clock(change.PreviousStart)
		}
		return Fired{Kind: event.Kind, Key: event.Kind + ":" + meeting.ID + fmt.Sprint(meeting.Start), At: at,
			Title: meeting.Subject + " at " + h.clock(meeting.Start), Change: &change, Fields: fields}, true
	}
	return Fired{}, false
}

func rank(severity string) int {
	return alert.Severity(severity).Rank()
}

// FromGjallar turns one of Gjallar's alert changes into opened, escalated or
// resolved, by what Huginn saw of that alert before.
func (h *Huginn) FromGjallar(event sse.Event) (Fired, bool) {
	var a Alert
	if event.Kind != "alert" || json.Unmarshal([]byte(event.Data), &a) != nil {
		return Fired{}, false
	}
	h.mu.Lock()
	before, known := h.alerts[a.ID]
	h.alerts[a.ID] = alertSeen{State: a.State, Severity: a.Severity}
	h.mu.Unlock()
	open := a.State == "open" || a.State == "acknowledged"
	kind := ""
	switch {
	case a.Muted:
	case !known:
		if a.State == "open" && h.now().Sub(a.FirstSeenAt) < 5*time.Minute {
			kind = "alert.opened"
		}
	case before.State == "resolved" && a.State == "open":
		kind = "alert.opened"
	case open && rank(a.Severity) > rank(before.Severity):
		kind = "alert.escalated"
	case a.State == "resolved" && before.State != "resolved":
		kind = "alert.resolved"
	}
	if kind == "" {
		return Fired{}, false
	}
	return Fired{Kind: kind, Key: fmt.Sprintf("%s:%d:%s", kind, a.ID, a.LastSeenAt.Format(time.RFC3339)), At: a.LastSeenAt, Title: a.Title,
		Link: a.Link, Urgent: a.Severity == "critical", Alert: &a,
		Fields: map[string]string{"title": a.Title, "detail": a.Detail, "severity": a.Severity, "source": a.Source, "key": a.Key, "link": a.Link}}, true
}

// Matches reports whether the trigger takes the event.
func (t Trigger) Matches(f Fired, now time.Time) bool {
	if t.Event != f.Kind {
		return false
	}
	if item := f.Inbox; item != nil {
		if (len(t.Relation) > 0 && !slices.Contains(t.Relation, item.Relation)) ||
			(len(t.Band) > 0 && !slices.Contains(t.Band, item.Band)) ||
			(t.Direct != nil && *t.Direct != item.Direct) ||
			(t.MentionsMe != nil && *t.MentionsMe != item.MentionsMe) {
			return false
		}
	}
	if change := f.Change; change != nil {
		if t.Calendar != "" && t.Calendar != change.Calendar {
			return false
		}
		if t.StartsWithinMinutes > 0 {
			until := time.UnixMilli(change.Event.Start).Sub(now)
			if until <= 0 || until > time.Duration(t.StartsWithinMinutes)*time.Minute {
				return false
			}
		}
	}
	if a := f.Alert; a != nil {
		if (len(t.Severity) > 0 && !slices.Contains(t.Severity, a.Severity)) || (t.Source != "" && t.Source != a.Source) {
			return false
		}
	}
	return true
}

var placeholder = regexp.MustCompile(`\{(\w+)\}`)

// Fill puts the event's fields into the text; an unknown one is left empty.
func Fill(text string, fields map[string]string) string {
	return placeholder.ReplaceAllStringFunc(text, func(match string) string {
		return fields[match[1:len(match)-1]]
	})
}

// Handle runs every enabled rule the event matches. The rules' work runs in
// the background, so a slow answer never holds up the stream.
func (h *Huginn) Handle(ctx context.Context, f Fired) {
	if f.Kind == "calendar.ended" && f.Change != nil {
		h.endMeeting(f.Change.Event.ID)
	}
	rules, err := h.Store.Rules(ctx)
	if err != nil {
		h.logf("reading the rules: %v", err)
		return
	}
	now := h.now()
	var wg sync.WaitGroup
	for _, rule := range rules {
		if !rule.Enabled || !rule.On.Matches(f, now) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.react(ctx, rule, f)
		}()
	}
	wg.Wait()
	if f.Kind == "calendar.ended" {
		h.releaseHeld(ctx)
	}
}

// claim reports whether this rule is the first to tell you about the event
// this way; the second rule matching the same message stays quiet.
func (h *Huginn) claim(f Fired, rule Rule) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := f.Key + "|" + rule.Action.Kind
	if first, ok := h.told[key]; ok {
		return first, false
	}
	if len(h.told) > 5000 {
		h.told = map[string]string{}
	}
	h.told[key] = rule.Name
	return "", true
}

func (h *Huginn) react(ctx context.Context, rule Rule, f Fired) {
	now := h.now()
	reaction := Reaction{ID: newID(), RuleID: rule.ID, RuleName: rule.Name, Event: f.Kind, EventAt: f.At, Title: f.Title,
		Link: f.Link, Deliver: rule.Deliver, Urgent: f.Urgent, At: now}
	switch first, ok := h.claim(f, rule); {
	case now.Sub(f.At) > TooOld:
		reaction.State, reaction.Reason = "skipped", "the event is more than an hour old"
	case !ok:
		reaction.State, reaction.Reason = "skipped", "already told by "+first
	default:
		text, err := h.content(ctx, rule, f)
		reaction.Text = text
		switch {
		case err != nil:
			reaction.State, reaction.Reason = "failed", err.Error()
		case rule.Hold && !f.Urgent:
			if until, busy := h.inMeeting(h.now()); busy {
				reaction.State, reaction.Reason = "held", "in a meeting until "+h.clock(until)
			}
		}
	}
	if reaction.State == "" {
		h.deliver(ctx, &reaction)
	}
	h.record(ctx, rule, reaction)
}

func (h *Huginn) content(ctx context.Context, rule Rule, f Fired) (string, error) {
	switch rule.Action.Kind {
	case "notify":
		return Fill(rule.Action.Text, f.Fields), nil
	case "ask":
		return h.Norn.Ask(ctx, Fill(rule.Action.Text, f.Fields), AskSession)
	case "alert-impact":
		return h.impact(ctx, f), nil
	}
	return "", fmt.Errorf("unknown action %q", rule.Action.Kind)
}

func (h *Huginn) deliver(ctx context.Context, reaction *Reaction) {
	if slices.Contains(reaction.Deliver, "phone") {
		priority := 3
		if reaction.Urgent {
			priority = 4
		}
		if err := h.Push.Push(ctx, reaction.Title, reaction.Text, priority, reaction.Link); err != nil {
			reaction.State, reaction.Reason = "failed", "phone push: "+err.Error()
			return
		}
	}
	at := h.now()
	reaction.State, reaction.Reason, reaction.DeliveredAt = "delivered", "", &at
}

func (h *Huginn) record(ctx context.Context, rule Rule, reaction Reaction) {
	if err := h.Store.PutReaction(ctx, reaction); err != nil {
		h.logf("keeping a reaction: %v", err)
	}
	if current, err := h.rule(ctx, rule.ID); err == nil {
		at := reaction.At
		current.LastFiredAt, current.LastResult = &at, reaction.State
		_ = h.Store.PutRule(ctx, current)
	}
}

// --- meetings and held reactions ---

func (h *Huginn) inMeeting(now time.Time) (int64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	nowMs := now.UnixMilli()
	var until int64
	for _, m := range h.meetings {
		if m.Busy && m.Start <= nowMs && nowMs < m.End && m.End > until {
			until = m.End
		}
	}
	return until, until > 0
}

func (h *Huginn) endMeeting(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.meetings {
		if h.meetings[i].ID == id {
			h.meetings[i].Busy = false
		}
	}
}

// SetMeetings replaces what Huginn knows of your calendars.
func (h *Huginn) SetMeetings(events []CalendarEvent) {
	var meetings []meeting
	for _, event := range events {
		if event.AllDay || event.Response == "declined" {
			continue
		}
		meetings = append(meetings, meeting{ID: event.ID, Start: event.Start, End: event.End, Busy: event.ShowAs != "free"})
	}
	h.mu.Lock()
	h.meetings = meetings
	h.mu.Unlock()
}

func (h *Huginn) readMeetings(ctx context.Context) error {
	if !h.Norn.On() {
		return nil
	}
	var all []CalendarEvent
	var failed []string
	for _, path := range []string{"/v1/calendar", "/v1/calendar/personal"} {
		var calendar struct {
			Events []CalendarEvent `json:"events"`
		}
		if err := h.Norn.Do(ctx, http.MethodGet, path, nil, &calendar); err != nil {
			failed = append(failed, err.Error())
			continue
		}
		all = append(all, calendar.Events...)
	}
	if len(failed) == 2 {
		return errors.New(failed[0])
	}
	h.SetMeetings(all)
	return nil
}

func (h *Huginn) releaseHeld(ctx context.Context) error {
	if err := h.Store.Forget(ctx, h.now().Add(-KeepReactions)); err != nil {
		return err
	}
	if _, busy := h.inMeeting(h.now()); busy {
		return nil
	}
	held, err := h.Store.Reactions(ctx, "held", 200)
	if err != nil {
		return err
	}
	for _, reaction := range held {
		h.deliver(ctx, &reaction)
		if err := h.Store.PutReaction(ctx, reaction); err != nil {
			return err
		}
	}
	return nil
}

// --- streams ---

func (h *Huginn) watchStreams(ctx context.Context) error {
	h.mu.Lock()
	start := !h.started
	h.started = true
	h.mu.Unlock()
	if start {
		for _, stream := range h.Streams {
			from := h.FromNorn
			if stream.Part == "gjallar" {
				from = h.FromGjallar
			}
			go stream.Follow(ctx, func(event sse.Event) {
				if fired, ok := from(event); ok {
					h.Handle(ctx, fired)
				}
			})
		}
		return nil
	}
	var down []string
	for _, stream := range h.Streams {
		key := "stream:" + stream.Part
		gone := stream.DownFor(h.now())
		h.mu.Lock()
		raised := h.raised[key]
		h.mu.Unlock()
		switch {
		case gone >= StreamDownAlert && !raised:
			status := stream.Status()
			err := h.Gjallar.Raise(ctx, alert.Raise{Key: key, Severity: alert.Warning,
				Title: fmt.Sprintf("Huginn can't hear %s", stream.Part), Detail: status.Error})
			if err == nil {
				h.mu.Lock()
				h.raised[key] = true
				h.mu.Unlock()
			}
		case gone == 0 && raised:
			if err := h.Gjallar.Resolve(ctx, key); err == nil {
				h.mu.Lock()
				delete(h.raised, key)
				h.mu.Unlock()
			}
		}
		if gone > 0 {
			down = append(down, stream.Part)
		}
	}
	if len(down) > 0 {
		return fmt.Errorf("not connected to %s", strings.Join(down, ", "))
	}
	return nil
}

// --- alert impact ---

// Agent is the little of Sindri's agent Huginn reads.
type Agent struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Status string `json:"status"`
	Engine string `json:"engine"`
}

var affects = []struct{ word, what string }{
	{"anthropic", "Claude Code agents can't get answers"},
	{"github", "pushes, pull requests and checks"},
	{"ollama", "Norn's dispatch, asks and inbox summaries"},
	{"xestrata", "Norn's answers (it falls back to Ollama)"},
	{"laya", "the inbox judge and the tone badge"},
	{"microsoft-graph", "Outlook, Teams and the work calendar"},
	{"slack", "Slack in the inbox"},
	{"google", "Gmail and the personal calendar"},
	{"linear", "Linear goals and lookups"},
	{"down:norn", "the assistant, the inbox and calendar events"},
	{"down:sindri", "every agent"},
}

func (h *Huginn) impact(ctx context.Context, f Fired) string {
	lines := []string{}
	if f.Alert != nil {
		key := strings.ToLower(f.Alert.Key + " " + f.Alert.Title)
		for _, a := range affects {
			if strings.Contains(key, a.word) {
				lines = append(lines, "Affects: "+a.what+".")
				break
			}
		}
	}
	var agents []Agent
	if err := h.Sindri.Do(ctx, http.MethodGet, "/v1/agents", nil, &agents); err != nil {
		lines = append(lines, "Could not read Sindri's agents: "+err.Error())
		return strings.Join(lines, "\n")
	}
	var running []string
	for _, agent := range agents {
		if slices.Contains([]string{"starting", "working", "needs-approval", "waiting"}, agent.Status) {
			label := agent.Label
			if label == "" {
				label = agent.ID
			}
			running = append(running, label)
		}
	}
	switch len(running) {
	case 0:
		lines = append(lines, "No agents are running.")
	default:
		shown := running[:min(5, len(running))]
		more := ""
		if len(running) > len(shown) {
			more = fmt.Sprintf(" and %d more", len(running)-len(shown))
		}
		lines = append(lines, fmt.Sprintf("%d running: %s%s.", len(running), strings.Join(shown, ", "), more))
	}
	return strings.Join(lines, "\n")
}
