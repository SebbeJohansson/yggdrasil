// Package hermodr carries every message meant for you: into one history the
// widget reads, and, when you set it up, to your phone (ntfy). Urgent messages go at once; the rest wait through quiet hours, and
// through a meeting when they ask to hold.
//
//	POST /v1/messages              any part: { title, text?, link?, priority?, hold?, push? }
//	GET  /v1/messages              the last 7 days; ?state= and ?unseen=true
//	POST /v1/messages/{id}/seen    owner
//	GET  /v1/channels              each channel, quiet hours, in a meeting
//	POST /v1/channels/{name}/test  owner
package hermodr

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
	"yggdrasil/internal/part"
)

// Name is this part's name.
const Name = "hermodr"

const (
	// Keep is how long messages are listed.
	Keep = 7 * 24 * time.Hour
	// FailingAfter is how many failed sends in a row a channel is raised after.
	FailingAfter = 3
)

// Delivery is one channel's send of one message.
type Delivery struct {
	Channel string    `json:"channel"`
	OK      bool      `json:"ok"`
	Error   string    `json:"error,omitempty"`
	At      time.Time `json:"at"`
}

// Message is one thing told to you.
type Message struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Title    string `json:"title"`
	Text     string `json:"text,omitempty"`
	Link     string `json:"link,omitempty"`
	Priority string `json:"priority"`
	Hold     bool   `json:"hold"`
	Push     bool   `json:"push"`
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
	// NotBefore holds it until then, whatever its priority: "later" was asked for.
	NotBefore  *time.Time `json:"notBefore,omitempty"`
	At         time.Time  `json:"at"`
	SentAt     *time.Time `json:"sentAt,omitempty"`
	SeenAt     *time.Time `json:"seenAt,omitempty"`
	Deliveries []Delivery `json:"deliveries"`
}

// Quiet is the hours non-urgent messages wait through, in your zone; zero is none.
type Quiet struct {
	From, To int // minutes after midnight
	On       bool
}

// ParseQuiet reads "22:00-07:00"; empty or "off" is none.
func ParseQuiet(text string) (Quiet, error) {
	text = strings.TrimSpace(text)
	if text == "" || text == "off" {
		return Quiet{}, nil
	}
	from, to, ok := strings.Cut(text, "-")
	if !ok {
		return Quiet{}, fmt.Errorf("quiet hours look like 22:00-07:00, not %q", text)
	}
	a, errA := clock(from)
	b, errB := clock(to)
	if errA != nil || errB != nil || a == b {
		return Quiet{}, fmt.Errorf("quiet hours look like 22:00-07:00, not %q", text)
	}
	return Quiet{From: a, To: b, On: true}, nil
}

func clock(text string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(text))
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

func hhmm(minutes int) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}

// Until is when quiet hours end, if t is inside them.
func (q Quiet) Until(t time.Time) (time.Time, bool) {
	if !q.On {
		return time.Time{}, false
	}
	m := t.Hour()*60 + t.Minute()
	inside := (q.From < q.To && m >= q.From && m < q.To) || (q.From > q.To && (m >= q.From || m < q.To))
	if !inside {
		return time.Time{}, false
	}
	end := time.Date(t.Year(), t.Month(), t.Day(), q.To/60, q.To%60, 0, 0, t.Location())
	if !end.After(t) {
		end = end.AddDate(0, 0, 1)
	}
	return end, true
}

type meeting struct {
	Start, End int64
}

type channelState struct {
	lastSentAt *time.Time
	lastError  string
	failures   int
	raised     bool
}

// Hermodr is the running service.
type Hermodr struct {
	db       *sql.DB
	Channels []Channel
	// Min is each channel's lowest priority it sends.
	Min     map[string]string
	Quiet   Quiet
	Zone    *time.Location
	Norn    *call.Part
	Gjallar *alert.Client

	now      func() time.Time
	logf     func(string, ...any)
	mu       sync.Mutex
	meetings []meeting
	states   map[string]*channelState
	focus    Focus
}

const schema = `CREATE TABLE IF NOT EXISTS messages (id TEXT PRIMARY KEY, at INTEGER NOT NULL, state TEXT NOT NULL, body TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS messages_at ON messages (at);`

