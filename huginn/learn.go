package huginn

// Huginn writes its own rules. The dashboard reports what you do
// (POST /v1/observations: a page opened, a draft copied, an alert seen,
// snoozed or muted), and Huginn already sees the events. What you do within a
// few minutes after an event becomes a pair: a meeting Anna organizes ends,
// and you open the meeting note. Five such pairs in 30 days, when you do it
// after at least 70 % of those events, become a suggestion: a ready rule that
// tells you, with a link to that page. The same alert quieted five times
// becomes a suggested Gjallar mute instead. You add it with one click or say
// no thanks, and a pattern you said no to is never suggested again.
//
// Only kinds and ids are kept: page names, an alert's source and key, an
// event's kind and the fields a rule filters on (relation, organizer,
// calendar). Never a message's text. Nothing leaves this machine but one
// quiet Hermóðr message a day at most, listed in the widget, never pushed.
//
//	POST  /v1/observations              what you just did (owner)
//	GET   /v1/suggestions               the ones waiting for you
//	POST  /v1/suggestions/{id}/accept   make the rule, or the mute (owner)
//	POST  /v1/suggestions/{id}/reject   never again for that pattern (owner)
//	GET   /v1/settings/learning         { on, windowMinutes }
//	PATCH /v1/settings/learning         change it (owner); off forgets what it saw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"yggdrasil/internal/call"
	"yggdrasil/internal/part"
	"yggdrasil/internal/tell"
)

const (
	// LearnFor is how far back pairs are counted, and how long observations and events are kept.
	LearnFor = 30 * 24 * time.Hour
	// LearnEvery is how often patterns are looked for.
	LearnEvery = 15 * time.Minute
	// MinPairs is how many times you must have done it before it is suggested.
	MinPairs = 5
	// FollowThrough is the share of those events you must have done it after.
	FollowThrough = 0.7
	// DefaultWindowMinutes is how soon after an event what you do counts.
	DefaultWindowMinutes = 10
	// QuietApart is how far apart two quiets of one alert must be to count twice.
	QuietApart = time.Hour
	// TellEvery is how often a new suggestion may be told through Hermóðr.
	TellEvery = 24 * time.Hour
)

// Observation is one thing you did on the dashboard.
type Observation struct {
	Action string    `json:"action"`
	Target string    `json:"target,omitempty"`
	At     time.Time `json:"at"`
}

// Seen is one event as learning keeps it: its kind and the fields a rule can
// filter on, nothing else.
type Seen struct {
	Key       string    `json:"key"`
	Kind      string    `json:"kind"`
	At        time.Time `json:"at"`
	Relation  string    `json:"relation,omitempty"`
	Organizer string    `json:"organizer,omitempty"`
	Calendar  string    `json:"calendar,omitempty"`
	Source    string    `json:"source,omitempty"`
	AlertKey  string    `json:"alertKey,omitempty"`
}

// LearnSetting is GET and PATCH /v1/settings/learning.
type LearnSetting struct {
	On            bool `json:"on"`
	WindowMinutes int  `json:"windowMinutes"`
}

// SuggestedRule is a suggestion's rule, in the shape POST /v1/rules takes.
type SuggestedRule struct {
	Name    string   `json:"name"`
	On      Trigger  `json:"on"`
	Action  Action   `json:"action"`
	Deliver []string `json:"deliver"`
	Hold    bool     `json:"hold"`
}

// Mute is a suggestion's Gjallar mute.
type Mute struct {
	Source string `json:"source"`
	Key    string `json:"key"`
}

// Suggestion is a rule or a mute Huginn offers from what it saw you do.
type Suggestion struct {
	ID        string         `json:"id"`
	Pattern   string         `json:"pattern"`
	Kind      string         `json:"kind"`
	Text      string         `json:"text"`
	Rule      *SuggestedRule `json:"rule,omitempty"`
	Mute      *Mute          `json:"mute,omitempty"`
	Times     int            `json:"times"`
	Of        int            `json:"of,omitempty"`
	State     string         `json:"state"`
	CreatedAt time.Time      `json:"createdAt"`
	ToldAt    *time.Time     `json:"toldAt,omitempty"`
	DecidedAt *time.Time     `json:"decidedAt,omitempty"`
	RuleID    string         `json:"ruleId,omitempty"`
}

