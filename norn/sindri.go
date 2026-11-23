package norn

// What Norn asks of Sindri, the coding orchestrator: the agents, and handing
// work over. Norn holds the conversation and builds the hand-over; Sindri
// owns the agents and posts what they say back (POST /v1/conversations/
// {session}/agent-turns).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Agent is a coding or question agent as Sindri lists it.
type Agent struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Cwd          string `json:"cwd"`
	Status       string `json:"status"`
	Tool         string `json:"tool"`
	LastText     string `json:"lastText"`
	Result       string `json:"result"`
	Task         string `json:"task"`
	LastActivity int64  `json:"lastActivity"`
	AsksYou      bool   `json:"asksYou"`
	Session      string `json:"session"` // the conversation Sindri started it for
	Resumable    bool   `json:"resumable"`
	Merged       bool   `json:"merged"`
	LastError    string `json:"lastError"`
	Pending      []struct {
		RequestID string `json:"requestId"`
		Summary   string `json:"summary"`
	} `json:"pending"`
	Worktree *struct {
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
		Base   string `json:"base"`
	} `json:"worktree"`
	Check *struct {
		Status string `json:"status"`
		At     int64  `json:"at"`
	} `json:"check"`
	Log []struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
	} `json:"log"`
}

// SindriClient calls Sindri. A zero URL turns agents off.
type SindriClient struct {
	URL, Token string
	HTTP       *http.Client
}

// On is whether there is a Sindri to hand work to.
func (c *SindriClient) On() bool {
	return c != nil && c.URL != ""
}

// SindriError is Sindri refusing, with its status and why.
type SindriError struct {
	Status  int
	Message string
}

func (e *SindriError) Error() string { return e.Message }

func (c *SindriClient) call(ctx context.Context, method, path string, body, into any) error {
	return c.callWithin(ctx, 30*time.Second, method, path, body, into)
}

// callWithin is call with its own time limit.
func (c *SindriClient) callWithin(ctx context.Context, limit time.Duration, method, path string, body, into any) error {
	if !c.On() {
		return &SindriError{Status: 503, Message: "agents are off here (no Sindri)"}
	}
	var reader io.Reader
	if body != nil {
		payload, _ := json.Marshal(body)
		reader = bytes.NewReader(payload)
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return &SindriError{Status: 502, Message: "Sindri is not reachable: " + err.Error()}
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if response.StatusCode >= 300 {
		var refusal struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &refusal)
		if refusal.Error == "" {
			refusal.Error = fmt.Sprintf("Sindri answered %d", response.StatusCode)
		}
		return &SindriError{Status: response.StatusCode, Message: refusal.Error}
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(data, into)
}

// Agents is every agent, the most recently active first.
func (c *SindriClient) Agents(ctx context.Context) ([]Agent, error) {
	var agents []Agent
	return agents, c.call(ctx, http.MethodGet, "/v1/agents", nil, &agents)
}

// Agent is one agent with its log.
func (c *SindriClient) Agent(ctx context.Context, id string) (*Agent, error) {
	var agent Agent
	if err := c.call(ctx, http.MethodGet, "/v1/agents/"+url.PathEscape(id), nil, &agent); err != nil {
		return nil, err
	}
	return &agent, nil
}

// Projects is the names of the projects agents work in.
func (c *SindriClient) Projects(ctx context.Context) []string {
	var projects []struct{ Name string }
	if c.call(ctx, http.MethodGet, "/v1/projects", nil, &projects) != nil {
		return nil
	}
	names := make([]string, len(projects))
	for i, p := range projects {
		names[i] = p.Name
	}
	return names
}

// Handed is what a coding hand-over did.
type Handed struct {
	Answer string `json:"answer"`
	Parts  []struct {
		AgentID string `json:"agentId"`
		Status  string `json:"status"`
		Project string `json:"project"`
		Branch  string `json:"branch"`
	} `json:"parts"`
}

// Code hands a coding task over: one agent per project.
func (c *SindriClient) Code(ctx context.Context, request map[string]any) (Handed, error) {
	var handed Handed
	return handed, c.call(ctx, http.MethodPost, "/v1/work/code", request, &handed)
}

// Ask hands a question that needs tools to a question agent.
func (c *SindriClient) Ask(ctx context.Context, request map[string]any) (*Agent, error) {
	var agent Agent
	if err := c.call(ctx, http.MethodPost, "/v1/work/ask", request, &agent); err != nil {
		return nil, err
	}
	return &agent, nil
}

// Reply sends your words to an agent that has stopped.
func (c *SindriClient) Reply(ctx context.Context, request map[string]any) (*Agent, error) {
	var agent Agent
	if err := c.call(ctx, http.MethodPost, "/v1/work/reply", request, &agent); err != nil {
		return nil, err
	}
	return &agent, nil
}

// Answer allows or denies a tool call an agent waits on.
func (c *SindriClient) Answer(ctx context.Context, agent, requestID string, allow bool, message string) error {
	return c.call(ctx, http.MethodPost, "/v1/agents/"+url.PathEscape(agent)+"/permissions/"+url.PathEscape(requestID),
		map[string]any{"allow": allow, "message": message}, nil)
}

// Part adds another project's part to the conversation's code task.
func (c *SindriClient) Part(ctx context.Context, request map[string]any) (Handed, error) {
	var handed Handed
	return handed, c.call(ctx, http.MethodPost, "/v1/work/part", request, &handed)
}
