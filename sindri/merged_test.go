package sindri

import "testing"

func TestAMergedGoalIsLoggedOnceAndOutlivesItsGoal(t *testing.T) {
	w, _ := newWork(t)
	goal, _ := w.Store.AddGoal("Add a work file", []string{"demo"}, "manual", "", "goal-1")
	started, _, err := w.StartGoal(goal.ID, nil)
	if err != nil || len(started) != 1 {
		t.Fatalf("got %+v %v", started, err)
	}
	agent := started[0].AgentID
	waitFor(t, w.Agents, agent, func(a *Agent) bool { return a.Status == Waiting })
	if err := w.Store.EndTask(agent, "done", "Committed."); err != nil {
		t.Fatal(err)
	}
	if got := must2(w.Store.MergedGoals()); len(got) != 0 {
		t.Fatalf("logged before it merged: %+v", got)
	}
	if _, err := w.Agents.Merge(agent); err != nil {
		t.Fatal(err)
	}
	if view := must2(w.goalView(goal.ID)); view.State != "merged" {
		t.Fatalf("state %s", view.State)
	}
	must2(w.GoalViews())
	logged := must2(w.Store.MergedGoals())
	if len(logged) != 1 || logged[0].ID != goal.ID || logged[0].Title != "Add a work file" || logged[0].MergedAt == 0 || logged[0].Repos[0] != "demo" {
		t.Fatalf("logged %+v", logged)
	}
	_ = w.Store.UpdateGoal(goal.ID, ptr("Renamed later"), nil, nil)
	if _, err := w.Store.DeleteGoal(goal.ID); err != nil {
		t.Fatal(err)
	}
	if again := must2(w.Store.MergedGoals()); len(again) != 1 || again[0].Title != "Add a work file" || again[0].MergedAt != logged[0].MergedAt {
		t.Fatalf("after a rename and a delete: %+v", again)
	}
}

func TestAMergedGoalIsDatedByItsLastMerge(t *testing.T) {
	if got := mergedAt([]GoalTask{{MergedAt: 5, LastActivity: 9}, {MergedAt: 7}}, 100); got != 7 {
		t.Fatalf("got %d", got)
	}
	if got := mergedAt([]GoalTask{{LastActivity: 9}}, 100); got != 9 {
		t.Fatalf("merged outside Sindri: %d", got)
	}
	if got := mergedAt(nil, 100); got != 100 {
		t.Fatalf("nothing known: %d", got)
	}
}
