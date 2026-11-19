package huginn

// Ratatoskr's gossip: a small group chat where the parts talk about what just
// happened to the system itself, in character. Just for fun, and a place you
// visit: nothing is sent to Hermóðr, ntfy or a toast.
//
// Only system events are talked about: alerts from Gjallar's stream, Loki's
// strikes, merged goals and self-repair, Heimdall's findings by check, parts
// going down and up, and Dellingr's runs. Each becomes a fact from fixed
// words: part names, check names and trick kinds from fixed lists. An
// alert's title, detail and link, a goal's title and a job's name are never
// read, so no inbox, mail, meeting or calendar text and no person's name can
// reach the prompt or the chat.
//
// The lines are written by the local model only (CHAT_URL on loopback, never
// Claude), checked (a real part speaks, short, no link, nothing that looks
// like a key), and when the model is down or answers junk a canned exchange
// is used instead. At most GossipPerHour exchanges an hour, GossipGap apart,
// in working hours only; the events of a burst fold into one exchange.
//
//	GET /v1/gossip?since=<id>   the lines after that id, oldest first
//	GET /v1/settings/gossip     { on }
//	PUT /v1/settings/gossip     { on }, owner only; on until turned off

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/llm"
	"yggdrasil/internal/part"
)

const (
	// GossipEvery is how often the pending events are looked at.
	GossipEvery = time.Minute
	// GossipWatchEvery is how often Loki, Sindri and Dellingr are read.
	GossipWatchEvery = 3 * time.Minute
	// GossipLookback is how far back those reads look for something new.
	GossipLookback = 2 * time.Hour
	// GossipSettle is how long after the newest event the parts wait, so a burst folds.
	GossipSettle = 2 * time.Minute
	// GossipGap is the least time between two exchanges.
	GossipGap = 15 * time.Minute
	// GossipPerHour is the most exchanges in an hour.
	GossipPerHour = 3
	// GossipMaxAge is how old an event may get waiting, so a night's events
	// make the morning's first exchange.
	GossipMaxAge = 16 * time.Hour
	// GossipFold is the most events one exchange talks about.
	GossipFold = 3
	// GossipKeep is how many lines are kept.
	GossipKeep = 500
	// MaxGossipLine is the longest line, in characters.
	MaxGossipLine = 140
	gossipPending = 50
	gossipLines   = 4
)

// GossipEvent is one real event the parts may gossip about. Kind, Part and
// Detail come from fixed lists only.
type GossipEvent struct {
	Kind   string    `json:"kind"`
	Part   string    `json:"part,omitempty"`
	Detail string    `json:"detail,omitempty"`
	Ref    string    `json:"ref"`
	At     time.Time `json:"at"`
}

// GossipLine is one line of the chat.
type GossipLine struct {
	ID       int64     `json:"id"`
	Exchange string    `json:"exchange"`
	At       time.Time `json:"at"`
	Speaker  string    `json:"speaker"`
	Name     string    `json:"name"`
	Text     string    `json:"text"`
	// About is the real events the exchange was about, in words.
	About  []string `json:"about"`
	Canned bool     `json:"canned,omitempty"`
}

// GossipSetting is GET and PUT /v1/settings/gossip.
type GossipSetting struct {
	On bool `json:"on"`
}

// Chatter is the local model.
type Chatter interface {
	Chat(ctx context.Context, messages []llm.Message, options llm.Options) (string, error)
}

// gossip is what the gossip holds between passes.
type gossip struct {
	mu      sync.Mutex
	pending []GossipEvent
}

// --- facts, from fixed words ---

var checkWords = map[string]string{
	"deps":      "dependencies",
	"listening": "open ports",
	"secrets":   "keys in commits",
	"files":     "file permissions",
	"scopes":    "sign-in scopes",
}

