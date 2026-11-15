package sindri

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestQuestionAgentInHerdr(t *testing.T) {
	root, claude := t.TempDir(), t.TempDir()
	var mu sync.Mutex
	var panes []map[string]any
	var prompts []string
	var startArgs []any
	busyOnce := true
	screen := ""
	f := &fakeHerdr{handle: func(method string, params map[string]any) (any, *HerdrError) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "agent.list":
			return map[string]any{"agents": panes}, nil
		case "workspace.create":
			return map[string]any{"workspace": map[string]any{"workspace_id": "w9"}, "root_pane": map[string]any{"pane_id": "w9:p1"}}, nil
		case "agent.start":
			if busyOnce {
				busyOnce = false
				return nil, &HerdrError{Code: "agent_pane_busy", Message: "the shell is still starting"}
			}
			startArgs = params["args"].([]any)
			pane := map[string]any{"pane_id": "w9:p1", "workspace_id": "w9", "tab_id": "w9:t1", "name": params["name"], "agent": "claude", "agent_status": "idle"}
			panes = append(panes, pane)
			return map[string]any{"agent": pane}, nil
		case "agent.prompt":
			prompts = append(prompts, params["text"].(string))
			panes[0]["agent_status"] = "working"
			return map[string]any{"agent": panes[0]}, nil
		case "agent.read":
			return map[string]any{"read": map[string]any{"text": screen}}, nil
		case "pane.process_info":
			return nil, &HerdrError{Code: "not_found", Message: "no"}
		}
		return map[string]any{"type": "ok"}, nil
	}}
	h := serveHerdr(t, f)
	m, err := NewManager(ManagerOptions{Roots: []string{root}, StatePath: filepath.Join(t.TempDir(), "agents.json"), MaxRunning: 1, Idle: time.Hour}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	m.UseHerdr(h, claude)
	ctx := context.Background()
	_ = m.RefreshHerdr(ctx)

	a, err := m.Start(StartRequest{Cwd: root, Prompt: "What does the README say?", Label: "ask: readme", AllowedTools: []string{"Read"}})
	if err != nil {
		t.Fatal(err)
	}
	if a.Engine != "herdr" || a.Status != Starting {
		t.Fatalf("started %s as %s", a.Engine, a.Status)
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("waited for %s; herdr saw %v", what, f.called())
			}
		}
	}
	waitFor("the prompt", func() bool { mu.Lock(); defer mu.Unlock(); return len(prompts) == 1 })
	mu.Lock()
	if !slices.Contains(startArgs, any("--session-id")) || !slices.Contains(startArgs, any(a.ID)) || !slices.Contains(startArgs, any("--allowedTools")) {
		t.Errorf("started with %v", startArgs)
	}
	mu.Unlock()

	// Claude answers in its transcript, and the pane goes back to its prompt.
	transcript := filepath.Join(claude, "projects", EncodeProjectDir(a.Cwd), a.ID+".jsonl")
	_ = os.MkdirAll(filepath.Dir(transcript), 0o755)
	later := time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
	_ = os.WriteFile(transcript, []byte(entry(map[string]any{"type": "assistant", "timestamp": later, "message": map[string]any{"stop_reason": "end_turn", "content": []any{map[string]any{"type": "text", "text": "It says hello. Want the rest?"}}}})+"\n"), 0o644)
	mu.Lock()
	panes[0]["agent_status"] = "idle"
	mu.Unlock()
	_ = m.RefreshHerdr(ctx)
	got, _ := m.Get(a.ID)
	if got.Status != Waiting || got.Result != "It says hello. Want the rest?" || !got.AsksForReply() {
		t.Fatalf("after the turn: %s %q", got.Status, got.Result)
	}

	// A dialog: denied, with the reason sent on as the next prompt.
	mu.Lock()
	panes[0]["agent_status"], screen = "blocked", readOutside
	mu.Unlock()
	_ = m.RefreshHerdr(ctx)
	got, _ = m.Get(a.ID)
	if got.Status != NeedsApproval || len(got.Pending) != 1 {
		t.Fatalf("blocked: %s %+v", got.Status, got.Pending)
	}
	if _, err := m.Answer(a.ID, got.Pending[0].RequestID, false, "Read the docs folder instead"); err != nil {
		t.Fatal(err)
	}
	waitFor("the reason", func() bool { mu.Lock(); defer mu.Unlock(); return len(prompts) == 2 })
	mu.Lock()
	if prompts[1] != "Read the docs folder instead" {
		t.Errorf("prompts %q", prompts)
	}
	mu.Unlock()
	keys := strings.Join(f.called(), ",")
	if !strings.Contains(keys, "agent.send_keys") {
		t.Errorf("no key pressed: %s", keys)
	}
	if _, err := m.Abort(a.ID); err == nil {
		t.Error("stopped a herdr agent from here")
	}
}
