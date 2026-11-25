package sindri

import (
	"strings"
	"testing"
)

func names() []Question {
	return []Question{
		{Question: "Resource group?", Options: []Choice{{Label: "rg-akeneo-mcp"}, {Label: "rg-mcp"}}},
		{Question: "Regions?", MultiSelect: true, Options: []Choice{{Label: "westeurope"}, {Label: "northeurope"}, {Label: "swedencentral"}}},
	}
}

func TestQuestionsOfReadsAskUserQuestion(t *testing.T) {
	input := map[string]any{"questions": []any{map[string]any{"question": "Names?", "header": "Azure", "multiSelect": false,
		"options": []any{map[string]any{"label": "a", "description": "the first"}, map[string]any{"label": "b"}}}}}
	got := QuestionsOf(AskTool, input)
	if len(got) != 1 || got[0].Header != "Azure" || len(got[0].Options) != 2 || got[0].Options[0].Description != "the first" {
		t.Fatalf("got %+v", got)
	}
	if QuestionsOf("Bash", input) != nil {
		t.Fatal("another tool asks no questions")
	}
}

func TestAnswersFromReadsNumbersLabelsAndWords(t *testing.T) {
	cases := []struct {
		reply      string
		group, reg string
	}{
		{"1\n2 3", "rg-akeneo-mcp", "northeurope, swedencentral"},
		{"rg-mcp; westeurope", "rg-mcp", "westeurope"},
		{"call it rg-shared", "call it rg-shared", "call it rg-shared"},
		{"9\n1", "9", "westeurope"},
	}
	for _, c := range cases {
		got := AnswersFrom(names(), c.reply)
		if got["Resource group?"] != c.group || got["Regions?"] != c.reg {
			t.Errorf("%q: got %v", c.reply, got)
		}
	}
	one := names()[:1]
	if got := AnswersFrom(one, "1 2")["Resource group?"]; got != "1 2" {
		t.Errorf("two picks on a single choice are words, got %q", got)
	}
}

func TestQuestionPostNumbersTheChoices(t *testing.T) {
	a := &Agent{Status: NeedsApproval, Pending: []Pending{{ToolName: AskTool, Questions: names()[:1]}}}
	got := PostFor(a, "", "")
	for _, want := range []string{"Resource group?", "1. rg-akeneo-mcp", "2. rg-mcp", "Reply with a number"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestWithAnswersKeepsTheQuestions(t *testing.T) {
	input := map[string]any{"questions": []any{"q"}}
	out := withAnswers(input, map[string]string{"q": "a"})
	if out["questions"] == nil || out["answers"].(map[string]any)["q"] != "a" || input["answers"] != nil {
		t.Fatalf("got %v, input %v", out, input)
	}
}
