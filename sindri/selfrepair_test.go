package sindri

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yggdrasil/internal/call"
)

func TestRepairableKeepsOnlyWhatThisRepoCanFix(t *testing.T) {
	for _, c := range []struct {
		alert RepairAlert
		want  bool
	}{
		{RepairAlert{Source: "ratatoskr", Key: "failing:norn:inbox", State: "open"}, true},
		{RepairAlert{Source: "ratatoskr", Key: "stalled:sindri:feed", State: "acknowledged"}, true},
		{RepairAlert{Source: "ratatoskr", Key: "down:huginn", State: "open"}, true},
		{RepairAlert{Source: "huginn", Key: "stream:norn", State: "open"}, false},
		{RepairAlert{Source: "gjallar", Key: "silent:ratatoskr", State: "open"}, false},
		{RepairAlert{Source: "munin", Key: "tcgpriser:down", State: "open"}, false},
		{RepairAlert{Source: "munin", Key: "offline", State: "open"}, false},
		{RepairAlert{Source: "munin", Key: "incident:github", State: "open"}, false},
		{RepairAlert{Source: "vedrfolnir", Key: "disk:/", State: "open"}, false},
		{RepairAlert{Source: "norn", Key: "signin:microsoft", State: "open"}, false},
		{RepairAlert{Source: "norn", Key: "model-failing", State: "open"}, false},
		{RepairAlert{Source: "hermodr", Key: "channel:ntfy", State: "open"}, false},
		{RepairAlert{Source: "loki", Key: "missed:norn:health", State: "open"}, true},
		{RepairAlert{Source: "heimdall", Key: "deps:yggdrasil:Go:golang.org/x/net@v0.1.0:GO-2024-0001", State: "open"}, true},
		{RepairAlert{Source: "heimdall", Key: "listening:0.0.0.0:8774", State: "open"}, true},
		{RepairAlert{Source: "heimdall", Key: "secrets:yggdrasil:abc123:main.go:3:GitHub-token", State: "open"}, false},
		{RepairAlert{Source: "heimdall", Key: "files:mode:~/.config/yggdrasil/owner.token", State: "open"}, false},
		{RepairAlert{Source: "heimdall", Key: "scopes:google:gmail", State: "open"}, false},
		{RepairAlert{Source: "glim", Key: "anything", State: "open"}, false},
		{RepairAlert{Source: "ratatoskr", Key: "down:norn", State: "resolved"}, false},
		{RepairAlert{Source: "ratatoskr", Key: "down:norn", State: "open", Muted: true}, false},
	} {
		if got := Repairable(c.alert); got != c.want {
			t.Errorf("%s %s: got %v, want %v", c.alert.Source, c.alert.Key, got, c.want)
		}
	}
}

func ptr(s string) *string { return &s }

func TestGroupCorrectionsCountsDisagreementsByKind(t *testing.T) {
	day := int64(24 * time.Hour / time.Millisecond)
	now := 100 * day
	said := func(action string, target *string) *repairPick {
		return &repairPick{Action: action, Target: target, At: now}
	}
	answer := repairPick{Action: "answer"}
	var decisions []RepairDecision
	for i := range 4 { // picked answer, you said code:yggdrasil, 4 times
		decisions = append(decisions, RepairDecision{ID: int64(i), At: now - int64(i)*day, Live: answer, LiveRef: ptr("answer"), Correction: said("code", ptr("yggdrasil"))})
	}
	decisions = append(decisions,
		RepairDecision{At: now, Live: answer, LiveRef: ptr("answer"), Correction: said("answer", nil)},                    // agreed
		RepairDecision{At: now, Live: answer, LiveRef: ptr("answer")},                                                     // not corrected
		RepairDecision{At: now - 9*day, Live: answer, LiveRef: ptr("answer"), Correction: said("code", ptr("yggdrasil"))}, // too old
		RepairDecision{At: now, Live: repairPick{Action: "code", Target: ptr("yggdrasil")}, LiveRef: ptr("code:yggdrasil"), Correction: said("code", ptr("yggdrasil"))},
	)
	// picked a reply to one agent, you said another: one group across agents, ids beside it
	for _, ids := range [][2]string{{"a1", "a2"}, {"a1", "a3"}, {"a2", "a3"}} {
		decisions = append(decisions, RepairDecision{At: now, Live: repairPick{Action: "reply", Target: ptr("Some agent label")}, LiveRef: ptr("reply_to_agent:" + ids[0]), Correction: said("reply", ptr(ids[1]))})
	}
	// the same agent both sides is agreement
	decisions = append(decisions, RepairDecision{At: now, Live: repairPick{Action: "status", Target: ptr("x")}, LiveRef: ptr("status:a1"), Correction: said("status", ptr("a1"))})
	// picked delegate, you said answer: only twice, under the threshold
	for range 2 {
		decisions = append(decisions, RepairDecision{At: now, Live: repairPick{Action: "delegate"}, LiveRef: ptr("delegate"), Correction: said("answer", nil)})
	}

	groups := GroupCorrections(decisions, now-7*day, nil)
	if len(groups) != 2 {
		t.Fatalf("got %+v", groups)
	}
	if g := groups[0]; g.Picked != "answer" || g.Said != "code:yggdrasil" || g.Count != 4 || len(g.Agents) != 0 {
		t.Errorf("first group: %+v", g)
	}
	if g := groups[1]; g.Picked != "reply_to_agent" || g.Said != "reply_to_agent" || g.Count != 3 || strings.Join(g.Agents, ",") != "a1,a2,a3" {
		t.Errorf("second group: %+v", g)
	}
	if strings.Contains(groups[1].context(), "Some agent label") || strings.Contains(groups[1].title(), "label") {
		t.Errorf("an agent's name got into the goal: %q", groups[1].context())
	}

	// After a goal was made for a group, only newer corrections count.
	after := map[string]int64{groups[0].Key(): now - 2*day}
	for _, g := range GroupCorrections(decisions, now-7*day, after) {
		if g.Key() == groups[0].Key() {
			t.Errorf("counted corrections from before its goal: %+v", g)
		}
	}
}

