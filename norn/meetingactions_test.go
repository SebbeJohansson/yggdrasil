package norn

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if code != http.StatusCreated || body.(map[string]any)["identifier"] != "AI-9" || body.(map[string]any)["note"] != nil {
		t.Fatalf("%d %v", code, body)
	}
	// With the meeting, the issue is linked under Åtgärder in its note, below what you wrote.
	n.Vault = testVault(t, nil)
	meeting := Meeting{Subject: "Snacka Jev", Start: time.Now().UnixMilli()}
	if _, _, err := n.Vault.WriteLiveNote(meeting, "abc123", "- Stäm av med Peter", n.Zone, time.Now()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		code, body = call(t, p, "POST", "/v1/linear/issues", map[string]any{"team": "AI", "title": "Stäm av", "meeting": meeting})
		if code != http.StatusCreated || body.(map[string]any)["note"] == nil {
			t.Fatalf("%d %v", code, body)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(n.Vault.Dir, body.(map[string]any)["note"].(map[string]any)["path"].(string)))
	got := string(raw)
	if strings.Count(got, "## Åtgärder") != 1 || strings.Count(got, "- [AI-9](https://linear.app/x/AI-9) Stäm av") != 2 ||
		strings.Index(got, "## Under mötet") > strings.Index(got, "## Åtgärder") {
		t.Fatalf("got:\n%s", got)
	}
}