var trickWords = map[string]string{
	"health": "fake fainting spell",
	"loop":   "loop-tripping",
	"slow":   "slow-motion",
	"error":  "wrong-answer",
	"drop":   "dropped-call",
	"hang":   "hanging-call",
	"freeze": "freeze",
}

// gossipPriority orders the events of a burst: the best story first.
var gossipPriority = []string{"loki.missed", "loki.caught", "part.down", "repair.merged", "heimdall.finding", "goal.merged",
	"ratatoskr.silent", "dellingr.failed", "munin.trouble", "machine.disk", "machine.unit", "part.stumbling", "part.stalled",
	"huginn.deaf", "alert.other", "part.up", "heimdall.clear", "part.recovered", "munin.calm", "alert.cleared", "dellingr.late", "dellingr.ran"}

func priorityOf(kind string) int {
	for i, k := range gossipPriority {
		if k == kind {
			return i
		}
	}
	return len(gossipPriority)
}

// Fact is the event in plain words, from fixed templates: what the model is
// told, and what a line shows it was about.
func Fact(e GossipEvent) string {
	name := PartName(e.Part)
	if name == "" {
		name = "a part"
	}
	switch e.Kind {
	case "part.down":
		return name + " went down."
	case "part.up":
		return name + " is back up."
	case "part.stumbling":
		return "One of " + name + "'s loops keeps failing."
	case "part.stalled":
		return "One of " + name + "'s loops stopped running."
	case "part.recovered":
		return name + "'s loop runs cleanly again."
	case "loki.caught":
		return "Loki played a " + trickWords[e.Detail] + " trick on " + name + ", and the watchers caught it."
	case "loki.missed":
		return "Loki played a " + trickWords[e.Detail] + " trick on " + name + ", and nobody noticed."
	case "heimdall.finding":
		return "Heimdall's nightly check of " + checkWords[e.Detail] + " found something to look at."
	case "heimdall.clear":
		return "Heimdall's check of " + checkWords[e.Detail] + " is clean again."
	case "goal.merged":
		if e.Detail == "night" {
			return "Sindri's coding agents got a goal merged after the night shift."
		}
		return "Sindri's coding agents got a goal merged."
	case "repair.merged":
		return "Sindri's self-repair fixed something in Yggdrasil itself, and the fix was merged."
	case "dellingr.ran":
		return "Dellingr ran a scheduled job on time."
	case "dellingr.late":
		return "Dellingr ran a scheduled job late, after the machine woke up."
	case "dellingr.failed":
		return "One of Dellingr's scheduled jobs failed."
	case "munin.trouble":
		return "Munin saw trouble in an outside system."
	case "munin.calm":
		return "The outside system Munin watches is calm again."
	case "machine.disk":
		return "Veðrfölnir says a disk on the machine is filling up."
	case "machine.unit":
		return "Veðrfölnir saw a service on the machine fail."
	case "huginn.deaf":
		return "Huginn can't hear " + name + "'s event stream."
	case "ratatoskr.silent":
		return "Gjallar stopped hearing Ratatoskr's heartbeat."
	case "alert.cleared":
		return name + "'s alert cleared."
	}
	if e.Part == "" {
		return "An alert opened."
	}
	return name + " raised an alert."
}

// speakersOf is who would talk about an event: the gossip itself, and those it is about.
func speakersOf(e GossipEvent) []string {
	who := []string{"ratatoskr"}
	switch {
	case strings.HasPrefix(e.Kind, "loki."):
		who = append(who, "loki", "gjallar")
	case strings.HasPrefix(e.Kind, "heimdall."):
		who = append(who, "heimdall")
	case e.Kind == "goal.merged" || e.Kind == "repair.merged":
		who = append(who, "sindri")
	case strings.HasPrefix(e.Kind, "dellingr."):
		who = append(who, "dellingr")
	case strings.HasPrefix(e.Kind, "munin."):
		who = append(who, "munin")
	case strings.HasPrefix(e.Kind, "machine."):
		who = append(who, "vedrfolnir")
	case e.Kind == "huginn.deaf":
		who = append(who, "huginn")
	default:
		who = append(who, "gjallar")
	}
	if PartName(e.Part) != "" {
		who = append(who, e.Part)
	}
	return who
}