// --- what counts ---

var (
	openAction  = regexp.MustCompile(`^open:(/[a-z0-9_-]+)*/?$`)
	quietAction = []string{"ack-alert", "snooze-alert", "mute-alert"}
	otherAction = []string{"copy-draft"}
	relations   = []string{"manager", "above", "leadership", "report", "peer"}
)

// PageOf is the page an open: action names, cut to /dashboard/<page>, so ids
// in a path are never kept. "" for one that isn't an open: action.
func PageOf(action string) string {
	path, ok := strings.CutPrefix(action, "open:")
	if !ok {
		return ""
	}
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) > 2 {
		segments = segments[:2]
	}
	return "/" + strings.Join(segments, "/")
}

// NormalAction is the action as it is kept, or why it is not one.
func NormalAction(action, target string) (string, string, error) {
	action = strings.TrimSpace(action)
	switch {
	case strings.HasPrefix(action, "open:"):
		if !openAction.MatchString(strings.ToLower(action)) || len(action) > 120 {
			return "", "", errors.New("open: takes a dashboard path, like open:/dashboard/meeting")
		}
		return "open:" + PageOf(strings.ToLower(action)), "", nil
	case slices.Contains(quietAction, action):
		source, key, ok := strings.Cut(target, ":")
		if !ok || source == "" || key == "" || len(target) > 200 || strings.ContainsAny(target, " \n\t") {
			return "", "", fmt.Errorf("%s takes target <source>:<key>", action)
		}
		return action, target, nil
	case slices.Contains(otherAction, action):
		return action, "", nil
	}
	return "", "", errors.New("action must be open:<path>, copy-draft, ack-alert, snooze-alert or mute-alert")
}

// Seeing is the event as learning keeps it; false for one it does not learn
// from (a message from someone with no known relation, a meeting with no
// organizer).
func Seeing(f Fired) (Seen, bool) {
	seen := Seen{Key: f.Kind + "|" + f.Key, Kind: f.Kind, At: f.At.UTC()}
	switch {
	case (f.Kind == "inbox.arrived" || f.Kind == "draft.ready") && f.Inbox != nil:
		if !slices.Contains(relations, f.Inbox.Relation) {
			return seen, false
		}
		seen.Relation = f.Inbox.Relation
	case (f.Kind == "calendar.soon" || f.Kind == "calendar.ended") && f.Change != nil:
		if f.Change.Event.Organizer == "" {
			return seen, false
		}
		seen.Organizer, seen.Calendar = f.Change.Event.Organizer, f.Change.Calendar
	case f.Kind == "alert.opened" && f.Alert != nil:
		seen.Source, seen.AlertKey = f.Alert.Source, f.Alert.Key
	default:
		return seen, false
	}
	return seen, true
}

// Signature is the event's kind and filter fields: events with the same one
// are the same kind of thing happening.
func (s Seen) Signature() string {
	parts := []string{s.Kind}
	for _, field := range [][2]string{{"relation", s.Relation}, {"organizer", strings.ToLower(s.Organizer)}, {"calendar", s.Calendar}, {"source", s.Source}, {"key", s.AlertKey}} {
		if field[1] != "" {
			parts = append(parts, field[0]+"="+field[1])
		}
	}
	return strings.Join(parts, "|")
}

// Trigger is the rule trigger that matches events like this one.
func (s Seen) Trigger() Trigger {
	t := Trigger{Event: s.Kind, Organizer: s.Organizer, Calendar: s.Calendar, Source: s.Source, Key: s.AlertKey}
	if s.Relation != "" {
		t.Relation = []string{s.Relation}
	}
	return t
}

// learnedFilters is the trigger's organizer and key, which Matches checks too.
func learnedFilters(t Trigger, f Fired) bool {
	if t.Organizer != "" && (f.Change == nil || !strings.EqualFold(t.Organizer, f.Change.Event.Organizer)) {
		return false
	}
	if t.Key != "" && (f.Alert == nil || t.Key != f.Alert.Key) {
		return false
	}
	return true
}

// --- patterns ---

// Pattern is something you do after a kind of event: how many times, of how
// many such events.
type Pattern struct {
	Seen   Seen
	Action string
	Times  int
	Of     int
}

