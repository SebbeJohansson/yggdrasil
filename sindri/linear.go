package sindri

// Goals from Linear. Open issues are goal candidates; a goal made from one
// (source "linear:<key>") moves its issue along as the goal moves:
//
//	draft                 Todo         (the team's first unstarted state)
//	running, needs-you    In Progress  (the first started state)
//	done                  In Progress  (branches ready, not merged yet)
//	merged                In Review    (a started state named "review"; else stays In Progress)
//
// Never Done: you close the issue once you have checked the work. Forward
// only: an issue already further along is left alone, and a canceled one is
// never touched. Each goal remembers the step last written (linear_synced),
// so an issue is written once per step.

import (
	"context"
	"time"

	"yggdrasil/internal/linear"
)

// LinearStep is the step a goal in state is at.
func LinearStep(state string) linear.Step {
	switch state {
	case "draft":
		return "todo"
	case "merged":
		return "review"
	}
	return "started"
}

// LinearCandidates are open Linear issues as goal candidates.
func LinearCandidates(ctx context.Context, client *linear.Client, filter linear.Filter, known map[string]int64) ([]Candidate, error) {
	issues, err := client.Open(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(issues))
	for _, issue := range issues {
		source := "linear:" + issue.Identifier
		context := issue.URL
		if issue.Description != nil && *issue.Description != "" {
			context = *issue.Description + "\n\n" + issue.URL
		}
		c := Candidate{Source: source, Title: clip(issue.Title, 200), Context: clip(context, MaxGoalContext), Repos: []string{}, URL: issue.URL,
			State: issue.State.Name, StateType: issue.State.Type}
		if id, ok := known[source]; ok {
			c.Goal = &id
		}
		out = append(out, c)
	}
	return out, nil
}

func clip(s string, n int) string {
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:n])
	}
	return s
}

// linearRetry: a failed write is tried again after this long, not on every pass.
const linearRetry = 5 * time.Minute

// SyncLinear moves the Linear issue of every goal whose step changed since it was last written.
func (w *Work) SyncLinear(ctx context.Context) error {
	if !w.Linear.On() {
		return nil
	}
	views, err := w.GoalViews()
	if err != nil {
		return err
	}
	now := time.Now()
	for _, goal := range views {
		key := linear.KeyOf(goal.Source)
		if key == "" {
			continue
		}
		step := LinearStep(goal.State)
		if w.Store.GoalLinearSynced(goal.ID) == string(step) {
			continue
		}
		if at, ok := w.linearFailed[goal.ID]; ok && now.Sub(at) < linearRetry {
			continue
		}
		moved, err := w.Linear.Move(ctx, key, step)
		if err != nil {
			w.linearFailed[goal.ID] = now
			w.logf("could not move %s for goal %d: %v", key, goal.ID, err)
			continue
		}
		delete(w.linearFailed, goal.ID)
		_ = w.Store.SetGoalLinearSynced(goal.ID, string(step))
		if moved != "" {
			w.logf("goal %d is %s: moved %s to %s", goal.ID, goal.State, key, moved)
		}
	}
	return nil
}
