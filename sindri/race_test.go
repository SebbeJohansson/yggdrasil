package sindri

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/part"
)

func TestKinds(t *testing.T) {
	for text, want := range map[string]string{
		"Fix the broken login":          "bugfix",
		"Add a Race toggle to the page": "ui",
		"Write tests for the merge":     "tests",
		"Refactor the store":            "refactor",
		"Update the README":             "docs",
		"Add a route for stats":         "backend",
		"Make it nicer":                 "other",
	} {
		if got := KeywordKind(text, ""); got != want {
			t.Errorf("%q: got %s, want %s", text, got, want)
		}
	}
	if ReadKind("**UI**.") != "ui" || ReadKind("I think backend") != "backend" || ReadKind("no idea") != "" {
		t.Errorf("ReadKind")
	}
}

type fixedKinds struct{ kind string }

func (f fixedKinds) Judge(context.Context, string, string) (string, error) { return f.kind, nil }

func TestAGoalsKindComesFromTheModelOnceItAnswers(t *testing.T) {
	w, _ := newWork(t)
	goal, _ := w.Store.AddGoal("Make it nicer", []string{"demo"}, "manual", "", "goal-1")
	if got := w.GoalKind(goal); got != "other" {
		t.Fatalf("keyword guess: got %s", got)
	}
	w.Kinds = fixedKinds{"ui"}
	deadline := time.Now().Add(5 * time.Second)
	for w.GoalKind(goal) != "ui" {
		if time.Now().After(deadline) {
			t.Fatalf("the model's kind never came")
		}
		time.Sleep(20 * time.Millisecond)
	}
	goal.Title = "Fix the broken login"
	w.Kinds = nil
	if got := w.GoalKind(goal); got != "bugfix" {
		t.Fatalf("a changed goal is judged again: got %s", got)
	}
}

// raceOn races the fake Claude against itself: "claude-b" is a second conduit on the same engine.
func raceOn(t *testing.T, w *Work) {
	t.Helper()
	saved := Conduits
	Conduits = append(append([]Conduit{}, Conduits...), Conduit{Name: "claude-b", Label: "Claude B", Engine: "claude", Up: func(*Manager) bool { return true }})
	t.Cleanup(func() { Conduits = saved })
	if err := w.Store.SetSetting("races", RaceSettings{Pair: []string{"claude", "claude-b"}, MinPicks: 5}); err != nil {
		t.Fatal(err)
	}
}

func readyRace(t *testing.T, w *Work, id int64) *GoalView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_ = w.Feed(context.Background())
		if view := must2(w.goalView(id)); view.Racing != nil && view.Racing.Ready {
			return view
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("the race never got ready: %+v", must2(w.goalView(id)).Racing)
	return nil
}