// Key names the pattern, so a suggestion for it is made once.
func (p Pattern) Key() string {
	return p.Seen.Signature() + " -> " + p.Action
}

// Suggests reports whether the pattern is strong enough to suggest.
func (p Pattern) Suggests() bool {
	return p.Times >= MinPairs && float64(p.Times) >= FollowThrough*float64(p.Of)
}

// learnable is an action a rule can point you to.
func learnable(action string) bool {
	if page := PageOf(action); page != "" {
		// The overview is where you land anyway.
		return page != "/" && page != "/dashboard"
	}
	return slices.Contains(otherAction, action)
}

// Patterns pairs each event with what you did within window after it, and
// counts, per kind of event and action, the events you did it after and all
// such events. Events whose window has not closed yet are left out.
func Patterns(events []Seen, observations []Observation, window time.Duration, now time.Time) []Pattern {
	type pair struct{ signature, action string }
	of := map[string]int{}
	example := map[string]Seen{}
	times := map[pair]int{}
	for _, event := range events {
		if event.At.Add(window).After(now) {
			continue
		}
		signature := event.Signature()
		of[signature]++
		example[signature] = event
		did := map[string]bool{}
		for _, o := range observations {
			if o.At.After(event.At) && !o.At.After(event.At.Add(window)) && learnable(o.Action) && !did[o.Action] {
				did[o.Action] = true
				times[pair{signature, o.Action}]++
			}
		}
	}
	patterns := []Pattern{}
	for p, n := range times {
		patterns = append(patterns, Pattern{Seen: example[p.signature], Action: p.action, Times: n, Of: of[p.signature]})
	}
	sort.Slice(patterns, func(i, j int) bool { return patterns[i].Key() < patterns[j].Key() })
	return patterns
}

// Quieted is an alert you acknowledged, snoozed or muted again and again.
type Quieted struct {
	Source, Key string
	Times       int
}

// Quiets counts, per alert, the times you quieted it, at least QuietApart
// apart, so a double click counts once.
func Quiets(observations []Observation) []Quieted {
	last := map[string]time.Time{}
	times := map[string]int{}
	for _, o := range observations {
		if !slices.Contains(quietAction, o.Action) || o.Target == "" {
			continue
		}
		if at, ok := last[o.Target]; ok && o.At.Sub(at) < QuietApart {
			continue
		}
		last[o.Target] = o.At
		times[o.Target]++
	}
	list := []Quieted{}
	for target, n := range times {
		source, key, _ := strings.Cut(target, ":")
		list = append(list, Quieted{Source: source, Key: key, Times: n})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Source+list[i].Key < list[j].Source+list[j].Key })
	return list
}

// --- words ---

var pageWords = map[string]string{
	"/dashboard/meeting": "the meeting note", "/dashboard/inbox": "the inbox", "/dashboard/goals": "Goals",
	"/dashboard/agents": "Agents", "/dashboard/week": "the week", "/dashboard/focus": "Focus", "/dashboard/chat": "Chat",
	"/dashboard/review": "Review", "/dashboard/memory": "Memory", "/dashboard/parts": "Parts and alerts",
	"/dashboard/automations": "Automations", "/dashboard/activity": "Activity", "/dashboard/sessions": "Sessions",
}

// doing is what the action does, in a few words, and the page it links to.
func doing(action string) (string, string) {
	if action == "copy-draft" {
		return "copy the reply draft", "/dashboard/inbox"
	}
	page := PageOf(action)
	words, ok := pageWords[page]
	if !ok {
		words = strings.TrimPrefix(page, "/dashboard/")
	}
	return "open " + words, page
}

// happening is the event in words, for the suggestion, and the notification's
// text with the event's fields to fill in.
func happening(s Seen) (string, string) {
	switch s.Kind {
	case "calendar.ended":
		return "a meeting " + s.Organizer + " organized ends", "{subject} with {organizer} ended"
	case "calendar.soon":
		return "a meeting " + s.Organizer + " organized is about to start", "{subject} with {organizer} starts at {start}"
	case "inbox.arrived":
		return "a message from your " + s.Relation + " arrives", "{from} wrote on {source}"
	case "draft.ready":
		return "Norn drafts a reply to your " + s.Relation, "Reply drafted to {from}"
	case "alert.opened":
		return "the alert " + s.Source + ":" + s.AlertKey + " opens", "{title}"
	}
	return s.Kind, s.Kind
}

