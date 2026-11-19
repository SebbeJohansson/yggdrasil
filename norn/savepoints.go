package norn

// Save points: when a meeting starts, Norn notes what you were doing (the
// goal most recently active, your most recently active own Claude Code
// session, your last prompt there), and when it ends, hands it back with
// savepoint.ready, for Huginn to tell you and the dashboard's "You were here"
// card. Everything stays on this machine: Norn's store, its event stream,
// Huginn and Hermóðr's widget list. Nothing here goes to Claude.
//
//	GET  /v1/savepoints?limit=      recent ones, newest first
//	POST /v1/savepoints/{id}/done   put one away

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"yggdrasil/internal/part"
)

const savePointSchema = `CREATE TABLE IF NOT EXISTS save_points (
	id INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	meeting_id TEXT NOT NULL,
	meeting_end INTEGER NOT NULL,
	body TEXT NOT NULL,
	ready_at INTEGER,
	done_at INTEGER
);
CREATE INDEX IF NOT EXISTS save_points_by_at ON save_points (at);`

const (
	// SavePointActive is how recently a goal or session must have moved to count as what you were doing.
	SavePointActive = 30 * time.Minute
	// SavePointPrompt is how much of your last prompt is kept.
	SavePointPrompt = 200
	// SavePointLate is how long after its meeting a save point is still handed back.
	SavePointLate = 2 * time.Hour
	// savePointsKept is how many are kept; older ones are removed.
	savePointsKept = 200
)

// SavedGoal is the goal you were on.
type SavedGoal struct {
	ID           int64  `json:"id"`
	Title        string `json:"title"`
	LastActivity int64  `json:"lastActivity"`
}

// SavedSession is your own Claude Code session you were in.
type SavedSession struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Cwd          string `json:"cwd,omitempty"`
	Project      string `json:"project,omitempty"`
	Branch       string `json:"branch,omitempty"`
	LastPrompt   string `json:"lastPrompt,omitempty"`
	LastActivity int64  `json:"lastActivity"`
}

// SavePoint is what you were doing when a meeting started.
type SavePoint struct {
	ID int64 `json:"id"`
	// At is when it was saved: the meeting's start, epoch ms.
	At           int64  `json:"at"`
	Meeting      string `json:"meeting"`
	MeetingID    string `json:"meetingId"`
	MeetingStart int64  `json:"meetingStart"`
	MeetingEnd   int64  `json:"meetingEnd"`
	// Title is the goal's or the session's, whichever moved last.
	Title string `json:"title"`
	// Link is the dashboard path that picks it up: the goal, else the session.
	Link    string        `json:"link"`
	Goal    *SavedGoal    `json:"goal"`
	Session *SavedSession `json:"session"`
	// ReadyAt is when the meeting ended and it was handed back; null before.
	ReadyAt *int64 `json:"readyAt"`
	// DoneAt is when you put it away; null before.
	DoneAt *int64 `json:"doneAt"`
}

// --- what counts -----------------------------------------------------------------

// SavePointMeeting is whether a meeting is one to keep your place through:
// not declined, not all day, not shown as free, not a break you kept free
// or a focus block you booked.
func SavePointMeeting(event CalendarEvent) bool {
	return event.ID != "" && !declined(event) && !event.AllDay && event.ShowAs != "free" && !event.Break && !event.Focus
}

// GoalActivity is the little of Sindri's goal a save point needs.
type GoalActivity struct {
	ID       int64  `json:"id"`
	At       int64  `json:"at"`
	Title    string `json:"title"`
	State    string `json:"state"`
	ClosedAt *int64 `json:"closedAt"`
	Tasks    []struct {
		LastActivity int64 `json:"lastActivity"`
	} `json:"tasks"`
}

// last is when the goal last moved: made, or one of its agents active.
func (g GoalActivity) last() int64 {
	last := g.At
	for _, task := range g.Tasks {
		last = max(last, task.LastActivity)
	}
	return last
}

// PickSavePoint is what you were doing at now: the open goal and your own
// session that moved last within SavePointActive. Nil, nil when nothing did.
func PickSavePoint(goals []GoalActivity, sessions []OwnSession, now time.Time) (*SavedGoal, *SavedSession) {
	from, nowMs := now.Add(-SavePointActive).UnixMilli(), now.UnixMilli()
	recent := func(at int64) bool { return at >= from && at <= nowMs }
	var goal *SavedGoal
	for _, g := range goals {
		if g.ClosedAt != nil || g.State == "merged" || g.State == "closed" {
			continue
		}
		if last := g.last(); recent(last) && (goal == nil || last > goal.LastActivity) {
			goal = &SavedGoal{ID: g.ID, Title: g.Title, LastActivity: last}
		}
	}
	var session *SavedSession
	for _, s := range sessions {
		if recent(s.LastActivity) && (session == nil || s.LastActivity > session.LastActivity) {
			session = &SavedSession{ID: s.ID, Title: s.Title, Cwd: s.Cwd, Project: s.Project, Branch: s.Branch,
				LastPrompt: clipPrompt(s.LastPrompt), LastActivity: s.LastActivity}
		}
	}
	return goal, session
}

