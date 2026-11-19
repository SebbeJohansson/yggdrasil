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
//	GET    /v1/reactions    what Huginn did lately
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
	"yggdrasil/internal/part"
	"yggdrasil/internal/sse"
	"yggdrasil/internal/tell"
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
	// Organizer (calendar.*) and Key (alert.*) are what learned rules filter on (learn.go).
	Organizer string `json:"organizer,omitempty"`
	Key       string `json:"key,omitempty"`
}

// Action is what a rule does: notify, ask or alert-impact.
type Action struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
	// Link is a dashboard page the message links to instead of the event's own link.
	Link string `json:"link,omitempty"`
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
var Events = []string{"inbox.arrived", "draft.ready", "calendar.soon", "calendar.added", "calendar.moved", "calendar.removed", "calendar.ended", "calendar.duplicate", "calendar.stale", "alert.opened", "alert.escalated", "alert.resolved", "linear.todo", "savepoint.ready"}

var (
	yes      = true
	briefAsk = `Brief me for my meeting "{subject}" at {start}{organizerWith}: what is open between me and the people invited, from my notes, inbox and memory, and what I should bring.{invited}{also}`
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
		{ID: "meeting-prep", Name: "Meeting in the next half hour", On: Trigger{Event: "calendar.soon"},
			Action: Action{Kind: "ask", Text: briefAsk}, Deliver: both, Hold: true},
		{ID: "meeting-moved", Name: "Meeting moved into the next hour", On: Trigger{Event: "calendar.moved", StartsWithinMinutes: 60},
			Action: Action{Kind: "ask", Text: briefAsk}, Deliver: both, Hold: true},
		{ID: "alert-impact", Name: "Critical alert", On: Trigger{Event: "alert.opened", Severity: []string{"critical"}},
			Action: Action{Kind: "alert-impact"}, Deliver: both},
		{ID: "calendar-duplicate", Name: "Your copy of a meeting has an invite now", On: Trigger{Event: "calendar.duplicate"},
			Action: Action{Kind: "notify", Text: `{inviteOrganizer} sent an invite for "{inviteSubject}". Your own "{subject}" on {day} at {start} is a duplicate: delete it?`}, Deliver: []string{"widget"}, Hold: true},
		{ID: "calendar-stale", Name: "The invite your copy matches was cancelled", On: Trigger{Event: "calendar.stale"},
			Action: Action{Kind: "notify", Text: `{inviteOrganizer} cancelled "{inviteSubject}", but your own "{subject}" on {day} at {start} is still there: delete it?`}, Deliver: []string{"widget"}, Hold: true},
		{ID: "linear-todo", Name: "Linear ticket with no goal", On: Trigger{Event: "linear.todo"},
			Action: Action{Kind: "notify", Text: "{key} is in {state} and no goal has it: {title}. Add it as a goal?"}, Deliver: both, Hold: true},
		savePointRule,
	}
}

// Huginn is the running service.
type Huginn struct {
	Store   *Store
	Norn    *call.Part
	Sindri  *call.Part
	Gjallar *alert.Client
	// Tell carries reactions to you (Hermóðr).
	Tell tell.Teller
	Zone *time.Location
	// DashboardURL is where a suggestion links to (the Goals page).
	DashboardURL string
	// Streams are Norn's and Gjallar's; nil in tests.
	Streams []*sse.Stream

	now     func() time.Time
	logf    func(string, ...any)
	mu      sync.Mutex
	started bool
	alerts  map[int64]alertSeen
	told    map[string]string
	raised  map[string]bool
	// linearFails is how many times in a row Norn could not judge each Linear ticket.
	linearFails map[string]int
}

type alertSeen struct {
	State    string
	Severity string
}

