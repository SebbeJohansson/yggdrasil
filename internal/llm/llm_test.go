package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNoFallbackSkipsOllama(t *testing.T) {
	var ollama atomic.Int32
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	defer broken.Close()
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ollama.Add(1)
		_, _ = w.Write([]byte(`{"message":{"content":"ok"}}`))
	}))
	defer local.Close()
	c := &Client{PrimaryURL: broken.URL + "/v1", OllamaURL: local.URL}

	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, Options{NoFallback: true}); err == nil {
		t.Fatal("NoFallback: want XeStrata's error, got an answer")
	}
	if ollama.Load() != 0 {
		t.Fatalf("NoFallback asked Ollama %d times", ollama.Load())
	}
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, Options{}); err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if ollama.Load() != 1 {
		t.Fatalf("without NoFallback Ollama was asked %d times, want 1", ollama.Load())
	}
}

func TestKeepWarmAsksOnlyWhenIdle(t *testing.T) {
	var asked atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer server.Close()
	recorded := 0
	c := &Client{PrimaryURL: server.URL + "/v1", Record: func(Call) { recorded++ }}

	if err := c.KeepWarm(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if asked.Load() != 1 {
		t.Fatalf("cold: asked %d times, want 1", asked.Load())
	}
	if err := c.KeepWarm(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if asked.Load() != 1 {
		t.Fatalf("warm: asked again (%d times)", asked.Load())
	}
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, Options{}); err != nil {
		t.Fatal(err)
	}
	if recorded != 1 {
		t.Fatalf("recorded %d calls, want only the real one", recorded)
	}
	if (&Client{}).KeepWarm(context.Background(), time.Minute) != nil {
		t.Fatal("no XeStrata: KeepWarm should do nothing")
	}
}

func TestNoFallbackNeverAsksOllama(t *testing.T) {
	ollama := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/chat" {
			ollama++
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	c := &Client{PrimaryURL: server.URL + "/v1", OllamaURL: server.URL, NoFallback: true}
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, Options{Model: "qwen2.5:7b"}); err == nil {
		t.Fatal("no error")
	}
	if ollama != 0 {
		t.Fatalf("asked Ollama %d times", ollama)
	}
}

func TestEmbedAsksTheEmbeddingServerInOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("asked %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`))
	}))
	defer server.Close()
	c := &Client{EmbedURL: server.URL + "/v1"}
	vectors, err := c.Embed(context.Background(), "nomic-embed-text-v1.5", []string{"a", "b"})
	if err != nil || len(vectors) != 2 || vectors[0][0] != 1 || vectors[1][1] != 1 {
		t.Fatalf("%v %v", vectors, err)
	}
	if !c.CanEmbed() || (&Client{}).CanEmbed() {
		t.Fatal("CanEmbed")
	}
}

func TestOllamaThinksOnlyWhenAsked(t *testing.T) {
	var think []any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		think = append(think, body["think"])
		_, _ = w.Write([]byte(`{"message":{"content":"ok"}}`))
	}))
	defer server.Close()
	c := &Client{OllamaURL: server.URL}
	yes := true
	for _, options := range []Options{{}, {Think: &yes}} {
		if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, options); err != nil {
			t.Fatal(err)
		}
	}
	if len(think) != 2 || think[0] != false || think[1] != true {
		t.Fatalf("think sent as %v, want [false true]", think)
	}
}

func TestToolChatOnOllamaWithoutChatURL(t *testing.T) {
	var sent struct {
		Model    string `json:"model"`
		Messages []struct {
			Role      string `json:"role"`
			ToolName  string `json:"tool_name"`
			ToolCalls []struct {
				Function struct {
					Arguments map[string]any `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []Tool `json:"tools"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("asked %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&sent)
		_, _ = w.Write([]byte(`{"message":{"content":"","tool_calls":[{"function":{"name":"issues","arguments":{"team":"AI"}}}]}}`))
	}))
	defer server.Close()
	var recorded Call
	c := &Client{OllamaURL: server.URL, Record: func(call Call) { recorded = call }}

	question, earlier := "what's open?", ""
	previous := ToolCall{ID: "call_0", Type: "function"}
	previous.Function.Name, previous.Function.Arguments = "cycle", `{"current":true}`
	result := "cycle 12"
	var tool Tool
	tool.Type, tool.Function.Name = "function", "issues"
	turn, err := c.ToolChat(context.Background(), []ToolMessage{
		{Role: "user", Content: &question},
		{Role: "assistant", Content: &earlier, ToolCalls: []ToolCall{previous}},
		{Role: "tool", Content: &result, ToolCallID: "call_0"},
	}, []Tool{tool}, Options{Model: "qwen3:4b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.ToolCalls) != 1 || turn.ToolCalls[0].Function.Name != "issues" || turn.ToolCalls[0].Function.Arguments != `{"team":"AI"}` || turn.ToolCalls[0].ID == "" {
		t.Fatalf("turn: %+v", turn)
	}
	if sent.Model != "qwen3:4b" || len(sent.Tools) != 1 || len(sent.Messages) != 3 {
		t.Fatalf("sent: %+v", sent)
	}
	if sent.Messages[1].ToolCalls[0].Function.Arguments["current"] != true || sent.Messages[2].ToolName != "cycle" {
		t.Fatalf("history not in Ollama's shape: %+v", sent.Messages)
	}
	if recorded.Backend != "ollama" {
		t.Fatalf("recorded backend %q", recorded.Backend)
	}
}