func clipPrompt(text string) string {
	runes := []rune(text)
	if len(runes) > SavePointPrompt {
		return string(runes[:SavePointPrompt-1]) + "…"
	}
	return text
}

// NewSavePoint is the save point for a meeting starting at now; nil when nothing was active.
func NewSavePoint(meeting CalendarEvent, goal *SavedGoal, session *SavedSession, now time.Time) *SavePoint {
	if goal == nil && session == nil {
		return nil
	}
	sp := &SavePoint{At: now.UnixMilli(), Meeting: meeting.Subject, MeetingID: meeting.ID, MeetingStart: meeting.Start,
		MeetingEnd: meeting.End, Goal: goal, Session: session}
	if goal != nil && (session == nil || goal.LastActivity >= session.LastActivity) {
		sp.Title, sp.Link = goal.Title, fmt.Sprintf("/dashboard/goals/%d", goal.ID)
	} else {
		sp.Title, sp.Link = session.Title, SessionPath(session.ID, session.Cwd, session.Title)
	}
	return sp
}

// SessionPath is the dashboard's page for one of your sessions.
func SessionPath(id, cwd, title string) string {
	query := url.Values{"name": {title}}
	if cwd != "" {
		query.Set("dir", cwd)
	}
	return "/dashboard/sessions/" + url.PathEscape(id) + "?" + query.Encode()
}

// --- the store ---------------------------------------------------------------------

// AddSavePoint keeps a new save point, and drops the oldest past savePointsKept.
func (s *Store) AddSavePoint(ctx context.Context, sp *SavePoint) error {
	body, _ := json.Marshal(sp)
	result, err := s.db.ExecContext(ctx, `INSERT INTO save_points (at, meeting_id, meeting_end, body) VALUES (?, ?, ?, ?)`,
		sp.At, sp.MeetingID, sp.MeetingEnd, string(body))
	if err != nil {
		return err
	}
	sp.ID, _ = result.LastInsertId()
	_, err = s.db.ExecContext(ctx, `DELETE FROM save_points WHERE id NOT IN (SELECT id FROM save_points ORDER BY id DESC LIMIT ?)`, savePointsKept)
	return err
}

func scanSavePoint(row interface{ Scan(...any) error }) (SavePoint, error) {
	var sp SavePoint
	var id int64
	var body string
	var ready, done sql.NullInt64
	if err := row.Scan(&id, &body, &ready, &done); err != nil {
		return sp, err
	}
	_ = json.Unmarshal([]byte(body), &sp)
	sp.ID, sp.ReadyAt, sp.DoneAt = id, nil, nil
	if ready.Valid {
		sp.ReadyAt = &ready.Int64
	}
	if done.Valid {
		sp.DoneAt = &done.Int64
	}
	return sp, nil
}

func (s *Store) savePoints(ctx context.Context, query string, args ...any) ([]SavePoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, body, ready_at, done_at FROM save_points `+query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SavePoint{}
	for rows.Next() {
		sp, err := scanSavePoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// SavePoints is the most recent save points, newest first.
func (s *Store) SavePoints(ctx context.Context, limit int) ([]SavePoint, error) {
	return s.savePoints(ctx, `ORDER BY at DESC, id DESC LIMIT ?`, limit)
}

// WaitingSavePoints is the save points whose meeting has not been seen to end.
func (s *Store) WaitingSavePoints(ctx context.Context) ([]SavePoint, error) {
	return s.savePoints(ctx, `WHERE ready_at IS NULL ORDER BY id`)
}

// SavePointReady marks a save point handed back at at.
func (s *Store) SavePointReady(ctx context.Context, id, at int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE save_points SET ready_at = ? WHERE id = ? AND ready_at IS NULL`, at, id)
	return err
}

// SavePointDone puts a save point away; nil when there is no such one.
func (s *Store) SavePointDone(ctx context.Context, id, at int64) (*SavePoint, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE save_points SET done_at = COALESCE(done_at, ?) WHERE id = ?`, at, id); err != nil {
		return nil, err
	}
	sp, err := scanSavePoint(s.db.QueryRowContext(ctx, `SELECT id, body, ready_at, done_at FROM save_points WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &sp, err
}

