package sindri

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func entry(v map[string]any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func assistantLine(stop any, content []any) string {
	if content == nil {
		content = []any{map[string]any{"type": "text", "text": "hi"}}
	}
	return entry(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "stop_reason": stop, "content": content}})
}

var toolCall = []any{map[string]any{"type": "tool_use", "name": "Bash"}}

func TestReadTranscriptState(t *testing.T) {
	ended := &TranscriptState{Phase: "turn-ended", LastText: "hi"}
	cases := map[string]struct {
		tail string
		want *TranscriptState
	}{
		"finished turn":        {assistantLine("end_turn", nil), ended},
		"running tool":         {assistantLine("tool_use", toolCall), &TranscriptState{Phase: "busy", Tool: "Bash"}},
		"last of several":      {assistantLine("tool_use", []any{map[string]any{"type": "tool_use", "name": "Read"}, map[string]any{"type": "tool_use", "name": "Grep"}}), &TranscriptState{Phase: "busy", Tool: "Grep"}},
		"still writing":        {assistantLine(nil, nil), &TranscriptState{Phase: "busy"}},
		"unanswered user turn": {assistantLine("end_turn", nil) + "\n" + entry(map[string]any{"type": "user", "message": map[string]any{"content": "go"}}), &TranscriptState{Phase: "busy"}},
		"unknown entry":        {assistantLine("end_turn", nil) + "\n" + entry(map[string]any{"type": "something-new"}), ended},
		"injected context":     {assistantLine("end_turn", nil) + "\n" + entry(map[string]any{"type": "user", "isMeta": true, "message": map[string]any{"content": "reminder"}}), ended},
		"compaction summary":   {assistantLine("end_turn", nil) + "\n" + entry(map[string]any{"type": "user", "isCompactSummary": true, "message": map[string]any{"content": "continued"}}), ended},
		"subagent finished":    {assistantLine("tool_use", toolCall) + "\n" + entry(map[string]any{"type": "assistant", "isSidechain": true, "message": map[string]any{"stop_reason": "end_turn", "content": []any{}}}), &TranscriptState{Phase: "busy"}},
		"cut first line":       {`{"type":"assis` + "\n" + assistantLine("end_turn", nil), ended},
		"api error":            {entry(map[string]any{"type": "assistant", "isApiErrorMessage": true, "message": map[string]any{"stop_reason": "stop_sequence", "content": []any{map[string]any{"type": "text", "text": "API Error: Connection lost"}}}}), &TranscriptState{Phase: "turn-ended", Ending: "error", LastText: "API Error: Connection lost"}},
		"interrupted":          {assistantLine("tool_use", toolCall) + "\n" + entry(map[string]any{"type": "user", "message": map[string]any{"content": "[Request interrupted by user for tool use]"}}), &TranscriptState{Phase: "turn-ended", Ending: "interrupted"}},
		"nothing yet":          {entry(map[string]any{"type": "bridge-session"}), nil},
	}
	for name, c := range cases {
		if got := ReadTranscriptState(c.tail); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", name, got, c.want)
		}
	}
	long := ReadTranscriptState(assistantLine("end_turn", []any{map[string]any{"type": "text", "text": strings.Repeat("x", LastTextMax) + " Which one do you want?"}}))
	if !strings.HasSuffix(long.LastText, "Which one do you want?") || len([]rune(long.LastText)) != LastTextMax {
		t.Errorf("long text kept %d runes", len([]rune(long.LastText)))
	}
	titles := ReadTranscriptTitles(entry(map[string]any{"type": "custom-title", "customTitle": "My rename"}) + "\n" + entry(map[string]any{"type": "ai-title", "aiTitle": "Later guess"}))
	if titles != (TranscriptTitles{Custom: "My rename", AI: "Later guess"}) {
		t.Errorf("titles %+v", titles)
	}
}

