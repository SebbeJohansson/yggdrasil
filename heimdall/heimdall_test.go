package heimdall

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/part"
)

// fakeCheck finds what it is given.
type fakeCheck struct {
	name     string
	findings []Finding
	err      error
	skipped  string
}

func (f *fakeCheck) Name() string { return f.name }
func (f *fakeCheck) Run(context.Context, *Env) (Result, error) {
	return Result{Findings: append([]Finding{}, f.findings...), Skipped: f.skipped}, f.err
}

// fakeGjallar records what is raised and resolved.
type fakeGjallar struct {
	mu       sync.Mutex
	raised   map[string]alert.Raise
	resolved []string
	held     []alert.Raise
	bodies   []string
	server   *httptest.Server
}

func newFakeGjallar(t *testing.T) *fakeGjallar {
	g := &fakeGjallar{raised: map[string]alert.Raise{}}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		g.bodies = append(g.bodies, string(body))
		switch r.URL.Path {
		case "/v1/alerts":
			var raise alert.Raise
			_ = json.Unmarshal(body, &raise)
			g.raised[raise.Key] = raise
			w.WriteHeader(http.StatusCreated)
		case "/v1/alerts/resolve":
			var key struct{ Key string }
			_ = json.Unmarshal(body, &key)
			delete(g.raised, key.Key)
			g.resolved = append(g.resolved, key.Key)
		case "/v1/alerts/mine":
			_ = json.NewEncoder(w).Encode(map[string]any{"alerts": g.held})
		}
	}))
	t.Cleanup(g.server.Close)
	return g
}

func (g *fakeGjallar) client() *alert.Client { return alert.NewClient(g.server.URL, "t") }

func run(h *Heimdall) { h.Run(context.Background()) }

func TestFindingsPersistAndResolveAndAreKept30Days(t *testing.T) {
	check := &fakeCheck{name: "deps", findings: []Finding{
		{ID: "a", Severity: Low, Title: "A", Detail: "a"},
		{ID: "b", Severity: High, Title: "B", Detail: "b"},
	}}
	state := filepath.Join(t.TempDir(), "heimdall.json")
	h := New([]Check{check}, nil, DefaultSettings(nil))
	h.StatePath = state
	day1 := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return day1 }
	run(h)
	report := h.Report(false)
	if len(report.Findings) != 2 || report.Findings[0].ID != "b" || report.CheckedAt == nil {
		t.Fatalf("high first: %+v", report)
	}
	day2 := day1.Add(24 * time.Hour)
	h.now = func() time.Time { return day2 }
	check.findings = check.findings[:1]
	run(h)
	active, resolved := h.Report(false), h.Report(true)
	if len(active.Findings) != 1 || !active.Findings[0].FirstSeen.Equal(day1) || !active.Findings[0].LastSeen.Equal(day2) {
		t.Fatalf("active: %+v", active.Findings)
	}
	if len(resolved.Findings) != 1 || resolved.Findings[0].ID != "b" || !resolved.Findings[0].ResolvedAt.Equal(day2) {
		t.Fatalf("resolved: %+v", resolved.Findings)
	}

	// Kept across a restart.
	again := New([]Check{check}, nil, DefaultSettings(nil))
	again.StatePath = state
	if err := again.Load(); err != nil {
		t.Fatal(err)
	}
	if len(again.Report(true).Findings) != 1 || len(again.Report(false).Findings) != 1 {
		t.Fatal("not kept")
	}
	// A check that fails leaves its findings alone.
	again.now = func() time.Time { return day2.Add(time.Hour) }
	check.err = io.ErrUnexpectedEOF
	run(again)
	if r := again.Report(false); len(r.Findings) != 1 || r.Checks[0].OK || r.Checks[0].Error == "" {
		t.Fatalf("failed check: %+v", r)
	}
	// After 30 days the resolved one is gone.
	check.err = nil
	again.now = func() time.Time { return day2.Add(31 * 24 * time.Hour) }
	run(again)
	if len(again.Report(true).Findings) != 0 {
		t.Fatal("resolved finding kept past 30 days")
	}
}

