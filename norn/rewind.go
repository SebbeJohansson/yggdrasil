package norn

// Rewind: "what was I doing on Tuesday at 3?". One timeline for a range,
// built when asked from what Yggdrasil already keeps, with no second copy of
// it: your meetings (the calendars, read for that range when it is older than
// the day Norn keeps), save points, promises, your messages to Norn and its
// agents, what Norn learned, the reply drafts you used, Sindri's goals,
// agents and merges, your own Claude Code prompts (Sindri's index), and the
// Obsidian notes changed (titles and links only, never note text).
//
// One small log exists only for this: the vault notes seen changing
// (rewind_vault.go: path and time), so a note changed again later still shows
// on the earlier day. Everything stays on this machine; questions are read by
// the local model only (rewind_ask.go).
//
//	GET  /v1/rewind?from=&to=   the timeline (epoch ms; today by default)
//	POST /v1/rewind/ask { q }   a question in plain words → a range, and what matches

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/part"
)

const (
	// RewindClip is how much of a prompt or message an entry shows.
	RewindClip = 200
	// RewindBurst is the longest pause inside one burst of prompts or messages.
	RewindBurst = 10 * time.Minute
	// RewindRangeMax is the longest range one call covers.
	RewindRangeMax = 31 * 24 * time.Hour
	// rewindMax is the most entries one answer carries.
	rewindMax = 2000
)

// RewindEntry is one thing on the timeline.
type RewindEntry struct {
	At  int64 `json:"at"`
	End int64 `json:"end,omitempty"`
	// Kind is meeting, break, session, prompt, goal, merge, agent, promise,
	// note, chat, savepoint, memory or reply.
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	// Link is a dashboard path, or obsidian:// for a note.
	Link string `json:"link,omitempty"`
	// Source is where it was read: calendar:work, calendar:personal, norn, sindri or obsidian.
	Source string `json:"source"`
	// Match is set on POST /v1/rewind/ask: the entry holds what was asked about.
	Match bool `json:"match,omitempty"`
}

// rewindState is Rewind's running state on Norn.
type rewindState struct {
	// Model splits questions; nil is Norn's local model.
	Model     Chatter
	calendars rangeCache
	vaultMu   sync.Mutex
	vaultInit bool
}

func rewindClip(text string, limit int) string {
	text = strings.TrimSpace(whitespace.ReplaceAllString(text, " "))
	runes := []rune(text)
	if len(runes) > limit {
		return strings.TrimRight(string(runes[:limit-1]), " ") + "…"
	}
	return text
}

func inside(at, from, to int64) bool { return at >= from && at < to }

// --- bursts ----------------------------------------------------------------------------

// Said is one prompt or message, for grouping into bursts.
type Said struct {
	At    int64
	Group string
	Text  string
}

// Bursts groups what was said in the same group with pauses of at most gap:
// one burst per run, oldest first.
func Bursts(said []Said, gap time.Duration) [][]Said {
	byGroup := map[string][]Said{}
	var order []string
	for _, s := range said {
		if _, ok := byGroup[s.Group]; !ok {
			order = append(order, s.Group)
		}
		byGroup[s.Group] = append(byGroup[s.Group], s)
	}
	var out [][]Said
	for _, group := range order {
		items := byGroup[group]
		sort.SliceStable(items, func(i, j int) bool { return items[i].At < items[j].At })
		start := 0
		for i := 1; i <= len(items); i++ {
			if i == len(items) || items[i].At-items[i-1].At > gap.Milliseconds() {
				out = append(out, items[start:i])
				start = i
			}
		}
	}
	return out
}

// burstEntry is one burst as an entry: the first words as the title, the last as the detail.
func burstEntry(burst []Said, kind, source, link, about string) RewindEntry {
	first, last := burst[0], burst[len(burst)-1]
	entry := RewindEntry{At: first.At, Kind: kind, Title: rewindClip(first.Text, RewindClip), Link: link, Source: source}
	var detail []string
	if len(burst) > 1 {
		entry.End = last.At
		detail = append(detail, "… "+rewindClip(last.Text, RewindClip))
	}
	if about != "" {
		detail = append(detail, about)
	}
	entry.Detail = strings.Join(detail, " · ")
	return entry
}

// --- each source as entries ---------------------------------------------------------------

