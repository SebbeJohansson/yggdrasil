package llm

import (
	"context"
	"strings"
	"testing"
)

func TestCloudModeAnswersInsteadOfTheLocalModel(t *testing.T) {
	var system, prompt string
	var recorded []Call
	c := &Client{PrimaryURL: "http://127.0.0.1:1/v1", Record: func(call Call) { recorded = append(recorded, call) },
		Cloud: func(_ context.Context, s, p string) (CloudAnswer, error) {
			system, prompt = s, p
			return CloudAnswer{Text: "```json\n{\"ok\":true}\n```", Engine: "claude", Model: "haiku"}, nil
		}}
	c.SetCloud(true)
	answer, err := c.Chat(context.Background(), []Message{{Role: "system", Content: "Be brief."}, {Role: "user", Content: "Hi"}},
		Options{Purpose: "route", Format: map[string]any{"type": "object"}})
	if err != nil || answer != `{"ok":true}` {
		t.Fatalf("answer %q, %v", answer, err)
	}
	if !strings.HasPrefix(system, "Be brief.") || !strings.Contains(system, "JSON schema") || prompt != "Hi" {
		t.Errorf("system %q, prompt %q", system, prompt)
	}
	if len(recorded) != 1 || recorded[0].Backend != "claude" || recorded[0].Model != "haiku" || recorded[0].Purpose != "route" {
		t.Errorf("recorded %+v", recorded)
	}
	c.SetCloud(false)
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "Hi"}}, Options{NoFallback: true}); err == nil {
		t.Error("local mode still answered from the cloud")
	}
}

func TestCloudGuardSwapsAndRefuses(t *testing.T) {
	sent := ""
	c := &Client{Cloud: func(_ context.Context, _, p string) (CloudAnswer, error) {
		sent = p
		return CloudAnswer{Text: "Write to Person 1."}, nil
	}, Guard: func(_ context.Context, purpose string, m []Message) ([]Message, func(string) string, error) {
		if purpose == "summary" {
			return nil, nil, ErrKeptLocal
		}
		return []Message{{Role: "user", Content: strings.ReplaceAll(m[0].Content, "Anna", "Person 1")}},
			func(s string) string { return strings.ReplaceAll(s, "Person 1", "Anna") }, nil
	}}
	c.SetCloud(true)
	answer, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "Who is Anna?"}}, Options{Purpose: "answer"})
	if err != nil || answer != "Write to Anna." || sent != "Who is Person 1?" {
		t.Fatalf("answer %q, sent %q, %v", answer, sent, err)
	}
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "mail"}}, Options{Purpose: "summary"}); err != ErrKeptLocal {
		t.Errorf("summary: %v", err)
	}
}

func TestCloudToolTurns(t *testing.T) {
	reply := `{"tool_calls":[{"name":"search","arguments":{"q":"bug"}}]}`
	c := &Client{Cloud: func(_ context.Context, s, _ string) (CloudAnswer, error) {
		if !strings.Contains(s, "- search: finds issues") {
			t.Errorf("tools not described: %q", s)
		}
		return CloudAnswer{Text: reply}, nil
	}}
	c.SetCloud(true)
	tool := Tool{Type: "function"}
	tool.Function.Name, tool.Function.Description = "search", "finds issues"
	q := "find the bug"
	turn, err := c.ToolChat(context.Background(), []ToolMessage{{Role: "user", Content: &q}}, []Tool{tool}, Options{})
	if err != nil || len(turn.ToolCalls) != 1 || turn.ToolCalls[0].Function.Name != "search" || turn.ToolCalls[0].Function.Arguments != `{"q":"bug"}` {
		t.Fatalf("turn %+v, %v", turn, err)
	}
	reply = `{"answer":"LIN-1 is the bug."}`
	turn, err = c.ToolChat(context.Background(), []ToolMessage{{Role: "user", Content: &q}}, []Tool{tool}, Options{})
	if err != nil || turn.Content != "LIN-1 is the bug." || len(turn.ToolCalls) != 0 {
		t.Fatalf("answer turn %+v, %v", turn, err)
	}
}
