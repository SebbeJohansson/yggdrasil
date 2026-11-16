package huginn

// Linear tickets in Todo that no goal covers yet. Every 5 minutes Huginn
// reads Sindri's goal candidates (your open Linear issues, each marked with
// the goal that has it), asks Norn whether each new Todo ticket is
// development work, and fires linear.todo for the ones that are. Each ticket
// is judged once; a judgement is kept, so a restart does not ask again.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LinearEvery is how often Huginn looks at your Linear tickets.
const LinearEvery = 5 * time.Minute

// Candidate is the little of Sindri's goal candidate Huginn reads.
type Candidate struct {
	Source    string `json:"source"`
	Title     string `json:"title"`
	Context   string `json:"context"`
	URL       string `json:"url"`
	Goal      *int64 `json:"goal"`
	State     string `json:"state"`
	StateType string `json:"stateType"`
}

// JudgeAsk is what Norn is asked about a ticket. Norn answers it itself
// (call.Ask sends `/local`), so the ticket never leaves this machine.
const JudgeAsk = `Is this Linear ticket software development work: writing, changing or fixing code? Research, admin, meetings and decisions are not. Answer with one word, yes or no.

Ticket: %s
%s`

// IsYes reads Norn's answer: yes when it starts with yes.
func IsYes(answer string) bool {
	answer = strings.ToLower(strings.TrimLeft(strings.TrimSpace(answer), `"'*`))
	return strings.HasPrefix(answer, "yes") || strings.HasPrefix(answer, "ja")
}

// GoalsLink is where the ticket is added as a goal: the dashboard's Goals page
// with the Linear import open on it.
func GoalsLink(dashboardURL, key string) string {
	if dashboardURL == "" || dashboardURL == "off" {
		return ""
	}
	return strings.TrimRight(dashboardURL, "/") + "/dashboard/goals?" + url.Values{"import": {"linear"}, "ticket": {key}}.Encode()
}

func clipText(text string, n int) string {
	runes := []rune(text)
	if len(runes) > n {
		return string(runes[:n]) + "…"
	}
	return text
}

func (h *Huginn) watchLinear(ctx context.Context) error {
	if !h.Sindri.On() || !h.Norn.On() {
		return nil
	}
	var answer struct {
		Candidates []Candidate `json:"candidates"`
	}
	if err := h.Sindri.Do(ctx, http.MethodGet, "/v1/goals/candidates?from=linear&assignee=mine", nil, &answer); err != nil {
		return err
	}
	for _, c := range answer.Candidates {
		key := strings.TrimPrefix(c.Source, "linear:")
		if c.StateType != "unstarted" || c.Goal != nil || key == c.Source {
			continue
		}
		if _, judged, err := h.Store.Judged(ctx, key); err != nil || judged {
			continue
		}
		reply, err := h.Norn.Ask(ctx, fmt.Sprintf(JudgeAsk, key+" "+c.Title, clipText(c.Context, 1500)), AskSession)
		if err != nil {
			return fmt.Errorf("asking Norn about %s: %w", key, err)
		}
		dev := IsYes(reply)
		if err := h.Store.Judge(ctx, key, dev); err != nil {
			return err
		}
		if !dev {
			continue
		}
		h.Handle(ctx, Fired{Kind: "linear.todo", Key: "linear.todo:" + key, At: h.now(), Title: key + " " + c.Title,
			Link:   GoalsLink(h.DashboardURL, key),
			Fields: map[string]string{"key": key, "title": c.Title, "url": c.URL, "state": c.State}})
	}
	return nil
}
