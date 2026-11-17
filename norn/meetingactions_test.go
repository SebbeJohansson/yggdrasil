package norn

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yggdrasil/internal/linear"
	"yggdrasil/internal/llm"
)

func TestParseActionsDropsBlanksAndRepeats(t *testing.T) {
	got, err := ParseActions("Here:\n```json\n" + `{"actions":[{"title":"Stäm av med Peter om AI-policyn.","description":"Om något måste stängas av."},{"title":" ","description":"x"},{"title":"stäm av med peter om ai-policyn","description":"igen"}]}` + "\n```")
	if err != nil || len(got) != 1 || got[0].Title != "Stäm av med Peter om AI-policyn" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := ParseActions("no json"); err == nil {
		t.Fatal("prose passed as actions")
	}
}

func TestSuggestMeetingActionsAsksTheLocalModel(t *testing.T) {
	n, p := newNorn(t)
	var asked string
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		asked = string(data)
		content, _ := json.Marshal(`{"actions":[{"title":"Stäm av med Peter","description":"AI-policyn."}]}`)
		_, _ = io.WriteString(w, `{"message":{"content":`+string(content)+`}}`)
	}))
	defer ollama.Close()
	n.LLM = &llm.Client{OllamaURL: ollama.URL}
	code, body := call(t, p, "POST", "/v1/notes/meeting/actions", map[string]any{"subject": "Snacka Jev", "text": "- Stäm av med Peter angående AI policyn"})
	answer := body.(map[string]any)
	actions := answer["actions"].([]any)
	if code != http.StatusOK || len(actions) != 1 || answer["team"] != "AI" || !strings.Contains(asked, "Stäm av med Peter angående") {
		t.Fatalf("%d %v", code, body)
	}
	if code, body := call(t, p, "POST", "/v1/notes/meeting/actions", map[string]any{"subject": "Snacka Jev", "text": "  "}); code != http.StatusOK || len(body.(map[string]any)["actions"].([]any)) != 0 {
		t.Fatalf("empty notes: %d %v", code, body)
	}
}

func TestCreateLinearIssueRoute(t *testing.T) {
	n, p := newNorn(t)
	if code, _ := call(t, p, "POST", "/v1/linear/issues", map[string]any{"team": "AI", "title": "x"}); code != http.StatusServiceUnavailable {
		t.Fatalf("no key: %d", code)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if strings.Contains(string(data), "mutation") {
			_, _ = io.WriteString(w, `{"data":{"issueCreate":{"success":true,"issue":{"identifier":"AI-9","title":"Stäm av","url":"https://linear.app/x/AI-9","state":{"name":"Todo","type":"unstarted"},"updatedAt":""}}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"viewer":{"id":"me"},"teams":{"nodes":[{"id":"t","key":"AI","name":"AI engineering"}]}}}`)
	}))
	defer server.Close()
	n.Linear = &linear.Client{Key: "key", URL: server.URL, HTTP: server.Client()}
	if code, _ := call(t, p, "POST", "/v1/linear/issues", map[string]any{"team": "AI", "title": " "}); code != http.StatusBadRequest {
		t.Fatalf("no title: %d", code)
	}
	code, body := call(t, p, "POST", "/v1/linear/issues", map[string]any{"team": "AI", "title": "Stäm av", "description": "Från mötet"})
	if code != http.StatusCreated || body.(map[string]any)["identifier"] != "AI-9" {
		t.Fatalf("%d %v", code, body)
	}
}
