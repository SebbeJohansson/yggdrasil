package dellingr

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
	"yggdrasil/internal/call"
	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/part"
)

func TestWordsBecomeCron(t *testing.T) {
	for words, want := range map[string]string{
		"every weekday at 8":           "0 8 * * 1-5",
		"every day at 07:30":           "30 7 * * *",
		"every monday and friday at 9": "0 9 * * 1,5",
		"the 1st of each month at 10":  "0 10 1 * *",
		"every 15 minutes":             "*/15 * * * *",
		"every hour":                   "0 * * * *",
		"every friday at 4pm":          "0 16 * * 5",
		"0 8 * * 1-5":                  "0 8 * * 1-5",
		"every weekend at 10am":        "0 10 * * 0,6",
	} {
		got, err := Words(words)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", words, got, err, want)
		}
	}
	for _, bad := range []string{"sometimes", "every day at 25", "the 31st of each month at 10"} {
		if got, err := Words(bad); err == nil {
			t.Errorf("%q gave %q", bad, got)
		}
	}
}

func TestSaidAndNext(t *testing.T) {
	if got := Said("0 8 * * 1-5"); got != "Monday to Friday at 08:00" {
		t.Errorf("got %q", got)
	}
	if got := Said("0 10 1 * *"); got != "the 1st of each month at 10:00" {
		t.Errorf("got %q", got)
	}
	if got := Said("0 9 * * 1,5"); got != "Mondays and Fridays at 09:00" {
		t.Errorf("got %q", got)
	}
	stockholm, _ := time.LoadLocation("Europe/Stockholm")
	schedule, _ := ParseCron("0 8 * * 1-5")
	friday := time.Date(2026, 11, 20, 9, 0, 0, 0, stockholm)
	if next := schedule.Next(friday, stockholm); !next.Equal(time.Date(2026, 11, 23, 8, 0, 0, 0, stockholm)) {
		t.Errorf("after Friday 09:00: %v", next)
	}
	monthly, _ := ParseCron("0 10 1 * *")
	if next := monthly.Next(friday, stockholm); !next.Equal(time.Date(2026, 12, 1, 10, 0, 0, 0, stockholm)) {
		t.Errorf("monthly: %v", next)
	}
}

type fake struct {
	mu      sync.Mutex
	goals   []map[string]any
	started int
}

func newTest(t *testing.T, now *time.Time) (*Dellingr, *fake, *httptest.Server) {
	t.Helper()
	f := &fake{}
	parts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/ask":
			_ = json.NewEncoder(w).Encode(map[string]any{"answer": "Three meetings today."})
		case r.URL.Path == "/v1/goals":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.goals = append(f.goals, body)
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "g1"})
		case strings.HasSuffix(r.URL.Path, "/start"):
			f.started++
		}
	}))
	t.Cleanup(parts.Close)
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	d := New(store, call.New("norn", parts.URL, "t", time.Second), call.New("sindri", parts.URL, "t", time.Second),
		alert.NewClient("", ""), nil, time.UTC, t.Logf)
	d.now = func() time.Time { return *now }
	p := part.New(Name, contracttest.Tokens(), nil)
	d.Mount(p)
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	return d, f, server
}

func TestMeetsItsContract(t *testing.T) {
	now := time.Now()
	d, _, _ := newTest(t, &now)
	p := part.New(Name, contracttest.Tokens(), nil)
	d.Mount(p)
	contracttest.Run(t, p)
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
		_ = json.NewDecoder(response.Body).Decode(out)
	}
	return response.StatusCode
}

func TestAGoalJobDraftsOnScheduleAndASleptRunHappensOnce(t *testing.T) {
	now := time.Date(2026, 11, 30, 12, 0, 0, 0, time.UTC)
	d, f, server := newTest(t, &now)
	var job Job
	status := do(t, server, contracttest.Owner, "POST", "/v1/jobs",
		`{"name":"Update deps","cron":"0 10 1 * *","action":{"kind":"goal","title":"Update dependencies","repos":["glim"]}}`, &job)
	if status != 201 || !job.Enabled || job.Said != "the 1st of each month at 10:00" || !job.NextRunAt.Equal(time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("%d %+v", status, job)
	}
	// The machine sleeps through the 1st and wakes on the 3rd.
	now = time.Date(2026, 12, 3, 8, 0, 0, 0, time.UTC)
	if err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, _ := d.Store.runs(context.Background(), job.ID, 10)
	if len(runs) != 1 || !runs[0].Missed || runs[0].State != "done" || runs[0].GoalID != "g1" || len(f.goals) != 1 || f.started != 0 {
		t.Fatalf("runs %+v, goals %v, started %d", runs, f.goals, f.started)
	}
	stored, _ := d.Store.job(context.Background(), job.ID)
	if !stored.NextRunAt.Equal(time.Date(2027, 1, 1, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("next %v", stored.NextRunAt)
	}
}

func TestAnAgentsJobStartsDisabledAndCantAutoStart(t *testing.T) {
	now := time.Now()
	_, _, server := newTest(t, &now)
	var job Job
	if status := do(t, server, contracttest.Sender, "POST", "/v1/jobs", `{"name":"Brief","cron":"0 8 * * 1-5","action":{"kind":"ask","text":"Summarise my day"}}`, &job); status != 201 || job.Enabled {
		t.Fatalf("%d %+v", status, job)
	}
	if status := do(t, server, contracttest.Sender, "PATCH", "/v1/jobs/"+job.ID, `{"enabled":true}`, nil); status != 403 {
		t.Fatalf("the agent enabled it: %d", status)
	}
	if status := do(t, server, contracttest.Sender, "POST", "/v1/jobs", `{"name":"Go","cron":"0 8 * * *","action":{"kind":"goal","title":"x","autoStart":true}}`, nil); status != 403 {
		t.Fatalf("the agent set autoStart: %d", status)
	}
	if status := do(t, server, contracttest.Owner, "PATCH", "/v1/jobs/"+job.ID, `{"enabled":true}`, &job); status != 200 || !job.Enabled {
		t.Fatalf("%d %+v", status, job)
	}
}
