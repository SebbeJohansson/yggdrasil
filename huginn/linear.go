package huginn

// Linear tickets in Todo that no goal covers yet. Every 5 minutes Huginn
// reads Sindri's goal candidates (your open Linear issues, each marked with
// the goal that has it), asks Norn whether each new Todo ticket is
// development work, and fires linear.todo for the ones that are. Each ticket
// is judged once; a judgement is kept, so a restart does not ask again.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LinearEvery is how often Huginn looks at your Linear tickets.
const LinearEvery = 5 * time.Minute

// LinearTries is how many passes in a row a ticket Norn cannot judge is asked
// about before it is set aside as not development work, so one ticket does not
// fail the loop for good.
const LinearTries = 3

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
	// One ticket Norn cannot judge does not stop the others; the pass fails
	// only when it asked and every ask failed.
	var failed []error
	asked := 0
	for _, c := range answer.Candidates {
		key := strings.TrimPrefix(c.Source, "linear:")
		if c.StateType != "unstarted" || c.Goal != nil || key == c.Source {
			continue
		}
		if _, judged, err := h.Store.Judged(ctx, key); err != nil || judged {
			continue
		}
		asked++
		reply, err := h.Norn.Ask(ctx, fmt.Sprintf(JudgeAsk, key+" "+c.Title, clipText(c.Context, 1500)), AskSession)
		if err != nil {
			if h.linearFailed(key) < LinearTries {
				failed = append(failed, fmt.Errorf("asking Norn about %s: %w", key, err))
				continue
			}
			h.logf("linear: Norn could not judge %s %d times in a row; set aside as not development work: %v", key, LinearTries, err)
			reply = "no"
		}
		h.linearJudged(key)
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
	if len(failed) > 0 && len(failed) == asked {
		return errors.Join(failed...)
	}
	for _, err := range failed {
		h.logf("linear: %v", err)
	}
	return nil
}

// linearFailed counts one more failed ask about `key`, and says how many in a row.
func (h *Huginn) linearFailed(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.linearFails == nil {
		h.linearFails = map[string]int{}
	}
	h.linearFails[key]++
	return h.linearFails[key]
}

func (h *Huginn) linearJudged(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.linearFails, key)
}