func TestHighFindingsRaiseAlertsAndResolveWhenGone(t *testing.T) {
	g := newFakeGjallar(t)
	g.held = []alert.Raise{{Key: "listening:0.0.0.0:9999", Severity: alert.Warning}}
	check := &fakeCheck{name: "secrets", findings: []Finding{
		{ID: "pushed", Severity: High, Critical: true, Title: "pushed key", Detail: "d"},
		{ID: "local", Severity: High, Title: "local key", Detail: "d"},
		{ID: "quiet", Severity: Medium, Title: "untracked .env", Detail: "d"},
	}}
	h := New([]Check{check}, g.client(), DefaultSettings(nil))
	run(h)
	g.mu.Lock()
	// One alert for the check, critical while any finding in it is.
	if len(g.raised) != 1 || g.raised["secrets:summary"].Severity != alert.Critical || !strings.Contains(g.raised["secrets:summary"].Title, "2 secrets findings") {
		t.Fatalf("raised: %+v", g.raised)
	}
	if !slices.Contains(g.resolved, "listening:0.0.0.0:9999") {
		t.Fatalf("an alert held from before a restart is resolved: %v", g.resolved)
	}
	g.mu.Unlock()
	check.findings = check.findings[1:]
	run(h)
	g.mu.Lock()
	defer g.mu.Unlock()
	if got := g.raised["secrets:summary"]; len(g.raised) != 1 || got.Severity != alert.Warning || got.Title != "Heimdall: local key" {
		t.Fatalf("the summary did not follow the findings: %+v", g.raised)
	}
	check.findings = nil
	g.mu.Unlock()
	run(h)
	g.mu.Lock()
	if _, still := g.raised["secrets:summary"]; still {
		t.Fatalf("the check's alert stayed with nothing found: %+v", g.raised)
	}
}

func TestTickRunsOnceANightAtTheSetTime(t *testing.T) {
	check := &fakeCheck{name: "deps"}
	h := New([]Check{check}, nil, DefaultSettings(nil))
	h.Zone = time.UTC
	now := time.Date(2026, 11, 2, 2, 59, 0, 0, time.UTC)
	h.now = func() time.Time { return now }
	h.Tick(context.Background())
	h.Wait()
	if h.Report(false).CheckedAt != nil {
		t.Fatal("ran before 03:00")
	}
	now = now.Add(2 * time.Minute)
	h.Tick(context.Background())
	h.Wait()
	first := h.Report(false).CheckedAt
	if first == nil {
		t.Fatal("did not run at 03:00")
	}
	now = now.Add(time.Hour)
	h.Tick(context.Background())
	h.Wait()
	if !h.Report(false).CheckedAt.Equal(*first) {
		t.Fatal("ran twice in one night")
	}
}

func send(t *testing.T, p *part.Part, token, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, request)
	var answer map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &answer)
	return recorder.Code, answer
}

func TestOwnerOnlyRoutesAndSettingsThatLast(t *testing.T) {
	state := filepath.Join(t.TempDir(), "heimdall.json")
	h := New([]Check{&fakeCheck{name: "deps"}}, nil, DefaultSettings([]string{"/home/me/projects/yggdrasil"}))
	h.StatePath = state
	p := part.New(Name, contracttest.Tokens(), nil)
	h.Mount(context.Background(), p)
	if code, _ := send(t, p, contracttest.Sender, "POST", "/v1/report/run", nil); code != http.StatusForbidden {
		t.Fatalf("a part may not start a run: %d", code)
	}
	if code, _ := send(t, p, contracttest.Sender, "PATCH", "/v1/settings", map[string]any{"at": "04:00"}); code != http.StatusForbidden {
		t.Fatalf("a part may not change settings: %d", code)
	}
	for _, bad := range []map[string]any{{"at": "25:00"}, {"tokenAgeDays": 0}, {"repos": []string{"relative/path"}}} {
		if code, _ := send(t, p, contracttest.Owner, "PATCH", "/v1/settings", bad); code != http.StatusBadRequest {
			t.Fatalf("%v: %d", bad, code)
		}
	}
	code, answer := send(t, p, contracttest.Owner, "PATCH", "/v1/settings", map[string]any{"at": "04:30", "tokenAgeDays": 90})
	if code != http.StatusOK || answer["at"] != "04:30" || answer["tokenAgeDays"] != float64(90) || len(answer["repos"].([]any)) != 1 {
		t.Fatalf("%d %v", code, answer)
	}
	if code, answer := send(t, p, contracttest.Owner, "POST", "/v1/report/run", nil); code != http.StatusAccepted || answer["running"] != true {
		t.Fatalf("%d %v", code, answer)
	}
	h.Wait()
	if code, _ := send(t, p, contracttest.Sender, "GET", "/v1/report?state=gone", nil); code != http.StatusBadRequest {
		t.Fatalf("bad state: %d", code)
	}
	again := New(nil, nil, DefaultSettings(nil))
	again.StatePath = state
	if err := again.Load(); err != nil || again.Settings().At != "04:30" {
		t.Fatalf("settings not kept: %v %+v", err, again.Settings())
	}
}

