package huginn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/part"
)

var learnStart = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

func ended(organizer string, at time.Time) Seen {
	return Seen{Key: fmt.Sprint("calendar.ended|", organizer, at.Unix()), Kind: "calendar.ended", At: at, Organizer: organizer, Calendar: "work"}
}

func TestAnObservationPairsWithAnEventOnlyWithinTheWindowAndItsFilters(t *testing.T) {
	var events []Seen
	var seen []Observation
	for day := range 6 {
		at := learnStart.Add(time.Duration(day) * 24 * time.Hour)
		events = append(events, ended("Anna", at), ended("Bo", at.Add(4*time.Hour)))
		after := 3 * time.Minute
		if day == 5 {
			after = 15 * time.Minute // outside the 10 minutes
		}
		seen = append(seen, Observation{Action: "open:/dashboard/meeting", At: at.Add(after)})
	}
	patterns := Patterns(events, seen, 10*time.Minute, learnStart.Add(30*24*time.Hour))
	if len(patterns) != 1 {
		t.Fatalf("got %+v", patterns)
	}
	p := patterns[0]
	if p.Seen.Organizer != "Anna" || p.Times != 5 || p.Of != 6 || !p.Suggests() {
		t.Fatalf("got %+v", p)
	}
	if p.Key() != "calendar.ended|organizer=anna|calendar=work -> open:/dashboard/meeting" {
		t.Fatalf("key %q", p.Key())
	}
}

