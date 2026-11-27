package saga

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"yggdrasil/internal/call"
	"yggdrasil/internal/part"
)

const sindriToken = "saga-at-sindri-0123456789abcdef012345"

// fakeSindri is a laptop's Sindri: one Claude Code session in a herdr pane,
// one outside herdr, and the calls Sága made.
type fakeSindri struct {
	mu       sync.Mutex
	activity int64
	calls    []string
	down     bool
}

func (f *fakeSindri) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+sindriToken {
		http.Error(w, `{"error":"no"}`, http.StatusUnauthorized)
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /v1/sessions":
		json.NewEncoder(w).Encode(map[string]any{
			"claude": map[string]any{"from": "/home/me/.claude", "sessions": []map[string]any{
				{"sessionId": "s-herdr", "pid": 41, "directory": "/home/me/projects/app", "name": "fix login", "lastActivity": f.activity,
					"state": map[string]any{"phase": "turn-ended", "lastText": "Done. Want me to add a test?"}, "bridgeSessionId": "session_RC1"},
				{"sessionId": "s-term", "pid": 42, "directory": "/home/me/projects/api", "name": "api work", "lastActivity": 5,
					"state": map[string]any{"phase": "busy", "tool": "Bash"}, "permission": map[string]any{"tool": "Bash", "since": 1}},
			}},
			"herdr": map[string]any{"from": "/run/herdr.sock", "sessions": []map[string]any{
				{"paneId": "w1:p1", "kind": "claude", "status": "idle", "cwd": "/home/me/projects/app", "pids": []int{41}},
			}},
			"opencode": map[string]any{"from": "off", "sessions": nil},
		})
	case "GET /v1/cloud":
		json.NewEncoder(w).Encode(map[string]any{"sessions": []map[string]any{
			{"id": "session_C1", "at": 1, "cwd": "/home/me/projects/app", "task": "Write docs", "url": "https://claude.ai/code/session_C1"},
		}})
	case "GET /v1/projects":
		json.NewEncoder(w).Encode([]map[string]any{{"name": "app", "path": "/home/me/projects/app", "branch": "main", "docs": false}})
	case "POST /v1/sessions/herdr/w1:p1/prompt", "POST /v1/cloud/session_C1/message":
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.calls = append(f.calls, r.URL.Path+" "+body["text"])
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	case "POST /v1/sessions/herdr":
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.calls = append(f.calls, "here "+body["kind"]+" "+body["cwd"]+" "+body["prompt"]+body["resume"])
		json.NewEncoder(w).Encode(map[string]any{"agent": map[string]any{"paneId": "w2:p1"}})
	case "POST /v1/cloud":
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.calls = append(f.calls, "start "+body["cwd"]+" "+body["task"])
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{"id": "session_C2", "url": "https://claude.ai/code/session_C2"}})
	default:
		http.Error(w, `{"error":"no such route"}`, http.StatusNotFound)
	}
}

// laptop is a second machine with Sindri and Sága, and no Gjallar.
func laptop(t *testing.T, f *fixture, driveFrom ...string) (*Saga, *fakeSindri) {
	t.Helper()
	sindri := &fakeSindri{activity: 100}
	server := httptest.NewServer(sindri)
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close(); store.Close() })
	mimirURL := f.saga.Mimir.URL
	s := New(store, &Mimir{URL: mimirURL, Token: "laptop-mimir", HTTP: &http.Client{Timeout: 5 * time.Second}}, &Gjallar{Owner: call.New("gjallar", "", "", time.Second)}, "laptop")
	s.Sindri = call.New("sindri", server.URL, sindriToken, 5*time.Second)
	s.DriveFrom = driveFrom
	return s, sindri
}

func (f *fixture) laptopSent(kind string) []json.RawMessage {
	f.mimir.mu.Lock()
	defer f.mimir.mu.Unlock()
	var out []json.RawMessage
	for _, e := range f.mimir.events {
		if e.Device == "laptop" && e.Kind == kind {
			out = append(out, e.Body)
		}
	}
	return out
}

func syncOf(t *testing.T, s *Saga) {
	t.Helper()
	if err := s.Sync(t.Context()); err != nil {
		t.Fatalf("%s sync: %v", s.Device, err)
	}
}

type sessionsAnswer struct {
	Device  string       `json:"device"`
	Devices []DeviceView `json:"devices"`
	Asked   []Asked      `json:"asked"`
}

func desktopServer(t *testing.T, f *fixture) *httptest.Server {
	t.Helper()
	p := part.New(Name, map[string]string{ownerToken: part.Owner, phoneToken: "phone"}, nil)
	f.saga.Mount(p)
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	return server
}

func do(t *testing.T, server *httptest.Server, token, method, path, body string, out any) int {
	t.Helper()
	request, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if out != nil {
		json.NewDecoder(response.Body).Decode(out)
	}
	return response.StatusCode
}