// New opens the store at path (":memory:" in tests).
func New(path string, channels []Channel, min map[string]string, quiet Quiet, zone *time.Location, norn *call.Part, gjallar *alert.Client, logf func(string, ...any)) (*Hermodr, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + focusSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("hermodr schema: %w", err)
	}
	h := &Hermodr{db: db, Channels: channels, Min: min, Quiet: quiet, Zone: zone, Norn: norn, Gjallar: gjallar,
		now: time.Now, logf: logf, states: map[string]*channelState{}}
	if err := h.loadFocus(); err != nil {
		db.Close()
		return nil, fmt.Errorf("hermodr focus: %w", err)
	}
	return h, nil
}

// Close closes the store.
func (h *Hermodr) Close() error {
	return h.db.Close()
}

// Mount adds the routes and loops.
func (h *Hermodr) Mount(p *part.Part) {
	p.Handle("POST /v1/messages", h.post)
	p.Handle("GET /v1/messages", h.list)
	p.Handle("POST /v1/messages/{id}/seen", h.seen)
	p.Handle("GET /v1/channels", h.channels)
	p.Handle("POST /v1/channels/{name}/test", h.test)
	p.Handle("GET /v1/focus", h.getFocus)
	p.Handle("PUT /v1/focus", h.putFocus)
	p.Handle("DELETE /v1/focus", h.endFocus)
	p.Handle("GET /v1/focus/history", h.getFocusHistory)
	p.Loop("meetings", 2*time.Minute, h.readMeetings)
	p.Loop("release", 30*time.Second, h.release)
}

// --- store ---

func (h *Hermodr) put(ctx context.Context, m Message) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = h.db.ExecContext(ctx, `INSERT INTO messages (id, at, state, body) VALUES (?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET state = excluded.state, body = excluded.body`, m.ID, m.At.UnixMilli(), m.State, string(body))
	return err
}