func TestAnEventWhoseWindowIsStillOpenIsNotACounterCase(t *testing.T) {
	now := learnStart.Add(5 * time.Minute)
	got := Patterns([]Seen{ended("Anna", learnStart)}, nil, 10*time.Minute, now)
	if len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestFiveTimesIsNotEnoughWhenYouUsuallyDoNot(t *testing.T) {
	for _, c := range []struct {
		times, of int
		want      bool
	}{{4, 4, false}, {5, 5, true}, {5, 7, true}, {5, 8, false}, {7, 10, true}, {6, 9, false}} {
		if got := (Pattern{Times: c.times, Of: c.of}).Suggests(); got != c.want {
			t.Errorf("%d of %d: got %v", c.times, c.of, got)
		}
	}
}

func TestTheOverviewIsNeverLearnedAndPathsLoseTheirIds(t *testing.T) {
	events := []Seen{}
	seen := []Observation{}
	for day := range 5 {
		at := learnStart.Add(time.Duration(day) * 24 * time.Hour)
		events = append(events, ended("Anna", at))
		seen = append(seen, Observation{Action: "open:/dashboard", At: at.Add(time.Minute)})
	}
	if got := Patterns(events, seen, 10*time.Minute, learnStart.Add(30*24*time.Hour)); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
	for in, want := range map[string]string{
		"open:/dashboard/goals/42/runs": "open:/dashboard/goals",
		"open:/dashboard/Meeting":       "open:/dashboard/meeting",
		"copy-draft":                    "copy-draft",
	} {
		if got, _, err := NormalAction(in, ""); err != nil || got != want {
			t.Errorf("%s: got %q %v", in, got, err)
		}
	}
	for _, bad := range [][2]string{{"open:https://evil.example", ""}, {"typed: hello", ""}, {"snooze-alert", "nokey"}, {"snooze-alert", "a:b c"}} {
		if _, _, err := NormalAction(bad[0], bad[1]); err == nil {
			t.Errorf("%v was taken", bad)
		}
	}
}

func TestQuietsOfOneAlertCountOnceAnHour(t *testing.T) {
	var seen []Observation
	for i := range 5 {
		at := learnStart.Add(time.Duration(i) * 2 * time.Hour)
		seen = append(seen, Observation{Action: "snooze-alert", Target: "munin:down:laya", At: at},
			Observation{Action: "ack-alert", Target: "munin:down:laya", At: at.Add(time.Minute)})
	}
	seen = append(seen, Observation{Action: "mute-alert", Target: "vedrfolnir:disk:/", At: learnStart})
	got := Quiets(seen)
	if len(got) != 2 || got[0].Source != "munin" || got[0].Key != "down:laya" || got[0].Times != 5 || got[1].Times != 1 {
		t.Fatalf("got %+v", got)
	}
}

// --- through the routes ---

type learnServer struct {
	*httptest.Server
	t *testing.T
}

func (s learnServer) do(token, method, path, body string, out any) int {
	s.t.Helper()
	request, _ := http.NewRequest(method, s.URL+path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		s.t.Fatal(err)
	}
	defer response.Body.Close()
	if out != nil {
		_ = json.NewDecoder(response.Body).Decode(out)
	}
	return response.StatusCode
}

func serve(t *testing.T, h *Huginn) learnServer {
	p := part.New(Name, contracttest.Tokens(), nil)
	h.Mount(p)
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	return learnServer{server, t}
}

// meetingsWithAnna has six of Anna's meetings end a day apart, and you open
// the meeting note a few minutes after `opened` of them.
func meetingsWithAnna(t *testing.T, h *Huginn, s learnServer, opened int, from time.Time) {
	t.Helper()
	for day := range 6 {
		end := from.Add(time.Duration(day) * 24 * time.Hour)
		h.now = func() time.Time { return end }
		fired, ok := h.FromNorn(norn("calendar.ended", map[string]any{"calendar": "work",
			"event": map[string]any{"id": fmt.Sprint("m", from.Unix(), day), "subject": "Sync", "start": end.Add(-time.Hour).UnixMilli(), "end": end.UnixMilli(), "organizer": "Anna Berg"}}))
		if !ok {
			t.Fatal("not read")
		}
		h.Handle(context.Background(), fired)
		if day < opened {
			h.now = func() time.Time { return end.Add(4 * time.Minute) }
			body := fmt.Sprintf(`{"action":"open:/dashboard/meeting","at":%q}`, end.Add(3*time.Minute).Format(time.RFC3339))
			if status := s.do(contracttest.Owner, "POST", "/v1/observations", body, nil); status != 204 {
				t.Fatalf("observing: %d", status)
			}
		}
	}
	h.now = func() time.Time { return from.Add(6 * 24 * time.Hour) }
}

func suggestions(t *testing.T, s learnServer) []Suggestion {
	t.Helper()
	var answer struct{ Suggestions []Suggestion }
	if status := s.do(contracttest.Owner, "GET", "/v1/suggestions", "", &answer); status != 200 {
		t.Fatalf("listing: %d", status)
	}
	return answer.Suggestions
}

func TestAHabitBecomesASuggestedRuleThatYouAddWithOneClick(t *testing.T) {
	h, fake := newTest(t, learnStart)
	h.DashboardURL = "http://localhost:17420"
	s := serve(t, h)
	ctx := context.Background()
	if status := s.do(contracttest.Sender, "POST", "/v1/observations", `{"action":"copy-draft"}`, nil); status != 403 {
		t.Fatalf("an agent reported what you did: %d", status)
	}
	meetingsWithAnna(t, h, s, 5, learnStart)
	if err := h.learn(ctx); err != nil {
		t.Fatal(err)
	}
	got := suggestions(t, s)
	if len(got) != 1 || got[0].Kind != "rule" || got[0].Times != 5 || got[0].Of != 6 || got[0].Rule == nil {
		t.Fatalf("got %+v", got)
	}
	rule := got[0].Rule
	if rule.On.Event != "calendar.ended" || rule.On.Organizer != "Anna Berg" || rule.Action.Link != "/dashboard/meeting" || rule.Deliver[0] != "widget" {
		t.Fatalf("rule %+v", rule)
	}
	// Told once, quietly: listed in the widget, held through a meeting, never pushed.
	if len(fake.pushed) != 1 || fake.pushed[0].Push == nil || *fake.pushed[0].Push || !fake.pushed[0].Hold || fake.pushed[0].Link != "http://localhost:17420/dashboard/automations" {
		t.Fatalf("told %+v", fake.pushed)
	}
	if err := h.learn(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.pushed) != 1 {
		t.Fatalf("told again: %+v", fake.pushed)
	}

	if status := s.do(contracttest.Sender, "POST", "/v1/suggestions/"+got[0].ID+"/accept", "", nil); status != 403 {
		t.Fatalf("an agent accepted: %d", status)
	}
	var accepted struct {
		Suggestion Suggestion
		Rule       Rule
	}
	if status := s.do(contracttest.Owner, "POST", "/v1/suggestions/"+got[0].ID+"/accept", "", &accepted); status != 200 {
		t.Fatalf("accepting: %d", status)
	}
	if !accepted.Rule.Enabled || accepted.Rule.CreatedBy != "owner" || accepted.Suggestion.State != "accepted" || accepted.Suggestion.RuleID != accepted.Rule.ID {
		t.Fatalf("got %+v", accepted)
	}
	if status := s.do(contracttest.Owner, "POST", "/v1/suggestions/"+got[0].ID+"/accept", "", nil); status != 409 {
		t.Fatalf("accepted twice: %d", status)
	}
	if left := suggestions(t, s); len(left) != 0 {
		t.Fatalf("still listed: %+v", left)
	}

	// The rule now tells you, with the meeting note's link.
	end := learnStart.Add(10 * 24 * time.Hour)
	h.now = func() time.Time { return end.Add(time.Minute) }
	fired, _ := h.FromNorn(norn("calendar.ended", map[string]any{"calendar": "work",
		"event": map[string]any{"id": "later", "subject": "Sync", "start": end.Add(-time.Hour).UnixMilli(), "end": end.UnixMilli(), "organizer": "anna berg"}}))
	h.Handle(ctx, fired)
	last := fake.pushed[len(fake.pushed)-1]
	if len(fake.pushed) != 2 || last.Text != "Sync with anna berg ended: open the meeting note?" || last.Link != "http://localhost:17420/dashboard/meeting" {
		t.Fatalf("told %+v", fake.pushed)
	}
}

func TestBelowSeventyPercentNothingIsSuggested(t *testing.T) {
	h, fake := newTest(t, learnStart)
	s := serve(t, h)
	meetingsWithAnna(t, h, s, 4, learnStart)
	meetingsWithAnna(t, h, s, 1, learnStart.Add(7*24*time.Hour))
	// 5 of 12.
	if err := h.learn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := suggestions(t, s); len(got) != 0 || len(fake.pushed) != 0 {
		t.Fatalf("got %+v, told %+v", got, fake.pushed)
	}
}

func TestNoThanksIsForever(t *testing.T) {
	h, _ := newTest(t, learnStart)
	s := serve(t, h)
	ctx := context.Background()
	meetingsWithAnna(t, h, s, 5, learnStart)
	_ = h.learn(ctx)
	got := suggestions(t, s)
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	var said struct{ Suggestion Suggestion }
	if status := s.do(contracttest.Owner, "POST", "/v1/suggestions/"+got[0].ID+"/reject", "", &said); status != 200 || said.Suggestion.State != "rejected" {
		t.Fatalf("%d %+v", status, said)
	}
	meetingsWithAnna(t, h, s, 6, learnStart.Add(7*24*time.Hour))
	_ = h.learn(ctx)
	if got := suggestions(t, s); len(got) != 0 {
		t.Fatalf("suggested again: %+v", got)
	}
	rules, _ := h.Store.Rules(ctx)
	for _, rule := range rules {
		if !rule.BuiltIn {
			t.Fatalf("made %+v", rule)
		}
	}
}

func TestAnAlertYouKeepQuietingIsSuggestedAsAMuteAndHuginnNeverCallsGjallarWithYourToken(t *testing.T) {
	h, _ := newTest(t, learnStart)
	var mu sync.Mutex
	var asked []string
	gjallar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		asked = append(asked, fmt.Sprint(r.Method, " ", r.URL.Path, " ", r.Header.Get("Authorization"), " ", body["source"], " ", body["key"], " ", body["muted"]))
		_ = json.NewEncoder(w).Encode(map[string]any{"mutes": []any{}})
	}))
	defer gjallar.Close()
	h.Gjallar = alert.NewClient(gjallar.URL, "huginn-token")
	s := serve(t, h)
	for i := range 5 {
		at := learnStart.Add(time.Duration(i) * 3 * time.Hour)
		h.now = func() time.Time { return at }
		body := `{"action":"snooze-alert","target":"munin:down:laya"}`
		if status := s.do(contracttest.Owner, "POST", "/v1/observations", body, nil); status != 204 {
			t.Fatalf("observing: %d", status)
		}
	}
	_ = h.learn(context.Background())
	got := suggestions(t, s)
	if len(got) != 1 || got[0].Kind != "mute" || got[0].Mute == nil || got[0].Mute.Source != "munin" || got[0].Mute.Key != "down:laya" {
		t.Fatalf("got %+v", got)
	}
	if status := s.do(contracttest.Owner, "POST", "/v1/suggestions/"+got[0].ID+"/accept", "", nil); status != 200 {
		t.Fatalf("accepting: %d", status)
	}
	if len(asked) != 0 {
		t.Fatalf("Huginn called Gjallar itself: %q", asked)
	}
}

func TestLearningOffKeepsNothingAndForgetsWhatItSaw(t *testing.T) {
	h, _ := newTest(t, learnStart)
	s := serve(t, h)
	ctx := context.Background()
	_ = s.do(contracttest.Owner, "POST", "/v1/observations", `{"action":"copy-draft"}`, nil)
	var setting LearnSetting
	if status := s.do(contracttest.Owner, "PATCH", "/v1/settings/learning", `{"on":false}`, &setting); status != 200 || setting.On || setting.WindowMinutes != 10 {
		t.Fatalf("%d %+v", status, setting)
	}
	_ = s.do(contracttest.Owner, "POST", "/v1/observations", `{"action":"copy-draft"}`, nil)
	if kept, _ := h.Store.Observations(ctx, learnStart.Add(-time.Hour)); len(kept) != 0 {
		t.Fatalf("kept %+v", kept)
	}
	if status := s.do(contracttest.Owner, "PATCH", "/v1/settings/learning", `{"windowMinutes":0}`, nil); status != 400 {
		t.Fatalf("took a 0 minute window: %d", status)
	}
}
