package contract

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yggdrasil/api"
)

func munin(t *testing.T) *Spec {
	t.Helper()
	spec, err := Load("munin", api.Spec("munin"))
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestTheSchemaCheckerCatchesWhatMatters(t *testing.T) {
	spec := munin(t)
	schema, _, _ := spec.Response("GET", "/v1/status", 200)
	good := map[string]any{"targets": []any{}, "checks": []any{}}
	if err := spec.Validate(schema, good); err != nil {
		t.Fatalf("a good answer failed: %v", err)
	}
	for name, bad := range map[string]any{
		"null list":      map[string]any{"targets": nil, "checks": []any{}},
		"missing field":  map[string]any{"targets": []any{}},
		"wrong type":     map[string]any{"targets": []any{}, "checks": "none"},
		"bad enum":       map[string]any{"targets": []any{map[string]any{"name": "x", "kind": "ftp", "url": "u", "local": true, "up": true}}, "checks": []any{}},
		"fraction count": map[string]any{"targets": []any{}, "checks": []any{map[string]any{"key": "k", "down": true, "fails": 1.5, "passes": 0, "since": "s", "checkedAt": "c", "raised": false}}},
	} {
		if err := spec.Validate(schema, bad); err == nil {
			t.Errorf("%s passed", name)
		}
	}
}

func TestAServiceThatBreaksTheContractFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Write([]byte(`{"ok":true,"part":"munin","version":"x","startedAt":"2026-01-01T00:00:00Z","loops":{}}`))
		case "/openapi.yaml":
			w.Write([]byte("openapi: 3.1.0"))
		default:
			// Answers without checking the token, and with the wrong shape.
			w.Write([]byte(`{"targets":null}`))
		}
	}))
	defer server.Close()
	results := Check(context.Background(), munin(t), server.URL, Options{OwnerToken: "t"})
	if Passed(results) {
		t.Fatalf("a broken service passed")
	}
	var failed []string
	for _, result := range results {
		if !result.OK && !result.Skip {
			failed = append(failed, result.Name)
		}
	}
	if strings.Join(failed, ",") != "refuses no token: GET /v1/status,GET /v1/status" {
		t.Fatalf("failed %v", failed)
	}
}

func TestEverySpecLoadsAndKeepsTheHealthContract(t *testing.T) {
	var first string
	for _, name := range api.Parts() {
		spec, err := Load(name, api.Spec(name))
		if err != nil {
			t.Fatal(err)
		}
		health, _, ok := spec.Response("GET", "/health", 200)
		if !ok || health == nil {
			t.Fatalf("%s has no /health", name)
		}
		resolved, _ := spec.resolve(health)
		// Marshalled, a map's keys come out sorted, so equal schemas give equal text.
		encoded, _ := json.Marshal(resolved)
		text := string(encoded)
		if first == "" {
			first = text
		} else if text != first {
			t.Errorf("%s's Health differs from the others: every part must answer /health the same way", name)
		}
	}
}
