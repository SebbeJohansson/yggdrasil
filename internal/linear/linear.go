// Package linear reads and moves Linear issues with your personal API key
// (Linear: Settings > Security & access > Personal API keys). Norn reads a
// ticket's state to answer "is AI-141 done?"; Sindri lists open issues as
// goal candidates and moves an issue along as its goal moves.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// API is Linear's GraphQL endpoint.
const API = "https://api.linear.app/graphql"

// Client talks to Linear. A zero Key is off.
type Client struct {
	Key  string
	URL  string
	HTTP *http.Client
}

// On is whether there is a key.
func (c *Client) On() bool { return c != nil && c.Key != "" }

// ErrNotFound is Linear having no such issue.
var ErrNotFound = errors.New("no such issue in Linear")

var notFound = regexp.MustCompile(`(?i)not found`)

func (c *Client) query(ctx context.Context, timeout time.Duration, query string, variables map[string]any, into any) error {
	payload, _ := json.Marshal(map[string]any{"query": query, "variables": variables})
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	target := c.URL
	if target == "" {
		target = API
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", c.Key)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	var body struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message    string `json:"message"`
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(data, &body)
	// An unknown key comes back as an error, not as a null issue.
	for _, e := range body.Errors {
		if e.Extensions.Code == "ENTITY_NOT_FOUND" || notFound.MatchString(e.Message) {
			return ErrNotFound
		}
	}
	if response.StatusCode != http.StatusOK || len(body.Data) == 0 || string(body.Data) == "null" || len(body.Errors) > 0 {
		message := fmt.Sprintf("Linear answered %d", response.StatusCode)
		if len(body.Errors) > 0 && body.Errors[0].Message != "" {
			message += ": " + body.Errors[0].Message
		}
		return errors.New(message)
	}
	return json.Unmarshal(body.Data, into)
}

// --- reading a ticket -------------------------------------------------------

// Issue is one issue's state.
type Issue struct {
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	State      struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`
	Assignee *struct {
		Name string `json:"name"`
	} `json:"assignee"`
	CompletedAt *string `json:"completedAt"`
	CanceledAt  *string `json:"canceledAt"`
	UpdatedAt   string  `json:"updatedAt"`
}

// Issue is one issue by its key, like AI-141; ErrNotFound when there is none.
func (c *Client) Issue(ctx context.Context, key string) (*Issue, error) {
	var data struct {
		Issue *Issue `json:"issue"`
	}
	err := c.query(ctx, 5*time.Second,
		`query($id: String!) { issue(id: $id) { identifier title url state { name type } assignee { name } completedAt canceledAt updatedAt } }`,
		map[string]any{"id": key}, &data)
	if err != nil {
		return nil, err
	}
	if data.Issue == nil {
		return nil, ErrNotFound
	}
	return data.Issue, nil
}

// Ticket is a ticket key, like AI-141.
var Ticket = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,9}-\d+\b`)

var (
	stateWords = regexp.MustCompile(`(?i)\b(done|finished|closed|completed?|resolved|cancel+ed|status|state|marked|open|in progress|in review|assigned|who('?s| is) (on|working on|assigned))\b`)
	tracker    = regexp.MustCompile(`(?i)\b(linear|ticket|issue)\b`)
	order      = regexp.MustCompile(`(?i)^\s*(please\s+|can you\s+|could you\s+)?(mark|set|move|close|reopen|assign|unassign|cancel|update|change)\b`)
)

// AsksState is whether the message asks about a ticket's state, and is not
// asking for it to change: "mark AI-141 as done" is an agent's work.
func AsksState(message string) bool {
	return stateWords.MatchString(message) && (Ticket.MatchString(message) || tracker.MatchString(message)) && !order.MatchString(message)
}

func day(iso string, zone *time.Location) string {
	at, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return iso
	}
	return at.In(zone).Format("2 Jan")
}

// Answer is the issue's state, as a yes or no when it is finished.
func Answer(issue *Issue, zone *time.Location) string {
	what := fmt.Sprintf("%s %q", issue.Identifier, issue.Title)
	who := " Not assigned."
	if issue.Assignee != nil {
		who = " Assigned to " + issue.Assignee.Name + "."
	}
	link := "\n[Open in Linear](" + issue.URL + ")"
	switch issue.State.Type {
	case "completed":
		since := ""
		if issue.CompletedAt != nil {
			since = " since " + day(*issue.CompletedAt, zone)
		}
		return "Yes, " + what + " is " + issue.State.Name + since + "." + who + link
	case "canceled":
		on := ""
		if issue.CanceledAt != nil {
			on = " on " + day(*issue.CanceledAt, zone)
		}
		return what + " was canceled (" + issue.State.Name + ")" + on + "." + who + link
	}
	return "No, " + what + " is " + issue.State.Name + ", not done." + who + " Last updated " + day(issue.UpdatedAt, zone) + "." + link
}

// --- listing open issues ----------------------------------------------------

