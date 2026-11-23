package sindri

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type post struct{ Session, Agent, Label, Content string }

// fakeNorn records what Sindri posts into conversations.
type fakeNorn struct {
	mu    sync.Mutex
	posts []post
}

func (f *fakeNorn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct{ Agent, Label, Content string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	session := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/conversations/"), "/agent-turns")
	f.mu.Lock()
	f.posts = append(f.posts, post{session, body.Agent, body.Label, body.Content})
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
}

func (f *fakeNorn) said() []post {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]post{}, f.posts...)
}

func newWork(t *testing.T) (*Work, *fakeNorn) {
	t.Helper()
	m, _ := setup(t)
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	norn := &fakeNorn{}
	server := httptest.NewServer(norn)
	t.Cleanup(server.Close)
	w := &Work{Agents: m, Store: store, Norn: &NornClient{URL: server.URL, Token: "t", HTTP: server.Client()}, AskCwd: m.roots[0], AutoAllowDefault: true, logf: t.Logf}
	return w, norn
}

func feedUntil(t *testing.T, w *Work, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_ = w.Feed(context.Background())
		if ok() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("never got there")
}

func TestACodingTaskPostsIntoItsConversationAndEndsInTheLedger(t *testing.T) {
	w, norn := newWork(t)
	handed := w.Code(CodeRequest{Session: "s1", Prompt: "change the work file", Name: "Change work", Projects: []string{"demo"}, Communication: "concise"}, nil)
	w.record("s1", "Change work", &handed)
	if len(handed.Parts) != 1 || !strings.HasPrefix(handed.Parts[0].Branch, "agent/") || !strings.Contains(handed.Answer, "Claude is coding this in demo") {
		t.Fatalf("got %+v", handed)
	}
	feedUntil(t, w, func() bool {
		for _, p := range norn.said() {
			if p.Agent == handed.Parts[0].AgentID && strings.Contains(p.Content, "Should I merge it?") {
				return true
			}
		}
		return false
	})
	posts := norn.said()
	if posts[len(posts)-1].Session != "s1" || posts[len(posts)-1].Label != "Change work" {
		t.Fatalf("got %+v", posts)
	}
	views := w.TaskViews(must2(w.Store.TasksSince(0)))
	if len(views) != 1 || views[0].Status != "needs-you" || *views[0].Outcome != "asked" {
		t.Fatalf("an agent that asks you something needs you: %+v", views)
	}
	// Nothing new: nothing posted again.
	before := len(norn.said())
	_ = w.Feed(context.Background())
	if len(norn.said()) != before {
		t.Fatalf("posted the same state twice")
	}
}

