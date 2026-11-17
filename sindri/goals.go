package sindri

// Goals: long-running work across repos. A goal's state is never stored; it
// is read from its tasks. And the routes for handing work over, the ledger,
// approvals and settings. Ported from my-agent's tasks/goals.ts.

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"yggdrasil/internal/linear"

	"yggdrasil/internal/part"
)

// Limits on goals.
const (
	MaxGoalContext    = 20_000
	MaxGoalImageBytes = 10 << 20
	MaxGoalImages     = 8
)

// GoalTask is one of a goal's tasks as the goal shows it.
type GoalTask struct {
	ID           int64      `json:"id"`
	Repo         *string    `json:"repo"`
	Agent        string     `json:"agent"`
	Status       string     `json:"status"`
	Label        *string    `json:"label"`
	Branch       *string    `json:"branch"`
	Merged       bool       `json:"merged"`
	LastActivity int64      `json:"lastActivity"`
	Result       *string    `json:"result"`
	Scheduled    *Scheduled `json:"scheduled,omitempty"`
}

// GoalNext is the one thing to do next for a goal.
type GoalNext struct {
	Action string `json:"action"` // start, resume, answer, review, none
	Text   string `json:"text"`
	Repo   string `json:"repo,omitempty"`
	Agent  string `json:"agent,omitempty"`
}

// GoalView is a goal with its pictures, tasks, state and next step.
type GoalView struct {
	Goal
	Images []GoalImage `json:"images"`
	State  string      `json:"state"` // draft, running, needs-you, done, merged, closed
	Tasks  []GoalTask  `json:"tasks"`
	Next   GoalNext    `json:"next"`
}

// GoalState is read from the tasks: draft without any, needs-you when one
// asks, failed or stopped, running while one runs or waits to resume on its
// own, merged when every branch is.
func GoalState(tasks []GoalTask) string {
	if len(tasks) == 0 {
		return "draft"
	}
	for _, t := range tasks {
		if t.Status == "needs-you" || t.Status == "failed" || t.Status == "stopped" {
			return "needs-you"
		}
	}
	for _, t := range tasks {
		if t.Status == "running" || t.Status == "scheduled" {
			return "running"
		}
	}
	branched, merged := 0, 0
	for _, t := range tasks {
		if t.Branch != nil {
			branched++
			if t.Merged {
				merged++
			}
		}
	}
	if branched > 0 && branched == merged {
		return "merged"
	}
	return "done"
}

// currentParts is the newest task of each repo, in the order given: a
// restarted part's older tasks do not count toward the goal's state.
func currentParts(tasks []GoalTask) []GoalTask {
	newest := map[string]int64{}
	for _, t := range tasks {
		newest[repoOf(t)] = max(newest[repoOf(t)], t.ID)
	}
	var current []GoalTask
	for _, t := range tasks {
		if newest[repoOf(t)] == t.ID {
			current = append(current, t)
		}
	}
	return current
}

func repoOf(t GoalTask) string {
	if t.Repo == nil {
		return ""
	}
	return *t.Repo
}

func theRepo(t GoalTask) string {
	if t.Repo == nil {
		return "the"
	}
	return *t.Repo
}

// NextFor is the one thing to do next.
func NextFor(tasks []GoalTask) GoalNext {
	for _, t := range tasks {
		if t.Status == "scheduled" {
			return GoalNext{Action: "resume", Text: "Waiting to resume the " + theRepo(t) + " part", Repo: repoOf(t)}
		}
	}
	if len(tasks) == 0 {
		return GoalNext{Action: "start"}
	}
	for _, t := range tasks {
		why := map[string]string{"needs-you": "Answer", "failed": "Look at the failure of", "stopped": "Pick up the stopped"}[t.Status]
		if why != "" {
			return GoalNext{Action: "answer", Text: why + " " + theRepo(t) + " agent", Repo: repoOf(t), Agent: t.Agent}
		}
	}
	running := slices.ContainsFunc(tasks, func(t GoalTask) bool { return t.Status == "running" })
	if !running {
		for _, t := range tasks {
			if t.Status == "done" && t.Branch != nil && !t.Merged {
				return GoalNext{Action: "review", Text: "Review " + theRepo(t) + " branch", Repo: repoOf(t), Agent: t.Agent}
			}
		}
	}
	if running {
		return GoalNext{Action: "none", Text: "Agents are working"}
	}
	if GoalState(tasks) == "merged" {
		return GoalNext{Action: "none", Text: "Merged"}
	}
	if !slices.ContainsFunc(tasks, func(t GoalTask) bool { return t.Status != "no-changes" }) {
		return GoalNext{Action: "none", Text: "No changes needed"}
	}
	return GoalNext{Action: "none", Text: "Nothing to do"}
}

