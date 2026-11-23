package sindri

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLoginClaude prints a sign-in page and takes "good" as the code.
func fakeLoginClaude(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
echo "Opening browser to sign in…"
echo "If the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true&state=x"
printf "Paste code here if prompted > "
read code
if [ "$code" = "good" ]; then echo "Login successful."; exit 0; fi
echo "Invalid code"; exit 1
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestClaudeLoginSignsInWithTheCode(t *testing.T) {
	l := NewClaudeLogin(fakeLoginClaude(t))
	l.MarkExpired("Failed to authenticate: OAuth session expired")
	if got := l.State().State; got != "expired" {
		t.Fatalf("state %q, want expired", got)
	}
	started, err := l.Start()
	if err != nil {
		t.Fatal(err)
	}
	if started.State != "linking" || !strings.HasPrefix(started.URL, "https://claude.com/cai/oauth/authorize?") {
		t.Fatalf("started %+v, want linking with the page", started)
	}
	again, _ := l.Start()
	if again.URL != started.URL {
		t.Errorf("a second start made a new sign-in: %+v", again)
	}
	done, err := l.Finish("good")
	if err != nil {
		t.Fatal(err)
	}
	if done.State != "ok" || done.SignedInAt == 0 {
		t.Fatalf("after the code %+v, want ok", done)
	}
}

func TestClaudeLoginSaysWhyTheCodeFailed(t *testing.T) {
	l := NewClaudeLogin(fakeLoginClaude(t))
	if _, err := l.Start(); err != nil {
		t.Fatal(err)
	}
	done, err := l.Finish("bad")
	if err != nil {
		t.Fatal(err)
	}
	if done.State != "error" || !strings.Contains(done.Error, "Invalid code") || strings.Contains(done.Error, "https://") {
		t.Fatalf("after a bad code %+v, want Claude's reason without the link", done)
	}
	if _, err := l.Finish("good"); err == nil {
		t.Error("a code with no sign-in waiting was taken")
	}
}
