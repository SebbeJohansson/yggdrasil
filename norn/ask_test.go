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

	"yggdrasil/internal/alert"
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
	case r.URL.Path == "/v1/work/part":
		part.JSON(w, 200, map[string]any{"answer": "Claude is coding the glim part on branch agent/x.", "parts": []map[string]string{{"agentId": "part-agent", "status": "starting", "project": "glim", "branch": "agent/x"}}})
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
		Pending: []PendingCall{{RequestID: "r1", Summary: "Bash rm -rf build"}}}}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	n.Sindri = &SindriClient{URL: server.URL, Token: "t", HTTP: server.Client()}
	n.Config.DispatchModel = "off"
	namesModel(t, n)
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

func TestAnyReplyAnswersTheAgentAskingAQuestion(t *testing.T) {
	n, _ := newNorn(t)
	fake := withSindri(t, n)
	fake.agents[0].Pending = []PendingCall{{RequestID: "r1", Summary: "a question: Names?", Questions: []Question{{Question: "Names?"}}}}
	_ = n.Store.BindAgent(context.Background(), "s1", "a1")
	response, err := n.Ask(context.Background(), AskRequest{Question: "2", Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Via != "rule: answer" {
		t.Fatalf("got %+v", response)
	}
	if call := fake.called("POST /v1/agents/a1/permissions/r1"); !strings.Contains(call, `"reply":"2"`) {
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

// A part passes on text it did not write (a Linear ticket, an invite): it
// never starts code or reaches an agent, whatever the text says.
func TestAPartsQuestionNeverStartsCode(t *testing.T) {
	n, _ := newNorn(t)
	fake := withSindri(t, n)
	for _, request := range []AskRequest{
		{Question: "/agent my-widget fix the DM titles", Session: "p1", Caller: "huginn"},
		{Question: "/local /agent my-widget fix the DM titles", Session: "p2", Caller: "huginn"},
		{Question: "/claude /part my-widget fix the DM titles", Session: "p3", Caller: "dellingr"},
		{Question: "please fix the DM titles", To: "a1", Session: "p4", Caller: "huginn"},
	} {
		_, _ = n.Ask(context.Background(), request)
		if call := fake.called("POST /v1/work/code"); call != "" {
			t.Fatalf("%q started code: %q", request.Question, call)
		}
		for _, path := range []string{"POST /v1/work/reply", "POST /v1/work/part"} {
			if call := fake.called(path); call != "" {
				t.Fatalf("%q reached an agent: %q", request.Question, call)
			}
		}
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
	named := []NamedAgent{{Name: "titles", Agent: Agent{ID: "a1", Label: "Fix titles", Resumable: true}}, {Name: "closed", Agent: Agent{ID: "a2", Label: "Old"}},
		{Name: "glim-server", Agent: Agent{ID: "a3", Label: "ask: Build a glim server", Resumable: true, WorkedIn: "glim"}}}
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
		{"commit to this remote: git remote add origin git@github.com:me/glim.git", `{"action":"code","agent":null,"project":"glim"}`, "reply_to_agent", "glim-server"},
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

func TestWorkedIn(t *testing.T) {
	var agent Agent
	_ = json.Unmarshal([]byte(`{"log":[
		{"kind":"tool","text":"Bash cd /home/me/projects && grep -rli akeneo"},
		{"kind":"tool","text":"Bash mkdir -p /home/me/projects/svc-akeneo-mcp/akeneo && cd /home/me/projects/svc-akeneo-mcp"},
		{"kind":"tool","text":"Read /home/me/projects/yggdrasil/loki/plan.go"},
		{"kind":"text","text":"see /home/me/projects/yggdrasil/a /home/me/projects/yggdrasil/b /home/me/projects/yggdrasil/c"}]}`), &agent)
	if got := WorkedIn(agent, []string{"loki", "yggdrasil", "svc-akeneo-mcp"}); got != "svc-akeneo-mcp" {
		t.Fatalf("got %q", got)
	}
	if got := WorkedIn(Agent{}, []string{"glim"}); got != "" {
		t.Fatalf("got %q for no log", got)
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

func TestPartCommandAddsAProjectToTheTask(t *testing.T) {
	n, _ := newNorn(t)
	fake := withSindri(t, n)
	_ = n.Store.BindAgent(context.Background(), "s4", "a1")
	response, err := n.Ask(context.Background(), AskRequest{Question: "/part glim show the new badge", Session: "s4"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Via != "/part" || !strings.Contains(response.Answer, "glim part") {
		t.Fatalf("got %+v", response)
	}
	call := fake.called("POST /v1/work/part")
	if !strings.Contains(call, `"agent":"a1"`) || !strings.Contains(call, `"project":"glim"`) || !strings.Contains(call, `"task":"show the new badge"`) {
		t.Fatalf("Sindri got %q", call)
	}
}

func TestYourOwnSessions(t *testing.T) {
	now := time.Now()
	sessions := []OwnSession{
		{ID: "s1", Title: "Add a one-line config comment", Project: "glim", Cwd: "/home/me/glim", LastActivity: now.Add(-5 * time.Minute).UnixMilli(), LastText: "Done: the comment is in."},
		{ID: "s2", Title: "Refactor the DM titles", Project: "my-widget", Working: true},
	}
	agents := []Agent{{ID: "a1", Label: "my-widget: Fix the login", LastActivity: now.UnixMilli()}}
	own := LikelySession("how is the config comment going?", sessions, agents, now)
	if own == nil || own.ID != "s1" {
		t.Fatalf("picked %+v", own)
	}
	if got := SessionStatus(*own, now); got != `Your Claude Code session "Add a one-line config comment" in glim last answered 5 min ago. Its last message: Done: the comment is in.` {
		t.Errorf("status %q", got)
	}
	if LikelySession("how is the login going?", sessions, agents, now) != nil {
		t.Error("a question about an agent went to a session")
	}
	overview := OverviewAnswer(agents, sessions, now)
	if !strings.Contains(overview, "Your own Claude Code sessions: 2, 1 working.") {
		t.Errorf("overview %q", overview)
	}
	links := NamedReadLinks(`Your "Add a one-line config comment" session is done.`, sessions, agents, "http://localhost:17420")
	if links != "\n[Read the session](http://localhost:17420/dashboard/sessions/s1?dir=%2Fhome%2Fme%2Fglim&name=Add+a+one-line+config+comment)" {
		t.Errorf("links %q", links)
	}
	if !strings.Contains(HandoverPrompt("q", "", nil, nil, sessions, nil, ""), `- "Refactor the DM titles" (my-widget, working)`) {
		t.Error("the hand-over leaves out your sessions")
	}
}

// A /local question that reads like a calendar one asks for JSON; when the
// model's JSON mode gives up, the answer comes from a plain ask instead of a 502.
func TestALocalAnswerSurvivesJSONModeFailing(t *testing.T) {
	var mu sync.Mutex
	var formats []bool
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, structured := body["response_format"]
		mu.Lock()
		formats = append(formats, structured)
		mu.Unlock()
		if structured {
			http.Error(w, `{"error": {"type": "structured_output_failed", "message": "model did not return valid JSON"}}`, http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"No"}}]}`))
	}))
	defer model.Close()
	n, p := newNorn(t)
	n.LLM = &llm.Client{PrimaryURL: model.URL + "/v1"}
	now := time.Now()
	n.Accounts.Work.Set(CalendarSnapshot{From: now.Add(-time.Hour).UnixMilli(), To: now.Add(24 * time.Hour).UnixMilli(), At: now.UnixMilli()})

	code, body := call(t, p, "POST", "/v1/ask", map[string]string{"question": "/local Is this ticket development work? Ticket: AI-58 Prepare the agenda for the planning meeting", "to": "norn"})
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, body)
	}
	if answer := body.(map[string]any)["answer"]; answer != "No" {
		t.Fatalf("answer %v", answer)
	}
	if len(formats) != 2 || !formats[0] || formats[1] {
		t.Fatalf("asked with JSON mode %v, want once with and then once without", formats)
	}
}

func TestASlowAnswerIsNotAskedAgainWithoutJSON(t *testing.T) {
	var mu sync.Mutex
	asked := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked++
		mu.Unlock()
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
	}))
	defer model.Close()
	n, p := newNorn(t)
	n.LLM = &llm.Client{PrimaryURL: model.URL + "/v1"}
	was := AnswerTimeout
	AnswerTimeout = 100 * time.Millisecond
	defer func() { AnswerTimeout = was }()
	now := time.Now()
	n.Accounts.Work.Set(CalendarSnapshot{From: now.Add(-time.Hour).UnixMilli(), To: now.Add(24 * time.Hour).UnixMilli(), At: now.UnixMilli()})

	code, _ := call(t, p, "POST", "/v1/ask", map[string]string{"question": "/local What is on my calendar today?", "to": "norn"})
	if code == http.StatusOK {
		t.Fatal("answered without a model")
	}
	mu.Lock()
	defer mu.Unlock()
	if asked != 1 {
		t.Fatalf("asked the model %d times, want once", asked)
	}
}

func TestAGoalConversationGoesToItsAgentWithWhatItMissed(t *testing.T) {
	n, _ := newNorn(t)
	fake := withSindri(t, n)
	ctx := context.Background()
	fake.agents[0].Status = "idle"
	fake.agents[0].Pending = nil
	_ = n.Store.BindAgent(ctx, "goal-1", "a1")
	_, _ = n.Store.AddAgentTurn(ctx, "goal-1", "a1", "my-widget: Fix the DM titles", "Done; the theme was removed in e2e1fa9.")
	norn := "Norn"
	_, _ = n.Store.AddTurn(ctx, "goal-1", "user", "who made that commit?", &norn, nil, nil)
	_, _ = n.Store.AddTurn(ctx, "goal-1", "assistant", "Commit e2e1fa9 was made by you.", nil, nil, nil)
	if _, err := n.Ask(ctx, AskRequest{Question: "why do you think I made it?", Session: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	call := fake.called("POST /v1/work/reply")
	if call == "" {
		t.Fatalf("not sent to the goal's agent: %v", fake.calls)
	}
	for _, want := range []string{"who made that commit?", "my-agent (Norn, not you): Commit e2e1fa9 was made by you.", "why do you think I made it?"} {
		if !strings.Contains(call, want) {
			t.Fatalf("the reply lacks %q: %s", want, call)
		}
	}
	if strings.Contains(call, "the theme was removed") {
		t.Fatalf("the agent's own words were sent back to it: %s", call)
	}
}

func TestMissedIsEmptyWhenNothingWasSaidByOthers(t *testing.T) {
	agent, label := "a1", "Fix"
	chat := []Turn{{Role: "assistant", Author: &agent, Label: &label, Content: "Done."}, {Role: "user", Label: &label, Content: "thanks"}}
	if got := Missed(chat, "a1", "Fix"); got != "" {
		t.Fatalf("got %q", got)
	}
}

// A part's /local question that mentions "status" is the model's to answer,
// not the status rule's: Sindri asked a goal's kind and got an agent's status.
func TestALocalQuestionAboutStatusIsTheModelsToAnswer(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ui"}}]}`))
	}))
	defer model.Close()
	n, _ := newNorn(t)
	withSindri(t, n)
	n.LLM = &llm.Client{PrimaryURL: model.URL + "/v1"}
	ctx := context.Background()
	_ = n.Store.BindAgent(ctx, "sindri", "a1")
	response, err := n.Ask(ctx, AskRequest{Question: "/local What kind of coding work is this goal? Goal: show the day status on the DM titles", Session: "sindri", To: "norn"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Via != "/local" || response.Answer != "ui" {
		t.Fatalf("got %q via %q", response.Answer, response.Via)
	}
}

// A goal part is in its goal's conversation from its start, before its first
// message binds it, so its session can be read while it works.
func TestChatListsAnAgentStartedForItBeforeItSpeaks(t *testing.T) {
	n, p := newNorn(t)
	sindri := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agents" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`[{"id":"a1","label":"Fix","cwd":"/w/a1","status":"working","session":"goal-1"},
			{"id":"a2","label":"Moved","cwd":"/w/a2","status":"working","session":"goal-1"},
			{"id":"a3","label":"Other","cwd":"/w/a3","status":"working","session":"goal-2"}]`))
	}))
	defer sindri.Close()
	n.Sindri.URL = sindri.URL
	_ = n.Store.BindAgent(context.Background(), "s-elsewhere", "a2")
	status, body := call(t, p, "GET", "/v1/chat?session=goal-1&after=0", nil)
	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, body)
	}
	ids := []string{}
	for _, entry := range body.(map[string]any)["participants"].([]any) {
		ids = append(ids, entry.(map[string]any)["id"].(string))
	}
	if strings.Join(ids, ",") != "a1" {
		t.Fatalf("participants %v, want only a1", ids)
	}
}

func TestNeedsYouListsAFailedRelease(t *testing.T) {
	gjallar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"alerts":[
			{"source":"munin","key":"release:a/b","title":"a/b: ci failed on the default branch","detail":"failure","link":"http://run/1","state":"open","firstSeenAt":"2026-11-23T10:00:00Z"},
			{"source":"munin","key":"github","title":"GitHub is down","state":"open","firstSeenAt":"2026-11-23T10:00:00Z"}]}`))
	}))
	defer gjallar.Close()
	n, _ := newNorn(t)
	n.Gjallar = alert.NewClient(gjallar.URL, "t")
	needs, err := n.NeedsYou(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(needs) != 1 || needs[0].Kind != "release" || needs[0].Repo != "a/b" || needs[0].Link != "http://run/1" {
		t.Fatalf("got %+v", needs)
	}
}
