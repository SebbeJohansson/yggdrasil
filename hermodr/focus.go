package hermodr

// Focus: for a while you are working and nothing that can wait reaches you.
// Every message but an urgent one is held until it ends, on the phone and in
// the widget alike, and goes out then, as a held one does after a meeting.
// Kept in the store, so a restart does not end it early. Each focus is also
// logged, start to end, of which kind and on which goal, for the widget's
// week (focus history).
//
// A focus is of one kind, which says how much of the dashboard it takes over:
// general (the overview hides what is not work), dev (the dashboard becomes
// the goal workbench) or goal (the workbench on one goal). Messages wait the
// same way in all three.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"yggdrasil/internal/part"
)

// MaxFocus is the longest a focus may be set for.
const MaxFocus = 12 * time.Hour

// MaxIntent is the longest the line on what you are doing may be, in characters.
const MaxIntent = 200

// The kinds of focus.
const (
	KindGeneral = "general"
	KindDev     = "dev"
	KindGoal    = "goal"
)

// FocusGoal is the goal a focus is on, when it is on one.
type FocusGoal struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

// Focus is whether you are focusing, until when, and on what.
type Focus struct {
	On        bool       `json:"on"`
	Kind      string     `json:"kind,omitempty"`
	Until     *time.Time `json:"until,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	Goal      *FocusGoal `json:"goal,omitempty"`
	// Intent is what you said you are doing, if you did.
	Intent string `json:"intent,omitempty"`
}

// kindOf is a focus's kind; one kept before kinds is general, or goal when on one.
func kindOf(f Focus) string {
	switch {
	case f.Kind != "":
		return f.Kind
	case f.Goal != nil:
		return KindGoal
	}
	return KindGeneral
}

const focusSchema = `CREATE TABLE IF NOT EXISTS focus (id INTEGER PRIMARY KEY CHECK (id = 1), body TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS focus_log (id INTEGER PRIMARY KEY AUTOINCREMENT, started INTEGER NOT NULL, ended INTEGER,
  until INTEGER NOT NULL, goal_id INTEGER, goal_title TEXT);
CREATE INDEX IF NOT EXISTS focus_log_started ON focus_log (started);`

// focusLogColumns are the log's columns added after it was first made.
var focusLogColumns = []string{"kind TEXT", "intent TEXT"}

// addFocusLogColumns adds each of focusLogColumns the log does not have yet.
func addFocusLogColumns(db *sql.DB) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('focus_log')`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, column := range focusLogColumns {
		if have[strings.Fields(column)[0]] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE focus_log ADD COLUMN ` + column); err != nil {
			return err
		}
	}
	return nil
}

// FocusSession is one focus, from its start to its end. A focus that ran out
// ended at its time; EndedAt is unset only on the one going on now.
type FocusSession struct {
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	Until     time.Time  `json:"until"`
	Kind      string     `json:"kind"`
	Goal      *FocusGoal `json:"goal,omitempty"`
	Intent    string     `json:"intent,omitempty"`
}

// logFocus keeps the log in step with a focus that starts, changes or ends at
// now: a new kind, a new goal, or a new focus, is a new session; more time or
// a new intent is the same one.
func (h *Hermodr) logFocus(ctx context.Context, before, after Focus, now time.Time) error {
	sameGoal := (before.Goal == nil) == (after.Goal == nil) && (before.Goal == nil || before.Goal.ID == after.Goal.ID)
	if before.On && after.On && sameGoal && kindOf(before) == kindOf(after) {
		_, err := h.db.ExecContext(ctx, `UPDATE focus_log SET until = ?, intent = ? WHERE ended IS NULL`, after.Until.UnixMilli(), nullable(after.Intent))
		return err
	}
	if before.On {
		if _, err := h.db.ExecContext(ctx, `UPDATE focus_log SET ended = ? WHERE ended IS NULL`, now.UnixMilli()); err != nil {
			return err
		}
	}
	// One that ran out on its own, never closed: it ended at its time.
	if _, err := h.db.ExecContext(ctx, `UPDATE focus_log SET ended = until WHERE ended IS NULL AND until <= ?`, now.UnixMilli()); err != nil {
		return err
	}
	if !after.On {
		return nil
	}
	var goalID sql.NullInt64
	var goalTitle sql.NullString
	if after.Goal != nil {
		goalID, goalTitle = sql.NullInt64{Int64: int64(after.Goal.ID), Valid: true}, sql.NullString{String: after.Goal.Title, Valid: true}
	}
	_, err := h.db.ExecContext(ctx, `INSERT INTO focus_log (started, until, goal_id, goal_title, kind, intent) VALUES (?, ?, ?, ?, ?, ?)`,
		now.UnixMilli(), after.Until.UnixMilli(), goalID, goalTitle, kindOf(after), nullable(after.Intent))
	return err
}

func nullable(text string) sql.NullString {
	return sql.NullString{String: text, Valid: text != ""}
}

// focusHistory is every focus that started since `since`, oldest first.
func (h *Hermodr) focusHistory(ctx context.Context, since time.Time) ([]FocusSession, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT started, ended, until, goal_id, goal_title, kind, intent FROM focus_log WHERE started >= ? ORDER BY started`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := h.now()
	sessions := []FocusSession{}
	for rows.Next() {
		var started, until int64
		var ended, goalID sql.NullInt64
		var goalTitle, kind, intent sql.NullString
		if err := rows.Scan(&started, &ended, &until, &goalID, &goalTitle, &kind, &intent); err != nil {
			return nil, err
		}
		s := FocusSession{StartedAt: time.UnixMilli(started), Until: time.UnixMilli(until), Kind: kind.String, Intent: intent.String}
		switch {
		case ended.Valid:
			t := time.UnixMilli(ended.Int64)
			s.EndedAt = &t
		case !now.Before(s.Until):
			t := s.Until
			s.EndedAt = &t
		}
		if goalID.Valid {
			s.Goal = &FocusGoal{ID: int(goalID.Int64), Title: goalTitle.String}
		}
		s.Kind = kindOf(Focus{Kind: s.Kind, Goal: s.Goal})
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

func (h *Hermodr) getFocusHistory(w http.ResponseWriter, r *http.Request) {
	since := h.now().Add(-30 * 24 * time.Hour)
	if raw := r.URL.Query().Get("since"); raw != "" {
		ms, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || ms < 0 {
			part.Error(w, http.StatusBadRequest, "since is milliseconds since 1970")
			return
		}
		since = time.UnixMilli(ms)
	}
	sessions, err := h.focusHistory(r.Context(), since)
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// focusNow is the focus as it stands: off once its time is up.
func (h *Hermodr) focusNow() Focus {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.focus.Until == nil || !h.now().Before(*h.focus.Until) {
		return Focus{}
	}
	f := h.focus
	f.On = true
	f.Kind = kindOf(f)
	return f
}

func (h *Hermodr) setFocus(ctx context.Context, f Focus) error {
	body, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO focus (id, body) VALUES (1, ?) ON CONFLICT (id) DO UPDATE SET body = excluded.body`, string(body)); err != nil {
		return err
	}
	h.mu.Lock()
	h.focus = f
	h.mu.Unlock()
	return nil
}

func (h *Hermodr) loadFocus() error {
	var body string
	err := h.db.QueryRow(`SELECT body FROM focus WHERE id = 1`).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(body), &h.focus)
}

func (h *Hermodr) getFocus(w http.ResponseWriter, _ *http.Request) {
	part.JSON(w, http.StatusOK, h.focusNow())
}

type focusInput struct {
	Until  *time.Time `json:"until"`
	Kind   string     `json:"kind"`
	Goal   *FocusGoal `json:"goal"`
	Intent string     `json:"intent"`
}

func (h *Hermodr) putFocus(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only the owner may focus")
		return
	}
	var in focusInput
	if err := part.Decode(r, &in); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	now := h.now()
	in.Intent = strings.TrimSpace(in.Intent)
	if in.Kind == "" {
		in.Kind = kindOf(Focus{Goal: in.Goal})
	}
	switch {
	case in.Kind != KindGeneral && in.Kind != KindDev && in.Kind != KindGoal:
		part.Error(w, http.StatusBadRequest, "kind is general, dev or goal")
		return
	case (in.Kind == KindGoal) != (in.Goal != nil):
		part.Error(w, http.StatusBadRequest, "a goal focus, and only a goal focus, needs a goal")
		return
	case utf8.RuneCountInString(in.Intent) > MaxIntent:
		part.Error(w, http.StatusBadRequest, "the intent is at most 200 characters")
		return
	case in.Until == nil || !in.Until.After(now):
		part.Error(w, http.StatusBadRequest, "until must be ahead")
		return
	case in.Until.After(now.Add(MaxFocus)):
		part.Error(w, http.StatusBadRequest, "a focus is at most 12 hours")
		return
	case in.Goal != nil && (in.Goal.ID <= 0 || strings.TrimSpace(in.Goal.Title) == "" || utf8.RuneCountInString(in.Goal.Title) > 300):
		part.Error(w, http.StatusBadRequest, "a goal needs its id and a title of at most 300 characters")
		return
	}
	// Changing the time or the goal of a focus keeps when it started.
	started := now
	if current := h.focusNow(); current.On && current.StartedAt != nil {
		started = *current.StartedAt
	}
	f := Focus{Kind: in.Kind, Until: in.Until, StartedAt: &started, Goal: in.Goal, Intent: in.Intent}
	before := h.focusNow()
	if err := h.setFocus(r.Context(), f); err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.logFocus(r.Context(), before, h.focusNow(), now); err != nil {
		h.logf("logging a focus: %v", err)
	}
	part.JSON(w, http.StatusOK, h.focusNow())
}

func (h *Hermodr) endFocus(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only the owner may end a focus")
		return
	}
	before := h.focusNow()
	if err := h.setFocus(r.Context(), Focus{}); err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.logFocus(r.Context(), before, Focus{}, h.now()); err != nil {
		h.logf("logging a focus: %v", err)
	}
	// What waited goes now, not at the next release.
	if err := h.release(r.Context()); err != nil {
		h.logf("releasing after focus: %v", err)
	}
	part.JSON(w, http.StatusOK, Focus{})
}