func TestLiveClaudeSessions(t *testing.T) {
	claude, sockets := t.TempDir(), t.TempDir()
	write := func(path, text string) {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		_ = os.WriteFile(path, []byte(text), 0o644)
	}
	write(filepath.Join(claude, "sessions", "100.json"), `{"pid":100,"sessionId":"s-live","cwd":"/home/me/proj","name":"proj-3f","nameSource":"derived","entrypoint":"cli"}`)
	write(filepath.Join(claude, "sessions", "200.json"), `{"pid":200,"sessionId":"s-dead","cwd":"/home/me/old","startedAt":1000}`)
	write(filepath.Join(sockets, "100.sock"), "")
	write(filepath.Join(claude, "projects", EncodeProjectDir("/home/me/proj"), "s-live.jsonl"),
		entry(map[string]any{"type": "ai-title", "aiTitle": "Fix the login"})+"\n"+assistantLine("tool_use", toolCall)+"\n")
	write(filepath.Join(claude, "agent-conduit", "permissions", "s-live.json"), `{"at":1,"tool":"Bash"}`)

	sessions, degraded := ReadLiveClaudeSessions(ClaudeRoot{ClaudeDir: claude, SocketDir: sockets}, NewLiveClaudeCache(), time.Now())
	if degraded || len(sessions) != 1 {
		t.Fatalf("got %+v (degraded %v)", sessions, degraded)
	}
	s := sessions[0]
	if s.SessionID != "s-live" || s.Name != "Fix the login" || s.State.Tool != "Bash" || s.Permission == nil || s.Permission.Tool != "Bash" {
		t.Errorf("got %+v", s)
	}
	// No socket directory: the registry, but only recently active sessions.
	sessions, degraded = ReadLiveClaudeSessions(ClaudeRoot{ClaudeDir: claude}, NewLiveClaudeCache(), time.Now().Add(time.Hour))
	if !degraded || len(sessions) != 0 {
		t.Errorf("degraded read kept %+v", sessions)
	}
	if args, err := ResumeArgs("claude", "--evil"); err == nil {
		t.Errorf("took %v", args)
	}
	if got := ResumeCommand("claude", "abc", "/home/me/my proj"); got != `cd '/home/me/my proj' && claude --resume abc` {
		t.Errorf("command %q", got)
	}
}

func TestOpencodeSessions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directory := r.URL.Query().Get("directory")
		switch r.URL.Path {
		case "/project":
			fmt.Fprint(w, `[{"worktree":"/home/me/proj"},{"worktree":"/"}]`)
		case "/session":
			if directory == "/home/me/proj" {
				fmt.Fprint(w, `[{"id":"root","directory":"/home/me/proj","title":"Fix it","time":{"created":1,"updated":5}},{"id":"child","parentID":"root","directory":"/home/me/proj","time":{"created":2,"updated":3}}]`)
			} else {
				fmt.Fprint(w, `[{"id":"idle","directory":"/tmp","title":"Old","time":{"created":1,"updated":2}}]`)
			}
		case "/session/status":
			fmt.Fprint(w, `{"child":{"type":"busy"}}`)
		case "/permission":
			if directory == "/home/me/proj" {
				fmt.Fprint(w, `[{"id":"perm1","sessionID":"child","permission":"bash","patterns":["git push"]}]`)
			} else {
				fmt.Fprint(w, `[]`)
			}
		case "/question":
			w.WriteHeader(404)
		case "/permission/perm1/reply":
			if directory != "/home/me/proj" {
				t.Errorf("reply without the directory")
			}
			fmt.Fprint(w, `true`)
		}
	}))
	defer server.Close()
	client := &OpencodeClient{BaseURL: server.URL, HTTP: server.Client()}
	sessions, err := client.Sessions(context.Background())
	if err != nil || len(sessions) != 2 {
		t.Fatalf("got %+v, %v", sessions, err)
	}
	root := sessions[0]
	if root.SessionID != "root" || root.Activity != "approval" || len(root.Pending) != 1 || root.Pending[0].Summary != "bash git push" || root.Pending[0].Directory != "/home/me/proj" {
		t.Errorf("root %+v", root)
	}
	s := NewAgentSessions(ClaudeRoot{}, client, nil)
	_ = s.ReadOpencode(context.Background())
	if err := s.answerOpencode(context.Background(), "perm1", true); err != nil {
		t.Error(err)
	}
}

// fakeHerdr is a herdr stand-in on a unix socket: one JSON request per line in, one answer out.
type fakeHerdr struct {
	mu     sync.Mutex
	calls  []string
	handle func(method string, params map[string]any) (any, *HerdrError)
}

func (f *fakeHerdr) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...)
}

func serveHerdr(t *testing.T, f *fakeHerdr) *Herdr {
	dir, err := os.MkdirTemp("", "herdr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "h.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				scanner := bufio.NewScanner(conn)
				for scanner.Scan() {
					var request struct {
						ID     string         `json:"id"`
						Method string         `json:"method"`
						Params map[string]any `json:"params"`
					}
					_ = json.Unmarshal(scanner.Bytes(), &request)
					f.mu.Lock()
					f.calls = append(f.calls, request.Method)
					f.mu.Unlock()
					result, failure := f.handle(request.Method, request.Params)
					answer := map[string]any{"id": request.ID, "result": result}
					if failure != nil {
						answer = map[string]any{"id": request.ID, "error": map[string]string{"code": failure.Code, "message": failure.Message}}
					}
					data, _ := json.Marshal(answer)
					_, _ = conn.Write(append(data, '\n'))
				}
			}()
		}
	}()
	return &Herdr{Socket: socket}
}

