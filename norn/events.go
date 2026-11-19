package norn

// What just changed, as server-sent events, for parts that react to it
// (Huginn): an inbox item worth your attention, a reply draft written for one,
// and meetings that appear, move, go away or end. Events carry Norn's summaries, never raw inbox text.
// The last 500 are kept in memory, so a listener that reconnects with
// Last-Event-ID gets what it missed while Norn kept running.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/part"
)

const (
	// EventsKept is how many events a reconnecting listener can catch up on.
	EventsKept = 500
	// EventsAhead is how far ahead a new or moved meeting is told about.
	EventsAhead = 24 * time.Hour
	// EndedEvery is how often Norn looks for meetings that just ended, and
	// for meetings about to start.
	EndedEvery = 30 * time.Second
	// SoonLead is how long before its start calendar.soon tells about a meeting.
	SoonLead = 30 * time.Minute
)

// InboxArrived is an inbox item without its text.
type InboxArrived struct {
	ID           string   `json:"id"`
	Source       string   `json:"source"`
	From         string   `json:"from"`
	Relation     string   `json:"relation,omitempty"`
	Band         string   `json:"band"`
	Reasons      []string `json:"reasons"`
	Direct       bool     `json:"direct"`
	MentionsMe   bool     `json:"mentionsMe"`
	Summary      string   `json:"summary"`
	Conversation string   `json:"conversation,omitempty"`
	Link         string   `json:"link,omitempty"`
	At           int64    `json:"at"`
}

// DraftReady is a reply draft Norn wrote by itself, for the inbox item it
// answers: the item's fields as inbox.arrived has them, without its text or
// the draft's, and how the draft reads.
type DraftReady struct {
	ID         string `json:"id"`
	Source     string `json:"source"`
	From       string `json:"from"`
	Relation   string `json:"relation,omitempty"`
	Band       string `json:"band"`
	Direct     bool   `json:"direct"`
	MentionsMe bool   `json:"mentionsMe"`
	Tone       string `json:"tone,omitempty"`
	Link       string `json:"link,omitempty"`
	At         int64  `json:"at"`
}

// CalendarChange is a meeting that appeared, moved, went away or ended.
type CalendarChange struct {
	Calendar      string        `json:"calendar"`
	Event         CalendarEvent `json:"event"`
	PreviousStart int64         `json:"previousStart,omitempty"`
	PreviousEnd   int64         `json:"previousEnd,omitempty"`
	// Prompt is what you add to the brief's prompt for this meeting (its
	// series), and Model what writes it ("local", "claude" or "" when you
	// never picked one), on calendar.soon, .added and .moved.
	Prompt string `json:"prompt,omitempty"`
	Model  string `json:"model,omitempty"`
	// Invite is the invite that covers your copy (Event), on
	// calendar.duplicate and calendar.stale.
	Invite *CalendarEvent `json:"invite,omitempty"`
}

// StreamEvent is one event as it goes out.
type StreamEvent struct {
	ID   int64
	Kind string
	Data []byte
}

// Events is the stream's state: what was sent, who listens, and what was
// last seen in the inbox and the calendars to tell what changed.
type Events struct {
	mu        sync.Mutex
	next      int64
	kept      []StreamEvent
	listeners map[chan StreamEvent]struct{}

	inboxPrimed bool
	inboxSeen   map[string]bool
	calendars   map[string]map[string]CalendarEvent
	endedAfter  time.Time
	// soonFrom is when Norn started: a meeting already that close by then
	// is not told about, so a restart doesn't repeat it.
	soonFrom time.Time
	soonSent map[string]bool
	// PrepFor is how a meeting's brief is written (your addition to its
	// prompt, and the model); nil for none.
	PrepFor func(CalendarEvent) MeetingPrep
}

// NewEvents starts with nothing seen; meetings that end from now on count.
// Ids start from now in ms, so they keep growing across a restart: a
// listener reconnecting with the last process's Last-Event-ID still gets
// what this one sent before it came back.
func NewEvents(now time.Time) *Events {
	return &Events{next: now.UnixMilli(), listeners: map[chan StreamEvent]struct{}{}, inboxSeen: map[string]bool{},
		calendars: map[string]map[string]CalendarEvent{}, endedAfter: now, soonFrom: now, soonSent: map[string]bool{}}
}

