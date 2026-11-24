package sindri

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/part"
)

func transcriptServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	claude := t.TempDir()
	folder := filepath.Join(claude, "projects", EncodeProjectDir("/home/me/site"))
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "s1.jsonl"), []byte("line one\nline two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := part.New(Name, contracttest.Tokens(), nil)
	s := &Sindri{Sessions: NewAgentSessions(ClaudeRoot{ClaudeDir: claude}, nil, nil)}
	s.mountTranscripts(p)
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	return server, claude
}

func getTranscript(t *testing.T, url, token string) (*http.Response, Transcript) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body Transcript
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res, body
}

func TestTranscriptReadsTheEndOfTheFile(t *testing.T) {
	server, _ := transcriptServer(t)
	res, body := getTranscript(t, server.URL+"/v1/sessions/claude/s1/transcript?dir=/home/me/site&tail=9", contracttest.Owner)
	if res.StatusCode != http.StatusOK || body.Size != 18 || body.Text != "line two\n" || body.Modified == 0 {
		t.Fatalf("got %d %+v", res.StatusCode, body)
	}
	// Another folder than the one dir names: found by the sweep.
	res, body = getTranscript(t, server.URL+"/v1/sessions/claude/s1/transcript?dir=/elsewhere", contracttest.Owner)
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(body.Text, "line one") {
		t.Fatalf("got %d %+v", res.StatusCode, body)
	}
}

func TestTranscriptIsTheOwnersOnly(t *testing.T) {
	server, _ := transcriptServer(t)
	if res, _ := getTranscript(t, server.URL+"/v1/sessions/claude/s1/transcript", contracttest.Sender); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a sender got %d", res.StatusCode)
	}
	if res, _ := getTranscript(t, server.URL+"/v1/sessions/claude/nope/transcript", contracttest.Owner); res.StatusCode != http.StatusNotFound {
		t.Fatalf("a missing session got %d", res.StatusCode)
	}
	if res, _ := getTranscript(t, server.URL+"/v1/sessions/claude/a.b/transcript", contracttest.Owner); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a dotted id got %d", res.StatusCode)
	}
}

func TestTranscriptLeavesOutTheTextWhenUnchanged(t *testing.T) {
	server, _ := transcriptServer(t)
	_, first := getTranscript(t, server.URL+"/v1/sessions/claude/s1/transcript", contracttest.Owner)
	since := strconv.FormatInt(first.Size, 10) + ":" + strconv.FormatInt(first.Modified, 10)
	res, again := getTranscript(t, server.URL+"/v1/sessions/claude/s1/transcript?since="+since, contracttest.Owner)
	if res.StatusCode != http.StatusOK || !again.Unchanged || again.Text != "" || again.Size != first.Size {
		t.Fatalf("got %d %+v", res.StatusCode, again)
	}
	_, stale := getTranscript(t, server.URL+"/v1/sessions/claude/s1/transcript?since=1:1", contracttest.Owner)
	if stale.Unchanged || stale.Text == "" {
		t.Fatalf("a stale since got %+v", stale)
	}
}
