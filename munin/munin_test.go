package munin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"yggdrasil/internal/watch"
)

func TestParseTarget(t *testing.T) {
	target, err := ParseTarget("ollama", "ollama:http://127.0.0.1:11434/api/tags")
	if err != nil || target.Kind != Ollama || !target.Local || target.URL != "http://127.0.0.1:11434/api/tags" {
		t.Fatalf("got %+v, %v", target, err)
	}
	target, err = ParseTarget("slack", "http:https://slack.com/api/api.test")
	if err != nil || target.Local {
		t.Fatalf("slack counted as local: %+v, %v", target, err)
	}
	for _, bad := range []string{"ftp:ftp://x", "http:not a url", "http"} {
		if _, err := ParseTarget("x", bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestMissingModels(t *testing.T) {
	tags := []byte(`{"models":[{"name":"qwen2.5:7b"},{"name":"nomic-embed-text:latest"}]}`)
	missing, err := MissingModels(tags, []string{"qwen2.5:7b", "nomic-embed-text", "llama3.2:1b"})
	if err != nil || !reflect.DeepEqual(missing, []string{"llama3.2:1b"}) {
		t.Fatalf("got %v, %v", missing, err)
	}
}

func TestIncident(t *testing.T) {
	indicator, description, err := Incident([]byte(`{"status":{"indicator":"major","description":"Partial outage"}}`))
	if err != nil || indicator != "major" || description != "Partial outage" {
		t.Fatalf("got %q %q %v", indicator, description, err)
	}
	if _, _, err := Incident([]byte(`{}`)); err == nil {
		t.Fatalf("an empty object read as a status")
	}
}

func resultsByKey(results []watch.Result) map[string]watch.Result {
	byKey := map[string]watch.Result{}
	for _, result := range results {
		byKey[result.Key] = result
	}
	return byKey
}

func TestAnOllamaThatIsDownIsOneAlertNotTwo(t *testing.T) {
	m := New(nil, []string{"qwen2.5:7b"}, "", nil)
	results, seen := m.check(context.Background(), Target{Name: "ollama", Kind: Ollama, URL: "http://127.0.0.1:1/api/tags", Local: true})
	byKey := resultsByKey(results)
	if byKey["down:ollama"].OK || !byKey["models:ollama"].Skip || seen.Up {
		t.Fatalf("got %+v", results)
	}
}

func TestAnAnswerOfAnyStatusIsUp(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	m := New(nil, nil, "", nil)
	results, seen := m.check(context.Background(), Target{Name: "laya", Kind: HTTP, URL: server.URL, Local: true})
	if !resultsByKey(results)["down:laya"].OK || !seen.Up {
		t.Fatalf("a 404 counted as down: %+v", results)
	}
}

func TestOfflineSkipsTheInternetTargets(t *testing.T) {
	m := New([]Target{{Name: "slack", Kind: HTTP, URL: "https://slack.example/", Local: false}}, nil, "http://127.0.0.1:1/", nil)
	m.Gjallar = nil
	for range 2 {
		_ = m.round(context.Background(), false)
	}
	offline, _ := m.watch.Get(OfflineKey)
	if !offline.Down {
		t.Fatalf("offline is not down: %+v", offline)
	}
	if _, known := m.watch.Get("down:slack"); known {
		t.Fatalf("slack was checked while offline")
	}
}