// GoalViews is every goal as the dashboard shows it, newest first.
func (w *Work) GoalViews() ([]GoalView, error) {
	goals, err := w.Store.Goals()
	if err != nil {
		return nil, err
	}
	linked, err := w.Store.GoalTasks()
	if err != nil {
		return nil, err
	}
	byGoal := map[int64][]GoalTask{}
	for _, view := range w.TaskViews(linked) {
		task := GoalTask{ID: view.ID, Repo: view.Project, Agent: view.Agent, Status: view.Status, Label: view.Label, Branch: view.Branch,
			Merged: view.Merged, LastActivity: view.LastActivity, Result: view.Result, Scheduled: view.Task.Scheduled}
		if view.Status == "no-changes" {
			task.Branch, task.Merged = nil, false // removed unchanged: nothing to review, ship or count as merged
		}
		byGoal[*view.GoalID] = append(byGoal[*view.GoalID], task)
	}
	views := []GoalView{}
	for _, goal := range goals {
		images, _ := w.Store.GoalImages(goal.ID)
		tasks := byGoal[goal.ID]
		if tasks == nil {
			tasks = []GoalTask{}
		}
		current := currentParts(tasks)
		view := GoalView{Goal: goal, Images: images, State: GoalState(current), Tasks: tasks, Next: NextFor(current)}
		if goal.ClosedAt != nil {
			view.State, view.Next = "closed", GoalNext{Action: "none", Text: "Closed"}
		}
		if goal.Night && view.State == "draft" {
			view.Next.Text = "Starts tonight"
		}
		views = append(views, view)
	}
	return views, nil
}

func (w *Work) goalView(id int64) (*GoalView, error) {
	views, err := w.GoalViews()
	if err != nil {
		return nil, err
	}
	for _, v := range views {
		if v.ID == id {
			return &v, nil
		}
	}
	return nil, httpErr(404, "no goal %d", id)
}

// ClosedBranch is one branch of a goal being closed: removed when it held no
// work, kept when it did or its agent was busy.
type ClosedBranch struct {
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	Removed bool   `json:"removed"`
	Why     string `json:"why,omitempty"`
}

// CloseGoal closes a goal without merging the rest. Every branch of it that
// holds no work, no commit past its base and nothing uncommitted, is removed
// with its worktree; branches with work stay for you to merge or remove.
func (w *Work) CloseGoal(id int64) ([]ClosedBranch, error) {
	if goal, err := w.Store.Goal(id); err != nil || goal == nil {
		return nil, cmp.Or(err, httpErr(404, "no goal %d", id))
	}
	linked, err := w.Store.GoalTasks()
	if err != nil {
		return nil, err
	}
	branches := []ClosedBranch{}
	seen := map[string]bool{}
	for _, view := range w.TaskViews(linked) {
		if *view.GoalID != id || view.Branch == nil || view.Merged || seen[view.Agent] {
			continue
		}
		seen[view.Agent] = true
		branch := ClosedBranch{Repo: repoOf(GoalTask{Repo: view.Project}), Branch: *view.Branch}
		removed, err := w.Agents.RemoveIfEmpty(view.Agent)
		switch {
		case err != nil:
			branch.Why = err.Error()
		case removed:
			branch.Removed = true
		default:
			branch.Why = "has work"
		}
		branches = append(branches, branch)
	}
	return branches, w.Store.SetGoalClosed(id, true)
}

var kindText = map[string]string{"design": "design, not built yet: how it should look", "current": "how it looks now"}

// StartGoal starts one coding agent per repo that has none yet, all on one
// branch name, with the goal's pictures where they may read them.
func (w *Work) StartGoal(id int64, only []string) ([]Started, []string, error) {
	goal, err := w.Store.Goal(id)
	if err != nil {
		return nil, nil, err
	}
	if goal == nil {
		return nil, nil, httpErr(404, "no goal %d", id)
	}
	linked, _ := w.Store.GoalTasks()
	taken := map[string]bool{}
	for _, t := range linked {
		if t.GoalID != nil && *t.GoalID == id && t.Project != nil {
			taken[*t.Project] = true
		}
	}
	wanted := only
	if wanted == nil {
		for _, repo := range goal.Repos {
			if !taken[repo] {
				wanted = append(wanted, repo)
			}
		}
	}
	for _, repo := range wanted {
		if !slices.Contains(goal.Repos, repo) {
			return nil, nil, httpErr(400, "%s is not one of the goal's repos", repo)
		}
	}
	var repos []string
	for _, repo := range wanted {
		if !taken[repo] && !slices.Contains(repos, repo) {
			repos = append(repos, repo)
		}
	}
	switch {
	case len(goal.Repos) == 0:
		return nil, nil, httpErr(400, "the goal has no repos to start in")
	case len(repos) == 0:
		return nil, nil, httpErr(400, "every repo of the goal already has an agent")
	}
	known := projectNames(FindProjects(w.Agents.roots[0]))
	for _, repo := range repos {
		if !slices.Contains(known, repo) {
			return nil, nil, httpErr(400, "%s is not a project under %s", repo, w.Agents.roots[0])
		}
	}
	started, failed := w.startParts(goal, repos)
	return started, failed, nil
}

