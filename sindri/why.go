package sindri

// A goal's why: when a goal's coding agents start, Sindri asks Norn
// (POST /v1/why) what lies behind the goal, the mail and chats, meeting
// notes and Rewind moments about it, each summarized by Norn's local model
// with names swapped for placeholders, and puts them in the agent's prompt
// under "Why this goal exists". So the agent builds what was asked for, not
// only what the one-line goal says.
//
// Norn being slow or down never holds a goal up: Sindri waits WhyTimeout at
// most and starts the agents without it. What was sent is kept on the goal
// (`why` on GET /v1/goals), so you can see what the agent was told. A pack
// is reused for an hour, so a race's two lanes and a restart get the same
// (Norn not answering, for 5 minutes: a race does not wait twice).
// On until you turn it off (PATCH /v1/settings/why); off, Norn is not asked.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"yggdrasil/internal/call"
	"yggdrasil/internal/part"
)

const whySchema = `CREATE TABLE IF NOT EXISTS goal_why (
	goal_id INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	state TEXT NOT NULL,
	sources TEXT NOT NULL DEFAULT '[]'
);`

// WhyTimeout is how long Sindri waits for Norn's pack before starting without it.
var WhyTimeout = 30 * time.Second

// whyReuse is how long a pack is used again for the same goal; whyRetry,
// how long after Norn did not answer it is asked again.
const (
	whyReuse = time.Hour
	whyRetry = 5 * time.Minute
)

// WhySource is one reason behind a goal, as Norn sent it.
type WhySource struct {
	// Kind is mail, meeting or rewind.
	Kind string `json:"kind"`
	// At is when it happened, epoch ms; 0 when unknown.
	At int64 `json:"at"`
	// Summary is one line from Norn's local model, names as placeholders.
	Summary string `json:"summary"`
	// Ref is where Norn found it; shown to you, never sent to the agent.
	Ref string `json:"ref"`
}

// GoalWhy is what a goal's agents were told about why it exists.
type GoalWhy struct {
	At int64 `json:"at"`
	// State is sent (the sources went into the prompt), none (Norn found
	// nothing it was sure of), unreachable (Norn did not answer in time) or
	// off (turned off: Norn was not asked).
	State   string      `json:"state"`
	Sources []WhySource `json:"sources"`
}

// WhyAsker asks for a goal's why.
type WhyAsker interface {
	Why(ctx context.Context, goal *Goal) ([]WhySource, error)
}

// NornWhy asks Norn's POST /v1/why, with Sindri's token there.
type NornWhy struct{ Norn *call.Part }

// Why is Norn's pack for the goal.
func (n NornWhy) Why(ctx context.Context, goal *Goal) ([]WhySource, error) {
	var answer struct {
		Sources []WhySource `json:"sources"`
	}
	body := map[string]any{"goal": goal.Title, "context": goal.Context, "repos": goal.Repos, "goalId": goal.ID}
	if err := n.Norn.Do(ctx, http.MethodPost, "/v1/why", body, &answer); err != nil {
		return nil, err
	}
	return answer.Sources, nil
}

// WhySettings is whether goals' agents get the why.
type WhySettings struct {
	On bool `json:"on"`
}

// WhyOn is whether goals' agents get the why: on until turned off.
func (w *Work) WhyOn() WhySettings {
	settings := WhySettings{On: true}
	w.Store.Setting("why", &settings)
	return settings
}

// --- the store ----------------------------------------------------------------------

// GoalWhy is what a goal's agents were last told; nil when never asked.
func (s *Store) GoalWhy(goal int64) (*GoalWhy, error) {
	why := &GoalWhy{}
	var sources string
	err := s.db.QueryRow(`SELECT at, state, sources FROM goal_why WHERE goal_id = ?`, goal).Scan(&why.At, &why.State, &sources)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(sources), &why.Sources)
	if why.Sources == nil {
		why.Sources = []WhySource{}
	}
	return why, nil
}

