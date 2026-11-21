package sindri

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/part"
)

type fakeCloudCLI struct {
	started []string
	sent    []string
	sendErr error
}

func (f *fakeCloudCLI) Start(_ context.Context, cwd, task string) (CloudStarted, error) {
	f.started = append(f.started, cwd+": "+task)
	return CloudStarted{ID: "session_01ABC", URL: "https://claude.ai/code/session_01ABC", Pane: "w1:p1"}, nil
}

func (f *fakeCloudCLI) Send(_ context.Context, cwd, id, text string) error {
	f.sent = append(f.sent, id+": "+text)
	return f.sendErr
}

func (f *fakeCloudCLI) Teleport(_ context.Context, cwd, id string) (HerdrAgent, error) {
	return HerdrAgent{PaneID: "w2:p1", Kind: "claude", Cwd: cwd}, nil
}

func cloudServer(t *testing.T) (*httptest.Server, *fakeCloudCLI) {
	t.Helper()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cli := &fakeCloudCLI{}
	cloud, err := NewCloud(store, cli)
	if err != nil {
		t.Fatal(err)
	}
	tokens := contracttest.Tokens()
	tokens["saga-token-0123456789abcdef0123456789"] = "saga"
	p := part.New(Name, tokens, nil)
	(&Sindri{Cloud: cloud}).Mount(p)
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	return server, cli
}

func cloudCall(t *testing.T, server *httptest.Server, token, method, path, body string, out any) int {
	t.Helper()
	request, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if out != nil {
		_ = json.NewDecoder(response.Body).Decode(out)
	}
	return response.StatusCode
}

func TestACloudSessionIsStartedFollowedAndBroughtHome(t *testing.T) {
	server, cli := cloudServer(t)
	var started struct{ Session CloudSession }
	if status := cloudCall(t, server, contracttest.Owner, "POST", "/v1/cloud", `{"cwd":"/home/me/projects/demo","task":"Fix the login bug"}`, &started); status != 201 {
		t.Fatalf("start answered %d", status)
	}
	if started.Session.ID != "session_01ABC" || started.Session.URL != "https://claude.ai/code/session_01ABC" || started.Session.Task != "Fix the login bug" {
		t.Fatalf("started %+v", started.Session)
	}
	if len(cli.started) != 1 || cli.started[0] != "/home/me/projects/demo: Fix the login bug" {
		t.Fatalf("the CLI was asked %v", cli.started)
	}

	// Sága may follow up for you, from another device.
	var sent struct{ Session CloudSession }
	if status := cloudCall(t, server, "saga-token-0123456789abcdef0123456789", "POST", "/v1/cloud/session_01ABC/message", `{"text":"Add a test too"}`, &sent); status != 200 {
		t.Fatalf("message answered %d", status)
	}
	if sent.Session.LastSentAt == nil || cli.sent[0] != "session_01ABC: Add a test too" {
		t.Fatalf("sent %+v, CLI %v", sent.Session, cli.sent)
	}

	var home struct {
		Session CloudSession
		Agent   HerdrAgent
	}
	if status := cloudCall(t, server, contracttest.Owner, "POST", "/v1/cloud/session_01ABC/teleport", ``, &home); status != 200 {
		t.Fatalf("teleport answered %d", status)
	}
	if home.Agent.PaneID != "w2:p1" || home.Session.TeleportedAt == nil {
		t.Fatalf("teleported %+v", home)
	}

	var list struct{ Sessions []CloudSession }
	cloudCall(t, server, contracttest.Owner, "GET", "/v1/cloud", ``, &list)
	if len(list.Sessions) != 1 {
		t.Fatalf("listed %+v", list.Sessions)
	}
	if status := cloudCall(t, server, contracttest.Owner, "DELETE", "/v1/cloud/session_01ABC", ``, nil); status != 200 {
		t.Fatalf("forget answered %d", status)
	}
	cloudCall(t, server, contracttest.Owner, "GET", "/v1/cloud", ``, &list)
	if len(list.Sessions) != 0 {
		t.Fatalf("still listed %+v", list.Sessions)
	}
}

func TestOnlyYouOrSagaDriveCloudSessions(t *testing.T) {
	server, cli := cloudServer(t)
	if status := cloudCall(t, server, contracttest.Sender, "POST", "/v1/cloud", `{"cwd":"/x","task":"t"}`, nil); status != 403 {
		t.Fatalf("a sender starting answered %d", status)
	}
	if status := cloudCall(t, server, contracttest.Owner, "POST", "/v1/cloud/session_none/message", `{"text":"hi"}`, nil); status != 404 {
		t.Fatalf("an unknown session answered %d", status)
	}
	if status := cloudCall(t, server, contracttest.Owner, "POST", "/v1/cloud", `{"cwd":"relative","task":"t"}`, nil); status != 400 {
		t.Fatalf("a relative cwd answered %d", status)
	}
	if len(cli.started) != 0 {
		t.Fatalf("the CLI was asked %v", cli.started)
	}
}

func TestAFailedMessageIsKept(t *testing.T) {
	server, cli := cloudServer(t)
	cloudCall(t, server, contracttest.Owner, "POST", "/v1/cloud", `{"cwd":"/x","task":"t"}`, nil)
	cli.sendErr = httpErr(502, "the cloud session did not take it: archived")
	if status := cloudCall(t, server, contracttest.Owner, "POST", "/v1/cloud/session_01ABC/message", `{"text":"hi"}`, nil); status != 502 {
		t.Fatalf("answered %d", status)
	}
	var list struct{ Sessions []CloudSession }
	cloudCall(t, server, contracttest.Owner, "GET", "/v1/cloud", ``, &list)
	if !strings.Contains(list.Sessions[0].LastError, "archived") {
		t.Fatalf("kept %+v", list.Sessions[0])
	}
}

func TestTheCloudLinkIsFoundInWhatTheCLIPrints(t *testing.T) {
	url, id := FindCloudURL("Created cloud session.\n  View: https://claude.ai/code/session_01Xy-z9 (opens in browser)\n")
	if url != "https://claude.ai/code/session_01Xy-z9" || id != "session_01Xy-z9" {
		t.Fatalf("found %q %q", url, id)
	}
	if _, id := FindCloudURL("no link here"); id != "" {
		t.Fatalf("found %q", id)
	}
}

// The real CLI, faked by a script: a message's JSON answer is read, ok or not.
func TestClaudeCloudReadsTheCLIsAnswer(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	script := `#!/bin/sh
case "$2" in
  fail) echo '{"ok":false,"session_id":"session_1","error":"session is archived"}' ;;
  *) echo '{"ok":true,"session_id":"session_1","url":"https://claude.ai/code/session_1"}' ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cli := &ClaudeCloud{Bin: bin}
	if err := cli.Send(context.Background(), dir, "session_1", "hello"); err != nil {
		t.Fatalf("ok answer: %v", err)
	}
	err := cli.Send(context.Background(), dir, "session_1", "fail")
	if err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("failed answer: %v", err)
	}
	if _, err := (&ClaudeCloud{Bin: bin}).Teleport(context.Background(), dir, "session_1"); err == nil {
		t.Fatal("teleport without herdr should be refused")
	}
}

func TestClaudeCloudStartsWithoutHerdr(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	script := "#!/bin/sh\necho 'Creating cloud session...'\necho \"Task: $2\"\necho 'View: https://claude.ai/code/session_42abc'\nsleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	started, err := (&ClaudeCloud{Bin: bin}).Start(context.Background(), dir, "Fix it")
	if err != nil || started.ID != "session_42abc" {
		t.Fatalf("started %+v, %v", started, err)
	}
}