// startParts starts one coding agent per repo for the goal, all on one branch
// name, with the goal's pictures where they may read them.
func (w *Work) startParts(goal *Goal, repos []string) ([]Started, []string) {
	id := goal.ID
	parts := []string{goal.Title}
	if goal.Context != "" {
		parts = append(parts, "Context (background for the goal, not the goal itself):\n"+goal.Context)
	}
	images, _ := w.Store.GoalImages(id)
	if len(images) > 0 {
		var lines []string
		for _, image := range images {
			data, _, _ := w.Store.GoalImageData(id, image.ID)
			saved, err := SaveUploads(UploadRequest{Cwd: "/yggdrasil/goals", SessionID: goal.Session, Files: []struct {
				Name string `json:"name"`
				Data string `json:"data"`
			}{{Name: image.Name, Data: base64.StdEncoding.EncodeToString(data)}}})
			if err != nil {
				continue
			}
			line := fmt.Sprintf("- %s (%s)", saved[0].Path, kindText[image.Kind])
			if image.Note != "" {
				line += ": " + image.Note
			}
			lines = append(lines, line)
		}
		if len(lines) > 0 {
			parts = append(parts, "Pictures for this goal. Read each one with the Read tool before you start:\n"+strings.Join(lines, "\n"))
		}
	}
	parts = append(parts, fmt.Sprintf("(From goal %d, source: %s.)", goal.ID, goal.Source))
	request := CodeRequest{Session: goal.Session, Prompt: strings.Join(parts, "\n\n"), Name: ShortTask(goal.Title, 50), Projects: repos, GoalID: &goal.ID, Goal: goal.Title}
	handed := w.Code(request, nil)
	w.record(goal.Session, goal.Title, &handed)
	var failed []string
	for _, repo := range repos {
		if !slices.ContainsFunc(handed.Parts, func(s Started) bool { return s.Project == repo }) {
			failed = append(failed, "Could not start a coding agent in "+repo)
		}
	}
	return handed.Parts, failed
}

// latestPart is the newest task of each of a goal's repos: what its part is
// now. A restarted part's older tasks are history.
func (w *Work) latestPart(id int64) (map[string]TaskView, error) {
	linked, err := w.Store.GoalTasks()
	if err != nil {
		return nil, err
	}
	latest := map[string]TaskView{}
	for _, view := range w.TaskViews(linked) {
		if *view.GoalID != id || view.Agent == "" {
			continue
		}
		repo := nullString(view.Project, "")
		if old, ok := latest[repo]; !ok || view.ID > old.ID {
			latest[repo] = view
		}
	}
	return latest, nil
}

// RestartPart starts a fresh agent for one repo of a started goal, in place
// of its part that stopped, failed, was lost or needed no changes. The old
// worktree goes with it when it holds no work; one with work stays to review.
func (w *Work) RestartPart(id int64, repo string) ([]Started, []string, error) {
	goal, err := w.Store.Goal(id)
	if err != nil || goal == nil {
		return nil, nil, cmp.Or(err, httpErr(404, "no goal %d", id))
	}
	if !slices.Contains(goal.Repos, repo) {
		return nil, nil, httpErr(400, "%s is not one of the goal's repos", repo)
	}
	latest, err := w.latestPart(id)
	if err != nil {
		return nil, nil, err
	}
	if old, ok := latest[repo]; ok {
		if old.Status == "running" {
			return nil, nil, httpErr(409, "the %s part is still working: stop it first", repo)
		}
		_, _ = w.Agents.RemoveIfEmpty(old.Agent)
	}
	started, failed := w.startParts(goal, []string{repo})
	return started, failed, nil
}

// ResumeGoal makes the goal's parts waiting for Claude usage due now and
// starts a pass over the agents, so they resume without waiting for their
// next try. It returns how many parts it made due.
func (w *Work) ResumeGoal(ctx context.Context, id int64) (int, error) {
	goal, err := w.Store.Goal(id)
	if err != nil || goal == nil {
		return 0, cmp.Or(err, httpErr(404, "no goal %d", id))
	}
	tasks, err := w.Store.ScheduledTasks()
	if err != nil {
		return 0, err
	}
	due := 0
	for _, task := range tasks {
		if task.GoalID == nil || *task.GoalID != id || task.Scheduled == nil {
			continue
		}
		next := *task.Scheduled
		next.NextTryAt = w.Store.ms()
		if _, err := w.Store.db.Exec(`UPDATE tasks SET scheduled = ? WHERE id = ?`, mustJSON(next), task.ID); err != nil {
			return due, err
		}
		due++
	}
	if due == 0 {
		return 0, httpErr(409, "no part of goal %d is waiting to resume", id)
	}
	go func() { _ = w.Feed(context.WithoutCancel(ctx)) }()
	return due, nil
}