func TestRepairDraftsSkipOpenGoalsAndKeepToTheCap(t *testing.T) {
	at := time.Date(2026, 11, 17, 22, 0, 0, 0, time.UTC)
	alerts := []RepairAlert{
		{ID: 1, Source: "ratatoskr", Key: "failing:norn:inbox", Severity: "warning", Title: "Norn's inbox loop fails", Detail: "boom", State: "open", LastSeenAt: at},
		{ID: 2, Source: "ratatoskr", Key: "down:huginn", Severity: "critical", Title: "Huginn is down", State: "open", LastSeenAt: at},
		{ID: 3, Source: "munin", Key: "offline", Severity: "warning", Title: "Offline", State: "open", LastSeenAt: at},
		{ID: 4, Source: "ratatoskr", Key: "stalled:sindri:feed", Severity: "warning", Title: "Sindri's feed stalled", State: "open", LastSeenAt: at.Add(-time.Hour)},
	}
	groups := []DispatchGroup{{Picked: "answer", Said: "code:yggdrasil", Count: 4}}
	open := map[string]bool{AlertRepairKey(alerts[0]): true}

	drafts := RepairDrafts(alerts, groups, open, 10, func(a RepairAlert) []AlertEvent {
		return []AlertEvent{{At: at, Kind: "opened", By: a.Source}}
	})
	var keys []string
	for _, d := range drafts {
		keys = append(keys, d.key)
	}
	want := []string{"self-repair:alert:ratatoskr:down:huginn", "self-repair:alert:ratatoskr:stalled:sindri:feed", "self-repair:dispatch:answer>code:yggdrasil"}
	if strings.Join(keys, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v, want %v", keys, want)
	}
	if !strings.Contains(drafts[0].context, "opened by ratatoskr") {
		t.Errorf("no history: %q", drafts[0].context)
	}
	if got := RepairDrafts(alerts, groups, open, 2, nil); len(got) != 2 || got[0].key != want[0] {
		t.Errorf("cap of 2: got %+v", got)
	}
	if got := RepairDrafts(alerts, groups, open, 0, nil); len(got) != 0 {
		t.Errorf("cap of 0: got %+v", got)
	}
}

// fakeParts serves Gjallar's alerts and Norn's decisions.
func fakeParts(t *testing.T, alerts *[]RepairAlert, decisions *[]RepairDecision) (*call.Part, *call.Part) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/alerts":
			_ = json.NewEncoder(rw).Encode(map[string]any{"alerts": *alerts})
		case strings.HasPrefix(r.URL.Path, "/v1/alerts/"):
			_ = json.NewEncoder(rw).Encode(map[string]any{"events": []AlertEvent{{Kind: "opened", By: "ratatoskr"}}})
		case r.URL.Path == "/v1/decisions":
			_ = json.NewEncoder(rw).Encode(map[string]any{"decisions": *decisions})
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(server.Close)
	return call.New("gjallar", server.URL, "t", time.Second), call.New("norn", server.URL, "t", time.Second)
}

