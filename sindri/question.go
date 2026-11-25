package sindri

// AskUserQuestion: Claude asks you questions with choices. It arrives as a
// tool call waiting on you, like any other, but it is answered, not allowed:
// the answers go back in the call's input, and Claude reads them as yours.

import (
	"maps"
	"regexp"
	"strconv"
	"strings"
)

// AskTool is the tool Claude asks you questions with.
const AskTool = "AskUserQuestion"

// Question is one question an agent asks you.
type Question struct {
	Question    string   `json:"question"`
	Header      string   `json:"header,omitempty"`
	MultiSelect bool     `json:"multiSelect,omitempty"`
	Options     []Choice `json:"options,omitempty"`
}

// Choice is one answer a question offers.
type Choice struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// QuestionsOf reads the questions of an AskUserQuestion call; nil for any other tool.
func QuestionsOf(tool string, input map[string]any) []Question {
	if tool != AskTool {
		return nil
	}
	list, _ := input["questions"].([]any)
	var questions []Question
	for _, item := range list {
		raw, _ := item.(map[string]any)
		q := Question{}
		q.Question, _ = raw["question"].(string)
		q.Header, _ = raw["header"].(string)
		q.MultiSelect, _ = raw["multiSelect"].(bool)
		options, _ := raw["options"].([]any)
		for _, option := range options {
			o, _ := option.(map[string]any)
			c := Choice{}
			c.Label, _ = o["label"].(string)
			c.Description, _ = o["description"].(string)
			if c.Label != "" {
				q.Options = append(q.Options, c)
			}
		}
		if strings.TrimSpace(q.Question) != "" {
			questions = append(questions, q)
		}
	}
	return questions
}

// QuestionSummary is a question call as a line.
func QuestionSummary(questions []Question) string {
	if len(questions) == 0 {
		return AskTool
	}
	more := ""
	if len(questions) > 1 {
		more = " (+" + strconv.Itoa(len(questions)-1) + " more)"
	}
	return "a question: " + Clip(questions[0].Question, 120) + more
}

// QuestionsText is the questions as a message: each with its numbered
// choices, and how to answer.
func QuestionsText(questions []Question) string {
	var b strings.Builder
	for i, q := range questions {
		if i > 0 {
			b.WriteString("\n\n")
		}
		if len(questions) > 1 {
			b.WriteString(strconv.Itoa(i+1) + ") ")
		}
		b.WriteString(q.Question)
		for n, c := range q.Options {
			b.WriteString("\n" + strconv.Itoa(n+1) + ". " + c.Label)
			if c.Description != "" {
				b.WriteString(": " + c.Description)
			}
		}
	}
	b.WriteString("\n\n")
	switch {
	case len(questions) > 1:
		b.WriteString("Reply with one line per question: a number, or your own words.")
	case len(questions) == 1 && questions[0].MultiSelect:
		b.WriteString("Reply with one or more numbers, or in your own words.")
	default:
		b.WriteString("Reply with a number, or in your own words.")
	}
	return b.String()
}

var choiceNumbers = regexp.MustCompile(`^\d+(?:\s*[,\s]\s*\d+)*$`)

// AnswersFrom reads a reply as answers, one per question: one line per
// question when there are several, a choice's number or label picks it, and
// anything else is the answer in your own words.
func AnswersFrom(questions []Question, reply string) map[string]string {
	reply = strings.TrimSpace(reply)
	pieces := []string{reply}
	if len(questions) > 1 {
		var lines []string
		for _, line := range strings.Split(reply, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				lines = append(lines, line)
			}
		}
		if len(lines) != len(questions) {
			lines = nil
			for _, part := range strings.Split(reply, ";") {
				if part = strings.TrimSpace(part); part != "" {
					lines = append(lines, part)
				}
			}
		}
		if len(lines) == len(questions) {
			pieces = lines
		}
	}
	answers := map[string]string{}
	for i, q := range questions {
		piece := pieces[min(i, len(pieces)-1)]
		answers[q.Question] = pick(q, piece)
	}
	return answers
}

// pick is the answer a reply gives one question: the labels its numbers name,
// the label it spells, else its own words.
func pick(q Question, reply string) string {
	reply = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(reply), "."))
	if choiceNumbers.MatchString(reply) {
		var labels []string
		for _, field := range strings.FieldsFunc(reply, func(r rune) bool { return r == ',' || r == ' ' }) {
			n, _ := strconv.Atoi(field)
			if n < 1 || n > len(q.Options) {
				labels = nil
				break
			}
			labels = append(labels, q.Options[n-1].Label)
		}
		if len(labels) == 1 || (len(labels) > 1 && q.MultiSelect) {
			return strings.Join(labels, ", ")
		}
	}
	for _, c := range q.Options {
		if strings.EqualFold(c.Label, reply) {
			return c.Label
		}
	}
	return reply
}

// withAnswers is a question call's input with your answers in it, as Claude
// reads them back.
func withAnswers(input map[string]any, answers map[string]string) map[string]any {
	out := maps.Clone(input)
	if out == nil {
		out = map[string]any{}
	}
	given := map[string]any{}
	for key, answer := range answers {
		given[key] = answer
	}
	out["answers"] = given
	return out
}
