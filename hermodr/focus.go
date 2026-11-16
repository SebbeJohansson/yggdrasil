package hermodr

// Focus: for a while you are working and nothing that can wait reaches you.
// Every message but an urgent one is held until it ends, on the phone and in
// the widget alike, and goes out then, as a held one does after a meeting.
// Kept in the store, so a restart does not end it early.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"yggdrasil/internal/part"
)

// MaxFocus is the longest a focus may be set for.
const MaxFocus = 12 * time.Hour

// FocusGoal is the goal a focus is on, when it is on one.
type FocusGoal struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

// Focus is whether you are focusing, until when, and on what.
type Focus struct {
	On        bool       `json:"on"`
	Until     *time.Time `json:"until,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	Goal      *FocusGoal `json:"goal,omitempty"`
}

const focusSchema = `CREATE TABLE IF NOT EXISTS focus (id INTEGER PRIMARY KEY CHECK (id = 1), body TEXT NOT NULL);`

// focusNow is the focus as it stands: off once its time is up.
func (h *Hermodr) focusNow() Focus {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.focus.Until == nil || !h.now().Before(*h.focus.Until) {
		return Focus{}
	}
	f := h.focus
	f.On = true
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
	Until *time.Time `json:"until"`
	Goal  *FocusGoal `json:"goal"`
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
	switch {
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
	f := Focus{Until: in.Until, StartedAt: &started, Goal: in.Goal}
	if err := h.setFocus(r.Context(), f); err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, h.focusNow())
}

func (h *Hermodr) endFocus(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only the owner may end a focus")
		return
	}
	if err := h.setFocus(r.Context(), Focus{}); err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	// What waited goes now, not at the next release.
	if err := h.release(r.Context()); err != nil {
		h.logf("releasing after focus: %v", err)
	}
	part.JSON(w, http.StatusOK, Focus{})
}