func must2[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func TestEverydayToolCallsAreAllowedAndWrittenDown(t *testing.T) {
	w, _ := newWork(t)
	a, err := w.start(StartRequest{Cwd: "demo", Prompt: "ask first", Session: "s1"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, w.Agents, a.ID, func(a *Agent) bool { return a.Status == NeedsApproval })
	_ = w.Feed(context.Background())
	if allowed := must2(w.Store.AutoAllowedSince(0)); len(allowed) != 0 {
		t.Fatalf("rm -rf was allowed: %+v", allowed)
	}
	for tool, want := range map[string]string{"git diff HEAD": "git read", "npm test": "tests", "git push": "", "cat ../secret": "", "npm test; rm -rf /": ""} {
		if got := AutoAllowRule("Bash", map[string]any{"command": tool}, []string{"/w"}); got != want {
			t.Errorf("%q: got %q, want %q", tool, got, want)
		}
	}
	for path, want := range map[string]string{"/w/src/a.go": "read", "src/a.go": "read", "/w/.env": "", "/w/.env.example": "read", "/etc/passwd": ""} {
		if got := AutoAllowRule("Read", map[string]any{"file_path": path}, []string{"/w"}); got != want {
			t.Errorf("%q: got %q, want %q", path, got, want)
		}
	}
}

func TestAGoalStartsItsRepoAndReadsItsStateFromItsTasks(t *testing.T) {
	w, _ := newWork(t)
	goal, err := w.Store.AddGoal("Add a work file", []string{"demo"}, "manual", "", "goal-1")
	if err != nil {
		t.Fatal(err)
	}
	if views := must2(w.GoalViews()); views[0].State != "draft" || views[0].Next.Action != "start" {
		t.Fatalf("got %+v", views[0])
	}
	started, failed, err := w.StartGoal(goal.ID, nil)
	if err != nil || len(started) != 1 || len(failed) != 0 {
		t.Fatalf("got %+v %v %v", started, failed, err)
	}
	waitFor(t, w.Agents, started[0].AgentID, func(a *Agent) bool { return a.Status == Waiting })
	views := must2(w.GoalViews())
	if views[0].State != "needs-you" || views[0].Next.Action != "answer" || len(views[0].Tasks) != 1 {
		t.Fatalf("got %+v", views[0])
	}
	if _, _, err := w.StartGoal(goal.ID, nil); err == nil {
		t.Fatalf("started the same repo twice")
	}
}

func TestClosingAGoalRemovesItsEmptyBranchesAndKeepsTheRest(t *testing.T) {
	w, _ := newWork(t)
	goal, _ := w.Store.AddGoal("Leave it as it is", []string{"demo"}, "manual", "", "goal-1")
	started, _, err := w.StartGoal(goal.ID, nil)
	if err != nil || len(started) != 1 {
		t.Fatalf("got %+v %v", started, err)
	}
	a := waitFor(t, w.Agents, started[0].AgentID, func(a *Agent) bool { return a.Status == Waiting })
	path := a.Worktree.Path
	branches, err := w.CloseGoal(goal.ID)
	if err != nil || len(branches) != 1 || !branches[0].Removed {
		t.Fatalf("got %+v %v", branches, err)
	}
	if exists(path) || a.Worktree.branchExists() {
		t.Fatalf("the empty branch is still there")
	}
	if view := must2(w.goalView(goal.ID)); view.State != "closed" || view.ClosedAt == nil {
		t.Fatalf("got %+v", view)
	}
	if err := w.Store.SetGoalClosed(goal.ID, false); err != nil || must2(w.goalView(goal.ID)).State == "closed" {
		t.Fatalf("could not open it again: %v", err)
	}

	kept, _ := w.Store.AddGoal("Change something", []string{"demo"}, "manual", "", "goal-2")
	started, _, _ = w.StartGoal(kept.ID, nil)
	b := waitFor(t, w.Agents, started[0].AgentID, func(a *Agent) bool { return a.Status == Waiting })
	if err := os.WriteFile(filepath.Join(b.Worktree.Path, "work.txt"), []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}
	branches, err = w.CloseGoal(kept.ID)
	if err != nil || len(branches) != 1 || branches[0].Removed || !exists(b.Worktree.Path) {
		t.Fatalf("removed a branch with work: %+v %v", branches, err)
	}
}

func TestAGoalPartThatChangedNothingLosesItsWorktreeWhenItFinishes(t *testing.T) {
	w, _ := newWork(t)
	goal, _ := w.Store.AddGoal("Leave it as it is", []string{"demo"}, "manual", "", "goal-1")
	started, _, err := w.StartGoal(goal.ID, nil)
	if err != nil || len(started) != 1 {
		t.Fatalf("got %+v %v", started, err)
	}
	a := waitFor(t, w.Agents, started[0].AgentID, func(a *Agent) bool { return a.Status == Waiting })
	path := a.Worktree.Path
	feedUntil(t, w, func() bool { return !exists(path) })
	if a.Worktree.branchExists() {
		t.Fatalf("the empty branch is still there")
	}
	view := must2(w.goalView(goal.ID))
	if len(view.Tasks) != 1 || view.Tasks[0].Status != "no-changes" || view.Tasks[0].Branch != nil || view.Next.Text != "No changes needed" {
		t.Fatalf("got %+v", view)
	}
}

func TestAnAgentStoppedMidWorkKeepsItsWorktree(t *testing.T) {
	w, _ := newWork(t)
	for _, snap := range []snapshot{
		{agent: Agent{ID: "a", Status: Done, LastText: "", Worktree: &Worktree{}}, turnDone: false},
		{agent: Agent{ID: "a", Status: Done, LastText: "", Worktree: &Worktree{}}, turnDone: true},
		{agent: Agent{ID: "a", Status: Waiting, LastText: "Reading the code.", Worktree: &Worktree{}}, turnDone: false},
	} {
		if w.removeIfNoChanges(&snap) {
			t.Fatalf("removed the worktree of %+v", snap)
		}
	}
}

func TestAPartWhoseWorktreeIsGoneGetsItBackOnLookAgainAndStartsAfreshOnRestart(t *testing.T) {
	w, _ := newWork(t)
	goal, _ := w.Store.AddGoal("Leave it as it is", []string{"demo"}, "manual", "", "goal-1")
	started, _, _ := w.StartGoal(goal.ID, nil)
	first := started[0].AgentID
	a := waitFor(t, w.Agents, first, func(a *Agent) bool { return a.Status == Waiting })
	path := a.Worktree.Path
	feedUntil(t, w, func() bool { return !exists(path) })

	told, failed, err := w.RelookGoal(goal.ID)
	if err != nil || len(failed) != 0 || len(told) != 1 || told[0].AgentID != first {
		t.Fatalf("look again did not reach the part: %+v %v %v", told, failed, err)
	}
	if !exists(path) || !a.Worktree.branchExists() {
		t.Fatalf("look again did not make the worktree again")
	}
	waitFor(t, w.Agents, first, func(a *Agent) bool { return a.Status == Waiting })
	second := first

	restarted, failed, err := w.RestartPart(goal.ID, "demo")
	if err != nil || len(failed) != 0 || len(restarted) != 1 || restarted[0].AgentID == second {
		t.Fatalf("got %+v %v %v", restarted, failed, err)
	}
	if _, _, err := w.RestartPart(goal.ID, "other"); err == nil {
		t.Fatalf("restarted a repo the goal does not have")
	}
}

func TestTodoCandidates(t *testing.T) {
	todo := "# Todo\n- [x] done\n- [ ] Add **dark** mode to [the panel](x.md)\n  with a toggle\n- [ ] Second\n"
	got := TodoCandidates("demo", todo, map[string]int64{"todo:demo:- [ ] Second": 7})
	if len(got) != 2 || got[0].Title != "Add dark mode to the panel" || got[0].Context != "with a toggle" || got[1].Goal == nil || *got[1].Goal != 7 {
		t.Fatalf("got %+v", got)
	}
}

func TestNeededPartsAndMessages(t *testing.T) {
	text := "Done.\nNeeds a part in `api`: add the route\nTell the web part: the field is called total\nNeeds a part in unknown: x"
	if parts := NeededParts(text, []string{"api", "web"}); len(parts) != 1 || parts[0].Task != "add the route" {
		t.Fatalf("got %+v", parts)
	}
	if messages := PartMessages(text, []string{"api", "web"}); len(messages) != 1 || messages[0].Project != "web" {
		t.Fatalf("got %+v", messages)
	}
	if PickedTask("Task: **Fix the DM title**.\nDone") != "Fix the DM title" {
		t.Fatalf("got %q", PickedTask("Task: **Fix the DM title**.\nDone"))
	}
}

func TestResumingAGoalMakesItsParkedPartsDueNow(t *testing.T) {
	w, _ := newWork(t)
	goal, _ := w.Store.AddGoal("Wait for usage", []string{"demo"}, "manual", "", "goal-1")
	if _, err := w.ResumeGoal(context.Background(), goal.ID); err == nil {
		t.Fatalf("resumed a goal with nothing waiting")
	}
	if _, err := w.Store.AddTask("goal-1", "Wait for usage", "code", "a1", "demo", "", 0, &goal.ID); err != nil {
		t.Fatal(err)
	}
	later := w.Store.now().Add(time.Hour).UnixMilli()
	if err := w.Store.Schedule("a1", Scheduled{AgentID: "a1", NextTryAt: later}, "usage limit"); err != nil {
		t.Fatal(err)
	}
	w.feeding.Lock() // no pass: only the due time is under test
	defer w.feeding.Unlock()
	due, err := w.ResumeGoal(context.Background(), goal.ID)
	if err != nil || due != 1 {
		t.Fatalf("got %d %v", due, err)
	}
	tasks := must2(w.Store.ScheduledTasks())
	if len(tasks) != 1 || tasks[0].Scheduled.NextTryAt > w.Store.ms() {
		t.Fatalf("got %+v", tasks)
	}
}

func TestAskAgentFollowsTheConversationsMode(t *testing.T) {
	w, _ := newWork(t)
	a, err := w.Ask(AskRequest{Session: "s", Prompt: "check something", Name: "check", Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if a.PermissionMode != "auto" {
		t.Fatalf("auto conversation started the question agent in %q", a.PermissionMode)
	}
	// The fake Claude stops on a tool whenever its text says "ask", as the auto rule does.
	waitFor(t, w.Agents, a.ID, func(a *Agent) bool { return a.Status == NeedsApproval })
	if _, err := w.Agents.Answer(a.ID, "r1", true, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, w.Agents, a.ID, func(a *Agent) bool { return settled(a.Status) })
	again, err := w.Ask(AskRequest{Session: "s", Prompt: "and this", Name: "check", Mode: "manual", Agent: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != a.ID || again.PermissionMode != "default" {
		t.Fatalf("manual follow-up: agent %s in %q, want %s in default", again.ID, again.PermissionMode, a.ID)
	}
	plain, err := w.Ask(AskRequest{Session: "t", Prompt: "check", Name: "check"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.PermissionMode != "default" {
		t.Fatalf("a conversation with no mode started in %q", plain.PermissionMode)
	}
}

func TestRemoteControlLinkIsKept(t *testing.T) {
	a := &Agent{Status: Working}
	link := "https://claude.ai/code/session_01Abc"
	a.applyClaude(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "other",
		"response": map[string]any{"session_url": "https://claude.ai/code/elsewhere"}}}, 0, 1)
	if a.RemoteURL != "" {
		t.Fatalf("another request's answer set the link to %q", a.RemoteURL)
	}
	a.applyClaude(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": remoteControlID,
		"response": map[string]any{"session_url": link}}}, 0, 2)
	if a.RemoteURL != link || a.Status != Working {
		t.Fatalf("link %q status %s, want %q working", a.RemoteURL, a.Status, link)
	}
	b := &Agent{Status: Working}
	b.applyClaude(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": remoteControlID,
		"error": "Remote Control is not enabled for this account"}}, 0, 3)
	if b.RemoteURL != "" || len(b.Log) != 1 || !strings.Contains(b.Log[0].Text, "not enabled") {
		t.Fatalf("a refusal left link %q and log %+v", b.RemoteURL, b.Log)
	}
}

func TestPlanCandidatesReadDocsFolder(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "demo")
	for path, text := range map[string]string{
		".git/HEAD":            "",
		"TODO.md":              "- [ ] Root\n",
		"PLAN.md":              "- [ ] Top plan\n",
		"docs/plan.md":         "# Plan\n- [x] done\n- [ ] Build it\n  with care\n",
		"docs/01-plan-ui.md":   "- [ ] Build it\n",
		"docs/ideas.md":        "# Ideas\nintro\n## 1. First idea\nBody one\n\n## 2) Second **idea**\n",
		"docs/architecture.md": "## Not a task\n",
		"docs/README.md":       "## Skip me\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(project, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, path), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := PlanCandidates(root, "demo", map[string]int64{"todo:demo:docs/plan.md:- [ ] Build it": 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 || got[0].Title != "Top plan" || got[1].Source != "todo:demo:- [ ] Root" || got[2].Source != "todo:demo:docs/01-plan-ui.md:- [ ] Build it" || got[3].Title != "First idea" || got[3].Context != "Body one" || got[4].Title != "Second idea" || got[5].Context != "with care" || got[5].Goal == nil {
		t.Fatalf("got %+v", got)
	}
}