func TestALaptopsSessionsShowOnTheDesktop(t *testing.T) {
	f := newFixture(t)
	l, sindri := laptop(t, f, "desktop")
	syncOf(t, l)
	if sent := f.laptopSent(kindSessions); len(sent) != 1 {
		t.Fatalf("the laptop sent %d session lists", len(sent))
	}
	// Only Claude's activity moved: nothing new to tell.
	sindri.mu.Lock()
	sindri.activity = 200
	sindri.mu.Unlock()
	syncOf(t, l)
	if sent := f.laptopSent(kindSessions); len(sent) != 1 {
		t.Fatalf("the laptop sent again for activity alone: %d", len(sent))
	}

	f.sync()
	server := desktopServer(t, f)
	var answer sessionsAnswer
	do(t, server, ownerToken, "GET", "/v1/sessions", "", &answer)
	if len(answer.Devices) != 1 || answer.Devices[0].Device != "laptop" || !answer.Devices[0].Drivable {
		t.Fatalf("devices %+v", answer.Devices)
	}
	got := answer.Devices[0].Sessions
	if len(got.Sessions) != 2 || len(got.Cloud) != 1 || len(got.Projects) != 1 || !got.Herdr {
		t.Fatalf("sessions %+v", got)
	}
	inHerdr, inTerminal := got.Sessions[0], got.Sessions[1]
	if inHerdr.ID != "s-herdr" || inHerdr.Pane != "w1:p1" || inHerdr.BridgeSessionID != "session_RC1" || inHerdr.LastText == "" {
		t.Fatalf("the herdr session %+v", inHerdr)
	}
	if inTerminal.Pane != "" || inTerminal.Permission != "Bash" {
		t.Fatalf("the terminal session %+v", inTerminal)
	}
}