// --- events in ---

// knownPart is the part's id when it is one, else "".
func knownPart(name string) string {
	if v, ok := voiceOf(name); ok {
		return v.ID
	}
	return ""
}

// GossipFromAlert is the gossip in one of Gjallar's alert changes, read from
// its source and key only: never its title, detail or link. lokiOn and
// dellingrOn leave out the alerts those reads already tell.
func GossipFromAlert(kind string, a *Alert, lokiOn, dellingrOn bool) (GossipEvent, bool) {
	if a == nil || (kind != "alert.opened" && kind != "alert.resolved") {
		return GossipEvent{}, false
	}
	opened := kind == "alert.opened"
	fields := strings.Split(a.Key, ":")
	field := func(i int) string {
		if i < len(fields) {
			return fields[i]
		}
		return ""
	}
	source := knownPart(a.Source)
	e := GossipEvent{Ref: fmt.Sprintf("alert:%d:%s:%d", a.ID, kind, a.LastSeenAt.Unix()), At: a.LastSeenAt}
	pick := func(open, resolved string) string {
		if opened {
			return open
		}
		return resolved
	}
	switch {
	case source == "ratatoskr" && field(0) == "down":
		e.Kind, e.Part = pick("part.down", "part.up"), knownPart(field(1))
	case source == "ratatoskr" && field(0) == "failing":
		e.Kind, e.Part = pick("part.stumbling", "part.recovered"), knownPart(field(1))
	case source == "ratatoskr" && field(0) == "stalled":
		e.Kind, e.Part = pick("part.stalled", "part.recovered"), knownPart(field(1))
	case source == "loki" && field(0) == "missed":
		if lokiOn || !opened || trickWords[field(2)] == "" {
			return GossipEvent{}, false
		}
		e.Kind, e.Part, e.Detail = "loki.missed", knownPart(field(1)), field(2)
	case source == "heimdall" && checkWords[field(0)] != "":
		e.Kind, e.Detail = pick("heimdall.finding", "heimdall.clear"), field(0)
	case source == "dellingr" && field(0) == "failing":
		if dellingrOn || !opened {
			return GossipEvent{}, false
		}
		e.Kind = "dellingr.failed"
	case source == "munin":
		e.Kind = pick("munin.trouble", "munin.calm")
	case source == "vedrfolnir" && field(0) == "disk":
		if !opened {
			return GossipEvent{}, false
		}
		e.Kind = "machine.disk"
	case source == "vedrfolnir" && field(0) == "failed":
		if !opened {
			return GossipEvent{}, false
		}
		e.Kind = "machine.unit"
	case source == "gjallar" && a.Key == "silent:ratatoskr":
		e.Kind = pick("ratatoskr.silent", "part.up")
		e.Part = "ratatoskr"
	case source == "huginn" && field(0) == "stream":
		if !opened {
			return GossipEvent{}, false
		}
		e.Kind, e.Part = "huginn.deaf", knownPart(field(1))
	default:
		e.Kind, e.Part = pick("alert.other", "alert.cleared"), source
	}
	if (e.Kind == "part.down" || e.Kind == "part.up" || e.Kind == "part.stumbling" || e.Kind == "part.stalled" || e.Kind == "part.recovered") && e.Part == "" {
		return GossipEvent{}, false
	}
	return e, true
}