// RuleFor is the rule that would do it for you, and the suggestion's words.
func RuleFor(p Pattern) (SuggestedRule, string) {
	what, link := doing(p.Action)
	when, told := happening(p.Seen)
	name := clipText("After "+when+": "+what, 99)
	return SuggestedRule{
		Name:    name,
		On:      p.Seen.Trigger(),
		Action:  Action{Kind: "notify", Text: told + ": " + what + "?", Link: link},
		Deliver: []string{"widget"},
		Hold:    true,
	}, "When " + when + ", you usually " + what + ". Remind you then?"
}

func suggestionID(pattern string) string {
	sum := sha256.Sum256([]byte(pattern))
	return hex.EncodeToString(sum[:8])
}

// --- the loop ---

func (h *Huginn) learnSetting(ctx context.Context) LearnSetting {
	setting := LearnSetting{On: true, WindowMinutes: DefaultWindowMinutes}
	if _, err := h.Store.LearnState(ctx, "setting", &setting); err != nil {
		h.logf("reading the learning setting: %v", err)
	}
	return setting
}

// see keeps an event to learn from; called for every event Huginn handles.
func (h *Huginn) see(ctx context.Context, f Fired) {
	seen, ok := Seeing(f)
	if !ok || !h.learnSetting(ctx).On {
		return
	}
	if err := h.Store.See(ctx, seen); err != nil {
		h.logf("keeping an event to learn from: %v", err)
	}
}

// linkOf is where a reaction links: the rule's dashboard page when it has
// one, the event's own link otherwise.
func (h *Huginn) linkOf(rule Rule, f Fired) string {
	if rule.Action.Link != "" && h.DashboardURL != "" && h.DashboardURL != "off" {
		return strings.TrimRight(h.DashboardURL, "/") + rule.Action.Link
	}
	return f.Link
}

// validateLink checks a rule's dashboard link.
func validateLink(link string) error {
	if link != "" && (!strings.HasPrefix(link, "/") || len(link) > 200 || strings.ContainsAny(link, " \n\t")) {
		return errors.New("action.link is a dashboard path, like /dashboard/meeting")
	}
	return nil
}

func sameRule(rule Rule, suggested SuggestedRule) bool {
	a, _ := json.Marshal(rule.On)
	b, _ := json.Marshal(suggested.On)
	return string(a) == string(b) && rule.Action.Link == suggested.Action.Link
}

// learn looks for patterns, keeps a suggestion for each new strong one, and
// tells you of new ones, once a day at most.
func (h *Huginn) learn(ctx context.Context) error {
	now := h.now()
	if err := h.Store.ForgetLearning(ctx, now.Add(-LearnFor)); err != nil {
		return err
	}
	setting := h.learnSetting(ctx)
	if !setting.On {
		return nil
	}
	since := now.Add(-LearnFor)
	events, err := h.Store.SeenSince(ctx, since)
	if err != nil {
		return err
	}
	observations, err := h.Store.Observations(ctx, since)
	if err != nil {
		return err
	}
	kept, err := h.Store.Suggestions(ctx)
	if err != nil {
		return err
	}
	rules, err := h.Store.Rules(ctx)
	if err != nil {
		return err
	}
	byID := map[string]Suggestion{}
	for _, s := range kept {
		byID[s.ID] = s
	}
	var found []Suggestion
	for _, p := range Patterns(events, observations, time.Duration(setting.WindowMinutes)*time.Minute, now) {
		if !p.Suggests() {
			continue
		}
		rule, text := RuleFor(p)
		if slices.ContainsFunc(rules, func(r Rule) bool { return sameRule(r, rule) }) {
			continue
		}
		found = append(found, Suggestion{Pattern: p.Key(), Kind: "rule", Text: text, Rule: &rule, Times: p.Times, Of: p.Of})
	}
	for _, q := range Quiets(observations) {
		if q.Times < MinPairs {
			continue
		}
		target := q.Source + ":" + q.Key
		found = append(found, Suggestion{Pattern: "quiet " + target, Kind: "mute", Mute: &Mute{Source: q.Source, Key: q.Key}, Times: q.Times,
			Text: fmt.Sprintf("You keep quieting the alert %s. Mute it, so it is listed but never pushed?", target)})
	}
	for _, s := range found {
		s.ID = suggestionID(s.Pattern)
		old, known := byID[s.ID]
		switch {
		case known && old.State != "open":
			// Accepted or said no to: never again.
			continue
		case known:
			old.Times, old.Of = s.Times, s.Of
			s = old
		default:
			s.State, s.CreatedAt = "open", now
		}
		if err := h.Store.PutSuggestion(ctx, s); err != nil {
			return err
		}
		byID[s.ID] = s
	}
	return h.tellSuggestions(ctx, byID)
}

