package norn

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestShadow(t *testing.T) {
	agents := []Agent{
		{ID: "a1", Label: "my-widget: Fix the DM titles", Status: "waiting", AsksYou: true, LastText: "Which DM view?"},
		{ID: "a2", Label: "glim: Fix the DM titles", Status: "working"},
	}
	state, idOf, _ := ShadowState(ShadowInput{Message: "the inbox one", Last: &[2]string{"a1", "Which DM view?"}, Agents: agents, Projects: []string{"my-widget", "glim"}})
	want := "message: the inbox one\nlast said by dm-titles: Which DM view?\nagents: dm-titles (Fix the DM titles; asks: Which DM view?); dm-titles-2 (Fix the DM titles; working)\nprojects: my-widget, glim"
	if state != want || idOf["dm-titles-2"] != "a2" {
		t.Fatalf("state %q", state)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(data, &body)
		if body["model"] != "typed-decisions" || !strings.Contains(string(data), `"dm-titles-2"`) {
			t.Errorf("sent %s", data)
		}
		_, _ = io.WriteString(w, `{"answers":{"action":{"choice":"reply","probabilities":{"reply":0.8}},"agent":{"choice":"dm-titles","probabilities":{"dm-titles":0.9}}}}`)
	}))
	defer server.Close()
	n, _ := newNorn(t)
	d, err := AskShadow(context.Background(), server.Client(), server.URL, "k", ShadowInput{Message: "the inbox one", Agents: agents}, time.Second)
	if err != nil || d.Action != "reply" || d.AgentID == nil || *d.AgentID != "a1" {
		t.Fatalf("got %+v, %v", d, err)
	}
	id, _ := n.Store.LogDecision(context.Background(), "s", "the inbox one", "", "model", nil, map[string]any{"action": "answer"}, nil, 0)
	_ = n.Store.SetShadow(context.Background(), id, d)
	rows, _ := n.Store.Decisions(context.Background(), 1)
	if len(rows) != 1 || !strings.Contains(string(rows[0].Shadow), `"agentId":"a1"`) {
		t.Fatalf("rows %+v", rows)
	}
}