// RelookText asks an agent to read its goal again.
func RelookText(goal Goal) string {
	context := ""
	if goal.Context != "" {
		context = "\n\nContext (background for the goal, not the goal itself):\n" + goal.Context
	}
	repos := strings.Join(goal.Repos, ", ")
	if repos == "" {
		repos = "none"
	}
	return fmt.Sprintf("Look at the goal again: it may have changed since you started. It reads now:\n\n%s%s\n\nRepos: %s. (Goal %d.) Compare it with what you have done and planned, adjust your work if it differs, and carry on. If nothing differs, say so in one line.", goal.Title, context, repos, goal.ID)
}

// RelookGoal tells each part's newest agent to read the goal again, making
// its worktree again when it was removed. A part whose agent is gone starts
// afresh.
func (w *Work) RelookGoal(id int64) ([]Started, []string, error) {
	goal, err := w.Store.Goal(id)
	if err != nil || goal == nil {
		return nil, nil, httpErr(404, "no goal %d", id)
	}
	latest, err := w.latestPart(id)
	if err != nil {
		return nil, nil, err
	}
	told, failed := []Started{}, []string{}
	var restart []string
	for _, repo := range slices.Sorted(maps.Keys(latest)) {
		t := latest[repo]
		_, err := w.Tell(t.Agent, RelookText(*goal))
		var gone *HTTPError
		switch {
		case err == nil:
			told = append(told, Started{AgentID: t.Agent, Project: repo})
		case errors.As(err, &gone) && (gone.Status == 404 || gone.Status == 410) && slices.Contains(goal.Repos, repo):
			restart = append(restart, repo) // its agent is gone, or its worktree could not be made again: start the part afresh
		default:
			failed = append(failed, "Could not tell the "+cmp.Or(repo, "goal")+" agent: "+err.Error())
		}
	}
	if len(restart) > 0 {
		started, notStarted := w.startParts(goal, restart)
		told, failed = append(told, started...), append(failed, notStarted...)
	}
	if len(told) == 0 && len(failed) == 0 {
		return nil, nil, httpErr(400, "the goal has no agents to look again: start it first")
	}
	return told, failed, nil
}

// --- candidates -------------------------------------------------------------------

// Candidate is something that could become a goal.
type Candidate struct {
	Source  string   `json:"source"`
	Title   string   `json:"title"`
	Context string   `json:"context"`
	Repos   []string `json:"repos"`
	URL     string   `json:"url,omitempty"`
	Goal    *int64   `json:"goal,omitempty"`
	// State and StateType are a Linear issue's: "Todo", "unstarted".
	State     string `json:"state,omitempty"`
	StateType string `json:"stateType,omitempty"`
}

var (
	openTodo = regexp.MustCompile(`^(\s*)[-*]\s+\[ \]\s+(.+)$`)
	mdLink   = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	emphasis = regexp.MustCompile("\\*\\*|`")
	repoName = regexp.MustCompile(`^[\w.-]{1,64}$`)
)

// TodoCandidates is the open "- [ ]" lines of a project's TODO.md, each with
// the lines indented under it as context.
func TodoCandidates(repo, todo string, known map[string]int64) []Candidate {
	lines := strings.Split(todo, "\n")
	found := []Candidate{}
	for i, line := range lines {
		match := openTodo.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		indent := len(match[1])
		var below []string
		for _, next := range lines[i+1:] {
			if strings.TrimSpace(next) != "" && len(next)-len(strings.TrimLeft(next, " \t")) <= indent {
				break
			}
			below = append(below, next)
		}
		context := strings.TrimSpace(strings.Join(below, "\n"))
		if len(context) > MaxGoalContext {
			context = context[:MaxGoalContext]
		}
		source := "todo:" + repo + ":" + strings.TrimSpace(line)
		if len(source) > 300 {
			source = source[:300]
		}
		title := strings.TrimSpace(mdLink.ReplaceAllString(emphasis.ReplaceAllString(match[2], ""), "$1"))
		if runes := []rune(title); len(runes) > 200 {
			title = string(runes[:200])
		}
		candidate := Candidate{Source: source, Title: title, Context: context, Repos: []string{repo}}
		if id, ok := known[source]; ok {
			candidate.Goal = &id
		}
		found = append(found, candidate)
	}
	return found
}

// --- routes ----------------------------------------------------------------------

func newSession() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "goal-" + hex.EncodeToString(b)
}

