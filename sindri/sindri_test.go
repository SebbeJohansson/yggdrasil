package sindri

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/part"
)

// fakeClaude answers each message with one commit in its folder, and asks
// for permission first when the message says "ask", and changes nothing when
// it says "Leave it as it is".
const fakeClaude = `#!/bin/sh
echo '{"type":"system","subtype":"init","model":"fake-model"}'
while read -r line; do
  case "$line" in
    *control_response*) echo '{"type":"result","subtype":"success","result":"Allowed and done.","total_cost_usd":0.02,"num_turns":1}'; continue ;;
    *"Leave it as it is"*) echo '{"type":"result","subtype":"success","result":"Nothing to change.","num_turns":1}'; continue ;;
    *"Take screenshots"*) echo '{"type":"result","subtype":"success","result":"Nothing visible.","num_turns":1}'; continue ;;
    *ask*) echo '{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"rm -rf build"}}}'; continue ;;
  esac
  echo change >> work.txt
  git add work.txt >/dev/null 2>&1 && git commit -qm "Change work.txt" >/dev/null 2>&1
  echo '{"type":"assistant","message":{"content":[{"type":"text","text":"Committed. Should I merge it?"},{"type":"tool_use","name":"Bash","input":{"command":"git commit"}}]}}'
  echo '{"type":"result","subtype":"success","result":"Committed. Should I merge it?","total_cost_usd":0.01,"num_turns":1}'
done
`