// Publish sends an event to every listener and keeps it for catching up.
func (e *Events) Publish(kind string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.publishLocked(kind, payload)
}

func (e *Events) publishLocked(kind string, payload []byte) {
	e.next++
	event := StreamEvent{ID: e.next, Kind: kind, Data: payload}
	e.kept = append(e.kept, event)
	if len(e.kept) > EventsKept {
		e.kept = e.kept[len(e.kept)-EventsKept:]
	}
	for listener := range e.listeners {
		select {
		case listener <- event:
		default:
			// A listener that does not keep up misses it; it can reconnect with Last-Event-ID.
		}
	}
}

// Listening is whether anyone follows the stream right now.
func (e *Events) Listening() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.listeners) > 0
}

// subscribe returns what came after lastID, and a channel for what comes next.
func (e *Events) subscribe(lastID int64) ([]StreamEvent, chan StreamEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var missed []StreamEvent
	if lastID > 0 {
		for _, event := range e.kept {
			if event.ID > lastID {
				missed = append(missed, event)
			}
		}
	}
	listener := make(chan StreamEvent, 64)
	e.listeners[listener] = struct{}{}
	return missed, listener
}

func (e *Events) unsubscribe(listener chan StreamEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.listeners, listener)
}

// NewInboxItems is the urgent and notable items not seen before. The first
// poll after Norn starts only takes note of what is there, so a restart
// doesn't send the whole inbox again.
func (e *Events) NewInboxItems(shown []Ranked) []Ranked {
	e.mu.Lock()
	defer e.mu.Unlock()
	present := map[string]bool{}
	var fresh []Ranked
	for _, entry := range shown {
		id := entry.Item.ID
		present[id] = true
		if e.inboxPrimed && !e.inboxSeen[id] && (entry.Band == "urgent" || entry.Band == "notable") {
			fresh = append(fresh, entry)
		}
	}
	e.inboxSeen, e.inboxPrimed = present, true
	return fresh
}

func declined(event CalendarEvent) bool {
	return event.Response == "declined"
}