// MountWork adds the routes for work, the ledger, approvals, settings and goals.
func (w *Work) MountWork(p *part.Part) {
	p.Handle("POST /v1/work/code", w.codeRoute)
	p.Handle("POST /v1/work/ask", w.askRoute)
	p.Handle("POST /v1/work/reply", w.replyRoute)
	p.Handle("POST /v1/work/tell", w.tellRoute)
	p.Handle("POST /v1/work/part", w.partRoute)
	// A day back by default, at most a year: my-agent's shapes, {tasks} and {enabled, calls}.
	since := func(r *http.Request) int64 {
		asked, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		now := w.Store.now()
		if asked == 0 {
			asked = now.Add(-24 * time.Hour).UnixMilli()
		}
		return max(asked, now.Add(-365*24*time.Hour).UnixMilli())
	}
	p.Handle("GET /v1/tasks", func(rw http.ResponseWriter, r *http.Request) {
		tasks, err := w.Store.TasksSince(since(r))
		if err != nil {
			fail(rw, err)
			return
		}
		views := w.TaskViews(tasks)
		if views == nil {
			views = []TaskView{}
		}
		part.JSON(rw, http.StatusOK, map[string]any{"tasks": views})
	})
	p.Handle("GET /v1/auto-allowed", func(rw http.ResponseWriter, r *http.Request) {
		calls, err := w.Store.AutoAllowedSince(since(r))
		if err != nil {
			fail(rw, err)
			return
		}
		if calls == nil {
			calls = []AutoAllowed{}
		}
		part.JSON(rw, http.StatusOK, map[string]any{"enabled": w.Automation().AutoAllow, "calls": calls})
	})
	p.Handle("GET /v1/settings/coding-agent", func(rw http.ResponseWriter, _ *http.Request) {
		part.JSON(rw, http.StatusOK, w.CodingAgent())
	})
	p.Handle("PATCH /v1/settings/coding-agent", func(rw http.ResponseWriter, r *http.Request) {
		var body CodingAgentSettings
		if err := part.Decode(r, &body); err != nil || !slices.Contains(Engines, body.Primary) {
			part.Error(rw, http.StatusBadRequest, "`primary` must be claude or copilot")
			return
		}
		if err := w.Store.SetSetting("coding-agent", body); err != nil {
			fail(rw, err)
			return
		}
		part.JSON(rw, http.StatusOK, body)
	})
	p.Handle("GET /v1/settings/automation", func(rw http.ResponseWriter, _ *http.Request) {
		part.JSON(rw, http.StatusOK, w.Automation())
	})
	p.Handle("PATCH /v1/settings/automation", func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			AutoAllow     *bool `json:"autoAllow"`
			FollowThrough *bool `json:"followThrough"`
		}
		if err := part.Decode(r, &body); err != nil || (body.AutoAllow == nil && body.FollowThrough == nil) {
			part.Error(rw, http.StatusBadRequest, "send `autoAllow` and/or `followThrough`, true or false")
			return
		}
		settings := w.Automation()
		if body.AutoAllow != nil {
			settings.AutoAllow = *body.AutoAllow
		}
		if body.FollowThrough != nil {
			settings.FollowThrough = *body.FollowThrough
		}
		if err := w.Store.SetSetting("automation", settings); err != nil {
			fail(rw, err)
			return
		}
		part.JSON(rw, http.StatusOK, settings)
	})
	p.Handle("GET /v1/goals", func(rw http.ResponseWriter, _ *http.Request) {
		views, err := w.GoalViews()
		if err != nil {
			fail(rw, err)
			return
		}
		part.JSON(rw, http.StatusOK, map[string]any{"goals": views})
	})
	p.Handle("POST /v1/goals", w.addGoal)
	p.Handle("GET /v1/goals/candidates", w.candidates)
	p.Handle("PATCH /v1/goals/{id}", w.patchGoal)
	p.Handle("DELETE /v1/goals/{id}", func(rw http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		deleted, err := w.Store.DeleteGoal(id)
		if err != nil {
			fail(rw, err)
			return
		}
		if !deleted {
			part.Error(rw, http.StatusNotFound, "no goal "+r.PathValue("id"))
			return
		}
		part.JSON(rw, http.StatusOK, map[string]bool{"ok": true})
	})
	p.Handle("POST /v1/goals/{id}/start", w.startGoalRoute)
	p.Handle("POST /v1/goals/{id}/relook", func(rw http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		told, failed, err := w.RelookGoal(id)
		if err != nil {
			fail(rw, err)
			return
		}
		view, _ := w.goalView(id)
		part.JSON(rw, http.StatusOK, map[string]any{"goal": view, "told": told, "failed": failed})
	})
	p.Handle("POST /v1/goals/{id}/parts/{repo}/restart", func(rw http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		started, failed, err := w.RestartPart(id, r.PathValue("repo"))
		if err != nil {
			fail(rw, err)
			return
		}
		view, _ := w.goalView(id)
		part.JSON(rw, http.StatusOK, map[string]any{"goal": view, "started": started, "failed": failed})
	})
	p.Handle("POST /v1/goals/{id}/resume", func(rw http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		due, err := w.ResumeGoal(r.Context(), id)
		if err != nil {
			fail(rw, err)
			return
		}
		view, _ := w.goalView(id)
		part.JSON(rw, http.StatusOK, map[string]any{"goal": view, "due": due})
	})
	p.Handle("POST /v1/goals/{id}/close", func(rw http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		branches, err := w.CloseGoal(id)
		if err != nil {
			fail(rw, err)
			return
		}
		view, _ := w.goalView(id)
		part.JSON(rw, http.StatusOK, map[string]any{"goal": view, "branches": branches})
	})
	p.Handle("POST /v1/goals/{id}/reopen", func(rw http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if goal, _ := w.Store.Goal(id); goal == nil {
			part.Error(rw, http.StatusNotFound, "no goal "+r.PathValue("id"))
			return
		}
		if err := w.Store.SetGoalClosed(id, false); err != nil {
			fail(rw, err)
			return
		}
		view, _ := w.goalView(id)
		part.JSON(rw, http.StatusOK, view)
	})
	p.Handle("POST /v1/goals/{id}/images", w.addImage)
	p.Handle("GET /v1/goals/{id}/images/{imageId}", func(rw http.ResponseWriter, r *http.Request) {
		goal, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		image, _ := strconv.ParseInt(r.PathValue("imageId"), 10, 64)
		data, kind, ok := w.Store.GoalImageData(goal, image)
		if !ok {
			part.Error(rw, http.StatusNotFound, "no such picture")
			return
		}
		rw.Header().Set("Content-Type", kind)
		_, _ = rw.Write(data)
	})
	p.Handle("PATCH /v1/goals/{id}/images/{imageId}", w.patchImage)
	p.Handle("DELETE /v1/goals/{id}/images/{imageId}", func(rw http.ResponseWriter, r *http.Request) {
		goal, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		image, _ := strconv.ParseInt(r.PathValue("imageId"), 10, 64)
		deleted, err := w.Store.DeleteGoalImage(goal, image)
		if err != nil {
			fail(rw, err)
			return
		}
		if !deleted {
			part.Error(rw, http.StatusNotFound, "no such picture")
			return
		}
		part.JSON(rw, http.StatusOK, map[string]bool{"ok": true})
	})
}