// --- the loop ----------------------------------------------------------------------

// SavePoints watches the calendar for meetings starting and ending.
type SavePoints struct {
	mu sync.Mutex
	// startedAfter is the last look: a meeting that started before Norn did is not saved for.
	startedAfter time.Time
	// Goals and Sessions read what you were doing; Sindri's by default, swapped in tests.
	Goals    func(context.Context) []GoalActivity
	Sessions func(context.Context) []OwnSession
}

// meetings is every meeting Norn last read, by its id.
func (e *Events) meetings() map[string]CalendarEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	all := map[string]CalendarEvent{}
	for _, events := range e.calendars {
		for id, event := range events {
			all[id] = event
		}
	}
	return all
}

// Goals is Sindri's goals; empty when Sindri cannot say.
func (c *SindriClient) Goals(ctx context.Context) []GoalActivity {
	var answer struct {
		Goals []GoalActivity `json:"goals"`
	}
	if c.call(ctx, http.MethodGet, "/v1/goals", nil, &answer) != nil {
		return nil
	}
	return answer.Goals
}

// SavePointTick saves your place for a meeting that started since the last
// look, and hands back the ones whose meeting has ended.
func (n *Norn) SavePointTick(ctx context.Context, now time.Time) error {
	state := n.SavePoints
	state.mu.Lock()
	defer state.mu.Unlock()
	after := state.startedAfter
	state.startedAfter = now
	meetings := n.Events.meetings()

	waiting, err := n.Store.WaitingSavePoints(ctx)
	if err != nil {
		return err
	}
	// The end as the calendar says now: a meeting that ran over, or moved, ends later.
	endOf := func(sp SavePoint) int64 {
		if current, ok := meetings[sp.MeetingID]; ok && current.Start == sp.MeetingStart {
			return current.End
		}
		return sp.MeetingEnd
	}
	open := false
	for _, sp := range waiting {
		end := endOf(sp)
		if end > now.UnixMilli() {
			open = true
			continue
		}
		at := now.UnixMilli()
		if err := n.Store.SavePointReady(ctx, sp.ID, at); err != nil {
			return err
		}
		if now.Sub(time.UnixMilli(end)) > SavePointLate {
			continue // Norn was away while it ended; too late to hand back.
		}
		sp.ReadyAt, sp.MeetingEnd = &at, end
		n.Events.Publish("savepoint.ready", sp)
	}

	// The meeting that started last since the last look; one save point at a
	// time, so the second of two overlapping meetings keeps the first's.
	var started *CalendarEvent
	for _, event := range meetings {
		if SavePointMeeting(event) && event.Start > after.UnixMilli() && event.Start <= now.UnixMilli() && event.End > now.UnixMilli() &&
			(started == nil || event.Start > started.Start) {
			started = &event
		}
	}
	if started == nil || open {
		return nil
	}
	goal, session := PickSavePoint(state.Goals(ctx), state.Sessions(ctx), now)
	sp := NewSavePoint(*started, goal, session, now)
	if sp == nil {
		return nil
	}
	return n.Store.AddSavePoint(ctx, sp)
}

// --- routes ------------------------------------------------------------------------

func (n *Norn) mountSavePoints(p *part.Part) {
	n.SavePoints = &SavePoints{startedAfter: time.Now(), Goals: n.Sindri.Goals, Sessions: n.Sindri.Sessions}
	p.Handle("GET /v1/savepoints", n.listSavePoints)
	p.Handle("POST /v1/savepoints/{id}/done", n.savePointDone)
	p.Loop("save-points", EndedEvery, func(ctx context.Context) error {
		return n.SavePointTick(ctx, time.Now())
	})
}

func (n *Norn) listSavePoints(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			part.Error(w, http.StatusBadRequest, "`limit` is 1 to 100")
			return
		}
		limit = value
	}
	points, err := n.Store.SavePoints(r.Context(), limit)
	if err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"savepoints": points})
}

func (n *Norn) savePointDone(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only you can put a save point away")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		part.Error(w, http.StatusNotFound, "no such save point")
		return
	}
	sp, err := n.Store.SavePointDone(r.Context(), id, time.Now().UnixMilli())
	switch {
	case err != nil:
		n.fail(w, err)
	case sp == nil:
		part.Error(w, http.StatusNotFound, "no such save point")
	default:
		part.JSON(w, http.StatusOK, sp)
	}
}