func TestAPromptFromTheDesktopIsTypedOnTheLaptop(t *testing.T) {
	f := newFixture(t)
	l, sindri := laptop(t, f, "desktop")
	syncOf(t, l)
	f.sync()
	server := desktopServer(t, f)

	var asked struct{ Asked Asked }
	if status := do(t, server, ownerToken, "POST", "/v1/sessions/laptop/requests", `{"kind":"session.prompt","session":"s-herdr","text":"yes, add the test"}`, &asked); status != 202 {
		t.Fatalf("asking answered %d", status)
	}
	if status := do(t, server, ownerToken, "POST", "/v1/sessions/laptop/requests", `{"kind":"cloud.message","session":"session_C1","text":"also the README"}`, nil); status != 202 {
		t.Fatalf("asking answered %d", status)
	}
	if status := do(t, server, phoneToken, "POST", "/v1/sessions/laptop/requests", `{"kind":"session.prompt","session":"s-herdr","text":"x"}`, nil); status != 403 {
		t.Fatalf("a sender asking answered %d", status)
	}
	if status := do(t, server, ownerToken, "POST", "/v1/sessions/laptop/requests", `{"kind":"session.prompt","session":"s-herdr"}`, nil); status != 400 {
		t.Fatalf("a prompt with no text answered %d", status)
	}
	f.sync()

	syncOf(t, l)
	if err := l.RunRequests(t.Context()); err != nil {
		t.Fatal(err)
	}
	sindri.mu.Lock()
	calls := append([]string{}, sindri.calls...)
	sindri.mu.Unlock()
	if len(calls) != 2 || calls[0] != "/v1/sessions/herdr/w1:p1/prompt yes, add the test" || calls[1] != "/v1/cloud/session_C1/message also the README" {
		t.Fatalf("Sindri was asked %v", calls)
	}
	syncOf(t, l)

	// Carried out once, even when the laptop reads the requests again.
	if err := l.Store.SetMark(t.Context(), "read", 0); err != nil {
		t.Fatal(err)
	}
	syncOf(t, l)
	if err := l.RunRequests(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(sindri.calls) != 2 {
		t.Fatalf("carried out again: %v", sindri.calls)
	}

	f.sync()
	var answer sessionsAnswer
	do(t, server, ownerToken, "GET", "/v1/sessions", "", &answer)
	if len(answer.Asked) != 2 {
		t.Fatalf("asked %+v", answer.Asked)
	}
	for _, a := range answer.Asked {
		if a.State != "done" {
			t.Fatalf("not done: %+v", a)
		}
	}
}

func TestALaptopTakesRequestsOnlyFromWhomItTrusts(t *testing.T) {
	f := newFixture(t)
	l, sindri := laptop(t, f) // SAGA_DRIVE_FROM empty: look only
	syncOf(t, l)
	f.sync()
	server := desktopServer(t, f)
	var answer sessionsAnswer
	do(t, server, ownerToken, "GET", "/v1/sessions", "", &answer)
	if answer.Devices[0].Drivable {
		t.Fatal("a look-only laptop says it is drivable")
	}
	do(t, server, ownerToken, "POST", "/v1/sessions/laptop/requests", `{"kind":"session.prompt","session":"s-herdr","text":"rm -rf"}`, nil)
	f.sync()
	syncOf(t, l)
	if err := l.RunRequests(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(sindri.calls) != 0 {
		t.Fatalf("carried out %v", sindri.calls)
	}
	syncOf(t, l)
	f.sync()
	do(t, server, ownerToken, "GET", "/v1/sessions", "", &answer)
	if answer.Asked[0].State != "failed" || !strings.Contains(answer.Asked[0].Error, "SAGA_DRIVE_FROM") {
		t.Fatalf("asked %+v", answer.Asked[0])
	}
}

func TestASessionOutsideHerdrCantBeTypedInto(t *testing.T) {
	f := newFixture(t)
	l, sindri := laptop(t, f, "desktop")
	syncOf(t, l)
	f.sync()
	server := desktopServer(t, f)
	do(t, server, ownerToken, "POST", "/v1/sessions/laptop/requests", `{"kind":"session.prompt","session":"s-term","text":"go on"}`, nil)
	f.sync()
	syncOf(t, l)
	if err := l.RunRequests(t.Context()); err != nil {
		t.Fatal(err)
	}
	syncOf(t, l)
	f.sync()
	var answer sessionsAnswer
	do(t, server, ownerToken, "GET", "/v1/sessions", "", &answer)
	if len(sindri.calls) != 0 || answer.Asked[0].State != "failed" || !strings.Contains(answer.Asked[0].Error, "Remote Control") {
		t.Fatalf("asked %+v, calls %v", answer.Asked[0], sindri.calls)
	}
}

func TestASessionStartsOnTheLaptopItself(t *testing.T) {
	f := newFixture(t)
	l, sindri := laptop(t, f, "desktop")
	syncOf(t, l)
	f.sync()
	server := desktopServer(t, f)
	if status := do(t, server, ownerToken, "POST", "/v1/sessions/laptop/requests", `{"kind":"session.start","cwd":"/home/me/projects/app"}`, nil); status != 400 {
		t.Fatalf("a start with no task answered %d", status)
	}
	if status := do(t, server, ownerToken, "POST", "/v1/sessions/laptop/requests", `{"kind":"session.start","cwd":"/home/me/projects/app","task":"Fix the scroll bug"}`, nil); status != 202 {
		t.Fatalf("asking answered %d", status)
	}
	f.sync()
	syncOf(t, l)
	if err := l.RunRequests(t.Context()); err != nil {
		t.Fatal(err)
	}
	sindri.mu.Lock()
	calls := append([]string{}, sindri.calls...)
	sindri.mu.Unlock()
	if len(calls) != 1 || calls[0] != "here claude /home/me/projects/app Fix the scroll bug" {
		t.Fatalf("Sindri was asked %v", calls)
	}
}

func TestAnOldRequestIsNotCarriedOut(t *testing.T) {
	f := newFixture(t)
	l, sindri := laptop(t, f, "desktop")
	syncOf(t, l)
	f.sync()
	server := desktopServer(t, f)
	do(t, server, ownerToken, "POST", "/v1/sessions/laptop/requests", `{"kind":"cloud.start","cwd":"/home/me/projects/app","task":"Write docs"}`, nil)
	f.sync()
	syncOf(t, l)
	l.now = func() time.Time { return time.Now().Add(time.Hour) }
	if err := l.RunRequests(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(sindri.calls) != 0 {
		t.Fatalf("carried out %v", sindri.calls)
	}
}

func TestARequestWaitsWhileSindriIsDown(t *testing.T) {
	f := newFixture(t)
	l, sindri := laptop(t, f, "desktop")
	syncOf(t, l)
	f.sync()
	server := desktopServer(t, f)
	do(t, server, ownerToken, "POST", "/v1/sessions/laptop/requests", `{"kind":"cloud.start","cwd":"/home/me/projects/app","task":"Write docs"}`, nil)
	f.sync()
	syncOf(t, l)
	sindri.mu.Lock()
	sindri.down = true
	sindri.mu.Unlock()
	// Down answers 503: a definite answer from Sindri, so it is answered as failed.
	// A Sindri that is not there at all leaves the request to try again.
	l.Sindri.URL = "http://127.0.0.1:1"
	if err := l.RunRequests(t.Context()); err == nil {
		t.Fatal("no error while Sindri is unreachable")
	}
	waiting, _ := l.Store.waitingRequests(t.Context())
	if len(waiting) != 1 {
		t.Fatalf("waiting %d", len(waiting))
	}
}

func TestStatusSaysWhatIsSynced(t *testing.T) {
	f := newFixture(t)
	l, _ := laptop(t, f, "desktop", "phone")
	status, err := l.currentStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status.Alerts || !status.Sessions || len(status.DriveFrom) != 2 {
		t.Fatalf("status %+v", status)
	}
}