// noteGossip queues an event for the next exchange, once per event, and only
// while the gossip is on.
func (h *Huginn) noteGossip(ctx context.Context, e GossipEvent) {
	if e.Kind == "" || h.now().Sub(e.At) > GossipMaxAge || !h.gossipSetting(ctx).On {
		return
	}
	if told, err := h.Store.GossipTold(ctx, e.Ref); err != nil || told {
		return
	}
	if err := h.Store.TellGossip(ctx, e.Ref, h.now()); err != nil {
		h.logf("keeping a gossip mark: %v", err)
		return
	}
	h.gossip.mu.Lock()
	defer h.gossip.mu.Unlock()
	h.gossip.pending = append(h.gossip.pending, e)
	if len(h.gossip.pending) > gossipPending {
		h.gossip.pending = h.gossip.pending[len(h.gossip.pending)-gossipPending:]
	}
}

// gossipAlert hands an alert event to the gossip; anything else is left alone.
func (h *Huginn) gossipAlert(ctx context.Context, f Fired) {
	if !strings.HasPrefix(f.Kind, "alert.") {
		return
	}
	if e, ok := GossipFromAlert(f.Kind, f.Alert, h.Loki.On(), h.Dellingr.On()); ok {
		h.noteGossip(ctx, e)
	}
}

// LokiStrike is the little of Loki's strike the gossip reads.
type LokiStrike struct {
	ID    string `json:"id"`
	Part  string `json:"part"`
	Trick struct {
		Kind string `json:"kind"`
	} `json:"trick"`
	EndsAt   time.Time  `json:"endsAt"`
	HealedAt *time.Time `json:"healedAt"`
	State    string     `json:"state"`
}

// GoalMerges is the little of Sindri's goals the gossip reads: never a title.
type GoalMerges struct {
	ID     int64  `json:"id"`
	Source string `json:"source"`
	Night  bool   `json:"night"`
	Tasks  []struct {
		ID       int64 `json:"id"`
		MergedAt int64 `json:"mergedAt"`
	} `json:"tasks"`
}

// DellingrRun is the little of Dellingr's run the gossip reads: never a name or text.
type DellingrRun struct {
	ID        string     `json:"id"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt"`
	State     string     `json:"state"`
	Missed    bool       `json:"missed"`
}

// GossipFromStrike is a finished strike as gossip; a stopped one is none.
func GossipFromStrike(s LokiStrike) (GossipEvent, bool) {
	if (s.State != "caught" && s.State != "missed") || trickWords[s.Trick.Kind] == "" || knownPart(s.Part) == "" {
		return GossipEvent{}, false
	}
	at := s.EndsAt
	if s.HealedAt != nil {
		at = *s.HealedAt
	}
	return GossipEvent{Kind: "loki." + s.State, Part: knownPart(s.Part), Detail: s.Trick.Kind, Ref: "strike:" + s.ID, At: at}, true
}

// GossipFromGoals is every task merged since then.
func GossipFromGoals(goals []GoalMerges, since time.Time) []GossipEvent {
	var events []GossipEvent
	for _, g := range goals {
		for _, t := range g.Tasks {
			if t.MergedAt == 0 || time.UnixMilli(t.MergedAt).Before(since) {
				continue
			}
			e := GossipEvent{Kind: "goal.merged", Ref: fmt.Sprintf("merge:%d:%d", g.ID, t.ID), At: time.UnixMilli(t.MergedAt)}
			if strings.HasPrefix(g.Source, "self-repair:") {
				e.Kind = "repair.merged"
			} else if g.Night {
				e.Detail = "night"
			}
			events = append(events, e)
		}
	}
	return events
}

// GossipFromRun is one of Dellingr's finished runs.
func GossipFromRun(r DellingrRun) (GossipEvent, bool) {
	at := r.StartedAt
	if r.EndedAt != nil {
		at = *r.EndedAt
	}
	e := GossipEvent{Ref: "run:" + r.ID, At: at}
	switch {
	case r.State == "failed":
		e.Kind = "dellingr.failed"
	case r.State == "done" && r.Missed:
		e.Kind = "dellingr.late"
	case r.State == "done":
		e.Kind = "dellingr.ran"
	default:
		return GossipEvent{}, false
	}
	return e, true
}