// Calendar compares a calendar's new read with the last one and sends what
// changed. The first read only takes note. Meetings without an id are left
// out: there is no telling whether one moved.
func (e *Events) Calendar(calendar string, events []CalendarEvent, now time.Time) {
	current := map[string]CalendarEvent{}
	for _, event := range events {
		// A break you kept free, or a focus block, is not a meeting: nothing to brief on.
		if event.ID != "" && !event.AllDay && !event.Break && !event.Focus {
			current[event.ID] = event
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	previous, primed := e.calendars[calendar]
	e.calendars[calendar] = current
	if !primed {
		return
	}
	nowMs, horizon := now.UnixMilli(), now.Add(EventsAhead).UnixMilli()
	soon := func(start int64) bool { return start > nowMs && start <= horizon }
	send := func(kind string, change CalendarChange) {
		if kind != "calendar.removed" {
			prep := e.prep(change.Event)
			change.Prompt, change.Model = prep.Prompt, prep.Model
		}
		payload, err := json.Marshal(change)
		if err == nil {
			e.publishLocked(kind, payload)
		}
	}
	for id, event := range current {
		before, known := previous[id]
		switch {
		case declined(event):
			if known && !declined(before) && soon(before.Start) {
				send("calendar.removed", CalendarChange{Calendar: calendar, Event: event})
			}
		case !known || declined(before):
			if soon(event.Start) {
				send("calendar.added", CalendarChange{Calendar: calendar, Event: event})
			}
		case before.Start != event.Start || before.End != event.End:
			if soon(event.Start) || soon(before.Start) {
				send("calendar.moved", CalendarChange{Calendar: calendar, Event: event, PreviousStart: before.Start, PreviousEnd: before.End})
			}
		}
	}
	for id, before := range previous {
		if _, still := current[id]; !still && !declined(before) && soon(before.Start) {
			send("calendar.removed", CalendarChange{Calendar: calendar, Event: before})
		}
	}
}

// Ended sends calendar.ended for each meeting you didn't decline whose end
// passed since the last look.
func (e *Events) Ended(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	after, nowMs := e.endedAfter.UnixMilli(), now.UnixMilli()
	e.endedAfter = now
	for calendar, events := range e.calendars {
		for _, event := range events {
			if !declined(event) && event.End > after && event.End <= nowMs {
				payload, err := json.Marshal(CalendarChange{Calendar: calendar, Event: event})
				if err == nil {
					e.publishLocked("calendar.ended", payload)
				}
			}
		}
	}
}

// Soon sends calendar.soon once for each meeting you didn't decline that
// starts within SoonLead: every meeting, not only one just added or moved.
// A meeting that moves is told about again for its new start.
func (e *Events) Soon(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	nowMs, horizon, from := now.UnixMilli(), now.Add(SoonLead).UnixMilli(), e.soonFrom.Add(SoonLead).UnixMilli()
	for key := range e.soonSent {
		if start, err := strconv.ParseInt(key[strings.LastIndexByte(key, '@')+1:], 10, 64); err == nil && start <= nowMs {
			delete(e.soonSent, key)
		}
	}
	for calendar, events := range e.calendars {
		for id, event := range events {
			key := fmt.Sprintf("%s@%d", id, event.Start)
			if declined(event) || event.Start <= nowMs || event.Start > horizon || event.Start <= from || e.soonSent[key] {
				continue
			}
			e.soonSent[key] = true
			prep := e.prep(event)
			payload, err := json.Marshal(CalendarChange{Calendar: calendar, Event: event, Prompt: prep.Prompt, Model: prep.Model})
			if err == nil {
				e.publishLocked("calendar.soon", payload)
			}
		}
	}
}

func (e *Events) prep(event CalendarEvent) MeetingPrep {
	if e.PrepFor == nil {
		return MeetingPrep{}
	}
	return e.PrepFor(event)
}

// inboxEvents sends inbox.arrived for each new item worth it, each with a
// one-line summary of its own.
func (n *Norn) inboxEvents(ctx context.Context, shown []Ranked, now time.Time) {
	for _, entry := range n.Events.NewInboxItems(shown) {
		summary, _ := n.Summarise(ctx, []Ranked{entry}, 1, now, SourceScope[entry.Item.Source])
		reasons := entry.Reasons
		if reasons == nil {
			reasons = []string{}
		}
		n.Events.Publish("inbox.arrived", InboxArrived{
			ID: entry.Item.ID, Source: entry.Item.Source, From: entry.Item.From, Relation: entry.Item.Relation,
			Band: entry.Band, Reasons: reasons, Direct: entry.Item.Direct, MentionsMe: entry.Item.MentionsMe,
			Summary: summary, Conversation: entry.Item.Conversation, Link: entry.Item.Link, At: entry.Item.At,
		})
	}
}

func (n *Norn) calendarEvents(calendar string, c *Calendar) {
	now := time.Now()
	if snapshot := c.Current(now); snapshot != nil {
		n.Events.Calendar(calendar, snapshot.Events, now)
	}
}

func (n *Norn) mountEvents(p *part.Part) {
	p.Handle("GET /v1/events/stream", n.eventStream)
	p.Loop("meetings-ended", EndedEvery, func(context.Context) error {
		now := time.Now()
		n.Events.Ended(now)
		n.Events.Soon(now)
		return nil
	})
}

func (n *Norn) eventStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		part.Error(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}
	lastID, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	missed, listener := n.Events.subscribe(lastID)
	defer n.Events.unsubscribe(listener)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": norn\n\n")
	for _, event := range missed {
		writeEvent(w, event)
	}
	flusher.Flush()
	keepAlive := time.NewTicker(25 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			fmt.Fprint(w, ": keep-alive\n\n")
		case event := <-listener:
			writeEvent(w, event)
		}
		flusher.Flush()
	}
}

func writeEvent(w http.ResponseWriter, event StreamEvent) {
	fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", event.Kind, event.ID, event.Data)
}
