package sindri

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionsAndActivity(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "-home-me-projects-demo")
	_ = os.MkdirAll(folder, 0o755)
	write := func(id, text string) {
		_ = os.WriteFile(filepath.Join(folder, id+".jsonl"), []byte(text), 0o644)
	}
	write("mine", `{"type":"user","timestamp":"2026-11-14T08:00:00Z","cwd":"/home/me/projects/demo","message":{"content":[{"type":"text","text":"<ide_selection>x</ide_selection>"},{"type":"text","text":"Fix   the login"}]}}
{"type":"assistant","cwd":"/home/me/projects/demo","gitBranch":"feature/login","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"Fixed it."}]}}
`)
	write("agent-1", `{"type":"user","timestamp":"2026-11-14T09:00:00Z","message":{"content":"hand-over"}}`)
	write("empty", `{"type":"summary"}`)

	sessions := ListSessions(dir, map[string]bool{"agent-1": true}, 20, time.Now())
	if len(sessions) != 1 {
		t.Fatalf("got %+v", sessions)
	}
	s := sessions[0]
	if s.ID != "mine" || s.Title != "Fix the login" || s.Project != "demo" || s.LastText != "Fixed it." || s.Branch != "feature/login" || s.Working {
		t.Errorf("got %+v", s)
	}

	since := time.Now().Add(-48 * time.Hour)
	starts := SessionStarts(dir, map[string]bool{"agent-1": true}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if len(starts) != 2 { // "empty" has no timestamp: its file time
		t.Errorf("starts %v", starts)
	}
	merged := time.Now().UnixMilli()
	days := Activity(nil, [][2]int64{{merged, merged}}, time.UTC, since)
	if d := days[time.Now().UTC().Format("2006-01-02")]; d == nil || d.Agents != 1 || d.Merges != 1 {
		t.Errorf("days %+v", days)
	}
}