// Listed is an open issue, as a goal candidate.
type Listed struct {
	Identifier  string  `json:"identifier"`
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Description *string `json:"description"`
	State       struct {
		Name string `json:"name"`
		// Type is triage, backlog, unstarted (Todo), started, completed or canceled.
		Type string `json:"type"`
	} `json:"state"`
}

// Assignees are whose issues to list: the key owner's, nobody's, or everyone's.
var Assignees = []string{"mine", "unassigned", "all"}

// Filter narrows the open issues: project is a Linear project name, query matches the title.
type Filter struct {
	Project, Query, Assignee string
}

// Open is the open issues, newest update first; the key owner's by default.
func (c *Client) Open(ctx context.Context, filter Filter) ([]Listed, error) {
	where := map[string]any{"state": map[string]any{"type": map[string]any{"nin": []string{"completed", "canceled"}}}}
	switch filter.Assignee {
	case "", "mine":
		where["assignee"] = map[string]any{"isMe": map[string]any{"eq": true}}
	case "unassigned":
		where["assignee"] = map[string]any{"null": true}
	}
	if filter.Project != "" {
		where["project"] = map[string]any{"name": map[string]any{"containsIgnoreCase": filter.Project}}
	}
	if filter.Query != "" {
		where["title"] = map[string]any{"containsIgnoreCase": filter.Query}
	}
	var data struct {
		Issues struct {
			Nodes []Listed `json:"nodes"`
		} `json:"issues"`
	}
	err := c.query(ctx, 8*time.Second,
		`query($filter: IssueFilter) { issues(filter: $filter, first: 50, orderBy: updatedAt) { nodes { identifier title url description state { name type } } } }`,
		map[string]any{"filter": where}, &data)
	return data.Issues.Nodes, err
}

// --- moving an issue along --------------------------------------------------

// Step is how far along a goal is, as Linear sees it: todo, started, review.
type Step string

// WorkflowState is one of a team's states.
type WorkflowState struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Position float64 `json:"position"`
}

var review = regexp.MustCompile(`(?i)review`)

// rank is how far along a state is; -1 for canceled, which is never moved.
func rank(name, kind string) int {
	switch kind {
	case "triage", "backlog":
		return 0
	case "unstarted":
		return 1
	case "started":
		if review.MatchString(name) {
			return 3
		}
		return 2
	case "completed":
		return 4
	}
	return -1
}

// PickState is the team's state to move the issue to for step, or nil when
// it should stay: forward only, and never out of canceled.
func PickState(states []WorkflowState, currentName, currentType string, step Step) *WorkflowState {
	now := rank(currentName, currentType)
	if now < 0 {
		return nil
	}
	ordered := append([]WorkflowState{}, states...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Position < ordered[j].Position })
	find := func(ok func(WorkflowState) bool) *WorkflowState {
		for i := range ordered {
			if ok(ordered[i]) {
				return &ordered[i]
			}
		}
		return nil
	}
	var target *WorkflowState
	switch step {
	case "todo":
		target = find(func(s WorkflowState) bool { return s.Type == "unstarted" })
	case "started":
		target = find(func(s WorkflowState) bool { return s.Type == "started" && !review.MatchString(s.Name) })
	case "review":
		target = find(func(s WorkflowState) bool { return s.Type == "started" && review.MatchString(s.Name) })
		if target == nil {
			target = find(func(s WorkflowState) bool { return s.Type == "started" })
		}
	default:
		target = find(func(s WorkflowState) bool { return s.Type == "completed" })
	}
	if target == nil || rank(target.Name, target.Type) <= now {
		return nil
	}
	return target
}

// Move moves the issue forward to step. It returns the new state's name, or
// "" when the issue stays where it is.
func (c *Client) Move(ctx context.Context, key string, step Step) (string, error) {
	var found struct {
		Issue *struct {
			ID    string        `json:"id"`
			State WorkflowState `json:"state"`
			Team  struct {
				States struct {
					Nodes []WorkflowState `json:"nodes"`
				} `json:"states"`
			} `json:"team"`
		} `json:"issue"`
	}
	if err := c.query(ctx, 8*time.Second,
		`query($id: String!) { issue(id: $id) { id state { id name type position } team { states { nodes { id name type position } } } } }`,
		map[string]any{"id": key}, &found); err != nil {
		return "", err
	}
	if found.Issue == nil {
		return "", ErrNotFound
	}
	target := PickState(found.Issue.Team.States.Nodes, found.Issue.State.Name, found.Issue.State.Type, step)
	if target == nil {
		return "", nil
	}
	var updated struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	if err := c.query(ctx, 8*time.Second,
		`mutation($id: String!, $stateId: String!) { issueUpdate(id: $id, input: { stateId: $stateId }) { success } }`,
		map[string]any{"id": found.Issue.ID, "stateId": target.ID}, &updated); err != nil {
		return "", err
	}
	if !updated.IssueUpdate.Success {
		return "", errors.New("Linear did not update the issue")
	}
	return target.Name, nil
}

// KeyOf is the issue key of a goal source "linear:<key>", or "".
func KeyOf(source string) string {
	key, ok := strings.CutPrefix(source, "linear:")
	if !ok {
		return ""
	}
	return key
}
