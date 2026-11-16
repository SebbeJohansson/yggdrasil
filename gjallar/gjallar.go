// Package gjallar is Yggdrasil's alerts service, named for Gjallarhorn, the
// horn that warns all the worlds.
//
// Every part sends its warnings here, and Gjallar decides how loudly to tell
// you. See docs/architecture.md for the design; in short:
//
//   - An alert is identified by its source and key. Sending the same pair
//     again updates it (count, last seen) instead of adding a second one.
//   - The source is the name on the sender's token, so no part can raise or
//     resolve another part's alerts. Only you (the owner token) acknowledge,
//     snooze and mute.
//   - info shows on the dashboard, warning adds a widget badge, critical a
//     widget toast and a phone push (ntfy), at most once per alert per 15 min.
//   - Gjallar never acts on an alert.
//
// Routes (all but /health need a token):
//
//	GET  /health
//	POST /v1/alerts                  sender: raise or update { key, severity, title, detail?, link?, traceId?, expiresAt? }
//	POST /v1/alerts/resolve          sender: { key }, the condition is gone
//	GET  /v1/alerts                  ?state=active|open|acknowledged|resolved|all &limit=
//	GET  /v1/alerts/mine             sender: its own open and acknowledged alerts (owner: none)
//	GET  /v1/alerts/stream           server-sent events: every change, as `alert`
//	GET  /v1/alerts/{id}             the alert and its history
//	POST /v1/alerts/{id}/ack         owner
//	POST /v1/alerts/{id}/snooze      owner: { until }
//	GET  /v1/mutes                   owner
//	POST /v1/mutes                   owner: { source, key, muted }
//	POST /v1/heartbeat               sender: I am alive
package gjallar

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/part"
	"yggdrasil/internal/tell"
)

// Name is this part's name, and the source of the alerts Gjallar raises itself.
const Name = "gjallar"

// PushEvery is the least time between two pushes for the same alert.
const PushEvery = 15 * time.Minute

// Limits on what a sender may send. Alerts carry titles, not message bodies.
const (
	maxKey    = 200
	maxTitle  = 200
	maxDetail = 2000
	maxLink   = 1000
)

// Gjallar is the running service.
type Gjallar struct {
	Store *Store
	// Push tells you of a critical alert: Hermóðr, then ntfy straight.
	Push  tell.Teller
	// Heartbeats names the senders expected to beat every minute (Ratatoskr),
	// and how long a silence may last before Gjallar raises it.
	Heartbeats map[string]time.Duration
	// LimitedSources may send info and warning only (Glim).
	LimitedSources map[string]bool
	// DashboardURL makes the phone push open the alert.
	DashboardURL string

	started time.Time
	hub     hub
	now     func() time.Time
}

// New makes a Gjallar over the store.
func New(store *Store, push tell.Teller) *Gjallar {
	return &Gjallar{Store: store, Push: push, Heartbeats: map[string]time.Duration{}, LimitedSources: map[string]bool{}, started: time.Now(), now: time.Now}
}

// Mount adds Gjallar's routes and loops to the part.
func (g *Gjallar) Mount(p *part.Part) {
	p.Handle("POST /v1/alerts", g.raise)
	p.Handle("POST /v1/alerts/resolve", g.resolve)
	p.Handle("GET /v1/alerts", g.list)
	p.Handle("GET /v1/alerts/stream", g.stream)
	p.Handle("GET /v1/alerts/mine", g.mine)
	p.Handle("GET /v1/alerts/{id}", g.get)
	p.Handle("POST /v1/alerts/{id}/ack", g.owner(g.ack))
	p.Handle("POST /v1/alerts/{id}/snooze", g.owner(g.snooze))
	p.Handle("GET /v1/mutes", g.owner(g.mutes))
	p.Handle("POST /v1/mutes", g.owner(g.mute))
	p.Handle("POST /v1/heartbeat", g.heartbeat)
	p.Loop("expire", 30*time.Second, g.expire)
	p.Loop("heartbeats", 30*time.Second, g.checkHeartbeats)
}

func (g *Gjallar) owner(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if part.Caller(r) != part.Owner {
			part.Error(w, http.StatusForbidden, "only the owner may do this")
			return
		}
		handler(w, r)
	}
}

