package norn

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yggdrasil/internal/llm"
	"yggdrasil/internal/mcp"
)

func fakeMCP(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID     *int           `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		if body.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != "s1" {
			t.Errorf("%s without the session", body.Method)
		}
		var result any
		switch body.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			result = map[string]any{}
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "list_cycles", "inputSchema": map[string]any{"type": "object"}}, {"name": "save_issue", "inputSchema": map[string]any{}}}}
		case "tools/call":
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "Cycle 42, ends Friday"}}}
		}
		// Answered as an event stream, as Linear's server does.
		w.Header().Set("Content-Type", "text/event-stream")
		encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": result})
		_, _ = io.WriteString(w, "event: message\ndata: "+string(encoded)+"\n\n")
	}))
}

func TestLookupAnswersWithReadOnlyTools(t *testing.T) {
	server := fakeMCP(t)
	defer server.Close()
	var offered []string
	steps := 0
	l := &Lookup{Model: "lookup", Servers: []LookupServer{{Name: "linear", Client: &mcp.Client{URL: server.URL, Token: "k", HTTP: server.Client()}}},
		Chat: func(_ context.Context, messages []llm.ToolMessage, tools []llm.Tool, _ llm.Options) (llm.ToolTurn, error) {
			steps++
			if steps == 1 {
				for _, tool := range tools {
					offered = append(offered, tool.Function.Name)
				}
				call := llm.ToolCall{ID: "c1", Type: "function"}
				call.Function.Name, call.Function.Arguments = "linear__list_cycles", `{}`
				return llm.ToolTurn{ToolCalls: []llm.ToolCall{call}}, nil
			}
			last := messages[len(messages)-1]
			return llm.ToolTurn{Content: "The current cycle is " + strings.TrimPrefix(*last.Content, "Cycle ")}, nil
		}}
	question := "What's in the current cycle?"
	if AsksForChange(question) || len(ServersFor(question, l.Servers)) != 1 {
		t.Fatal("the question should go to linear")
	}
	found := l.Run(context.Background(), question, "", nil, l.Servers, t.Logf)
	if found.Answer != "The current cycle is 42, ends Friday" || strings.Join(found.Tools, ",") != "linear__list_cycles" {
		t.Fatalf("got %+v", found)
	}
	if strings.Join(offered, ",") != "linear__list_cycles,hand_to_claude" {
		t.Errorf("offered %v", offered)
	}
	if !AsksForChange("move AI-1 to done") {
		t.Error("a change is not a lookup")
	}
}