func TestSelfRepairDraftsOncePerNightWithoutDuplicates(t *testing.T) {
	w, _ := newWork(t)
	now := must2(time.Parse(time.DateTime, "2026-11-17 22:00:00"))
	w.Store.now = func() time.Time { return now }
	alerts := []RepairAlert{
		{ID: 1, Source: "ratatoskr", Key: "failing:norn:inbox", Severity: "warning", Title: "Norn's inbox loop fails", Detail: "boom", State: "open"},
		{ID: 2, Source: "munin", Key: "offline", Severity: "warning", Title: "Offline", State: "open"},
	}
	var decisions []RepairDecision
	gjallar, norn := fakeParts(t, &alerts, &decisions)
	w.Repair = &Repair{Gjallar: gjallar, Norn: norn, Repo: "demo"}
	ctx := context.Background()

	if ids, err := w.DraftSelfRepair(ctx, "2026-11-17"); err != nil || len(ids) != 0 {
		t.Fatalf("drafted while off: %v %v", ids, err)
	}
	w.Repair.Default = true
	ids, err := w.DraftSelfRepair(ctx, "2026-11-17")
	if err != nil || len(ids) != 1 {
		t.Fatalf("got %v %v", ids, err)
	}
	goal := must2(w.goalView(ids[0]))
	if !goal.Night || goal.Source != "self-repair:alert:ratatoskr:failing:norn:inbox" || !strings.Contains(goal.Context, "boom") || goal.Repos[0] != "demo" {
		t.Fatalf("got %+v", goal)
	}
	if !strings.Contains(SelfRepairBrief(&goal.Goal), notSure) {
		t.Fatal("the brief does not say how to give up")
	}
	if again, _ := w.DraftSelfRepair(ctx, "2026-11-17"); len(again) != 0 {
		t.Fatalf("drafted twice in one night: %v", again)
	}
	// The next night the alert is still there, and its goal still open: no new one.
	if again, _ := w.DraftSelfRepair(ctx, "2026-11-18"); len(again) != 0 {
		t.Fatalf("drafted a second goal for an open one: %v", again)
	}
	// The alert resolved before its draft ran: the draft is closed, nothing new.
	alerts = nil
	if again, _ := w.DraftSelfRepair(ctx, "2026-11-19"); len(again) != 0 {
		t.Fatalf("drafted for a resolved alert: %v", again)
	}
	if view := must2(w.goalView(ids[0])); view.State != "closed" || view.Night {
		t.Fatalf("a draft for a resolved alert stayed: %+v", view)
	}
}

func TestTheMorningReportSortsSelfRepairs(t *testing.T) {
	task := func(status string, branch bool, result string) GoalTask {
		t := GoalTask{Status: status}
		if branch {
			t.Branch = ptr("b")
		}
		if result != "" {
			t.Result = ptr(result)
		}
		return t
	}
	view := func(title, source string, tasks ...GoalTask) GoalView {
		current := currentParts(tasks)
		return GoalView{Goal: Goal{Title: title, Source: source}, Tasks: tasks, State: GoalState(current), Next: NextFor(current)}
	}
	goals := []GoalView{
		view("Self-repair: Huginn is down", "self-repair:alert:ratatoskr:down:huginn", task("done", true, "Fixed the reconnect.")),
		view("Self-repair: Norn's inbox loop fails", "self-repair:alert:ratatoskr:failing:norn:inbox", task("no-changes", false, "NOT SURE: could not reproduce it.")),
		view("Self-repair: Sindri's feed stalled", "self-repair:alert:ratatoskr:stalled:sindri:feed", task("no-changes", false, "Already fixed on main.")),
		view("Your own goal", "manual", task("done", true, "")),
		view("Self-repair: Dellingr's clock fails", "self-repair:alert:ratatoskr:failing:dellingr:clock", task("running", false, "")),
	}
	for i, want := range []string{"fixed", "not-sure", "left-alone", "", "running"} {
		if want == "" {
			continue
		}
		if got := RepairOutcome(goals[i]); got != want {
			t.Errorf("%s: got %s, want %s", goals[i].Title, got, want)
		}
	}
	m := NightReportWithRepairs(goals)
	fixed := strings.Index(m.Text, "Fixed, ready to review:\n- Huginn is down")
	unsure := strings.Index(m.Text, "Not sure:\n- Norn's inbox loop fails")
	running := strings.Index(m.Text, "Still running:\n- Dellingr's clock fails")
	alone := strings.Index(m.Text, "Left alone:\n- Sindri's feed stalled")
	if fixed < 0 || unsure < fixed || running < unsure || alone < running || !strings.Contains(m.Text, "Ready to review:\n- Your own goal") || !strings.Contains(m.Text, "nothing was merged") {
		t.Fatalf("got %q", m.Text)
	}
	if strings.Count(m.Text, "Huginn is down") != 1 {
		t.Fatalf("a self-repair goal is listed twice: %q", m.Text)
	}
}

func TestAMergedFixBlocksANewGoalForADay(t *testing.T) {
	now := time.Date(2026, 11, 19, 20, 0, 0, 0, time.UTC)
	merged := func(source string, ago time.Duration) GoalView {
		return GoalView{Goal: Goal{Source: source, At: now.Add(-48 * time.Hour).UnixMilli()}, State: "merged",
			Tasks: []GoalTask{{LastActivity: now.Add(-ago).UnixMilli()}}}
	}
	open, _ := openRepairs([]GoalView{
		merged("self-repair:alert:ratatoskr:down:huginn", 3*time.Hour),
		merged("self-repair:alert:ratatoskr:down:norn", 30*time.Hour),
		{Goal: Goal{Source: "self-repair:alert:ratatoskr:down:dellingr"}, State: "closed"},
		{Goal: Goal{Source: "self-repair:alert:ratatoskr:down:hermodr"}, State: "running"},
	}, now)
	want := map[string]bool{"self-repair:alert:ratatoskr:down:huginn": true, "self-repair:alert:ratatoskr:down:hermodr": true}
	if !maps.Equal(open, want) {
		t.Fatalf("got %v, want %v", open, want)
	}
}