func (h *Hermodr) messages(ctx context.Context, state string, limit int) ([]Message, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT body FROM messages WHERE ? = '' OR state = ? ORDER BY at DESC LIMIT ?`, state, state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Message{}
	for rows.Next() {
		var body string
		var m Message
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(body), &m) == nil {
			list = append(list, m)
		}
	}
	return list, rows.Err()
}

func (h *Hermodr) message(ctx context.Context, id string) (Message, error) {
	var body string
	var m Message
	err := h.db.QueryRowContext(ctx, `SELECT body FROM messages WHERE id = ?`, id).Scan(&body)
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal([]byte(body), &m)
}

// --- routes ---

type input struct {
	Title     string     `json:"title"`
	Text      string     `json:"text"`
	Link      string     `json:"link"`
	Priority  string     `json:"priority"`
	Hold      bool       `json:"hold"`
	Push      *bool      `json:"push"`
	NotBefore *time.Time `json:"notBefore"`
}

// Later is how far ahead NotBefore may be.
const Later = 30 * 24 * time.Hour

func (h *Hermodr) post(w http.ResponseWriter, r *http.Request) {
	var in input
	if err := part.Decode(r, &in); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Priority == "" {
		in.Priority = "normal"
	}
	switch {
	case strings.TrimSpace(in.Title) == "" || utf8.RuneCountInString(in.Title) > 200:
		part.Error(w, http.StatusBadRequest, "title must be 1 to 200 characters")
		return
	case utf8.RuneCountInString(in.Text) > 4000 || len(in.Link) > 1000:
		part.Error(w, http.StatusBadRequest, "text is at most 4000 characters and link 1000")
		return
	case !slices.Contains(Priorities, in.Priority):
		part.Error(w, http.StatusBadRequest, "priority must be low, normal, high or urgent")
		return
	case in.NotBefore != nil && in.NotBefore.After(h.now().Add(Later)):
		part.Error(w, http.StatusBadRequest, "notBefore is at most 30 days ahead")
		return
	}
	m := Message{ID: newID(), Source: part.Caller(r), Title: strings.TrimSpace(in.Title), Text: in.Text, Link: in.Link,
		Priority: in.Priority, Hold: in.Hold, Push: in.Push == nil || *in.Push, NotBefore: in.NotBefore, At: h.now(), Deliveries: []Delivery{}}
	m = h.Take(r.Context(), m)
	part.JSON(w, http.StatusCreated, m)
}

// Take sends the message now, holds it, or only lists it, and keeps it.
func (h *Hermodr) Take(ctx context.Context, m Message) Message {
	if reason, held := h.holdFor(m); held {
		m.State, m.Reason = "held", reason
	} else {
		h.send(ctx, &m)
	}
	if err := h.put(ctx, m); err != nil {
		h.logf("keeping a message: %v", err)
	}
	return m
}

func (h *Hermodr) holdFor(m Message) (string, bool) {
	now := h.now().In(h.Zone)
	if m.NotBefore != nil && now.Before(*m.NotBefore) {
		return "later, at " + m.NotBefore.In(h.Zone).Format("Mon 15:04"), true
	}
	if f := h.focusNow(); f.On && m.Priority != "urgent" {
		return "you are focusing until " + f.Until.In(h.Zone).Format("15:04"), true
	}
	if !m.Push || m.Priority == "urgent" {
		return "", false
	}
	if until, quiet := h.Quiet.Until(now); quiet {
		return "quiet hours until " + until.Format("15:04"), true
	}
	if m.Hold {
		if until, busy := h.inMeeting(now); busy {
			return "in a meeting until " + time.UnixMilli(until).In(h.Zone).Format("15:04"), true
		}
	}
	return "", false
}

// send goes to every channel that is on and takes the message's priority.
func (h *Hermodr) send(ctx context.Context, m *Message) {
	now := h.now()
	m.Reason = ""
	if !m.Push {
		m.State = "listed"
		return
	}
	out := Out{Title: m.Title, Text: m.Text, Link: m.Link, Priority: m.Priority, Source: m.Source}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, channel := range h.Channels {
		if !channel.On() || Rank(m.Priority) < Rank(h.minOf(channel.Name())) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := channel.Send(ctx, out)
			h.result(ctx, channel.Name(), err)
			d := Delivery{Channel: channel.Name(), OK: err == nil, At: h.now()}
			if err != nil {
				d.Error = err.Error()
			}
			mu.Lock()
			m.Deliveries = append(m.Deliveries, d)
			mu.Unlock()
		}()
	}
	wg.Wait()
	slices.SortFunc(m.Deliveries, func(a, b Delivery) int { return strings.Compare(a.Channel, b.Channel) })
	switch {
	case len(m.Deliveries) == 0:
		m.State = "listed"
	case !slices.ContainsFunc(m.Deliveries, func(d Delivery) bool { return d.OK }):
		m.State = "failed"
	default:
		m.State = "sent"
		m.SentAt = &now
	}
}

func (h *Hermodr) minOf(channel string) string {
	if min, ok := h.Min[channel]; ok && min != "" {
		return min
	}
	return "normal"
}

// result keeps the channel's last send, and raises it on Gjallar after 3 failures in a row.
func (h *Hermodr) result(ctx context.Context, channel string, err error) {
	h.mu.Lock()
	state := h.states[channel]
	if state == nil {
		state = &channelState{}
		h.states[channel] = state
	}
	raise, resolve := false, false
	if err != nil {
		state.failures++
		state.lastError = err.Error()
		raise = state.failures >= FailingAfter && !state.raised
		state.raised = state.raised || raise
	} else {
		at := h.now()
		state.lastSentAt, state.lastError, state.failures = &at, "", 0
		resolve, state.raised = state.raised, false
	}
	h.mu.Unlock()
	key := "channel:" + channel
	switch {
	case raise:
		_ = h.Gjallar.Raise(ctx, alert.Raise{Key: key, Severity: alert.Warning, Title: fmt.Sprintf("Hermóðr can't send to %s", channel), Detail: err.Error()})
	case resolve:
		_ = h.Gjallar.Resolve(ctx, key)
	}
}

func (h *Hermodr) list(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state != "" && !slices.Contains([]string{"held", "sent", "listed", "failed"}, state) {
		part.Error(w, http.StatusBadRequest, "state must be held, sent, listed or failed")
		return
	}
	list, err := h.messages(r.Context(), state, 300)
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if r.URL.Query().Get("unseen") == "true" {
		list = slices.DeleteFunc(list, func(m Message) bool { return m.SeenAt != nil })
	}
	part.JSON(w, http.StatusOK, map[string]any{"messages": list})
}

func (h *Hermodr) seen(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only the owner may mark a message seen")
		return
	}
	m, err := h.message(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		part.Error(w, http.StatusNotFound, "no such message")
		return
	case err != nil:
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if m.SeenAt == nil {
		at := h.now()
		m.SeenAt = &at
		if err := h.put(r.Context(), m); err != nil {
			part.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	part.JSON(w, http.StatusOK, m)
}

func (h *Hermodr) channels(w http.ResponseWriter, _ *http.Request) {
	type view struct {
		Name       string     `json:"name"`
		On         bool       `json:"on"`
		Min        string     `json:"min"`
		LastSentAt *time.Time `json:"lastSentAt,omitempty"`
		LastError  string     `json:"lastError,omitempty"`
		Failures   int        `json:"failures"`
	}
	list := []view{}
	h.mu.Lock()
	for _, channel := range h.Channels {
		v := view{Name: channel.Name(), On: channel.On(), Min: h.minOf(channel.Name())}
		if state := h.states[channel.Name()]; state != nil {
			v.LastSentAt, v.LastError, v.Failures = state.lastSentAt, state.lastError, state.failures
		}
		list = append(list, v)
	}
	h.mu.Unlock()
	now := h.now().In(h.Zone)
	quiet := map[string]any{"now": false}
	if h.Quiet.On {
		_, inside := h.Quiet.Until(now)
		quiet = map[string]any{"from": hhmm(h.Quiet.From), "to": hhmm(h.Quiet.To), "now": inside}
	}
	answer := map[string]any{"channels": list, "quiet": quiet, "inMeeting": false}
	if until, busy := h.inMeeting(now); busy {
		answer["inMeeting"], answer["meetingEndsAt"] = true, time.UnixMilli(until)
	}
	part.JSON(w, http.StatusOK, answer)
}

func (h *Hermodr) test(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only the owner may send a test")
		return
	}
	name := r.PathValue("name")
	for _, channel := range h.Channels {
		if channel.Name() != name {
			continue
		}
		if !channel.On() {
			part.Error(w, http.StatusNotFound, name+" is not set up in hermodr.env")
			return
		}
		err := channel.Send(r.Context(), Out{Title: "Hermóðr test", Text: "If you can read this, " + name + " works.", Priority: "normal", Source: Name})
		h.result(r.Context(), name, err)
		if err != nil {
			part.Error(w, http.StatusBadGateway, err.Error())
			return
		}
		part.JSON(w, http.StatusOK, Delivery{Channel: name, OK: true, At: h.now()})
		return
	}
	part.Error(w, http.StatusNotFound, "no channel called "+name)
}

// --- meetings and held messages ---

func (h *Hermodr) inMeeting(now time.Time) (int64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	nowMs := now.UnixMilli()
	var until int64
	for _, m := range h.meetings {
		if m.Start <= nowMs && nowMs < m.End && m.End > until {
			until = m.End
		}
	}
	return until, until > 0
}

// CalendarEvent is the little of Norn's meeting Hermóðr reads.
type CalendarEvent struct {
	Start    int64  `json:"start"`
	End      int64  `json:"end"`
	AllDay   bool   `json:"allDay"`
	Response string `json:"response"`
	ShowAs   string `json:"showAs"`
}

// SetMeetings replaces what Hermóðr knows of your calendars.
func (h *Hermodr) SetMeetings(events []CalendarEvent) {
	var meetings []meeting
	for _, e := range events {
		if !e.AllDay && e.Response != "declined" && e.ShowAs != "free" {
			meetings = append(meetings, meeting{Start: e.Start, End: e.End})
		}
	}
	h.mu.Lock()
	h.meetings = meetings
	h.mu.Unlock()
}

func (h *Hermodr) readMeetings(ctx context.Context) error {
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

func (h *Hermodr) release(ctx context.Context) error {
	if _, err := h.db.ExecContext(ctx, `DELETE FROM messages WHERE at < ? AND state != 'held'`, h.now().Add(-Keep).UnixMilli()); err != nil {
		return err
	}
	held, err := h.messages(ctx, "held", 300)
	if err != nil {
		return err
	}
	for _, m := range held {
		if reason, still := h.holdFor(m); still {
			if reason != m.Reason {
				m.Reason = reason
				_ = h.put(ctx, m)
			}
			continue
		}
		h.send(ctx, &m)
		if err := h.put(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