// MeetingEntries is a calendar's meetings: declined, all-day and free ones
// left out; a break you kept free is a break.
func MeetingEntries(events []CalendarEvent, calendar string, from, to int64) []RewindEntry {
	out := []RewindEntry{}
	for _, e := range events {
		if declined(e) || e.AllDay || (e.ShowAs == "free" && !e.Break) || e.Start >= to || e.End <= from {
			continue
		}
		entry := RewindEntry{At: e.Start, End: e.End, Kind: "meeting", Title: e.Subject, Source: "calendar:" + calendar}
		if e.Break {
			entry.Kind, entry.Title = "break", "Break"
		} else if e.Location != "" {
			entry.Detail = e.Location
		}
		out = append(out, entry)
	}
	return out
}

// SavePointEntries is the places Norn kept through a meeting.
func SavePointEntries(points []SavePoint, from, to int64) []RewindEntry {
	out := []RewindEntry{}
	for _, sp := range points {
		if !inside(sp.At, from, to) {
			continue
		}
		entry := RewindEntry{At: sp.At, Kind: "savepoint", Title: "Kept your place: " + sp.Title, Link: sp.Link, Source: "norn"}
		if sp.Meeting != "" {
			entry.Detail = "for " + sp.Meeting
		}
		out = append(out, entry)
	}
	return out
}

// PromiseEntries is the promises made in the range, and the ones done in it.
func PromiseEntries(promises []Promise, from, to int64) []RewindEntry {
	out := []RewindEntry{}
	for _, p := range promises {
		link := "/dashboard/inbox#promises"
		if inside(p.CreatedAt, from, to) {
			title := "Promised: " + p.What
			if p.To != "" {
				title = "Promised " + p.To + ": " + p.What
			}
			detail := ""
			if p.Due != "" {
				detail = "by " + p.Due
			}
			out = append(out, RewindEntry{At: p.CreatedAt, Kind: "promise", Title: rewindClip(title, RewindClip), Detail: detail, Link: link, Source: "norn"})
		}
		if p.State == "done" && inside(p.ChangedAt, from, to) {
			out = append(out, RewindEntry{At: p.ChangedAt, Kind: "promise", Title: rewindClip("Kept: "+p.What, RewindClip), Detail: rewindClip(p.DoneWhy, RewindClip), Link: link, Source: "norn"})
		}
	}
	return out
}

// ChatTurn is one of your messages, with its conversation.
type ChatTurn struct {
	Session string
	At      int64
	Content string
	// Label is who it went to, when it went to an agent.
	Label string
}

// ChatEntries is your messages to Norn and its agents, a burst per conversation.
func ChatEntries(turns []ChatTurn) []RewindEntry {
	said := make([]Said, 0, len(turns))
	labels := map[string]string{}
	for _, t := range turns {
		said = append(said, Said{At: t.At, Group: t.Session, Text: t.Content})
		if t.Label != "" {
			labels[t.Session] = t.Label
		}
	}
	out := []RewindEntry{}
	for _, burst := range Bursts(said, RewindBurst) {
		session := burst[0].Group
		about := ""
		if label := labels[session]; label != "" {
			about = "to " + label
		}
		out = append(out, burstEntry(burst, "chat", "norn", "/dashboard/chat?session="+url.QueryEscape(session), about))
	}
	return out
}

// MemoryEntries is what Norn learned about you.
func MemoryEntries(memories []Memory, from, to int64) []RewindEntry {
	out := []RewindEntry{}
	for _, m := range memories {
		if inside(m.At, from, to) {
			out = append(out, RewindEntry{At: m.At, Kind: "memory", Title: "Learned: " + rewindClip(m.Text, RewindClip), Link: "/dashboard/memory", Source: "norn"})
		}
	}
	return out
}

// ReplyEntries is the reply drafts you copied to send.
func ReplyEntries(replies []CopiedReply, from, to int64) []RewindEntry {
	out := []RewindEntry{}
	for _, r := range replies {
		if inside(r.At, from, to) {
			out = append(out, RewindEntry{At: r.At, Kind: "reply", Title: "Used a reply draft (" + r.Source + ")", Detail: rewindClip(r.Sent, RewindClip), Link: "/dashboard/inbox", Source: "norn"})
		}
	}
	return out
}

// RewindGoal is a goal as Sindri lists it, with what Rewind needs.
type RewindGoal struct {
	ID       int64  `json:"id"`
	At       int64  `json:"at"`
	Title    string `json:"title"`
	State    string `json:"state"`
	ClosedAt *int64 `json:"closedAt"`
	Tasks    []struct {
		Agent     string  `json:"agent"`
		Repo      *string `json:"repo"`
		Branch    *string `json:"branch"`
		StartedAt int64   `json:"startedAt"`
		MergedAt  int64   `json:"mergedAt"`
	} `json:"tasks"`
}