func validate(raise alert.Raise) error {
	switch {
	case raise.Key == "" || utf8.RuneCountInString(raise.Key) > maxKey:
		return fmt.Errorf("key must be 1 to %d characters", maxKey)
	case strings.TrimSpace(raise.Title) == "" || utf8.RuneCountInString(raise.Title) > maxTitle:
		return fmt.Errorf("title must be 1 to %d characters", maxTitle)
	case utf8.RuneCountInString(raise.Detail) > maxDetail:
		return fmt.Errorf("detail must be at most %d characters", maxDetail)
	case len(raise.Link) > maxLink:
		return fmt.Errorf("link must be at most %d characters", maxLink)
	case !raise.Severity.Valid():
		return errors.New("severity must be info, warning or critical")
	}
	return nil
}

func (g *Gjallar) raise(w http.ResponseWriter, r *http.Request) {
	source := part.Caller(r)
	if source == part.Owner {
		part.Error(w, http.StatusForbidden, "alerts are raised by parts, not by the owner")
		return
	}
	var raise alert.Raise
	if err := part.Decode(r, &raise); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validate(raise); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if g.LimitedSources[source] && raise.Severity == alert.Critical {
		part.Error(w, http.StatusForbidden, source+" may send info and warning only")
		return
	}
	a, change, err := g.Store.Raise(r.Context(), source, raise)
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	g.changed(r.Context(), a, change)
	status := http.StatusOK
	if change == Opened {
		status = http.StatusCreated
	}
	part.JSON(w, status, map[string]any{"alert": a, "change": change})
}

// Raise is how Gjallar raises its own alerts (a silent heartbeat).
func (g *Gjallar) Raise(ctx context.Context, raise alert.Raise) error {
	a, change, err := g.Store.Raise(ctx, Name, raise)
	if err == nil {
		g.changed(ctx, a, change)
	}
	return err
}

// changed tells the listeners, and pushes a critical alert that is new,
// opened again or escalated, unless it is muted, snoozed or pushed recently.
func (g *Gjallar) changed(ctx context.Context, a Alert, change Change) {
	g.hub.publish(a)
	if change == Updated || a.Severity != alert.Critical || a.Muted || g.Push == nil || !g.Push.On() {
		return
	}
	now := g.now()
	if a.SnoozedUntil != nil && a.SnoozedUntil.After(now) {
		return
	}
	if a.pushedAt != nil && now.Sub(*a.pushedAt) < PushEvery {
		return
	}
	link := ""
	if g.DashboardURL != "" {
		link = fmt.Sprintf("%s/alerts/%d", strings.TrimRight(g.DashboardURL, "/"), a.ID)
	}
	// Recorded before the push, so a slow push is not sent twice.
	if err := g.Store.MarkPushed(ctx, a.ID); err != nil {
		return
	}
	go func() {
		pushCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = g.Push.Tell(pushCtx, tell.Message{Title: a.Title, Text: fmt.Sprintf("%s · %s", a.Source, orNone(a.Detail)), Priority: "urgent", Link: link})
	}()
}

func orNone(text string) string {
	if text == "" {
		return "no detail"
	}
	return text
}

func (g *Gjallar) resolve(w http.ResponseWriter, r *http.Request) {
	source := part.Caller(r)
	if source == part.Owner {
		part.Error(w, http.StatusForbidden, "only the part that raised an alert resolves it; acknowledge it instead")
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := part.Decode(r, &body); err != nil || body.Key == "" {
		part.Error(w, http.StatusBadRequest, "body must be { key }")
		return
	}
	a, found, err := g.Store.Resolve(r.Context(), source, body.Key, source)
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if found {
		g.hub.publish(a)
	}
	part.JSON(w, http.StatusOK, map[string]any{"resolved": found})
}

func (g *Gjallar) list(w http.ResponseWriter, r *http.Request) {
	filter, err := ParseFilter(r.URL.Query().Get("state"))
	if err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := 200
	if text := r.URL.Query().Get("limit"); text != "" {
		if limit, err = strconv.Atoi(text); err != nil || limit < 1 || limit > 1000 {
			part.Error(w, http.StatusBadRequest, "limit must be 1 to 1000")
			return
		}
	}
	alerts, err := g.Store.List(r.Context(), filter, limit)
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"alerts": alerts})
}

// mine is the caller's own active alerts: what a part raised before it
// restarted, so it can resolve them when they come back. The owner raises
// none, so it has none.
func (g *Gjallar) mine(w http.ResponseWriter, r *http.Request) {
	source := part.Caller(r)
	if source == part.Owner {
		part.JSON(w, http.StatusOK, map[string]any{"alerts": []Alert{}})
		return
	}
	alerts, err := g.Store.Held(r.Context(), source)
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"alerts": alerts})
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (g *Gjallar) get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		part.Error(w, http.StatusNotFound, "no such alert")
		return
	}
	a, err := g.Store.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		part.Error(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	events, err := g.Store.Events(r.Context(), id)
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"alert": a, "events": events})
}