func setup(t *testing.T) (*Manager, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	for name, value := range map[string]string{"GIT_AUTHOR_NAME": "Test", "GIT_AUTHOR_EMAIL": "t@example.com", "GIT_COMMITTER_NAME": "Test", "GIT_COMMITTER_EMAIL": "t@example.com", "HOME": t.TempDir()} {
		t.Setenv(name, value)
	}
	root := t.TempDir()
	repo := filepath.Join(root, "demo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "Start"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(ManagerOptions{ClaudeBin: bin, CopilotBin: bin, Roots: []string{root}, StatePath: filepath.Join(t.TempDir(), "agents.json"), MaxRunning: 4, Idle: time.Hour}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	return m, repo
}

func waitFor(t *testing.T, m *Manager, id string, ok func(*Agent) bool) *Agent {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		a := m.agents[id]
		done := ok(a)
		m.mu.Unlock()
		if done {
			return a
		}
		time.Sleep(20 * time.Millisecond)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t.Fatalf("agent never got there: %s, log %+v", m.agents[id].Status, m.agents[id].Log)
	return nil
}

func TestAnAgentWorksInAWorktreeAndItsBranchMerges(t *testing.T) {
	m, repo := setup(t)
	a, err := m.Start(StartRequest{Cwd: "demo", Prompt: "change the work file", Worktree: true, Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Worktree == nil || !strings.HasPrefix(a.Worktree.Branch, "agent/change-the-work-file-") || a.Worktree.Base != "main" {
		t.Fatalf("got %+v", a.Worktree)
	}
	a = waitFor(t, m, a.ID, func(a *Agent) bool { return a.Status == Waiting })
	if a.Model != "fake-model" || a.CostUSD != 0.01 || !a.AsksForReply() || a.Session != "s1" {
		t.Fatalf("got %+v", a)
	}
	result, err := m.Review(a.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	review := result.(*Review)
	if len(review.Commits) != 1 || review.Commits[0].Subject != "Change work.txt" || review.Merged || len(review.Files) != 1 {
		t.Fatalf("got %+v", review)
	}
	if _, err := m.Merge(a.ID); err != nil {
		t.Fatal(err)
	}
	if out, _ := exec.Command("git", "-C", repo, "log", "--format=%s", "-1").Output(); strings.TrimSpace(string(out)) != "Change work.txt" {
		t.Fatalf("main is at %q", out)
	}
	if _, err := m.Merge(a.ID); err == nil || !strings.Contains(err.Error(), "already merged") {
		t.Fatalf("merging twice: %v", err)
	}
	// A follow-up resumes the same agent.
	if _, err := m.Prompt(a.ID, "once more", nil, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, a.ID, func(a *Agent) bool { return a.Status == Waiting && a.NumTurns == 2 })
}

func TestAPermissionWaitsForYou(t *testing.T) {
	m, _ := setup(t)
	a, err := m.Start(StartRequest{Cwd: "demo", Prompt: "ask first"})
	if err != nil {
		t.Fatal(err)
	}
	a = waitFor(t, m, a.ID, func(a *Agent) bool { return a.Status == NeedsApproval })
	if len(a.Pending) != 1 || a.Pending[0].Summary != "Bash rm -rf build" {
		t.Fatalf("got %+v", a.Pending)
	}
	if _, err := m.Prompt(a.ID, "hurry", nil, false); err == nil {
		t.Fatalf("a prompt to a busy agent went through")
	}
	if _, err := m.Answer(a.ID, "r1", true, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, a.ID, func(a *Agent) bool { return a.Status == Waiting && a.LastText == "Allowed and done." })
}

func TestAgentsStayInsideTheRoots(t *testing.T) {
	m, _ := setup(t)
	if _, err := m.Start(StartRequest{Cwd: "/etc", Prompt: "x"}); err == nil || err.(*HTTPError).Status != 403 {
		t.Fatalf("got %v", err)
	}
	if _, err := m.Start(StartRequest{Cwd: "demo", Prompt: "x", PermissionMode: "yolo"}); err == nil || err.(*HTTPError).Status != 400 {
		t.Fatalf("got %v", err)
	}
}

func TestAgentsSurviveARestart(t *testing.T) {
	m, _ := setup(t)
	a, err := m.Start(StartRequest{Cwd: "demo", Prompt: "change it"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, a.ID, func(a *Agent) bool { return a.Status == Waiting })
	m.Save()
	again, err := NewManager(m.options, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	got, err := again.Get(a.ID)
	if err != nil || got.Status != Done || got.Log[len(got.Log)-1].Text != "stopped when Sindri restarted" {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestBranchesMade(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	_, repo := setup(t)
	made := BranchesMade("cd "+repo+" && git checkout -b feature/x; git switch -c 'other'", "/")
	if len(made) != 2 || made[0].Branch != "feature/x" || made[1].Branch != "other" {
		t.Fatalf("got %+v", made)
	}
}

func TestCopilotRules(t *testing.T) {
	for rule, want := range map[string]string{"Bash(npm test:*)": "shell(npm test:*)", "Bash": "shell", "mcp__linear__get_issue": "linear(get_issue)", "mcp__linear": "linear", "Read": ""} {
		if got := CopilotRule(rule); got != want {
			t.Errorf("%s: got %q, want %q", rule, got, want)
		}
	}
}

func TestMeetsItsContract(t *testing.T) {
	m, _ := setup(t)
	a, err := m.Start(StartRequest{Cwd: "demo", Prompt: "change it", Worktree: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, a.ID, func(a *Agent) bool { return a.Status == Waiting })
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	work := &Work{Agents: m, Store: store, Norn: &NornClient{}, AskCwd: m.roots[0], logf: t.Logf}
	if _, err := store.AddGoal("A goal", []string{"demo"}, "manual", "", "goal-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.PutGoalWhy(1, GoalWhy{At: 1, State: "sent", Sources: []WhySource{{Kind: "mail", At: 1, Summary: "Asked for it.", Ref: "inbox:1"}}}); err != nil {
		t.Fatal(err)
	}
	p := part.New(Name, contracttest.Tokens(), nil)
	(&Sindri{Agents: m, Root: m.roots[0], Work: work}).Mount(p)
	contracttest.Run(t, p)
}

func TestScreenshotsStoppedByARestartAreTakenAgain(t *testing.T) {
	m, _ := setup(t)
	a, err := m.Start(StartRequest{Cwd: "demo", Prompt: "change it", Worktree: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, a.ID, func(a *Agent) bool { return a.Status == Waiting && a.ScreenshotHead != "" })
	m.mu.Lock()
	for _, shot := range m.agents {
		if shot.ScreenshotFor == a.ID {
			shot.Status = Working // as if Sindri stopped mid-shot
		}
	}
	m.mu.Unlock()
	m.Save()
	again, err := NewManager(m.options, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(again.Shutdown)
	again.RetakeScreenshots()
	again.mu.Lock()
	defer again.mu.Unlock()
	shots := 0
	for _, shot := range again.agents {
		if shot.ScreenshotFor == a.ID {
			shots++
		}
	}
	if shots != 2 {
		t.Fatalf("got %d screenshot agents, want the stopped one and a new one", shots)
	}
}

func TestShowsOnScreen(t *testing.T) {
	if !ShowsOnScreen([]string{"app/pages/a.vue"}) || !ShowsOnScreen([]string{"x/style.css"}) {
		t.Fatal("ui files should show")
	}
	if ShowsOnScreen([]string{"main.go", "README.md", "a_test.go"}) {
		t.Fatal("backend and docs should not show")
	}
}

func TestASignalStopsAnAgentWithoutFailingIt(t *testing.T) {
	m, _ := setup(t)
	a, err := m.Start(StartRequest{Cwd: "demo", Prompt: "Leave it as it is"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, a.ID, func(a *Agent) bool { return a.Status == Waiting })
	m.mu.Lock()
	_ = m.live[a.ID].command.Process.Signal(syscall.SIGTERM)
	m.mu.Unlock()
	a = waitFor(t, m, a.ID, func(a *Agent) bool { return !running(a.Status) })
	if a.Status != Done || !strings.HasPrefix(a.Log[len(a.Log)-1].Text, "stopped by a signal") {
		t.Fatalf("got %s, log %+v", a.Status, a.Log)
	}
}

func TestDismissForgetsAFailedAgentAndItsEmptyWorktree(t *testing.T) {
	m, repo := setup(t)
	a, err := m.Start(StartRequest{Cwd: "demo", Prompt: "Leave it as it is", Worktree: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, a.ID, func(a *Agent) bool { return a.Status == Waiting })
	if err := m.Dismiss(a.ID); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("dismissing a running agent: %v", err)
	}
	m.close(a.ID)
	m.mu.Lock()
	a.Status = Failed
	path := a.Worktree.Path
	m.mu.Unlock()
	if err := m.Dismiss(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(a.ID); err == nil {
		t.Fatal("the agent is still listed")
	}
	if exists(path) {
		t.Fatalf("the worktree %s is still there", path)
	}
	if out, _ := exec.Command("git", "-C", repo, "branch", "--list", "agent/*").Output(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("branches left: %s", out)
	}
}

func TestDismissKeepsABranchThatHoldsWork(t *testing.T) {
	m, _ := setup(t)
	a, err := m.Start(StartRequest{Cwd: "demo", Prompt: "change the work file", Worktree: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, a.ID, func(a *Agent) bool { return a.Status == Waiting })
	m.close(a.ID)
	if err := m.Dismiss(a.ID); err == nil || !strings.Contains(err.Error(), "has not merged") {
		t.Fatalf("dismissing unmerged work: %v", err)
	}
	if _, err := m.Get(a.ID); err != nil {
		t.Fatal("the agent was forgotten")
	}
}