func TestTheNightShiftRacesAMarkedGoalAndThePickMergesTheWinner(t *testing.T) {
	w, _ := newWork(t)
	raceOn(t, w)
	hermodr := &told{}
	w.Hermodr, w.Zone = hermodr, time.UTC
	now := must2(time.Parse(time.DateTime, "2026-11-17 22:30:00"))
	w.Store.now = func() time.Time { return now }
	goal, _ := w.Store.AddGoal("Add a work file", []string{"demo"}, "manual", "", "goal-1")
	_ = w.Store.SetGoalNight(goal.ID, true)
	_ = w.Store.SetGoalRace(goal.ID, true)

	if err := w.RunNight(context.Background()); err != nil {
		t.Fatal(err)
	}
	view := readyRace(t, w, goal.ID)
	if len(view.Racing.Lanes) != 2 || len(view.Tasks) != 2 || view.State != "done" || view.Next.Action != "pick" {
		t.Fatalf("got %+v %+v", view.Racing, view.Next)
	}
	a, b := view.Racing.Lanes[0], view.Racing.Lanes[1]
	if a.Conduit != "claude" || b.Conduit != "claude-b" || len(a.Tasks) != 1 || len(b.Tasks) != 1 || *a.Tasks[0].Branch == *b.Tasks[0].Branch {
		t.Fatalf("lanes %+v", view.Racing.Lanes)
	}
	report := NightReport([]GoalView{*view})
	if !strings.Contains(report.Text, "Races ready to pick:\n- Add a work file (Claude Code vs Claude B)") || strings.Contains(report.Text, "Ready to review") {
		t.Fatalf("report %q", report.Text)
	}

	loser := must2(w.Agents.Get(b.Tasks[0].Agent))
	if _, err := w.PickWinner(goal.ID, "nobody", true); err == nil {
		t.Fatalf("picked a conduit that did not race")
	}
	result, err := w.PickWinner(goal.ID, "claude", true)
	if err != nil || len(result.Merged) != 1 || len(result.Discarded) != 1 || len(result.Failed) != 0 {
		t.Fatalf("got %+v %v", result, err)
	}
	if exists(loser.Worktree.Path) || loser.Worktree.branchExists() {
		t.Fatalf("the loser's worktree or branch is still there")
	}
	if out, _ := exec.Command("git", "-C", loser.Worktree.Repo, "log", "--oneline", "main").Output(); !strings.Contains(string(out), "Change work.txt") {
		t.Fatalf("the winner did not merge: %s", out)
	}
	view = must2(w.goalView(goal.ID))
	if view.Racing.Pick == nil || view.Racing.Pick.Winner != "claude" || view.Racing.Pick.Kind != "other" || !view.Racing.Lanes[0].Tasks[0].Merged {
		t.Fatalf("got %+v %+v", view.Racing.Pick, view.Racing.Lanes[0])
	}
	if _, err := w.PickWinner(goal.ID, "claude-b", true); err == nil {
		t.Fatalf("picked twice")
	}
	stats := must2(w.Stats())
	if stats.Picks != 1 || len(stats.Kinds) != 1 || stats.Kinds[0].Wins["claude"] != 1 || stats.Kinds[0].Raced["claude-b"] != 1 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestNeitherRemovesBothBranchesAndClosesTheGoal(t *testing.T) {
	w, _ := newWork(t)
	raceOn(t, w)
	goal, _ := w.Store.AddGoal("Fix the broken work file", []string{"demo"}, "manual", "", "goal-1")
	if _, _, note, err := w.StartRace(goal.ID); err != nil || note != "" {
		t.Fatalf("got %q %v", note, err)
	}
	view := readyRace(t, w, goal.ID)
	result, err := w.PickWinner(goal.ID, "neither", true)
	if err != nil || len(result.Discarded) != 2 || len(result.Merged) != 0 {
		t.Fatalf("got %+v %v", result, err)
	}
	for _, lane := range view.Racing.Lanes {
		if a := must2(w.Agents.Get(lane.Tasks[0].Agent)); exists(a.Worktree.Path) || a.Worktree.branchExists() {
			t.Fatalf("%s's branch is still there", lane.Conduit)
		}
	}
	if view := must2(w.goalView(goal.ID)); view.State != "closed" || view.Racing.Pick.Winner != "neither" || view.Racing.Pick.Kind != "bugfix" {
		t.Fatalf("got %s %+v", view.State, view.Racing.Pick)
	}
	if stats := must2(w.Stats()); stats.Kinds[0].Neither != 1 || len(stats.Kinds[0].Wins) != 0 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestARaceWithOneConduitUpRunsOnceAndSaysSo(t *testing.T) {
	w, _ := newWork(t) // OpenCode is off here, so only Claude Code is up
	goal, _ := w.Store.AddGoal("Add a work file", []string{"demo"}, "manual", "", "goal-1")
	_ = w.Store.SetGoalRace(goal.ID, true)
	started, _, note, err := w.StartRace(goal.ID)
	if err != nil || len(started) != 1 || !strings.Contains(note, "only Claude Code was available (OpenCode was not)") {
		t.Fatalf("got %+v %q %v", started, note, err)
	}
	waitFor(t, w.Agents, started[0].AgentID, func(a *Agent) bool { return a.Status == Waiting })
	view := must2(w.goalView(goal.ID))
	if view.Racing != nil || view.RaceNote != note || view.Conduit != "claude" || len(view.Tasks) != 1 {
		t.Fatalf("got %+v", view)
	}
	lines, ready, rest := raceLines([]GoalView{*view})
	if ready || len(rest) != 1 || len(lines) != 1 || !strings.Contains(lines[0], "Ran once, not raced:\n- Add a work file: only Claude Code") {
		t.Fatalf("got %q %v", lines, ready)
	}
}

func TestASuggestionNeedsEnoughPicksAndAClearWinner(t *testing.T) {
	w, _ := newWork(t)
	pick := func(goal int64, kind, winner string) {
		t.Helper()
		if _, err := w.Store.db.Exec(`INSERT INTO race_picks (at, goal_id, kind, pair, winner) VALUES (1, ?, ?, '["claude","opencode"]', ?)`, goal, kind, winner); err != nil {
			t.Fatal(err)
		}
	}
	for i, winner := range []string{"opencode", "opencode", "claude", "opencode", "neither"} {
		pick(int64(100+i), "bugfix", winner)
	}
	if s := w.Suggest("bugfix"); s != nil {
		t.Fatalf("suggested with 4 picks that have a winner: %+v", s)
	}
	pick(105, "bugfix", "claude")
	if s := w.Suggest("bugfix"); s == nil || s.Conduit != "opencode" || s.Wins != 3 || s.Picks != 5 {
		t.Fatalf("got %+v", s)
	}
	for i, winner := range []string{"claude", "opencode", "claude", "opencode", "claude", "opencode"} {
		pick(int64(200+i), "ui", winner)
	}
	if s := w.Suggest("ui"); s != nil {
		t.Fatalf("suggested on a tie: %+v", s)
	}
	_ = w.Store.SetSetting("races", RaceSettings{Pair: []string{"claude", "opencode"}, MinPicks: 7})
	if s := w.Suggest("bugfix"); s != nil {
		t.Fatalf("suggested under the setting: %+v", s)
	}
	_ = w.Store.SetSetting("races", RaceSettings{Pair: []string{"claude", "opencode"}, MinPicks: 5})

	draft, _ := w.Store.AddGoal("Fix the broken login", []string{"demo"}, "manual", "", "goal-1")
	raced, _ := w.Store.AddGoal("Fix the broken logout", []string{"demo"}, "manual", "", "goal-2")
	_ = w.Store.SetGoalRace(raced.ID, true)
	if s := must2(w.goalView(draft.ID)).Suggestion; s == nil || s.Conduit != "opencode" {
		t.Fatalf("no suggestion on the draft: %+v", s)
	}
	if s := must2(w.goalView(raced.ID)).Suggestion; s != nil {
		t.Fatalf("suggested on a race: %+v", s)
	}
	if view := must2(w.goalView(draft.ID)); view.Conduit != "" {
		t.Fatalf("chose for you: %q", view.Conduit)
	}
	stats := must2(w.Stats())
	if stats.Picks != 12 || stats.Kinds[0].Kind != "bugfix" || stats.Kinds[0].Suggested != "opencode" || stats.Kinds[1].Suggested != "" {
		t.Fatalf("stats %+v", stats)
	}
}

func TestOnlyTheOwnerMarksRacesAndPicks(t *testing.T) {
	w, _ := newWork(t)
	goal, _ := w.Store.AddGoal("A goal", []string{"demo"}, "manual", "", "goal-1")
	p := part.New(Name, contracttest.Tokens(), nil)
	(&Sindri{Agents: w.Agents, Root: w.Agents.roots[0], Work: w}).Mount(p)
	server := httptest.NewServer(p)
	defer server.Close()
	send := func(token, method, path, body string) int {
		request, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	path := fmt.Sprintf("/v1/goals/%d/race", goal.ID)
	if got := send(contracttest.Sender, "PUT", path, `{"race":true}`); got != 403 {
		t.Fatalf("a part marked a race: %d", got)
	}
	if got := send(contracttest.Owner, "PUT", path, `{"race":true}`); got != 200 || !must2(w.goalView(goal.ID)).Race {
		t.Fatalf("the owner could not: %d", got)
	}
	if got := send(contracttest.Sender, "POST", path+"/pick", `{"winner":"claude"}`); got != 403 {
		t.Fatalf("a part picked: %d", got)
	}
	if got := send(contracttest.Owner, "POST", path+"/pick", `{"winner":"claude"}`); got != 409 {
		t.Fatalf("picked a race that never ran: %d", got)
	}
	if got := send(contracttest.Sender, "PUT", fmt.Sprintf("/v1/goals/%d/conduit", goal.ID), `{"conduit":"opencode"}`); got != 200 || must2(w.goalView(goal.ID)).Conduit != "opencode" {
		t.Fatalf("could not choose a conduit: %d", got)
	}
	if got := send(contracttest.Owner, "PATCH", "/v1/settings/races", `{"pair":["claude","claude"]}`); got != 400 {
		t.Fatalf("raced a conduit against itself: %d", got)
	}
}

// fakeOpencode is an `opencode serve` with one session that commits in its directory when prompted.
type fakeOpencode struct {
	mu        sync.Mutex
	directory string
	busy      bool
	prompts   []string
}

func (f *fakeOpencode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/project":
		fmt.Fprintf(w, `[{"worktree":%q}]`, f.directory)
	case r.Method == "POST" && r.URL.Path == "/session":
		f.directory = r.URL.Query().Get("directory")
		fmt.Fprint(w, `{"id":"ses_1"}`)
	case r.URL.Path == "/session":
		fmt.Fprintf(w, `[{"id":"ses_1","directory":%q,"title":"t","time":{"created":1,"updated":2}}]`, f.directory)
	case r.URL.Path == "/session/status":
		if f.busy {
			fmt.Fprint(w, `{"ses_1":{"type":"busy"}}`)
		} else {
			fmt.Fprint(w, `{}`)
		}
	case r.URL.Path == "/permission" || r.URL.Path == "/question":
		fmt.Fprint(w, `[]`)
	case r.URL.Path == "/session/ses_1/prompt_async":
		f.prompts = append(f.prompts, r.URL.Query().Get("directory"))
		f.busy = true
		dir := r.URL.Query().Get("directory")
		cmd := exec.Command("sh", "-c", "echo change >> work.txt && git add work.txt && git commit -qm 'Change in OpenCode'")
		cmd.Dir = dir
		_ = cmd.Run()
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/session/ses_1/message":
		fmt.Fprint(w, `[{"info":{"role":"user"},"parts":[{"type":"text","text":"do it"}]},{"info":{"role":"assistant"},"parts":[{"type":"text","text":"Committed in OpenCode."}]}]`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestAnAgentRunsInOpencodeInItsOwnWorktree(t *testing.T) {
	m, _ := setup(t)
	fake := &fakeOpencode{}
	server := httptest.NewServer(fake)
	defer server.Close()
	if _, err := m.Start(StartRequest{Cwd: "demo", Prompt: "change it", Worktree: true, Engine: "opencode"}); err == nil {
		t.Fatalf("started OpenCode with it off")
	}
	m.UseOpencode(&OpencodeClient{BaseURL: server.URL, HTTP: server.Client()})
	ctx := context.Background()
	if err := m.RefreshOpencode(ctx); err != nil || !m.OpencodeOn() {
		t.Fatalf("OpenCode is not up: %v", err)
	}
	a, err := m.Start(StartRequest{Cwd: "demo", Prompt: "change it", Worktree: true, Engine: "opencode"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, a.ID, func(a *Agent) bool { return a.Status == Working && a.Opencode.SessionID == "ses_1" })
	_ = m.RefreshOpencode(ctx)
	fake.mu.Lock()
	if len(fake.prompts) != 1 || fake.prompts[0] != a.Worktree.Path {
		t.Fatalf("prompted in %v, not its worktree", fake.prompts)
	}
	fake.busy = false
	fake.mu.Unlock()
	_ = m.RefreshOpencode(ctx)
	got := waitFor(t, m, a.ID, func(a *Agent) bool { return a.Status == Waiting })
	if got.Result != "Committed in OpenCode." || !got.Worktree.HasUnmergedCommits() {
		t.Fatalf("got %q", got.Result)
	}
	if _, err := m.Prompt(a.ID, "one more", nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Merge(a.ID); err == nil {
		t.Fatalf("merged while its turn runs")
	}
}
