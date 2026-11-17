package sindri

import "testing"

func TestGoalState(t *testing.T) {
	tasks := func(statuses ...string) []GoalTask {
		out := []GoalTask{}
		for i, s := range statuses {
			out = append(out, GoalTask{ID: int64(i), Status: s})
		}
		return out
	}
	for _, c := range []struct {
		name  string
		tasks []GoalTask
		want  string
	}{
		{"no tasks", nil, "draft"},
		{"one asks", tasks("running", "needs-you"), "needs-you"},
		{"one failed", tasks("done", "failed"), "needs-you"},
		{"one stopped", tasks("stopped"), "needs-you"},
		{"waiting to resume is not on you", tasks("scheduled", "done"), "running"},
		{"asks beats waiting to resume", tasks("scheduled", "needs-you"), "needs-you"},
		{"one runs", tasks("running", "done"), "running"},
		{"all done", tasks("done", "done"), "done"},
	} {
		if got := GoalState(c.tasks); got != c.want {
			t.Errorf("%s: GoalState = %q, want %q", c.name, got, c.want)
		}
	}
}
