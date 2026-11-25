package sindri

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestCloudHerdrNamesAreValid(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	for _, dir := range []string{"/home/sebbe/projects/sebbejohansson-front", "/x/A Very Long Project Folder Name Indeed", "/x/123"} {
		for _, name := range []string{HerdrAgentName(dir), PrefixedHerdrName("cloud-", dir)} {
			if !valid.MatchString(name) {
				t.Errorf("%s gives %q", dir, name)
			}
		}
	}
}

func TestCloudStartSaysWhenClaudeAsksToTrustTheFolder(t *testing.T) {
	var name any
	f := &fakeHerdr{handle: func(method string, params map[string]any) (any, *HerdrError) {
		switch method {
		case "workspace.create":
			return map[string]any{"workspace": map[string]any{"workspace_id": "w9"}, "root_pane": map[string]any{"pane_id": "w9:p1"}}, nil
		case "agent.start":
			name = params["name"]
			return map[string]any{"agent": map[string]any{"pane_id": "w9:p1", "name": params["name"], "agent": "claude"}}, nil
		case "agent.read":
			return map[string]any{"read": map[string]any{"text": " Quick safety check: Is this a project you created or one you trust?\n ❯ No, exit\n   Yes, I trust this folder"}}, nil
		}
		return map[string]any{"type": "ok"}, nil
	}}
	cloud := &ClaudeCloud{Bin: "claude", Herdr: serveHerdr(t, f), StartWait: 5 * time.Second}
	started := time.Now()
	_, err := cloud.startInHerdr(context.Background(), "/home/me/projects/sebbejohansson-front", "a task")
	if err == nil || !strings.Contains(err.Error(), "trust") || time.Since(started) > 3*time.Second {
		t.Fatalf("got %v after %s", err, time.Since(started))
	}
	if s, _ := name.(string); !regexp.MustCompile(`^cloud-sebbejohansson-front-[0-9a-f]{4}$`).MatchString(s) {
		t.Errorf("named %v", name)
	}
}
