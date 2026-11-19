package sindri

// The merged goals log: every goal Sindri has seen merged, once, never
// removed. A goal's state is read from its tasks and agents, so it stops
// reading merged when its agent is removed, and the goal itself can be
// deleted; this log is what remembers it (the widget's tree grows a branch
// for each). Filled wherever the goals are read (GoalViews), which the
// dashboard, the night shift and the Linear sync all do.

import (
	"encoding/json"
	"net/http"
	"sync"

	"yggdrasil/internal/part"
)

const mergedSchema = `
CREATE TABLE IF NOT EXISTS merged_goals (
	goal_id INTEGER PRIMARY KEY,
	title TEXT NOT NULL,
	repos TEXT NOT NULL,
	merged_at INTEGER NOT NULL
);
`

// MergedGoal is one goal as it was when Sindri first saw it merged.
type MergedGoal struct {
	ID       int64    `json:"id"`
	Title    string   `json:"title"`
	Repos    []string `json:"repos"`
	MergedAt int64    `json:"mergedAt"`
}

// mergedLog keeps which goals are logged, so reading the goals writes only
// the new ones.
type mergedLog struct {
	mu     sync.Mutex
	loaded bool
	ids    map[int64]bool
}

// RememberMerged logs the goal as merged, once; later calls change nothing.
func (s *Store) RememberMerged(goal MergedGoal) error {
	s.merged.mu.Lock()
	defer s.merged.mu.Unlock()
	if !s.merged.loaded {
		rows, err := s.db.Query(`SELECT goal_id FROM merged_goals`)
		if err != nil {
			return err
		}
		s.merged.ids = map[int64]bool{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			s.merged.ids[id] = true
		}
		rows.Close()
		s.merged.loaded = true
	}
	if s.merged.ids[goal.ID] {
		return nil
	}
	repos, _ := json.Marshal(goal.Repos)
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO merged_goals (goal_id, title, repos, merged_at) VALUES (?, ?, ?, ?)`, goal.ID, goal.Title, string(repos), goal.MergedAt); err != nil {
		return err
	}
	s.merged.ids[goal.ID] = true
	return nil
}

// MergedGoals is every goal logged as merged, the earliest merged first.
func (s *Store) MergedGoals() ([]MergedGoal, error) {
	rows, err := s.db.Query(`SELECT goal_id, title, repos, merged_at FROM merged_goals ORDER BY merged_at, goal_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	goals := []MergedGoal{}
	for rows.Next() {
		var g MergedGoal
		var repos string
		if err := rows.Scan(&g.ID, &g.Title, &repos, &g.MergedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(repos), &g.Repos)
		if g.Repos == nil {
			g.Repos = []string{}
		}
		goals = append(goals, g)
	}
	return goals, rows.Err()
}

// mergedAt is when a merged goal's last branch merged: its tasks' latest
// mergedAt, or, for branches merged outside Sindri, their latest activity.
func mergedAt(tasks []GoalTask, now int64) int64 {
	var merged, active int64
	for _, t := range tasks {
		merged, active = max(merged, t.MergedAt), max(active, t.LastActivity)
	}
	switch {
	case merged > 0:
		return merged
	case active > 0:
		return active
	}
	return now
}

func (w *Work) mergedGoals(rw http.ResponseWriter, _ *http.Request) {
	if _, err := w.GoalViews(); err != nil { // logs what merged since the last read
		fail(rw, err)
		return
	}
	goals, err := w.Store.MergedGoals()
	if err != nil {
		fail(rw, err)
		return
	}
	part.JSON(rw, http.StatusOK, map[string]any{"goals": goals})
}