func TestMeetsItsContract(t *testing.T) {
	h := New([]Check{&fakeCheck{name: "deps", findings: []Finding{{ID: "x", Severity: Medium, Title: "x", Detail: "x", Repo: "r", Where: "go.mod", Fix: "f"}}}, &fakeCheck{name: "scopes", skipped: "no token"}}, nil, DefaultSettings([]string{"/tmp/r"}))
	run(h)
	p := part.New(Name, contracttest.Tokens(), nil)
	h.Mount(context.Background(), p)
	contracttest.Run(t, p)
}

// --- deps ---------------------------------------------------------------------

func TestDepsAsksOSVWithOnlyNamesAndVersions(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "secret-project-name")
	write(t, repo, ".git/HEAD", "ref: refs/heads/main\n")
	write(t, repo, "go.mod", "module example.com/me\n\ngo 1.25\n\nrequire (\n\tgolang.org/x/net v0.1.0\n\tgithub.com/ok/ok v1.0.0 // indirect\n)\n")
	write(t, repo, "web/package.json", `{"name":"w"}`)
	write(t, repo, "web/yarn.lock", "# yarn lockfile v1\n\n\"left-pad@^1.0.0\", left-pad@^1.1:\n  version \"1.1.0\"\n  resolved \"https://x\"\n")
	write(t, repo, "web/node_modules/x/package.json", `{"dependencies":{"evil":"1.0.0"}}`)

	var queries []byte
	var agents []string
	osv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agents = append(agents, r.Header.Get("User-Agent"))
		switch r.URL.Path {
		case "/v1/querybatch":
			queries, _ = io.ReadAll(r.Body)
			var body struct {
				Queries []struct {
					Package struct{ Name string } `json:"package"`
				} `json:"queries"`
			}
			_ = json.Unmarshal(queries, &body)
			results := []map[string]any{}
			for _, q := range body.Queries {
				if q.Package.Name == "golang.org/x/net" {
					results = append(results, map[string]any{"vulns": []map[string]string{{"id": "GO-2024-0001"}}})
				} else if q.Package.Name == "left-pad" {
					results = append(results, map[string]any{"vulns": []map[string]string{{"id": "GHSA-aaaa"}}})
				} else {
					results = append(results, map[string]any{})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
		case "/v1/vulns/GO-2024-0001":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "GO-2024-0001", "summary": "Bad HTML parsing", "affected": []any{
				map[string]any{"package": map[string]string{"name": "golang.org/x/net", "ecosystem": "Go"}, "ranges": []any{map[string]any{"events": []any{map[string]string{"introduced": "0"}, map[string]string{"fixed": "0.0.9"}, map[string]string{"fixed": "0.23.0"}}}}},
			}})
		case "/v1/vulns/GHSA-aaaa":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "GHSA-aaaa", "summary": "Pads to the left", "database_specific": map[string]string{"severity": "CRITICAL"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer osv.Close()

	result, err := NewDeps(osv.URL).Run(context.Background(), &Env{Repos: []Repo{{Name: "secret-project-name", Path: repo}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"secret-project-name", root, "example.com/me", "evil"} {
		if bytes.Contains(queries, []byte(leak)) {
			t.Errorf("osv.dev was sent %q: %s", leak, queries)
		}
	}
	for _, agent := range agents {
		if agent != UserAgent {
			t.Errorf("User-Agent %q", agent)
		}
	}
	if len(result.Findings) != 2 {
		t.Fatalf("%+v", result.Findings)
	}
	byName := map[string]Finding{}
	for _, f := range result.Findings {
		byName[f.Where] = f
	}
	net := byName["go.mod"]
	if net.Severity != Medium || !strings.Contains(net.Fix, "0.23.0") || !strings.Contains(net.Detail, "GO-2024-0001") || net.Repo != "secret-project-name" {
		t.Errorf("go finding: %+v", net)
	}
	if pad := byName["web/yarn.lock"]; pad.Severity != High || !strings.Contains(pad.Title, "left-pad 1.1.0") {
		t.Errorf("npm finding: %+v", pad)
	}
}

func TestLockfileParsers(t *testing.T) {
	berry := "__metadata:\n  version: 6\n\n\"@babel/core@npm:^7.0.0, @babel/core@npm:^7.1.0\":\n  version: 7.2.0\n  resolution: \"@babel/core@npm:7.2.0\"\n\n\"app@workspace:.\":\n  version: 0.0.0-use.local\n"
	got := ParseYarnLock([]byte(berry))
	if len(got) != 1 || got[0].Name != "@babel/core" || got[0].Version != "7.2.0" {
		t.Errorf("berry: %+v", got)
	}
	classic := "\"@vue/x@^3\":\n  version \"3.4.1\"\n  dependencies:\n    foo \"^1\"\n\nfoo@^1:\n  version \"1.0.2\"\n"
	got = ParseYarnLock([]byte(classic))
	if len(got) != 2 || got[0].Name != "@vue/x" || got[1].Name != "foo" || got[1].Version != "1.0.2" {
		t.Errorf("classic: %+v", got)
	}
	v3 := `{"lockfileVersion":3,"packages":{"":{"name":"me"},"node_modules/a":{"version":"1.0.0"},"node_modules/a/node_modules/@s/b":{"version":"2.0.0"},"node_modules/l":{"link":true}}}`
	got = ParsePackageLock([]byte(v3))
	if len(got) != 2 || got[0].Name != "@s/b" || got[1].Name != "a" {
		t.Errorf("lock v3: %+v", got)
	}
	v1 := `{"lockfileVersion":1,"dependencies":{"a":{"version":"1.0.0","dependencies":{"b":{"version":"2.0.0"}}}}}`
	if got = ParsePackageLock([]byte(v1)); len(got) != 2 {
		t.Errorf("lock v1: %+v", got)
	}
	mod := "module m\nrequire x.org/a v1.2.3\nrequire (\n\ty.org/b v0.1.0 // indirect\n\tz.org/local v0.0.0\n)\nreplace z.org/local => ../local\n"
	got = ParseGoMod([]byte(mod))
	if len(got) != 2 || got[0].Name != "x.org/a" || got[1].Version != "v0.1.0" {
		t.Errorf("go.mod: %+v", got)
	}
	pkg := `{"dependencies":{"exact":"1.2.3","range":"^1.0.0"},"devDependencies":{"dev":"0.1.0-beta.1"}}`
	if got = ParsePackageJSON([]byte(pkg)); len(got) != 2 {
		t.Errorf("package.json: %+v", got)
	}
	if CompareVersions("v1.2.10", "1.2.9") <= 0 || CompareVersions("0.23.0", "0.1.0") <= 0 {
		t.Error("versions")
	}
	if ParseYarnLock(nil) != nil || ParsePackageLock([]byte("{")) != nil {
		t.Error("tolerant")
	}
}

// --- listening ------------------------------------------------------------------

func TestListeningFindsWhatIsReachableFromOutside(t *testing.T) {
	proc := t.TempDir()
	tcp := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:2246 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 101 1 0000000000000000 100 0 0 10 0
   1: 00000000:2246 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 102 1 0000000000000000 100 0 0 10 0
   2: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 103 1 0000000000000000 100 0 0 10 0
   3: 0100A8C0:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 104 1 0000000000000000 100 0 0 10 0
   4: 00000000:1F91 0100007F:9C40 01 00000000:00000000 00:00000000 00000000  1000        0 105 1 0000000000000000 100 0 0 10 0
`
	tcp6 := `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:1F92 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 201 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000000000000:22A8 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 202 1 0000000000000000 100 0 0 10 0
`
	write(t, proc, "net/tcp", tcp)
	write(t, proc, "net/tcp6", tcp6)
	write(t, proc, "4242/cmdline", "/home/me/.local/bin/ygg\x00gjallar\x00")
	if err := os.MkdirAll(filepath.Join(proc, "4242/fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("socket:[102]", filepath.Join(proc, "4242/fd/3")); err != nil {
		t.Fatal(err)
	}
	result, err := NewListening(proc).Run(context.Background(), &Env{})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]Finding{}
	for _, f := range result.Findings {
		found[f.Where] = f
	}
	if len(found) != 4 {
		t.Fatalf("%+v", found)
	}
	if f := found["0.0.0.0:8774"]; f.Severity != High || !f.Critical || !strings.Contains(f.Title, "ygg gjallar") {
		t.Errorf("Gjallar beyond loopback: %+v", f)
	}
	if f := found["0.0.0.0:8080"]; f.Severity != Medium || f.Critical || !strings.Contains(f.Detail, "can't see") {
		t.Errorf("wildcard: %+v", f)
	}
	if f := found["192.168.0.1:22"]; f.Severity != Low {
		t.Errorf("one address: %+v", f)
	}
	if f := found["[::]:8872"]; f.Severity != High {
		t.Errorf("Loki's proxy for Sindri on every v6 address: %+v", f)
	}
	if _, ok := found["[::1]:8082"]; ok {
		t.Error("::1 is loopback")
	}
	if _, err := NewListening(t.TempDir()).Run(context.Background(), &Env{}); err == nil {
		t.Error("no /proc/net/tcp is an error")
	}
}

// --- secrets --------------------------------------------------------------------

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fakeKey is built here so no secret-shaped text sits in this file.
func fakeKey() string { return "gh" + "p_" + "Zq81xV0kLm3PwR7sTn2YbC6dF9gH4jK5aE1u4f2a" }

func TestSecretsAreFoundMaskedAndStayUntilGoneFromHistory(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	write(t, repo, "README.md", "hello\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "first")
	key := fakeKey()
	assigned := "Rk9vQmFyQmF6UXV4MTIzNDU2Nzg5MGFiY2RlZg"
	write(t, repo, "config.go", "package x\n\nconst token = \""+key+"\"\nvar API_SECRET = \""+assigned+"\"\nvar apiKey = cfg.Secrets.Lookup\n")
	write(t, repo, ".gitignore", "ignored.env\n")
	write(t, repo, "ignored.env", "x")
	write(t, repo, ".env.example", "x")
	write(t, repo, ".env", "x")
	git(t, repo, "add", "config.go", ".gitignore")
	git(t, repo, "commit", "-qm", "oops")

	g := newFakeGjallar(t)
	check := &Secrets{Git: "git"}
	h := New([]Check{check}, g.client(), DefaultSettings([]string{repo}))
	run(h)
	report := h.Report(false)
	var commitFindings []Finding
	var untracked []string
	for _, f := range report.Findings {
		if f.Commit != "" {
			commitFindings = append(commitFindings, f)
		} else {
			untracked = append(untracked, f.Where)
		}
	}
	if len(commitFindings) != 2 || !slices.Equal(untracked, []string{".env"}) {
		t.Fatalf("findings: %+v", report.Findings)
	}
	gh := commitFindings[0]
	if !strings.Contains(gh.Title, "GitHub token") {
		gh = commitFindings[1]
	}
	if gh.Where != "config.go:3" || gh.Severity != High || gh.Critical || !strings.Contains(gh.Title, "ghp_…4f2a") {
		t.Errorf("GitHub token: %+v", gh)
	}

	// No value anywhere: the report, the state file, the alerts.
	assertNoSecret := func(where string, text []byte) {
		t.Helper()
		for _, secret := range []string{key, assigned, key[4 : len(key)-4], assigned[4 : len(assigned)-4]} {
			if bytes.Contains(text, []byte(secret)) {
				t.Errorf("%s holds a secret's value", where)
			}
		}
	}
	text, _ := json.Marshal(report)
	assertNoSecret("the report", text)
	g.mu.Lock()
	assertNoSecret("the alerts", []byte(strings.Join(g.bodies, "\n")))
	if g.raised["secrets:summary"].Severity != alert.Warning {
		t.Errorf("an unpushed secret is a warning: %+v", g.raised)
	}
	g.mu.Unlock()
	h.StatePath = filepath.Join(t.TempDir(), "state.json")
	h.save()
	text, _ = os.ReadFile(h.StatePath)
	assertNoSecret("the state", text)

	// Pushed: critical. The next run reads no old commits but keeps it.
	git(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	run(h)
	if r := h.Report(false); len(r.Findings) != 3 {
		t.Fatalf("kept: %+v", r.Findings)
	}
	g.mu.Lock()
	if g.raised["secrets:summary"].Severity != alert.Critical {
		t.Errorf("a pushed secret is critical: %+v", g.raised["secrets:summary"])
	}
	g.mu.Unlock()

	// Gone from history: resolved.
	git(t, repo, "reset", "-q", "--hard", "HEAD~1")
	git(t, repo, "update-ref", "-d", "refs/remotes/origin/main")
	run(h)
	if r := h.Report(true); len(r.Findings) != 2 {
		t.Fatalf("resolved once gone from history: %+v", r.Findings)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.raised) != 0 {
		t.Errorf("alerts left: %+v", g.raised)
	}
}

func TestScanLine(t *testing.T) {
	for _, c := range []struct {
		line string
		kind string
	}{
		{"-----BEGIN OPENSSH PRIVATE KEY-----", "private key"}, // heimdall:ignore, a fake
		{"SLACK=" + "xox" + "b-1234567890-abcdefghijkl", "Slack token"},
		{"key: " + "sk-" + "ant-api03-" + strings.Repeat("aB3", 12), "Anthropic key"},
		{"OPENAI=" + "sk-" + "proj-" + strings.Repeat("Zx9", 12), "OpenAI key"},
		{"aws = " + "AKIA" + "IOSFODNN7EXAMPLE", "AWS access key"},
		{"DB_PASSWORD_TOKEN: 'q8Wm2Lx0Pz7Nv5Rt3Ys1Ua9Kc'", "secret assignment"}, // heimdall:ignore, a fake
		{"export const TOKEN_KEY = 'auth_token_storage_key_name'", ""},
		{"apiKey = process.env.OPENAI_API_KEY_FROM_ENV", ""},
		{`ownerToken = "owner-token-0123456789abcdef0123456789"`, ""},
		{`keptSuggestionsKey = "calendar-suggestions-kept-for-later"`, ""},
		{"GITHUB_TOKEN=" + fakeKey() + " # " + IgnoreMarker, ""},
	} {
		got := ScanLine(c.line)
		switch {
		case c.kind == "" && len(got) != 0:
			t.Errorf("%q: found %+v", c.line, got)
		case c.kind != "" && (len(got) != 1 || got[0].Kind != c.kind):
			t.Errorf("%q: got %+v, want %s", c.line, got, c.kind)
		}
	}
	if Mask("short") != "…" || Mask(fakeKey()) != "ghp_…4f2a" {
		t.Error("mask")
	}
	for name, want := range map[string]bool{".env": true, ".env.local": true, "prod.env": true, ".env.example": false, "env.go": false, ".envrc": false} {
		if EnvLike(name) != want {
			t.Errorf("EnvLike(%q)", name)
		}
	}
}

// --- files ----------------------------------------------------------------------

func TestFilesOthersCanReadAndOldTokens(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, ".config/yggdrasil")
	write(t, config, "owner.token", "x")
	write(t, config, "gjallar.env", "GJALLAR_TOKENS=owner=abc\n")
	write(t, config, "parts.env", "#YGG_NORN_URL=x\n")
	write(t, config, "norn.env", "NORN_TOKENS=owner=abc\n")
	must(t, os.Chmod(config, 0o700))
	must(t, os.Chmod(filepath.Join(config, "owner.token"), 0o600))
	must(t, os.Chmod(filepath.Join(config, "gjallar.env"), 0o644))
	must(t, os.Chmod(filepath.Join(config, "norn.env"), 0o640))
	must(t, os.Chmod(filepath.Join(config, "parts.env"), 0o600))
	now := time.Now()
	old := now.Add(-200 * 24 * time.Hour)
	for _, name := range []string{"owner.token", "parts.env"} {
		must(t, os.Chtimes(filepath.Join(config, name), old, old))
	}
	result, err := (&Files{Dirs: []string{config}, Home: home}).Run(context.Background(), &Env{Now: now, TokenAge: 180 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]Finding{}
	for _, f := range result.Findings {
		found[f.ID] = f
	}
	if f := found["mode:~/.config/yggdrasil/gjallar.env"]; f.Severity != High || f.Fix != "chmod 600 ~/.config/yggdrasil/gjallar.env" {
		t.Errorf("readable by all: %+v", f)
	}
	if f := found["mode:~/.config/yggdrasil/norn.env"]; f.Severity != Medium {
		t.Errorf("readable by the group: %+v", f)
	}
	if f := found["age:~/.config/yggdrasil/owner.token"]; f.Severity != Low || !strings.Contains(f.Title, "200 days") {
		t.Errorf("old token: %+v", f)
	}
	if len(found) != 3 {
		t.Errorf("parts.env holds no token, the rest are fine: %+v", found)
	}
}

// --- scopes ---------------------------------------------------------------------

func TestScopesNornHoldsButNothingUses(t *testing.T) {
	norn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/settings/widgets":
			_, _ = w.Write([]byte(`{"place":"home","hideAtHome":["gmail","outlook-calendar"],"hideAway":["gmail"]}`))
		case "/v1/microsoft":
			_, _ = w.Write([]byte(`{"state":"signed-in","scopes":{"mail":true,"calendar":true}}`))
		case "/v1/google":
			_, _ = w.Write([]byte(`{"state":"signed-in","grants":{"gmail":true,"calendar":true,"tasks":false}}`))
		}
	}))
	defer norn.Close()
	result, err := (&Scopes{Norn: call.New("norn", norn.URL, "t", time.Second)}).Run(context.Background(), &Env{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Findings) != 1 || result.Findings[0].ID != "google:gmail" || result.Findings[0].Severity != Low {
		t.Fatalf("%+v", result.Findings)
	}
	skipped, _ := (&Scopes{Norn: call.New("norn", norn.URL, "", time.Second)}).Run(context.Background(), &Env{})
	if skipped.Skipped == "" {
		t.Error("no token: skipped, and says why")
	}
}

func write(t *testing.T, dir, name, text string) {
	t.Helper()
	path := filepath.Join(dir, name)
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, []byte(text), 0o600))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestALineLongerThanTheBufferNeitherEndsTheReadNorBlocksGit(t *testing.T) {
	long := "+" + strings.Repeat("a", 2<<20) // a 2 MB minified line
	log := strings.Join([]string{
		commitMark + "c1",
		"diff --git a/app.min.js b/app.min.js",
		"+++ b/app.min.js",
		"@@ -0,0 +1,2 @@",
		long,
		"+GITHUB_TOKEN=" + fakeKey(),
		commitMark + "c2",
		"diff --git a/b.txt b/b.txt",
		"+++ b/b.txt",
		"@@ -0,0 +1 @@",
		"+SLACK=" + "xox" + "b-1234567890-abcdefghijkl",
	}, "\n") + "\n"
	hits := ParseLog(strings.NewReader(log))
	if len(hits) != 2 || hits[0].commit != "c1" || hits[0].line != 2 || hits[1].commit != "c2" {
		t.Fatalf("hits after a long line: %+v", hits)
	}
}

func TestNamesAndReferencesAreNotSecrets(t *testing.T) {
	for _, line := range []string{
		`const token = config.APP_INTEGRATION_TOKEN_VALUE`,
		`keys.move_pane_left_key = "ctrl_shift_left_arrow_key"`,
		`clientSecret: env.STORYBLOK_CLIENT_SECRET_KEY`,
		`authSecretKey = "authSecretKeyForTheSessionCookie"`,
	} {
		if got := ScanLine(line); len(got) != 0 {
			t.Errorf("%q: found %+v", line, got)
		}
	}
	if got := ScanLine(`API_TOKEN = "q8Wm2Lx0Pz7Nv5Rt3Ys1Ua9Kc"`); len(got) != 1 { // heimdall:ignore, a fake
		t.Errorf("a real-looking token was missed: %+v", got)
	}
}