// PutGoalWhy keeps what a goal's agents were told.
func (s *Store) PutGoalWhy(goal int64, why GoalWhy) error {
	if why.Sources == nil {
		why.Sources = []WhySource{}
	}
	encoded, _ := json.Marshal(why.Sources)
	_, err := s.db.Exec(`INSERT INTO goal_why (goal_id, at, state, sources) VALUES (?, ?, ?, ?)
		ON CONFLICT(goal_id) DO UPDATE SET at = excluded.at, state = excluded.state, sources = excluded.sources`, goal, why.At, why.State, string(encoded))
	return err
}

// --- the prompt ---------------------------------------------------------------------

// WhySection is the prompt's "Why this goal exists" section; "" without sources.
func WhySection(sources []WhySource, zone *time.Location) string {
	if len(sources) == 0 {
		return ""
	}
	if zone == nil {
		zone = time.Local
	}
	lines := []string{
		"## Why this goal exists",
		"From the owner's mail, chats, meetings and timeline, summarized on their own machine; people are placeholders (Person 1…). " +
			"Build what was actually asked for here, not only what the one-line goal says. Where this disagrees with the goal, follow the goal and say so in your final answer.",
	}
	for _, s := range sources {
		day := "undated"
		if s.At > 0 {
			day = time.UnixMilli(s.At).In(zone).Format("2006-01-02")
		}
		lines = append(lines, fmt.Sprintf("- %s, %s: %s", day, s.Kind, strings.Join(strings.Fields(s.Summary), " ")))
	}
	return strings.Join(lines, "\n")
}

// goalWhy is the why section for a goal's prompt, asked of Norn (or the one
// asked within the hour), and kept on the goal. Off, or with no one to ask,
// Norn is not asked. Never longer than WhyTimeout.
func (w *Work) goalWhy(goal *Goal) string {
	if w.Why == nil {
		return ""
	}
	now := w.Store.now()
	if !w.WhyOn().On {
		_ = w.Store.PutGoalWhy(goal.ID, GoalWhy{At: now.UnixMilli(), State: "off"})
		return ""
	}
	if kept, _ := w.Store.GoalWhy(goal.ID); kept != nil && kept.State != "off" {
		age := now.Sub(time.UnixMilli(kept.At))
		if (kept.State == "unreachable" && age < whyRetry) || (kept.State != "unreachable" && age < whyReuse) {
			return WhySection(kept.Sources, w.Zone)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), WhyTimeout)
	defer cancel()
	sources, err := w.Why.Why(ctx, goal)
	why := GoalWhy{At: now.UnixMilli(), State: "sent", Sources: sources}
	switch {
	case err != nil:
		w.logf("goal %d: no why from Norn, starting without it: %v", goal.ID, err)
		why = GoalWhy{At: now.UnixMilli(), State: "unreachable"}
	case len(sources) == 0:
		why.State = "none"
	}
	if err := w.Store.PutGoalWhy(goal.ID, why); err != nil {
		w.logf("goal %d: keeping its why: %v", goal.ID, err)
	}
	return WhySection(why.Sources, w.Zone)
}

// --- the settings route -------------------------------------------------------------

func (w *Work) mountWhy(p *part.Part) {
	p.Handle("GET /v1/settings/why", func(rw http.ResponseWriter, _ *http.Request) {
		part.JSON(rw, http.StatusOK, w.WhyOn())
	})
	p.Handle("PATCH /v1/settings/why", func(rw http.ResponseWriter, r *http.Request) {
		if part.Caller(r) != part.Owner {
			part.Error(rw, http.StatusForbidden, "only the owner turns a goal's why on or off")
			return
		}
		var body struct {
			On *bool `json:"on"`
		}
		if err := part.Decode(r, &body); err != nil || body.On == nil {
			part.Error(rw, http.StatusBadRequest, "send `on`, true or false")
			return
		}
		settings := WhySettings{On: *body.On}
		if err := w.Store.SetSetting("why", settings); err != nil {
			fail(rw, err)
			return
		}
		part.JSON(rw, http.StatusOK, settings)
	})
}
