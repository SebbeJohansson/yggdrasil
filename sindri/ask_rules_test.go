package sindri

import (
	"slices"
	"testing"
)

func TestGitWritesAskInsteadOfFailing(t *testing.T) {
	if got := AskSettings([]string{"Bash(git commit:*)"}); got != `{"permissions":{"ask":["Bash(git commit:*)"]}}` {
		t.Fatalf("settings %s", got)
	}
	if AskSettings(nil) != "" {
		t.Fatal("no rules should give no settings")
	}
	args := ClaudeArgs(&Agent{PermissionMode: "auto", AskTools: AskAsked}, nil)
	at := slices.Index(args, "--settings")
	if at < 0 || args[at+1] != AskSettings(AskAsked) {
		t.Fatalf("args %v", args)
	}
}

func TestAnOlderQuestionAgentAsksForGitWrites(t *testing.T) {
	m, _ := setup(t)
	a, err := m.Start(StartRequest{Cwd: "demo", Prompt: "a question", DisallowedTools: []string{"Bash(git push:*)", "Bash(rm:*)"}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, a.ID, func(a *Agent) bool { return a.Status == Waiting })
	m.AskFor(a.ID, []string{"Bash(git push:*)"})
	m.mu.Lock()
	defer m.mu.Unlock()
	got := m.agents[a.ID]
	if !slices.Equal(got.DisallowedTools, []string{"Bash(rm:*)"}) || !slices.Equal(got.AskTools, []string{"Bash(git push:*)"}) {
		t.Fatalf("deny %v ask %v", got.DisallowedTools, got.AskTools)
	}
}