// watchGossip reads Loki's strikes, Sindri's merges and Dellingr's runs, each
// when Huginn has a token there, while the gossip is on.
func (h *Huginn) watchGossip(ctx context.Context) error {
	if !h.gossipSetting(ctx).On {
		return nil
	}
	since := h.now().Add(-GossipLookback)
	var failed []string
	if h.Loki.On() {
		var answer struct {
			Strikes []LokiStrike `json:"strikes"`
		}
		if err := h.Loki.Do(ctx, http.MethodGet, "/v1/strikes?limit=20", nil, &answer); err != nil {
			failed = append(failed, err.Error())
		}
		for _, s := range answer.Strikes {
			if e, ok := GossipFromStrike(s); ok && e.At.After(since) {
				h.noteGossip(ctx, e)
			}
		}
	}
	if h.Sindri.On() {
		var answer struct {
			Goals []GoalMerges `json:"goals"`
		}
		if err := h.Sindri.Do(ctx, http.MethodGet, "/v1/goals", nil, &answer); err != nil {
			failed = append(failed, err.Error())
		}
		for _, e := range GossipFromGoals(answer.Goals, since) {
			h.noteGossip(ctx, e)
		}
	}
	if h.Dellingr.On() {
		var answer struct {
			Runs []DellingrRun `json:"runs"`
		}
		if err := h.Dellingr.Do(ctx, http.MethodGet, "/v1/runs", nil, &answer); err != nil {
			failed = append(failed, err.Error())
		}
		for _, r := range answer.Runs {
			if e, ok := GossipFromRun(r); ok && e.At.After(since) {
				h.noteGossip(ctx, e)
			}
		}
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}

// --- when ---

// working is whether t is inside your working hours, Monday to Friday.
func (h *Huginn) working(t time.Time) bool {
	t = t.In(h.Zone)
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		return false
	}
	minute := t.Hour()*60 + t.Minute()
	return minute >= h.WorkFrom && minute < h.WorkTo
}

// ParseWorkHours reads "09:00-17:00" as minutes since midnight.
func ParseWorkHours(text string) (int, int, error) {
	from, to, ok := strings.Cut(text, "-")
	a, errA := time.Parse("15:04", strings.TrimSpace(from))
	b, errB := time.Parse("15:04", strings.TrimSpace(to))
	if !ok || errA != nil || errB != nil {
		return 0, 0, fmt.Errorf("working hours look like 09:00-17:00, not %q", text)
	}
	start, end := a.Hour()*60+a.Minute(), b.Hour()*60+b.Minute()
	if start >= end {
		return 0, 0, fmt.Errorf("working hours look like 09:00-17:00, not %q", text)
	}
	return start, end, nil
}

// fold is the events the next exchange talks about, the best story first, or
// none yet: the gossip is quiet outside working hours, waits for a burst to
// settle, keeps GossipGap between exchanges and GossipPerHour an hour. The
// events folded, and any too old, leave the queue.
func (h *Huginn) fold(ctx context.Context) ([]GossipEvent, error) {
	now := h.now()
	h.gossip.mu.Lock()
	defer h.gossip.mu.Unlock()
	fresh := h.gossip.pending[:0]
	newest := time.Time{}
	for _, e := range h.gossip.pending {
		if now.Sub(e.At) <= GossipMaxAge {
			fresh = append(fresh, e)
			if e.At.After(newest) {
				newest = e.At
			}
		}
	}
	h.gossip.pending = fresh
	if len(fresh) == 0 || !h.working(now) || now.Sub(newest) < GossipSettle {
		return nil, nil
	}
	last, count, err := h.Store.GossipExchanges(ctx, now.Add(-time.Hour))
	if err != nil {
		return nil, err
	}
	if count >= GossipPerHour || (!last.IsZero() && now.Sub(last) < GossipGap) {
		return nil, nil
	}
	events := append([]GossipEvent(nil), fresh...)
	sort.SliceStable(events, func(i, j int) bool {
		if a, b := priorityOf(events[i].Kind), priorityOf(events[j].Kind); a != b {
			return a < b
		}
		return events[i].At.After(events[j].At)
	})
	// One story per kind and part: two merges are one "merged again".
	seen := map[string]bool{}
	var picked []GossipEvent
	for _, e := range events {
		key := e.Kind + "|" + e.Part
		if seen[key] {
			continue
		}
		seen[key] = true
		if len(picked) < GossipFold {
			picked = append(picked, e)
		}
	}
	h.gossip.pending = nil
	return picked, nil
}

