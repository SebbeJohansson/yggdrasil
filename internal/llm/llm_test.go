package llm

import (
	"context"
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
