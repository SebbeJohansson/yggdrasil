package linear

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAsksState(t *testing.T) {
	for message, want := range map[string]bool{
		"Is AI-141 marked as done?":           true,
		"is the linear task done yet":         true,
		"Who is working on SHOP-12?":          true,
		"mark AI-141 as done":                 false,
		"Can you close AI-141?":               false,
		"Is the build done?":                  false,
		"What does AI-141 say about payments": false,
	} {
		if AsksState(message) != want {
			t.Errorf("%q: want %v", message, want)
		}
	}
}

func TestPickState(t *testing.T) {
	states := []WorkflowState{
		{ID: "d", Name: "Done", Type: "completed", Position: 4},
		{ID: "r", Name: "In Review", Type: "started", Position: 3},
		{ID: "p", Name: "In Progress", Type: "started", Position: 2},
		{ID: "t", Name: "Todo", Type: "unstarted", Position: 1},
		{ID: "b", Name: "Backlog", Type: "backlog", Position: 0},
	}
	for _, c := range []struct {
		name, kind string
		step       Step
		want       string
	}{
		{"Backlog", "backlog", "todo", "t"},
		{"Todo", "unstarted", "started", "p"},
		{"In Progress", "started", "review", "r"},
		{"In Review", "started", "started", ""}, // never backwards
		{"Done", "completed", "review", ""},
		{"Canceled", "canceled", "started", ""}, // never out of canceled
	} {
		got := PickState(states, c.name, c.kind, c.step)
		if (got == nil && c.want != "") || (got != nil && got.ID != c.want) {
			t.Errorf("%s → %s: got %+v, want %q", c.name, c.step, got, c.want)
		}
	}
}

func TestAnswer(t *testing.T) {
	zone, _ := time.LoadLocation("Europe/Stockholm")
	issue := &Issue{Identifier: "AI-141", Title: "Fix login", URL: "https://linear.app/x/AI-141", UpdatedAt: "2026-11-10T09:00:00Z"}
	issue.State.Name, issue.State.Type = "In Progress", "started"
	if got := Answer(issue, zone); !strings.HasPrefix(got, `No, AI-141 "Fix login" is In Progress, not done. Not assigned. Last updated 10 Nov.`) {
		t.Errorf("got %q", got)
	}
	done := "2026-11-12T23:30:00Z"
	issue.State.Name, issue.State.Type, issue.CompletedAt = "Done", "completed", &done
	if got := Answer(issue, zone); !strings.HasPrefix(got, `Yes, AI-141 "Fix login" is Done since 13 Nov.`) {
		t.Errorf("got %q", got)
	}
}

func TestMove(t *testing.T) {
	var updated string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "key" {
			t.Error("no key")
		}
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		switch {
		case body.Variables["id"] == "NOPE-1":
			_, _ = io.WriteString(w, `{"errors":[{"message":"Entity not found","extensions":{"code":"ENTITY_NOT_FOUND"}}]}`)
		case strings.HasPrefix(body.Query, "mutation"):
			updated = body.Variables["stateId"].(string)
			_, _ = io.WriteString(w, `{"data":{"issueUpdate":{"success":true}}}`)
		default:
			_, _ = io.WriteString(w, `{"data":{"issue":{"id":"uuid","state":{"id":"t","name":"Todo","type":"unstarted","position":1},
				"team":{"states":{"nodes":[{"id":"t","name":"Todo","type":"unstarted","position":1},{"id":"p","name":"In Progress","type":"started","position":2}]}}}}}`)
		}
	}))
	defer server.Close()
	c := &Client{Key: "key", URL: server.URL, HTTP: server.Client()}
	moved, err := c.Move(context.Background(), "AI-1", "started")
	if err != nil || moved != "In Progress" || updated != "p" {
		t.Errorf("moved %q to %q, %v", moved, updated, err)
	}
	if _, err := c.Issue(context.Background(), "NOPE-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want not found, got %v", err)
	}
}