func (w *Work) codeRoute(rw http.ResponseWriter, r *http.Request) {
	var body CodeRequest
	if err := part.Decode(r, &body); err != nil || body.Session == "" || strings.TrimSpace(body.Prompt) == "" || len(body.Projects) == 0 || len(body.Projects) > MaxParts {
		part.Error(rw, http.StatusBadRequest, fmt.Sprintf("send `session`, `prompt`, `name` and 1 to %d `projects`", MaxParts))
		return
	}
	if body.Name == "" {
		body.Name = ShortTask(strings.SplitN(body.Prompt, "\n", 2)[0], 50)
	}
	goal := body.Goal
	if goal == "" {
		goal = body.Name
	}
	handed := w.Code(body, nil)
	w.record(body.Session, goal, &handed)
	part.JSON(rw, http.StatusOK, handed)
}

func (w *Work) askRoute(rw http.ResponseWriter, r *http.Request) {
	var body AskRequest
	if err := part.Decode(r, &body); err != nil || body.Session == "" || strings.TrimSpace(body.Prompt) == "" {
		part.Error(rw, http.StatusBadRequest, "send `session` and `prompt`")
		return
	}
	if body.Name == "" {
		body.Name = ShortTask(strings.SplitN(body.Prompt, "\n", 2)[0], 50)
	}
	a, err := w.Ask(body)
	if err != nil {
		fail(rw, err)
		return
	}
	w.Agents.mu.Lock()
	summary := Summarise(a)
	w.Agents.mu.Unlock()
	part.JSON(rw, http.StatusOK, summary)
}

func (w *Work) replyRoute(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		Session       string   `json:"session"`
		Agent         string   `json:"agent"`
		Text          string   `json:"text"`
		Images        []string `json:"images"`
		Communication string   `json:"communication"`
		Mode          string   `json:"mode"`
	}
	if err := part.Decode(r, &body); err != nil || body.Agent == "" || strings.TrimSpace(body.Text) == "" {
		part.Error(rw, http.StatusBadRequest, "send `agent` and `text`")
		return
	}
	a, err := w.Reply(body.Session, body.Agent, body.Text, body.Images, body.Communication, body.Mode)
	if err != nil {
		fail(rw, err)
		return
	}
	w.Agents.mu.Lock()
	summary := Summarise(a)
	w.Agents.mu.Unlock()
	part.JSON(rw, http.StatusOK, summary)
}

func (w *Work) tellRoute(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		Agent string `json:"agent"`
		Text  string `json:"text"`
	}
	if err := part.Decode(r, &body); err != nil || body.Agent == "" || strings.TrimSpace(body.Text) == "" {
		part.Error(rw, http.StatusBadRequest, "send `agent` and `text`")
		return
	}
	a, err := w.Tell(body.Agent, body.Text)
	if err != nil {
		fail(rw, err)
		return
	}
	w.Agents.mu.Lock()
	summary := Summarise(a)
	w.Agents.mu.Unlock()
	part.JSON(rw, http.StatusOK, summary)
}

