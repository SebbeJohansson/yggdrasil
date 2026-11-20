package norn

import (
	"context"
	"net/http"
	"testing"

	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/llm"
)

func TestTheModelModeSetting(t *testing.T) {
	n, p := newNorn(t)
	n.StartModelMode(context.Background())
	if _, body := call(t, p, "GET", "/v1/settings/model", nil); body.(map[string]any)["mode"] != "local" || body.(map[string]any)["privacy"] != "as-is" {
		t.Fatalf("default: %v", body)
	}
	if code, body := call(t, p, "PUT", "/v1/settings/model", map[string]any{"mode": "cloud", "privacy": "local-sensitive"}); code != http.StatusOK || body.(map[string]any)["mode"] != "cloud" {
		t.Fatalf("cloud: %d %v", code, body)
	}
	if !n.LLM.CloudOn() {
		t.Error("the client is not in cloud mode")
	}
	if _, _, err := n.guardCloud(context.Background(), "summary", []llm.Message{{Role: "user", Content: "mail"}}); err != llm.ErrKeptLocal {
		t.Errorf("summary went out: %v", err)
	}
	if n.Config.Judge.Cloud() {
		t.Error("the inbox judge goes to the cloud with mail kept local")
	}
	if code, _ := call(t, p, "PUT", "/v1/settings/model", map[string]any{"mode": "cloud", "privacy": "nope"}); code != http.StatusBadRequest {
		t.Errorf("bad privacy: %d", code)
	}
	contracttest.Run(t, p)
	call(t, p, "PUT", "/v1/settings/model", map[string]any{"mode": "local", "privacy": "as-is"})
	if n.LLM.CloudOn() {
		t.Error("still in cloud mode")
	}
}