// gossipPass writes the next exchange, when there is one to write.
func (h *Huginn) gossipPass(ctx context.Context) error {
	if !h.gossipSetting(ctx).On {
		h.gossip.mu.Lock()
		h.gossip.pending = nil
		h.gossip.mu.Unlock()
		return nil
	}
	events, err := h.fold(ctx)
	if err != nil || len(events) == 0 {
		return err
	}
	lines := h.exchange(ctx, events)
	if len(lines) == 0 {
		return nil
	}
	if err := h.Store.AddGossip(ctx, lines); err != nil {
		return err
	}
	return h.Store.TrimGossip(ctx, GossipKeep)
}

// exchange is the lines about the events: the model's when it answers
// something sound, else a canned one.
func (h *Huginn) exchange(ctx context.Context, events []GossipEvent) []GossipLine {
	about := make([]string, len(events))
	for i, e := range events {
		about[i] = Fact(e)
	}
	id := newID()
	at := h.now()
	said, canned := h.askModel(ctx, events), false
	if len(said) == 0 {
		said, canned = Canned(events), true
	}
	lines := make([]GossipLine, 0, len(said))
	for _, s := range said {
		lines = append(lines, GossipLine{Exchange: id, At: at, Speaker: s.Speaker, Name: PartName(s.Speaker), Text: s.Text, About: about, Canned: canned})
	}
	return lines
}

// --- the model ---

// Said is one line as written: who and what.
type Said struct {
	Speaker string `json:"speaker"`
	Text    string `json:"text"`
}

// GossipPrompt is what the local model is asked: the voices, the rules, and
// the facts in fixed words.
func GossipPrompt(events []GossipEvent) []llm.Message {
	var voices strings.Builder
	for _, v := range Voices {
		fmt.Fprintf(&voices, "- %s: %s\n", v.Name, v.Talk)
	}
	system := `You write a tiny, playful group chat between the parts of Yggdrasil, a home computer system whose parts are named after Norse myth. It is harmless nonsense about what just happened to the system itself.

The parts, and how each one talks:
` + voices.String() + `
Rules:
- Write 2 to 4 lines. Each line is said by one of the parts above, by its name.
- Each line is at most 120 characters, one sentence or two, in English.
- Talk only about the events given. Never mention people, mail, messages, meetings or calendars.
- No links, no numbers, no file paths, no keys or passwords.
- Answer with JSON only: {"lines":[{"speaker":"<part name>","text":"<line>"}]}`
	var facts strings.Builder
	var who []string
	seen := map[string]bool{}
	for _, e := range events {
		facts.WriteString("- " + Fact(e) + "\n")
		for _, id := range speakersOf(e) {
			if !seen[id] {
				seen[id] = true
				who = append(who, PartName(id))
			}
		}
	}
	user := "What just happened:\n" + facts.String() + "\nWho might speak: " + strings.Join(who, ", ") + ". Ratatoskr carries the news."
	return []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: user}}
}

var gossipSchema = map[string]any{
	"type":     "object",
	"required": []string{"lines"},
	"properties": map[string]any{
		"lines": map[string]any{
			"type": "array", "minItems": 2, "maxItems": gossipLines,
			"items": map[string]any{
				"type":       "object",
				"required":   []string{"speaker", "text"},
				"properties": map[string]any{"speaker": map[string]any{"type": "string"}, "text": map[string]any{"type": "string"}},
			},
		},
	},
}