// partRoute adds another project's part to a conversation's code task, on
// the same branch name: the conversation's own agent's task first (a reply
// can pick up a coding agent started elsewhere), else its latest.
func (w *Work) partRoute(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
		Agent   string `json:"agent"`
		Project string `json:"project"`
		Task    string `json:"task"`
	}
	if err := part.Decode(r, &body); err != nil || body.Session == "" || body.Project == "" || strings.TrimSpace(body.Task) == "" {
		part.Error(rw, http.StatusBadRequest, "send `session`, `project` and `task`")
		return
	}
	var of *Task
	if body.Agent != "" {
		if t, _ := w.Store.TaskOfAgent(body.Agent); t != nil && t.Kind == "code" && t.Branch != nil {
			of = t
		}
	}
	if of == nil {
		var err error
		if of, err = w.Store.LatestCodeTask(body.Session); err != nil {
			fail(rw, err)
			return
		}
	}
	if of == nil {
		part.Error(rw, http.StatusNotFound, "No code task in this conversation to add a part to. Start one with /agent <project> <task>.")
		return
	}
	part.JSON(rw, http.StatusOK, w.startPart(r.Context(), of, ProjectTask{Project: body.Project, Task: strings.TrimSpace(body.Task)}))
}

type goalBody struct {
	Title   *string  `json:"title"`
	Repos   []string `json:"repos"`
	Source  string   `json:"source"`
	Context *string  `json:"context"`
	Night   *bool    `json:"night"`
}

func (b goalBody) check(creating bool) error {
	if (creating || b.Title != nil) && (b.Title == nil || strings.TrimSpace(*b.Title) == "" || len([]rune(strings.TrimSpace(*b.Title))) > 200) {
		return httpErr(400, "`title` must be 1 to 200 characters")
	}
	if len(b.Repos) > MaxParts || slices.ContainsFunc(b.Repos, func(repo string) bool { return !repoName.MatchString(repo) }) {
		return httpErr(400, "`repos` must be at most %d project names", MaxParts)
	}
	if b.Context != nil && len(*b.Context) > MaxGoalContext {
		return httpErr(400, "`context` must be at most %d characters", MaxGoalContext)
	}
	if len(b.Source) > 300 {
		return httpErr(400, "`source` must be 1 to 300 characters")
	}
	return nil
}

func (w *Work) addGoal(rw http.ResponseWriter, r *http.Request) {
	var body goalBody
	if err := part.Decode(r, &body); err != nil {
		part.Error(rw, http.StatusBadRequest, err.Error())
		return
	}
	if err := body.check(true); err != nil {
		fail(rw, err)
		return
	}
	source := strings.TrimSpace(body.Source)
	if source == "" {
		source = "manual"
	}
	context := ""
	if body.Context != nil {
		context = strings.TrimSpace(*body.Context)
	}
	repos := slices.Compact(slices.Clone(body.Repos))
	if repos == nil {
		repos = []string{}
	}
	goal, err := w.Store.AddGoal(strings.TrimSpace(*body.Title), repos, source, context, newSession())
	if err != nil {
		fail(rw, err)
		return
	}
	view, _ := w.goalView(goal.ID)
	part.JSON(rw, http.StatusCreated, view)
}

func (w *Work) patchGoal(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body goalBody
	if err := part.Decode(r, &body); err != nil {
		part.Error(rw, http.StatusBadRequest, err.Error())
		return
	}
	if body.Title == nil && body.Repos == nil && body.Context == nil && body.Night == nil {
		part.Error(rw, http.StatusBadRequest, "send `title`, `repos`, `context` and/or `night`")
		return
	}
	if err := body.check(false); err != nil {
		fail(rw, err)
		return
	}
	if goal, _ := w.Store.Goal(id); goal == nil {
		part.Error(rw, http.StatusNotFound, "no goal "+r.PathValue("id"))
		return
	}
	var title, context *string
	if body.Title != nil {
		trimmed := strings.TrimSpace(*body.Title)
		title = &trimmed
	}
	if body.Context != nil {
		trimmed := strings.TrimSpace(*body.Context)
		context = &trimmed
	}
	var repos []string
	if body.Repos != nil {
		repos = slices.Compact(slices.Clone(body.Repos))
	}
	if body.Night != nil {
		if err := w.Store.SetGoalNight(id, *body.Night); err != nil {
			fail(rw, err)
			return
		}
	}
	if title != nil || repos != nil || context != nil {
		if err := w.Store.UpdateGoal(id, title, repos, context); err != nil {
			fail(rw, err)
			return
		}
		// The agents already started read the goal again, best effort.
		_, _, _ = w.RelookGoal(id)
	}
	view, err := w.goalView(id)
	if err != nil {
		fail(rw, err)
		return
	}
	part.JSON(rw, http.StatusOK, view)
}

func (w *Work) startGoalRoute(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		Repos []string `json:"repos"`
	}
	_ = part.Decode(r, &body)
	started, failed, err := w.StartGoal(id, body.Repos)
	if err != nil {
		fail(rw, err)
		return
	}
	view, _ := w.goalView(id)
	if failed == nil {
		failed = []string{}
	}
	part.JSON(rw, http.StatusOK, map[string]any{"goal": view, "started": started, "failed": failed})
}