// New makes Huginn and writes the built-in rules into the store.
func New(ctx context.Context, store *Store, norn, sindri *call.Part, gjallar *alert.Client, teller tell.Teller, zone *time.Location, logf func(string, ...any)) (*Huginn, error) {
	h := &Huginn{Store: store, Norn: norn, Sindri: sindri, Gjallar: gjallar, Tell: teller, Zone: zone, now: time.Now, logf: logf,
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
	p.Loop("forget", time.Hour, h.forget)
	p.Loop("linear", LinearEvery, h.watchLinear)
	h.mountLearning(p)
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
	if err := validateLink(rule.Action.Link); err != nil {
		return err
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
	if state != "" && !slices.Contains([]string{"delivered", "failed", "skipped"}, state) {
		part.Error(w, http.StatusBadRequest, "state must be delivered, failed or skipped")
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
	ID        string   `json:"id"`
	Subject   string   `json:"subject"`
	Start     int64    `json:"start"`
	End       int64    `json:"end"`
	AllDay    bool     `json:"allDay"`
	Location  string   `json:"location"`
	Organizer string   `json:"organizer"`
	Response  string   `json:"response"`
	ShowAs    string   `json:"showAs"`
	Attendees []string `json:"attendees"`
}

// CalendarChange is Norn's calendar.* event.
type CalendarChange struct {
	Calendar      string        `json:"calendar"`
	Event         CalendarEvent `json:"event"`
	PreviousStart int64         `json:"previousStart"`
	// Prompt is what you add to this meeting's brief, and Model what writes
	// it ("claude" asks a Claude agent), kept per series.
	Prompt string `json:"prompt"`
	Model  string `json:"model"`
	// Invite is the invite that covers your copy, on calendar.duplicate and .stale.
	Invite *CalendarEvent `json:"invite"`
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
	case event.Kind == "savepoint.ready":
		return h.fromSavePoint(event)
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
	case event.Kind == "draft.ready":
		// The same fields as inbox.arrived, so a rule filters it the same way.
		var item InboxArrived
		if json.Unmarshal([]byte(event.Data), &item) != nil {
			return Fired{}, false
		}
		var draft struct {
			Tone string `json:"tone"`
		}
		_ = json.Unmarshal([]byte(event.Data), &draft)
		relation := ""
		if item.Relation != "" {
			relation = " (" + item.Relation + ")"
		}
		link := item.Link
		if h.DashboardURL != "" {
			link = strings.TrimRight(h.DashboardURL, "/") + "/dashboard/inbox"
		}
		return Fired{Kind: event.Kind, Key: "draft:" + item.ID, At: time.UnixMilli(item.At), Title: "Reply drafted to " + item.From + relation,
			Link: link, Urgent: item.Band == "urgent", Inbox: &item,
			Fields: map[string]string{"from": item.From, "source": item.Source, "relation": item.Relation, "band": item.Band, "tone": draft.Tone, "link": link}}, true
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
		fields["also"], fields["invited"] = "", ""
		if people := meetingPeople(meeting); len(people) > 0 {
			fields["invited"] = " Invited: " + strings.Join(people[:min(12, len(people))], ", ") + "."
		}
		if change.Prompt != "" {
			fields["also"], fields["prompt"] = " Also: "+change.Prompt, change.Prompt
		}
		if change.PreviousStart != 0 {
			fields["previousStart"] = h.clock(change.PreviousStart)
		}
		link := ""
		if invite := change.Invite; invite != nil {
			fields["inviteSubject"], fields["inviteOrganizer"] = strings.TrimSpace(invite.Subject), invite.Organizer
			fields["day"] = time.UnixMilli(meeting.Start).In(h.Zone).Format("Mon 2 Jan")
			// The overview, where its card deletes your copy or keeps it.
			if h.DashboardURL != "" && h.DashboardURL != "off" {
				link = strings.TrimRight(h.DashboardURL, "/") + "/"
			}
		}
		// A meeting coming up, whether just added, moved or about to start,
		// is one thing to tell: the second rule to brief on it stays quiet.
		key := event.Kind + ":" + meeting.ID + fmt.Sprint(meeting.Start)
		if event.Kind == "calendar.added" || event.Kind == "calendar.moved" || event.Kind == "calendar.soon" {
			key = "meeting:" + meeting.ID + fmt.Sprint(meeting.Start)
		}
		return Fired{Kind: event.Kind, Key: key, At: at, Link: link,
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
	return learnedFilters(t, f)
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
	h.see(ctx, f)
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
		Link: h.linkOf(rule, f), Deliver: rule.Deliver, Urgent: f.Urgent, At: now}
	switch first, ok := h.claim(f, rule); {
	case now.Sub(f.At) > TooOld:
		reaction.State, reaction.Reason = "skipped", "the event is more than an hour old"
	case !ok:
		reaction.State, reaction.Reason = "skipped", "already told by "+first
	default:
		text, err := h.content(ctx, rule, f)
		reaction.Text = text
		if err != nil {
			reaction.State, reaction.Reason = "failed", err.Error()
		} else {
			h.deliver(ctx, rule, f, &reaction)
		}
	}
	h.record(ctx, rule, reaction)
}

func (h *Huginn) content(ctx context.Context, rule Rule, f Fired) (string, error) {
	switch rule.Action.Kind {
	case "notify":
		return Fill(rule.Action.Text, f.Fields), nil
	case "ask":
		if f.Change != nil && f.Change.Model == "claude" {
			return h.claudeBrief(ctx, Fill(rule.Action.Text, f.Fields), f.Change.Event)
		}
		return h.Norn.Ask(ctx, Fill(rule.Action.Text, f.Fields), AskSession)
	case "alert-impact":
		return h.impact(ctx, f), nil
	}
	return "", fmt.Errorf("unknown action %q", rule.Action.Kind)
}

// Priority is how loudly Hermóðr should tell you: urgent for an urgent message
// or a critical alert, high for a message from above you, normal otherwise.
func Priority(f Fired) string {
	switch {
	case f.Urgent:
		return "urgent"
	case f.Inbox != nil && slices.Contains([]string{"manager", "above", "leadership"}, f.Inbox.Relation):
		return "high"
	}
	return "normal"
}

// deliver hands the reaction to Hermóðr, which holds it through a meeting
// when the rule says so, and carries it to your channels.
func (h *Huginn) deliver(ctx context.Context, rule Rule, f Fired, reaction *Reaction) {
	if h.Tell == nil || !h.Tell.On() {
		reaction.State, reaction.Reason = "failed", "Hermóðr is not set up (YGG_HERMODR_URL, HERMODR_TOKEN)"
		return
	}
	push := slices.Contains(rule.Deliver, "phone")
	err := h.Tell.Tell(ctx, tell.Message{Title: reaction.Title, Text: reaction.Text, Link: reaction.Link,
		Priority: Priority(f), Hold: rule.Hold, Push: &push})
	if err != nil {
		reaction.State, reaction.Reason = "failed", err.Error()
		return
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

func (h *Huginn) forget(ctx context.Context) error {
	return h.Store.Forget(ctx, h.now().Add(-KeepReactions))
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
			// Norn keeps its last 500 events, so Huginn picks up after the last
			// one it handled, even across its own restart. Gjallar starts fresh.
			resumes := stream.Part == "norn"
			if stream.Part == "gjallar" {
				from = h.FromGjallar
			}
			if resumes {
				if id, err := h.Store.Mark(ctx, stream.Part); err == nil && id != "" {
					stream.Resume(id)
				}
			}
			go stream.Follow(ctx, func(event sse.Event) {
				if fired, ok := from(event); ok {
					h.Handle(ctx, fired)
				}
				if resumes && event.ID != "" {
					if err := h.Store.SetMark(ctx, stream.Part, event.ID); err != nil {
						h.logf("keeping where the %s stream is: %v", stream.Part, err)
					}
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
	{"xestrata", "Norn's answers, summaries and the inbox judge"},
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