func (h *Huginn) askModel(ctx context.Context, events []GossipEvent) []Said {
	if h.Model == nil {
		return nil
	}
	answer, err := h.Model.Chat(ctx, GossipPrompt(events), llm.Options{Purpose: "gossip", Model: h.GossipModel, Temperature: 0.9, TopP: 0.95,
		MaxTokens: 400, Format: gossipSchema, Timeout: 3 * time.Minute, NoFallback: true})
	if err != nil {
		h.logf("the gossip's model did not answer: %v", err)
		return nil
	}
	said, err := ValidGossip(answer)
	if err != nil {
		h.logf("the gossip's model answered junk: %v", err)
		return nil
	}
	return said
}

var (
	gossipURL    = regexp.MustCompile(`(?i)(://|\bwww\.|\b[a-z0-9-]+\.(com|se|io|net|org|dev|app|ai|co|uk)\b|/[a-z0-9_.-]+/)`)
	gossipSecret = regexp.MustCompile(`(?i)(ghp_|gho_|github_pat_|\bsk-|xox[abpr]-|\bakia|private key|bearer\s|password|passwd|token\s*[:=]|key\s*[:=])`)
	// A long run with a digit in it reads like a key or an id.
	gossipToken = regexp.MustCompile(`[A-Za-z0-9_\-+/=]{20,}`)
	gossipTopic = regexp.MustCompile(`(?i)\b(inbox|e-?mails?|mail|teams|slack|outlook|gmail|meetings?|calendars?|invites?)\b`)
	gossipDigit = regexp.MustCompile(`[0-9]`)
)

// BadGossipLine says what is wrong with a line, or "".
func BadGossipLine(text string) string {
	switch {
	case text == "":
		return "empty"
	case len([]rune(text)) > MaxGossipLine:
		return "too long"
	case gossipURL.MatchString(text):
		return "a link"
	case strings.Contains(text, "@"):
		return "an address"
	case gossipSecret.MatchString(text):
		return "something secret-like"
	case gossipTopic.MatchString(text):
		return "not about the system"
	}
	for _, run := range gossipToken.FindAllString(text, -1) {
		if gossipDigit.MatchString(run) {
			return "something key-like"
		}
	}
	return ""
}

// ValidGossip reads the model's answer into lines: each by a real part,
// short, with no link, address or anything that looks like a key, and two
// at least, else an error and nothing.
func ValidGossip(answer string) ([]Said, error) {
	answer = strings.TrimSpace(answer)
	start, end := strings.Index(answer, "{"), strings.LastIndex(answer, "}")
	if start < 0 || end <= start {
		return nil, errors.New("no JSON in the answer")
	}
	var body struct {
		Lines []Said `json:"lines"`
	}
	if err := json.Unmarshal([]byte(answer[start:end+1]), &body); err != nil {
		return nil, fmt.Errorf("not the JSON asked for: %w", err)
	}
	var said []Said
	seen := map[string]bool{}
	for _, line := range body.Lines {
		voice, ok := voiceOf(line.Speaker)
		if !ok {
			continue
		}
		text := strings.Join(strings.Fields(line.Text), " ")
		text = strings.Trim(text, `"“”`)
		// "Loki: I did it" from Loki is "I did it".
		for _, prefix := range []string{voice.Name + ":", voice.ID + ":", plainName(voice.Name) + ":"} {
			if len(text) >= len(prefix) && strings.EqualFold(text[:len(prefix)], prefix) {
				text = strings.TrimSpace(text[len(prefix):])
			}
		}
		if BadGossipLine(text) != "" || seen[strings.ToLower(text)] {
			continue
		}
		seen[strings.ToLower(text)] = true
		said = append(said, Said{Speaker: voice.ID, Text: text})
		if len(said) == gossipLines {
			break
		}
	}
	if len(said) < 2 {
		return nil, fmt.Errorf("%d usable lines", len(said))
	}
	return said, nil
}