// tellSuggestions sends one quiet message through Hermóðr about a suggestion
// you have not heard of, at most once a day, listed in the widget and never
// pushed. Every suggestion open then counts as told.
func (h *Huginn) tellSuggestions(ctx context.Context, all map[string]Suggestion) error {
	var untold []Suggestion
	for _, s := range all {
		if s.State == "open" && s.ToldAt == nil {
			untold = append(untold, s)
		}
	}
	if len(untold) == 0 || h.Tell == nil || !h.Tell.On() {
		return nil
	}
	now := h.now()
	var last time.Time
	if _, err := h.Store.LearnState(ctx, "toldAt", &last); err != nil {
		return err
	}
	if now.Sub(last) < TellEvery {
		return nil
	}
	sort.Slice(untold, func(i, j int) bool { return untold[i].CreatedAt.Before(untold[j].CreatedAt) })
	link := ""
	if h.DashboardURL != "" && h.DashboardURL != "off" {
		link = strings.TrimRight(h.DashboardURL, "/") + "/dashboard/automations"
	}
	push := false
	if err := h.Tell.Tell(ctx, tell.Message{Title: "Huginn noticed something", Text: untold[0].Text, Link: link, Priority: "low", Hold: true, Push: &push}); err != nil {
		return err
	}
	if err := h.Store.SetLearnState(ctx, "toldAt", now); err != nil {
		return err
	}
	for _, s := range untold {
		at := now
		s.ToldAt = &at
		if err := h.Store.PutSuggestion(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// --- routes ---

func (h *Huginn) mountLearning(p *part.Part) {
	p.Handle("POST /v1/observations", h.observe)
	p.Handle("GET /v1/suggestions", h.listSuggestions)
	p.Handle("POST /v1/suggestions/{id}/accept", h.acceptSuggestion)
	p.Handle("POST /v1/suggestions/{id}/reject", h.rejectSuggestion)
	p.Handle("GET /v1/settings/learning", h.getLearning)
	p.Handle("PATCH /v1/settings/learning", h.patchLearning)
	p.Loop("learn", LearnEvery, h.learn)
}

func ownerOnly(w http.ResponseWriter, r *http.Request, what string) bool {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only the owner may "+what)
		return false
	}
	return true
}

func (h *Huginn) observe(w http.ResponseWriter, r *http.Request) {
	if !ownerOnly(w, r, "report what you did") {
		return
	}
	var in Observation
	if err := part.Decode(r, &in); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	action, target, err := NormalAction(in.Action, in.Target)
	if err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	now := h.now()
	at := in.At
	if at.IsZero() || at.After(now) {
		at = now
	}
	if now.Sub(at) > 24*time.Hour {
		part.Error(w, http.StatusBadRequest, "at must be within the last day")
		return
	}
	if h.learnSetting(r.Context()).On {
		if err := h.Store.Observe(r.Context(), Observation{Action: action, Target: target, At: at.UTC()}); err != nil {
			part.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Huginn) listSuggestions(w http.ResponseWriter, r *http.Request) {
	all, err := h.Store.Suggestions(r.Context())
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	open := []Suggestion{}
	for _, s := range all {
		if s.State == "open" {
			open = append(open, s)
		}
	}
	sort.Slice(open, func(i, j int) bool { return open[i].CreatedAt.After(open[j].CreatedAt) })
	part.JSON(w, http.StatusOK, map[string]any{"suggestions": open})
}

func (h *Huginn) openSuggestion(w http.ResponseWriter, r *http.Request) (Suggestion, bool) {
	s, err := h.Store.Suggestion(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, ErrNoSuggestion):
		part.Error(w, http.StatusNotFound, "no such suggestion")
		return s, false
	case err != nil:
		part.Error(w, http.StatusInternalServerError, err.Error())
		return s, false
	case s.State != "open":
		part.Error(w, http.StatusConflict, "already "+s.State)
		return s, false
	}
	return s, true
}

// bearer is the caller's own token, which accepting a mute hands to Gjallar:
// a mute is the owner's to make, and only the owner accepts.
func bearer(r *http.Request) string {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return token
}

func (h *Huginn) acceptSuggestion(w http.ResponseWriter, r *http.Request) {
	if !ownerOnly(w, r, "accept a suggestion") {
		return
	}
	s, ok := h.openSuggestion(w, r)
	if !ok {
		return
	}
	answer := map[string]any{}
	switch {
	case s.Kind == "rule" && s.Rule != nil:
		rule := Rule{ID: newID(), Name: s.Rule.Name, On: s.Rule.On, Action: s.Rule.Action, Deliver: s.Rule.Deliver, Hold: s.Rule.Hold,
			Enabled: true, CreatedBy: part.Owner}
		if err := Validate(rule); err != nil {
			part.Error(w, http.StatusInternalServerError, "the suggested rule is not valid: "+err.Error())
			return
		}
		if err := h.Store.PutRule(r.Context(), rule); err != nil {
			part.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.RuleID, answer["rule"] = rule.ID, rule
	case s.Kind == "mute" && s.Mute != nil:
		var gjallarURL string
		if h.Gjallar != nil {
			gjallarURL = h.Gjallar.URL
		}
		gjallar := call.New("gjallar", gjallarURL, bearer(r), 5*time.Second)
		body := map[string]any{"source": s.Mute.Source, "key": s.Mute.Key, "muted": true}
		if err := gjallar.Do(r.Context(), http.MethodPost, "/v1/mutes", body, nil); err != nil {
			part.Error(w, http.StatusBadGateway, err.Error())
			return
		}
		answer["mute"] = s.Mute
	default:
		part.Error(w, http.StatusInternalServerError, "a suggestion with nothing to make")
		return
	}
	h.decide(w, r, s, "accepted", answer)
}

func (h *Huginn) rejectSuggestion(w http.ResponseWriter, r *http.Request) {
	if !ownerOnly(w, r, "say no to a suggestion") {
		return
	}
	s, ok := h.openSuggestion(w, r)
	if !ok {
		return
	}
	h.decide(w, r, s, "rejected", map[string]any{})
}

func (h *Huginn) decide(w http.ResponseWriter, r *http.Request, s Suggestion, state string, answer map[string]any) {
	at := h.now()
	s.State, s.DecidedAt = state, &at
	if err := h.Store.PutSuggestion(r.Context(), s); err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	answer["suggestion"] = s
	part.JSON(w, http.StatusOK, answer)
}

func (h *Huginn) getLearning(w http.ResponseWriter, r *http.Request) {
	part.JSON(w, http.StatusOK, h.learnSetting(r.Context()))
}

func (h *Huginn) patchLearning(w http.ResponseWriter, r *http.Request) {
	if !ownerOnly(w, r, "change learning") {
		return
	}
	var in struct {
		On            *bool `json:"on"`
		WindowMinutes *int  `json:"windowMinutes"`
	}
	if err := part.Decode(r, &in); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	setting := h.learnSetting(r.Context())
	if in.On != nil {
		setting.On = *in.On
	}
	if in.WindowMinutes != nil {
		if *in.WindowMinutes < 1 || *in.WindowMinutes > 60 {
			part.Error(w, http.StatusBadRequest, "windowMinutes must be 1 to 60")
			return
		}
		setting.WindowMinutes = *in.WindowMinutes
	}
	if err := h.Store.SetLearnState(r.Context(), "setting", setting); err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Off forgets what it saw you do; suggestions you decided on stay decided.
	if !setting.On {
		if err := h.Store.ForgetLearning(r.Context(), h.now().Add(time.Hour)); err != nil {
			part.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	part.JSON(w, http.StatusOK, setting)
}
