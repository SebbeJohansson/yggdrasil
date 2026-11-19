package sindri

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func promptLine(at, text string) string {
	return `{"type":"user","timestamp":"` + at + `","cwd":"/home/me/projects/demo","gitBranch":"feature/x","message":{"content":"` + text + `"}}` + "\n"
}

func TestSessionPromptsBackfillReadOnAndDedupe(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "-home-me-projects-demo")
	_ = os.MkdirAll(folder, 0o755)
	mine := filepath.Join(folder, "mine.jsonl")
	_ = os.WriteFile(mine, []byte(promptLine("2026-11-10T08:00:00Z", "Fix the login")+
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Claude's words are never kept"}]}}`+"\n"+
		`{"type":"user","timestamp":"2026-11-10T08:01:00Z","isCompactSummary":true,"message":{"content":"summary"}}`+"\n"+
		`{"type":"user","timestamp":"2026-11-10T08:02:00Z","message":{"content":[{"type":"tool_result","content":"output"}]}}`+"\n"+
		`{"type":"ai-title","aiTitle":"Login fix"}`+"\n"+
		promptLine("2026-11-10T08:05:00Z", strings.Repeat("a", 300))), 0o644)
	_ = os.WriteFile(filepath.Join(folder, "agent-1.jsonl"), []byte(promptLine("2026-11-10T09:00:00Z", "hand-over")), 0o644)

	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	own := map[string]bool{"agent-1": true}
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	if err := store.IndexSessionPrompts(dir, own, 0, now); err != nil {
		t.Fatal(err)
	}
	day := func(d int) int64 { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC).UnixMilli() }
	prompts, oldest, err := store.SessionPrompts(day(10), day(11), own)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 || prompts[0].Prompt != "Fix the login" || prompts[0].Title != "Login fix" || prompts[0].Project != "demo" || prompts[0].Branch != "feature/x" {
		t.Fatalf("backfill: %+v", prompts)
	}
	if n := len([]rune(prompts[1].Prompt)); n != SessionPromptChars {
		t.Errorf("a long prompt is clipped to %d, got %d", SessionPromptChars, n)
	}
	if oldest == nil || *oldest != time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC).UnixMilli() {
		t.Errorf("oldest: %v", oldest)
	}

	// A new line, and a half-written one: only the whole line is read, from where the last pass left off.
	file, _ := os.OpenFile(mine, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = file.WriteString(promptLine("2026-11-12T10:00:00Z", "Add tests") + `{"type":"user","timestamp":"2026-11-12T10:0`)
	file.Close()
	_ = os.Chtimes(mine, now, now)
	if err := store.IndexSessionPrompts(dir, own, 0, now); err != nil {
		t.Fatal(err)
	}
	// Rewritten from the start (shorter than what was read): read again, nothing kept twice.
	_ = os.WriteFile(mine, []byte(promptLine("2026-11-10T08:00:00Z", "Fix the login")), 0o644)
	later := now.Add(time.Minute)
	_ = os.Chtimes(mine, later, later)
	if err := store.IndexSessionPrompts(dir, own, 0, now); err != nil {
		t.Fatal(err)
	}
	all, _, _ := store.SessionPrompts(day(1), day(19), nil)
	if len(all) != 3 || all[2].Prompt != "Add tests" {
		t.Fatalf("read on and dedupe: %+v", all)
	}

	// The transcripts deleted (Claude Code's cleanup): the index still answers.
	_ = os.RemoveAll(folder)
	if err := store.IndexSessionPrompts(dir, own, 0, now); err != nil {
		t.Fatal(err)
	}
	if kept, _, _ := store.SessionPrompts(day(12), day(13), own); len(kept) != 1 {
		t.Fatalf("after cleanup: %+v", kept)
	}

	// A retention keeps only the newer ones.
	if err := store.IndexSessionPrompts(dir, own, 8*24*time.Hour, now); err != nil {
		t.Fatal(err)
	}
	if kept, _, _ := store.SessionPrompts(day(1), day(19), own); len(kept) != 1 || kept[0].Prompt != "Add tests" {
		t.Fatalf("retention: %+v", kept)
	}
}