// Canned is a canned exchange about the first event, chosen by the event, and
// a line from Ratatoskr when there was more.
func Canned(events []GossipEvent) []Said {
	if len(events) == 0 {
		return nil
	}
	e := events[0]
	options := canned[e.Kind]
	if len(options) == 0 {
		options = canned["alert.other"]
	}
	hash := fnv.New32a()
	hash.Write([]byte(e.Ref))
	pick := int(hash.Sum32())
	name := PartName(e.Part)
	if name == "" {
		name = "someone"
	}
	detail := trickWords[e.Detail]
	if words := checkWords[e.Detail]; words != "" {
		detail = words
	}
	fill := strings.NewReplacer("{part}", name, "{detail}", detail)
	var said []Said
	for _, line := range options[pick%len(options)] {
		speaker := line.Speaker
		if speaker == "{part}" {
			speaker = knownPart(e.Part)
		}
		if speaker == "" {
			continue
		}
		said = append(said, Said{Speaker: speaker, Text: fill.Replace(line.Text)})
	}
	if len(events) > 1 {
		said = append(said, Said{Speaker: "ratatoskr", Text: cannedMore[pick%len(cannedMore)]})
	}
	return said
}

// --- routes ---

func (h *Huginn) mountGossip(p *part.Part) {
	p.Handle("GET /v1/gossip", h.getGossip)
	p.Handle("GET /v1/settings/gossip", h.getGossipSetting)
	p.Handle("PUT /v1/settings/gossip", h.putGossipSetting)
	p.Loop("gossip", GossipEvery, h.gossipPass)
	p.Loop("gossip-watch", GossipWatchEvery, h.watchGossip)
}

func (h *Huginn) gossipSetting(ctx context.Context) GossipSetting {
	setting := GossipSetting{On: true}
	if _, err := h.Store.GossipState(ctx, "setting", &setting); err != nil {
		h.logf("reading the gossip setting: %v", err)
	}
	return setting
}

func (h *Huginn) getGossip(w http.ResponseWriter, r *http.Request) {
	var since int64
	if text := r.URL.Query().Get("since"); text != "" {
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil || n < 0 {
			part.Error(w, http.StatusBadRequest, "since must be a line id, 0 or more")
			return
		}
		since = n
	}
	setting := h.gossipSetting(r.Context())
	lines := []GossipLine{}
	if setting.On {
		var err error
		if lines, err = h.Store.Gossip(r.Context(), since, 200); err != nil {
			part.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	part.JSON(w, http.StatusOK, map[string]any{"on": setting.On, "lines": lines})
}

func (h *Huginn) getGossipSetting(w http.ResponseWriter, r *http.Request) {
	part.JSON(w, http.StatusOK, h.gossipSetting(r.Context()))
}

func (h *Huginn) putGossipSetting(w http.ResponseWriter, r *http.Request) {
	if !ownerOnly(w, r, "turn the gossip on or off") {
		return
	}
	var in struct {
		On *bool `json:"on"`
	}
	if err := part.Decode(r, &in); err != nil || in.On == nil {
		part.Error(w, http.StatusBadRequest, "send { on: true|false }")
		return
	}
	setting := GossipSetting{On: *in.On}
	if err := h.Store.SetGossipState(r.Context(), "setting", setting); err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !setting.On {
		h.gossip.mu.Lock()
		h.gossip.pending = nil
		h.gossip.mu.Unlock()
	}
	part.JSON(w, http.StatusOK, setting)
}

// LocalChatURL is CHAT_URL when it is on this machine, else "": the gossip
// never goes to a model elsewhere.
func LocalChatURL(raw string) string {
	u, err := url.Parse(raw)
	if raw == "" || raw == "off" || err != nil {
		return ""
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
		return raw
	}
	return ""
}
