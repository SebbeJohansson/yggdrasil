package sindri

import (
	"strings"
	"testing"
)

func TestFailedTurnKeepsClaudesWords(t *testing.T) {
	a := &Agent{Status: Working}
	a.applyClaude(map[string]any{
		"type": "result", "subtype": "success", "is_error": true,
		"result": "Failed to authenticate: OAuth session expired and could not be refreshed",
	}, 0, 1)
	if a.Status != Failed {
		t.Fatalf("status %v, want Failed", a.Status)
	}
	got := a.Log[len(a.Log)-1].Text
	if strings.Contains(got, "(success)") {
		t.Errorf("error says success: %q", got)
	}
	if !strings.Contains(got, "OAuth session expired") || !strings.Contains(got, "/login") {
		t.Errorf("error lacks Claude's words or the login hint: %q", got)
	}
	if a.UsageLimited {
		t.Error("an expired login is not a usage limit")
	}
}

func TestClaudeResultAddsLoginHint(t *testing.T) {
	_, err := claudeResult([]byte(`{"is_error":true,"result":"Failed to authenticate: OAuth session expired and could not be refreshed"}`))
	if err == nil || !strings.Contains(err.Error(), "/login") {
		t.Fatalf("got %v, want the login hint", err)
	}
}