// GoalEntries is goals made, started, merged and closed.
func GoalEntries(goals []RewindGoal, from, to int64) []RewindEntry {
	out := []RewindEntry{}
	for _, g := range goals {
		link := fmt.Sprintf("/dashboard/goals/%d", g.ID)
		add := func(at int64, kind, title, detail string) {
			if inside(at, from, to) {
				out = append(out, RewindEntry{At: at, Kind: kind, Title: title, Detail: detail, Link: link, Source: "sindri"})
			}
		}
		add(g.At, "goal", "New goal: "+g.Title, "")
		var started int64
		for _, t := range g.Tasks {
			if t.StartedAt > 0 && (started == 0 || t.StartedAt < started) {
				started = t.StartedAt
			}
			if t.MergedAt > 0 {
				where := []string{}
				if t.Repo != nil && *t.Repo != "" {
					where = append(where, *t.Repo)
				}
				if t.Branch != nil && *t.Branch != "" {
					where = append(where, *t.Branch)
				}
				add(t.MergedAt, "merge", "Merged: "+g.Title, strings.Join(where, " · "))
			}
		}
		add(started, "goal", "Started: "+g.Title, "")
		if g.ClosedAt != nil {
			add(*g.ClosedAt, "goal", "Closed: "+g.Title, "")
		}
	}
	return out
}

// RewindAgent is a Sindri agent, with what Rewind needs.
type RewindAgent struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Cwd       string `json:"cwd"`
	StartedAt int64  `json:"startedAt"`
	MergedAt  int64  `json:"mergedAt"`
	Worktree  *struct {
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
	} `json:"worktree"`
}

// AgentEntries is agents started and merged outside any goal (a goal's say it already).
func AgentEntries(agents []RewindAgent, inGoals map[string]bool, from, to int64) []RewindEntry {
	out := []RewindEntry{}
	for _, a := range agents {
		if inGoals[a.ID] {
			continue
		}
		where := ""
		if a.Worktree != nil {
			where = strings.Trim(a.Worktree.Repo+" · "+a.Worktree.Branch, " ·")
		}
		link := SessionPath(a.ID, a.Cwd, a.Label)
		title := TaskOf(a.Label)
		if title == "" {
			title = a.Label
		}
		if inside(a.StartedAt, from, to) {
			out = append(out, RewindEntry{At: a.StartedAt, Kind: "agent", Title: "Agent: " + rewindClip(title, RewindClip), Detail: where, Link: link, Source: "sindri"})
		}
		if inside(a.MergedAt, from, to) {
			out = append(out, RewindEntry{At: a.MergedAt, Kind: "merge", Title: "Merged: " + rewindClip(title, RewindClip), Detail: where, Link: link, Source: "sindri"})
		}
	}
	return out
}

// OwnPrompt is one of your prompts in your own Claude Code sessions (Sindri's index).
type OwnPrompt struct {
	At      int64  `json:"at"`
	Session string `json:"session"`
	Title   string `json:"title"`
	Cwd     string `json:"cwd"`
	Project string `json:"project"`
	Branch  string `json:"branch"`
	Prompt  string `json:"prompt"`
}

// PromptEntries is each session you typed in (its span) and your prompts in it, a burst at a time.
func PromptEntries(prompts []OwnPrompt) []RewindEntry {
	said := make([]Said, 0, len(prompts))
	sessions := map[string]OwnPrompt{}
	span := map[string][2]int64{}
	for _, p := range prompts {
		said = append(said, Said{At: p.At, Group: p.Session, Text: p.Prompt})
		sessions[p.Session] = p
		s, ok := span[p.Session]
		if !ok {
			s = [2]int64{p.At, p.At}
		}
		span[p.Session] = [2]int64{min(s[0], p.At), max(s[1], p.At)}
	}
	out := []RewindEntry{}
	for id, p := range sessions {
		link := SessionPath(id, p.Cwd, p.Title)
		entry := RewindEntry{At: span[id][0], Kind: "session", Title: p.Title, Detail: strings.Trim(p.Project+" · "+p.Branch, " ·"), Link: link, Source: "sindri"}
		if span[id][1] > span[id][0] {
			entry.End = span[id][1]
		}
		out = append(out, entry)
	}
	for _, burst := range Bursts(said, RewindBurst) {
		p := sessions[burst[0].Group]
		out = append(out, burstEntry(burst, "prompt", "sindri", SessionPath(p.Session, p.Cwd, p.Title), ""))
	}
	return out
}

