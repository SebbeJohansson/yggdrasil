package norn

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/llm"
	"yggdrasil/internal/part"
)

// fakeSindri answers like Sindri, with one agent waiting on a tool.
type fakeSindri struct {
	mu     sync.Mutex
	calls  []string
	agents []Agent
}

func (f *fakeSindri) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	encoded, _ := json.Marshal(body)
	f.calls = append(f.calls, r.Method+" "+r.URL.Path+" "+string(encoded))
	switch {
	case r.URL.Path == "/v1/agents":
		part.JSON(w, 200, f.agents)
	case r.URL.Path == "/v1/projects":
		part.JSON(w, 200, []map[string]string{{"name": "my-widget"}, {"name": "glim"}})
	case strings.HasPrefix(r.URL.Path, "/v1/agents/") && strings.Contains(r.URL.Path, "/permissions/"):
		part.JSON(w, 200, f.agents[0])
	case strings.HasPrefix(r.URL.Path, "/v1/agents/"):
		part.JSON(w, 200, f.agents[0])
	case r.URL.Path == "/v1/work/code":
		part.JSON(w, 200, map[string]any{"answer": "Claude is coding this in my-widget on branch agent/x.", "parts": []map[string]string{{"agentId": "new-agent", "status": "starting", "project": "my-widget", "branch": "agent/x"}}})
	default:
		part.JSON(w, 200, f.agents[0])
	}
}

func (f *fakeSindri) called(prefix string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if strings.HasPrefix(call, prefix) {
			return call
		}
	}
	return ""
}

func withSindri(t *testing.T, n *Norn) *fakeSindri {
	t.Helper()
	fake := &fakeSindri{agents: []Agent{{ID: "a1", Label: "my-widget: Fix the DM titles", Status: "needs-approval", Resumable: true, LastActivity: time.Now().UnixMilli(),
		Pending: []struct {
			RequestID string `json:"requestId"`
			Summary   string `json:"summary"`
		}{{"r1", "Bash rm -rf build"}}}}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	n.Sindri = &SindriClient{URL: server.URL, Token: "t", HTTP: server.Client()}
	n.Config.DispatchModel = "off"
	return fake
}

func TestYesAnswersTheAgentWaitingOnATool(t *testing.T) {
	n, _ := newNorn(t)
	fake := withSindri(t, n)
	_ = n.Store.BindAgent(context.Background(), "s1", "a1")
	response, err := n.Ask(context.Background(), AskRequest{Question: "no, keep the build folder", Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Via != "rule: approval" || !strings.Contains(response.Answer, `told it: "keep the build folder"`) {
		t.Fatalf("got %+v", response)
	}
	if call := fake.called("POST /v1/agents/a1/permissions/r1"); !strings.Contains(call, `"allow":false`) {
		t.Fatalf("Sindri was told %q", call)
	}
}

func TestAgentCommandHandsCodingToSindri(t *testing.T) {
	n, _ := newNorn(t)
	fake := withSindri(t, n)
	response, err := n.Ask(context.Background(), AskRequest{Question: "/agent my-widget fix the DM titles", Session: "s2"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Delegated == nil || response.Delegated.AgentID != "new-agent" || response.Via != "/agent" {
		t.Fatalf("got %+v", response)
	}
	call := fake.called("POST /v1/work/code")
	if !strings.Contains(call, `"projects":["my-widget"]`) || !strings.Contains(call, `fix the DM titles`) {
		t.Fatalf("Sindri got %q", call)
	}
	if agents, _ := n.Store.AgentsOf(context.Background(), "s2"); len(agents) != 1 || agents[0] != "new-agent" {
		t.Fatalf("the agent is not in the conversation: %v", agents)
	}
}

func TestOnlySindriPostsAgentTurns(t *testing.T) {
	n, _ := newNorn(t)
	p := part.New(Name, map[string]string{contracttest.Owner: part.Owner, contracttest.Sender: "sindri"}, nil)
	n.Mount(p)
	post := func(token string) int {
		request := httptest.NewRequest("POST", "/v1/conversations/s3/agent-turns", strings.NewReader(`{"agent":"a1","label":"Fix","content":"Done. Merge it?"}`))
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		p.ServeHTTP(recorder, request)
		return recorder.Code
	}
	if code := post(contracttest.Owner); code != http.StatusForbidden {
		t.Fatalf("the owner posted: %d", code)
	}
	if code := post(contracttest.Sender); code != http.StatusCreated {
		t.Fatalf("sindri could not post: %d", code)
	}
	unread, _ := n.Store.UnreadAgentTurns(context.Background())
	if len(unread) != 1 || unread[0].Session != "s3" {
		t.Fatalf("got %+v", unread)
	}
	if session, _ := n.Store.SessionOf(context.Background(), "a1"); session != "s3" {
		t.Fatalf("the agent was not bound: %q", session)
	}
}

func TestNeedsYou(t *testing.T) {
	n, _ := newNorn(t)
	withSindri(t, n)
	_, _ = n.Store.AddAgentTurn(context.Background(), "s4", "gone-agent", "Old task", "Failed: tests broke")
	needs, err := n.NeedsYou(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, need := range needs {
		kinds[need.Agent] = need.Kind
	}
	if kinds["a1"] != "approval" || kinds["gone-agent"] != "failed" {
		t.Fatalf("got %+v", needs)
	}
}

func TestDispatchGuards(t *testing.T) {
	named := []NamedAgent{{Name: "titles", Agent: Agent{ID: "a1", Label: "Fix titles", Resumable: true}}, {Name: "closed", Agent: Agent{ID: "a2", Label: "Old"}}}
	input := func(message string) DispatchInput {
		return DispatchInput{Message: message, Projects: []string{"my-widget", "glim"}, Agents: named,
			Conversation: []Line{{"person", "fix the titles"}, {"titles", "Done. Should I also do the tabs?"}}}
	}
	complete := func(answer string) func(context.Context, []llm.Message, map[string]any) (string, error) {
		return func(context.Context, []llm.Message, map[string]any) (string, error) { return answer, nil }
	}
	cases := []struct{ message, model, action, target string }{
		{"add a dark mode toggle to my-widget", `{"action":"answer","agent":null,"project":null}`, "code", "my-widget"},
		{"what's the weather tomorrow?", `{"action":"answer","agent":null,"project":null}`, "delegate", ""},
		{"yes, the tabs too", `{"action":"reply_to_agent","agent":null,"project":null}`, "reply_to_agent", "titles"},
		{"how's it going", `{"action":"reply_to_agent","agent":"closed","project":null}`, "status", "closed"},
		{"hm", `not json`, "answer", ""},
	}
	for _, c := range cases {
		result := Dispatch(context.Background(), input(c.message), complete(c.model))
		got := result.Decision
		target := ""
		if got.Project != nil {
			target = *got.Project
		}
		if got.Agent != nil {
			target = *got.Agent
		}
		if got.Action != c.action || target != c.target {
			t.Errorf("%q: got %s %q, want %s %q (guards %v)", c.message, got.Action, target, c.action, c.target, result.Guards)
		}
	}
}

func TestApprovalReply(t *testing.T) {
	for text, want := range map[string]string{"yes": "allow", "Nej, vänta": "deny:vänta", "ok so what about the other branch we talked about yesterday": "", "sure thing": "allow"} {
		allow, message, ok := ApprovalReply(text)
		got := ""
		if ok && allow {
			got = "allow"
		} else if ok {
			got = "deny:" + message
		}
		if got != want {
			t.Errorf("%q: got %q, want %q", text, got, want)
		}
	}
}