func (g *Gjallar) ownerResult(w http.ResponseWriter, a Alert, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		part.Error(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrResolved):
		part.Error(w, http.StatusConflict, err.Error())
	case err != nil:
		part.Error(w, http.StatusInternalServerError, err.Error())
	default:
		g.hub.publish(a)
		part.JSON(w, http.StatusOK, map[string]any{"alert": a})
	}
}

func (g *Gjallar) ack(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		part.Error(w, http.StatusNotFound, "no such alert")
		return
	}
	a, err := g.Store.Acknowledge(r.Context(), id, part.Owner)
	g.ownerResult(w, a, err)
}

func (g *Gjallar) snooze(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		part.Error(w, http.StatusNotFound, "no such alert")
		return
	}
	var body struct {
		Until time.Time `json:"until"`
	}
	if err := part.Decode(r, &body); err != nil {
		part.Error(w, http.StatusBadRequest, "body must be { until } as an RFC 3339 time")
		return
	}
	now := g.now()
	if !body.Until.After(now) || body.Until.Sub(now) > 30*24*time.Hour {
		part.Error(w, http.StatusBadRequest, "until must be in the next 30 days")
		return
	}
	a, err := g.Store.Snooze(r.Context(), id, body.Until, part.Owner)
	g.ownerResult(w, a, err)
}

func (g *Gjallar) mutes(w http.ResponseWriter, r *http.Request) {
	mutes, err := g.Store.Mutes(r.Context())
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"mutes": mutes})
}

func (g *Gjallar) mute(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source string `json:"source"`
		Key    string `json:"key"`
		Muted  bool   `json:"muted"`
	}
	if err := part.Decode(r, &body); err != nil || body.Source == "" || body.Key == "" {
		part.Error(w, http.StatusBadRequest, "body must be { source, key, muted }")
		return
	}
	if err := g.Store.SetMute(r.Context(), body.Source, body.Key, body.Muted); err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	g.mutes(w, r)
}

func (g *Gjallar) heartbeat(w http.ResponseWriter, r *http.Request) {
	source := part.Caller(r)
	if err := g.Store.Beat(r.Context(), source); err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// expire resolves every alert whose expiresAt has passed.
func (g *Gjallar) expire(ctx context.Context) error {
	expired, err := g.Store.Expired(ctx)
	if err != nil {
		return err
	}
	for _, a := range expired {
		resolved, found, err := g.Store.Resolve(ctx, a.Source, a.Key, Name)
		if err != nil {
			return err
		}
		if found {
			g.hub.publish(resolved)
		}
	}
	return nil
}

// checkHeartbeats raises "<name> is silent" for an expected sender that has
// not beaten within its allowance, and resolves it once it beats again.
// Gjallar's own start counts as a beat, so a restart does not raise them all.
func (g *Gjallar) checkHeartbeats(ctx context.Context) error {
	now := g.now()
	for source, allowance := range g.Heartbeats {
		last, err := g.Store.LastBeat(ctx, source)
		if err != nil {
			return err
		}
		if last.Before(g.started) {
			last = g.started
		}
		key := "silent:" + source
		if now.Sub(last) <= allowance {
			resolved, found, err := g.Store.Resolve(ctx, Name, key, Name)
			if err != nil {
				return err
			}
			if found {
				g.hub.publish(resolved)
			}
			continue
		}
		err = g.Raise(ctx, alert.Raise{
			Key:      key,
			Severity: alert.Warning,
			Title:    fmt.Sprintf("%s is silent", source),
			Detail:   fmt.Sprintf("no heartbeat since %s", last.Format(time.RFC3339)),
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// hub hands every change to the open streams.
type hub struct {
	mu   sync.Mutex
	subs map[chan Alert]struct{}
}

func (h *hub) subscribe() chan Alert {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs == nil {
		h.subs = map[chan Alert]struct{}{}
	}
	ch := make(chan Alert, 32)
	h.subs[ch] = struct{}{}
	return ch
}

func (h *hub) unsubscribe(ch chan Alert) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, ch)
}

func (h *hub) publish(a Alert) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- a:
		default:
			// A listener that does not keep up misses a change; it reloads the list.
		}
	}
}