// NoteEntries is the vault notes changed: titles and links only.
func NoteEntries(changes []NoteChange, from, to int64) []RewindEntry {
	latest := map[string]NoteChange{}
	for _, c := range changes {
		if inside(c.At, from, to) && c.At > latest[c.Path].At {
			latest[c.Path] = c
		}
	}
	out := []RewindEntry{}
	for _, c := range latest {
		out = append(out, RewindEntry{At: c.At, Kind: "note", Title: c.Title, Link: c.Link, Source: "obsidian"})
	}
	return out
}

// SortEntries puts entries in time order, and the same time by kind and title.
func SortEntries(entries []RewindEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.At != b.At {
			return a.At < b.At
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Title < b.Title
	})
}

// --- reading the sources ------------------------------------------------------------------

// yourTurns is your messages in from..to, with their conversation and who they went to.
func (s *Store) yourTurns(ctx context.Context, from, to int64) ([]ChatTurn, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session, at, content, label FROM turns
		WHERE role = 'user' AND author IS NULL AND at >= ? AND at < ? ORDER BY at LIMIT 5000`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatTurn
	for rows.Next() {
		var t ChatTurn
		var label sql.NullString
		if err := rows.Scan(&t.Session, &t.At, &t.Content, &label); err != nil {
			return nil, err
		}
		t.Label = label.String
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) savePointsIn(ctx context.Context, from, to int64) ([]SavePoint, error) {
	return s.savePoints(ctx, `WHERE at >= ? AND at < ? ORDER BY at`, from, to)
}

func (p *Promises) touchedIn(ctx context.Context, from, to int64) ([]Promise, error) {
	if err := p.init(ctx); err != nil {
		return nil, err
	}
	rows, err := p.db().QueryContext(ctx, `SELECT `+promiseColumns+` FROM promises
		WHERE (created_at >= ? AND created_at < ?) OR (changed_at >= ? AND changed_at < ?) ORDER BY created_at`, from, to, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Promise{}
	for rows.Next() {
		promise, err := scanPromise(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, promise)
	}
	return out, rows.Err()
}

func (n *Norn) repliesIn(ctx context.Context, from, to int64) ([]CopiedReply, error) {
	rows, err := n.Store.db.QueryContext(ctx, `SELECT at, item, source, COALESCE(relation, ''), draft, sent FROM sent_replies WHERE at >= ? AND at < ? ORDER BY at`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CopiedReply
	for rows.Next() {
		var c CopiedReply
		if err := rows.Scan(&c.At, &c.Item, &c.Source, &c.Relation, &c.Draft, &c.Sent); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RewindGoals is Sindri's goals with their tasks' times.
func (c *SindriClient) RewindGoals(ctx context.Context) ([]RewindGoal, error) {
	var answer struct {
		Goals []RewindGoal `json:"goals"`
	}
	return answer.Goals, c.call(ctx, http.MethodGet, "/v1/goals", nil, &answer)
}

// RewindAgents is Sindri's agents with when each started and merged.
func (c *SindriClient) RewindAgents(ctx context.Context) ([]RewindAgent, error) {
	var agents []RewindAgent
	return agents, c.call(ctx, http.MethodGet, "/v1/agents", nil, &agents)
}

// OwnPrompts is your own Claude Code prompts in from..to, from Sindri's index.
func (c *SindriClient) OwnPrompts(ctx context.Context, from, to int64) ([]OwnPrompt, error) {
	var answer struct {
		Prompts []OwnPrompt `json:"prompts"`
	}
	return answer.Prompts, c.call(ctx, http.MethodGet, fmt.Sprintf("/v1/sessions/prompts?from=%d&to=%d", from, to), nil, &answer)
}

// Timeline is every source's entries in from..to, in time order, and the
// sources that could not be read.
func (n *Norn) Timeline(ctx context.Context, from, to time.Time) ([]RewindEntry, []string) {
	return n.timeline(ctx, from, to, true)
}

// timeline is Timeline, with Sindri's goals, agents and prompts left out
// when withSindri is false (a goal's why, asked by Sindri itself).
func (n *Norn) timeline(ctx context.Context, from, to time.Time, withSindri bool) ([]RewindEntry, []string) {
	f, t := from.UnixMilli(), to.UnixMilli()
	var mu sync.Mutex
	entries := []RewindEntry{}
	unavailable := []string{}
	var wg sync.WaitGroup
	run := func(name string, read func() ([]RewindEntry, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			found, err := read()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				n.logf("rewind: %s: %v", name, err)
				unavailable = append(unavailable, name)
				return
			}
			entries = append(entries, found...)
		}()
	}
	for _, calendar := range []string{"work", "personal"} {
		run("calendar:"+calendar, func() ([]RewindEntry, error) {
			events, err := n.calendarRange(ctx, calendar, from, to)
			return MeetingEntries(events, calendar, f, t), err
		})
	}
	run("norn", func() ([]RewindEntry, error) {
		var out []RewindEntry
		points, err := n.Store.savePointsIn(ctx, f, t)
		if err != nil {
			return nil, err
		}
		out = append(out, SavePointEntries(points, f, t)...)
		if n.Promises != nil {
			promises, err := n.Promises.touchedIn(ctx, f, t)
			if err != nil {
				return nil, err
			}
			out = append(out, PromiseEntries(promises, f, t)...)
		}
		turns, err := n.Store.yourTurns(ctx, f, t)
		if err != nil {
			return nil, err
		}
		out = append(out, ChatEntries(turns)...)
		stored, err := n.Store.Memories(ctx)
		if err != nil {
			return nil, err
		}
		memories := make([]Memory, len(stored))
		for i, m := range stored {
			memories[i] = m.Memory
		}
		out = append(out, MemoryEntries(memories, f, t)...)
		replies, err := n.repliesIn(ctx, f, t)
		if err != nil {
			return nil, err
		}
		return append(out, ReplyEntries(replies, f, t)...), nil
	})
	if withSindri && n.Sindri.On() {
		run("sindri", func() ([]RewindEntry, error) {
			goals, err := n.Sindri.RewindGoals(ctx)
			if err != nil {
				return nil, err
			}
			agents, err := n.Sindri.RewindAgents(ctx)
			if err != nil {
				return nil, err
			}
			inGoals := map[string]bool{}
			for _, g := range goals {
				for _, task := range g.Tasks {
					inGoals[task.Agent] = true
				}
			}
			prompts, err := n.Sindri.OwnPrompts(ctx, f, t)
			if err != nil {
				return nil, err
			}
			out := append(GoalEntries(goals, f, t), AgentEntries(agents, inGoals, f, t)...)
			return append(out, PromptEntries(prompts)...), nil
		})
	}
	if n.Vault.On() {
		run("obsidian", func() ([]RewindEntry, error) {
			changes, err := n.noteChanges(ctx, f, t, time.Now())
			return NoteEntries(changes, f, t), err
		})
	}
	wg.Wait()
	SortEntries(entries)
	if len(entries) > rewindMax {
		entries = entries[:rewindMax]
	}
	sort.Strings(unavailable)
	return entries, unavailable
}

// --- routes ----------------------------------------------------------------------------------

func (n *Norn) mountRewind(p *part.Part) {
	p.Handle("GET /v1/rewind", n.rewind)
	p.Handle("POST /v1/rewind/ask", n.rewindAsk)
	p.Loop("rewind-vault", vaultLogEvery, func(ctx context.Context) error {
		return n.logVault(ctx, time.Now())
	})
}

// dayRange is the day holding t in zone.
func dayRange(t time.Time, zone *time.Location) (time.Time, time.Time) {
	day := dayOf(t.In(zone))
	return day, day.AddDate(0, 0, 1)
}

// rewindRange reads from and to (epoch ms), today by default.
func (n *Norn) rewindRange(r *http.Request) (time.Time, time.Time, error) {
	from, to := dayRange(time.Now(), n.Zone)
	query := r.URL.Query()
	for name, into := range map[string]*time.Time{"from": &from, "to": &to} {
		if raw := query.Get(name); raw != "" {
			ms, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || ms < 0 {
				return from, to, fmt.Errorf("`%s` is epoch ms", name)
			}
			*into = time.UnixMilli(ms)
		}
	}
	if query.Get("from") != "" && query.Get("to") == "" {
		to = from.Add(24 * time.Hour)
	}
	if !to.After(from) || to.Sub(from) > RewindRangeMax {
		return from, to, fmt.Errorf("`to` is after `from`, at most 31 days on")
	}
	return from, to, nil
}

func (n *Norn) rewind(w http.ResponseWriter, r *http.Request) {
	from, to, err := n.rewindRange(r)
	if err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	entries, unavailable := n.Timeline(r.Context(), from, to)
	part.JSON(w, http.StatusOK, map[string]any{"from": from.UnixMilli(), "to": to.UnixMilli(), "entries": entries, "unavailable": unavailable})
}