const readOutside = "──────────────────────────────────────────────────────────────────────\n" +
	" Read outside the working directories\n" +
	"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
	" Read(/home/me/proj/app/pages/dashboa\n" +
	" rd/system.vue · lines 1-250)\n" +
	"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
	" Auto mode and the sandbox read outside the working directories\n" +
	" without asking. Block: the file tools refuse reads outside the                No changes this session\n" +
	"\n" +
	" Allow this read outside the working directories?\n" +
	" ❯ 1. Yes, and keep allowing any reads outside the working directories\n" +
	"   2. No, and block reads outside the working directories from now on\n" +
	"   3. No, and ask again next time\n" +
	"   4. Yes, but ask again next time\n" +
	"                                                                       +1 file edited before this session (show)\n" +
	" Esc to cancel · Tab to amend"

func TestHerdrDialogs(t *testing.T) {
	if DialogOf("claude", " Bash command\n  npm test\n Do you want to proceed?\n ❯ 1. Yes\n   2. Yes, and don't ask again for npm test\n   3. No, and tell Claude what to do differently (esc)") != "permission" ||
		DialogOf("claude", " Quick safety check: Is this a project you created or one you trust?\n ❯ No, exit\n   Yes, I trust this folder") != "other" ||
		DialogOf("opencode", "Do you want to proceed?\n 1. Yes\n 3. No") != "other" {
		t.Error("dialogOf")
	}
	got := DialogScreenOf(readOutside)
	want := &HerdrDialogScreen{
		Question: "Allow this read outside the working directories?",
		Detail:   " Read outside the working directories\n Read(/home/me/proj/app/pages/dashboa\n rd/system.vue · lines 1-250)\n Auto mode and the sandbox read outside the working directories\n without asking. Block: the file tools refuse reads outside the",
		Choices: []HerdrDialogChoice{
			{Key: "1", Label: "Yes, and keep allowing any reads outside the working directories", Selected: true},
			{Key: "2", Label: "No, and block reads outside the working directories from now on"},
			{Key: "3", Label: "No, and ask again next time"},
			{Key: "4", Label: "Yes, but ask again next time"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v", got)
	}
	wrapped := DialogScreenOf(" Plan:\n 1. Add the route\n 2. Test it\n──────────\n Bash command\n   npm test\n Do you want to proceed?\n ❯ 1. Yes\n   2. Yes, and don't ask again for npm test commands in\n      /home/me/proj\n   3. No\n")
	if wrapped == nil || wrapped.Choices[1].Label != "Yes, and don't ask again for npm test commands in /home/me/proj" || wrapped.Detail != " Bash command\n   npm test" {
		t.Errorf("wrapped %#v", wrapped)
	}
	if DialogScreenOf(" Is this a project you trust?\n ❯ No, exit\n   Yes, I trust this folder") != nil || DialogScreenOf("> working on it\n 1. one thing") != nil {
		t.Error("read choices that are not there")
	}
	if c := ChoiceFor(want.Choices, true); c.Key != "4" {
		t.Errorf("allow picked %+v", c)
	}
	if c := ChoiceFor(want.Choices, false); c.Key != "2" {
		t.Errorf("deny picked %+v", c)
	}

	f := &fakeHerdr{handle: func(method string, _ map[string]any) (any, *HerdrError) {
		if method == "agent.read" {
			return map[string]any{"read": map[string]any{"text": readOutside}}, nil
		}
		return map[string]any{"type": "ok"}, nil
	}}
	h := serveHerdr(t, f)
	if err := h.Choose(context.Background(), "w1:p2", DialogAnswer{Question: want.Question, Key: "3", Label: "No, and ask again next time"}); err != nil {
		t.Fatal(err)
	}
	err := h.Choose(context.Background(), "w1:p2", DialogAnswer{Question: "Something else?", Key: "1", Label: "Yes"})
	var herdrErr *HerdrError
	if !errors.As(err, &herdrErr) || herdrErr.Code != "dialog_changed" {
		t.Errorf("got %v", err)
	}
	if calls := f.called(); strings.Join(calls, ",") != "agent.read,agent.send_keys,agent.read" {
		t.Errorf("calls %v", calls)
	}
	var down *HerdrDown
	if _, err := (&Herdr{Socket: filepath.Join(t.TempDir(), "none.sock")}).List(context.Background()); !errors.As(err, &down) {
		t.Errorf("a missing herdr is %v", err)
	}
}