func (w *Work) candidates(rw http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	project := query.Get("project")
	goals, _ := w.Store.Goals()
	known := map[string]int64{}
	for _, g := range goals {
		known[g.Source] = g.ID
	}
	switch query.Get("from") {
	case "todo":
		if project == "" {
			part.Error(rw, http.StatusBadRequest, "`project` is required for todo")
			return
		}
		docs, err := ReadProjectDocs(w.Agents.roots[0], project)
		if err != nil {
			fail(rw, err)
			return
		}
		todo := ""
		for _, file := range docs.Files {
			if strings.EqualFold(file.File, "TODO.md") && file.Content != nil {
				todo = *file.Content
			}
		}
		part.JSON(rw, http.StatusOK, map[string]any{"from": "todo", "candidates": TodoCandidates(project, todo, known)})
	case "linear":
		if !w.Linear.On() {
			part.Error(rw, http.StatusServiceUnavailable, "no LINEAR_KEY in sindri.env")
			return
		}
		assignee := query.Get("assignee")
		if assignee != "" && !slices.Contains(linear.Assignees, assignee) {
			part.Error(rw, http.StatusBadRequest, "`assignee` must be mine, unassigned or all")
			return
		}
		found, err := LinearCandidates(r.Context(), w.Linear, linear.Filter{Project: project, Query: strings.TrimSpace(query.Get("query")), Assignee: assignee}, known)
		if err != nil {
			w.logf("Linear did not answer: %v", err)
			part.Error(rw, http.StatusBadGateway, "Linear did not answer")
			return
		}
		part.JSON(rw, http.StatusOK, map[string]any{"from": "linear", "candidates": found})
	default:
		part.Error(rw, http.StatusBadRequest, "`from` must be todo or linear")
	}
}

var imageMediaTypes = map[string]string{"png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp"}

func (w *Work) addImage(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	r.Body = http.MaxBytesReader(rw, r.Body, MaxGoalImageBytes*4/3+4096)
	var body struct {
		Name string `json:"name"`
		Data string `json:"data"`
		Kind string `json:"kind"`
		Note string `json:"note"`
	}
	if err := decodeLarge(r, &body); err != nil {
		part.Error(rw, http.StatusBadRequest, err.Error())
		return
	}
	if goal, _ := w.Store.Goal(id); goal == nil {
		part.Error(rw, http.StatusNotFound, "no goal "+r.PathValue("id"))
		return
	}
	mediaType := imageMediaTypes[strings.ToLower(body.Name[strings.LastIndex(body.Name, ".")+1:])]
	data, err := base64.StdEncoding.DecodeString(body.Data)
	kind := body.Kind
	if kind == "" {
		kind = "design"
	}
	switch {
	case strings.TrimSpace(body.Name) == "" || len(body.Name) > 200:
		part.Error(rw, http.StatusBadRequest, "`name` must be the file name")
	case mediaType == "":
		part.Error(rw, http.StatusBadRequest, "only png, jpg, gif and webp pictures")
	case err != nil || len(data) == 0:
		part.Error(rw, http.StatusBadRequest, "`data` must be the picture in base64")
	case len(data) > MaxGoalImageBytes:
		part.Error(rw, http.StatusBadRequest, body.Name+" is over 10 MB")
	case kind != "design" && kind != "current":
		part.Error(rw, http.StatusBadRequest, "`kind` must be `design` or `current`")
	case len(body.Note) > 500:
		part.Error(rw, http.StatusBadRequest, "`note` must be at most 500 characters")
	default:
		if images, _ := w.Store.GoalImages(id); len(images) >= MaxGoalImages {
			part.Error(rw, http.StatusBadRequest, fmt.Sprintf("a goal has at most %d pictures", MaxGoalImages))
			return
		}
		image, err := w.Store.AddGoalImage(id, strings.TrimSpace(body.Name), mediaType, kind, strings.TrimSpace(body.Note), data)
		if err != nil {
			fail(rw, err)
			return
		}
		part.JSON(rw, http.StatusCreated, image)
	}
}

func (w *Work) patchImage(rw http.ResponseWriter, r *http.Request) {
	goal, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	image, _ := strconv.ParseInt(r.PathValue("imageId"), 10, 64)
	var body struct {
		Kind *string `json:"kind"`
		Note *string `json:"note"`
	}
	if err := part.Decode(r, &body); err != nil || (body.Kind == nil && body.Note == nil) {
		part.Error(rw, http.StatusBadRequest, "send `kind` and/or `note`")
		return
	}
	if body.Kind != nil && *body.Kind != "design" && *body.Kind != "current" {
		part.Error(rw, http.StatusBadRequest, "`kind` must be `design` or `current`")
		return
	}
	updated, err := w.Store.UpdateGoalImage(goal, image, body.Kind, body.Note)
	if err != nil {
		fail(rw, err)
		return
	}
	if !updated {
		part.Error(rw, http.StatusNotFound, "no such picture")
		return
	}
	images, _ := w.Store.GoalImages(goal)
	for _, i := range images {
		if i.ID == image {
			part.JSON(rw, http.StatusOK, i)
			return
		}
	}
}
