package norn

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"yggdrasil/api"
	"yggdrasil/internal/contract"
	"yggdrasil/internal/llm"
)

var rewindZone, _ = time.LoadLocation("Europe/Stockholm")

// at is a time on 2026-11-<day> in Stockholm.
func at(day, hour, minute int) time.Time {
	return time.Date(2026, 11, day, hour, minute, 0, 0, rewindZone)
}

func ms(day, hour, minute int) int64 { return at(day, hour, minute).UnixMilli() }

func TestBurstsGroupByPauseAndGroup(t *testing.T) {
	said := []Said{
		{At: ms(18, 9, 0), Group: "a", Text: "one"},
		{At: ms(18, 9, 5), Group: "a", Text: "two"},
		{At: ms(18, 9, 14), Group: "a", Text: "three"},
		{At: ms(18, 9, 30), Group: "a", Text: "later"},
		{At: ms(18, 9, 6), Group: "b", Text: "other"},
	}
	bursts := Bursts(said, RewindBurst)
	if len(bursts) != 3 || len(bursts[0]) != 3 || bursts[1][0].Text != "later" || bursts[2][0].Group != "b" {
		t.Fatalf("got %+v", bursts)
	}
	entry := burstEntry(bursts[0], "prompt", "sindri", "/x", "")
	if entry.Title != "one" || entry.Detail != "… three" || entry.End != ms(18, 9, 14) {
		t.Fatalf("burst entry: %+v", entry)
	}
	single := burstEntry(bursts[1], "prompt", "sindri", "/x", "")
	if single.End != 0 || single.Detail != "" {
		t.Fatalf("one prompt is no span: %+v", single)
	}
}

func TestEachSourceAsEntries(t *testing.T) {
	from, to := ms(18, 0, 0), ms(19, 0, 0)
	meetings := MeetingEntries([]CalendarEvent{
		{Subject: "Standup", Start: ms(18, 9, 0), End: ms(18, 9, 15), Location: "Room 1"},
		{Subject: "Declined", Start: ms(18, 10, 0), End: ms(18, 11, 0), Response: "declined"},
		{Subject: "Holiday", Start: ms(18, 0, 0), End: ms(19, 0, 0), AllDay: true},
		{Subject: "Focus", Start: ms(18, 13, 0), End: ms(18, 14, 0), ShowAs: "free"},
		{Subject: "Break", Start: ms(18, 12, 0), End: ms(18, 12, 30), ShowAs: "busy", Break: true},
		{Subject: "Next day", Start: ms(19, 9, 0), End: ms(19, 10, 0)},
	}, "work", from, to)
	if len(meetings) != 2 || meetings[0].Title != "Standup" || meetings[0].Detail != "Room 1" || meetings[0].End != ms(18, 9, 15) ||
		meetings[0].Source != "calendar:work" || meetings[1].Kind != "break" {
		t.Fatalf("meetings: %+v", meetings)
	}

	points := SavePointEntries([]SavePoint{{At: ms(18, 14, 0), Title: "Races", Link: "/dashboard/goals/3", Meeting: "1:1"}, {At: ms(17, 14, 0)}}, from, to)
	if len(points) != 1 || points[0].Title != "Kept your place: Races" || points[0].Detail != "for 1:1" || points[0].Link != "/dashboard/goals/3" {
		t.Fatalf("save points: %+v", points)
	}

	promises := PromiseEntries([]Promise{
		{What: "Send the deck", To: "Anna", Due: "2026-11-20", State: "done", CreatedAt: ms(18, 10, 0), ChangedAt: ms(18, 16, 0), DoneWhy: "You sent it"},
		{What: "Old one", State: "open", CreatedAt: ms(10, 10, 0), ChangedAt: ms(10, 10, 0)},
	}, from, to)
	if len(promises) != 2 || promises[0].Title != "Promised Anna: Send the deck" || promises[0].Detail != "by 2026-11-20" || promises[1].Title != "Kept: Send the deck" {
		t.Fatalf("promises: %+v", promises)
	}

	long := strings.Repeat("x", 300)
	chats := ChatEntries([]ChatTurn{
		{Session: "s1", At: ms(18, 9, 0), Content: long},
		{Session: "s1", At: ms(18, 9, 3), Content: "and then", Label: "races"},
		{Session: "s2", At: ms(18, 15, 0), Content: "hello"},
	})
	if len(chats) != 2 || len([]rune(chats[0].Title)) != RewindClip || chats[0].Detail != "… and then · to races" ||
		chats[0].Link != "/dashboard/chat?session=s1" || chats[1].Title != "hello" {
		t.Fatalf("chat: %+v", chats)
	}

	memories := MemoryEntries([]Memory{{Text: "The user likes tea.", At: ms(18, 11, 0)}, {Text: "old", At: ms(1, 1, 0)}}, from, to)
	if len(memories) != 1 || memories[0].Title != "Learned: The user likes tea." {
		t.Fatalf("memories: %+v", memories)
	}

	replies := ReplyEntries([]CopiedReply{{At: ms(18, 11, 30), Source: "outlook", Sent: "Thanks, will do"}}, from, to)
	if len(replies) != 1 || replies[0].Title != "Used a reply draft (outlook)" || replies[0].Detail != "Thanks, will do" {
		t.Fatalf("replies: %+v", replies)
	}

	closed := ms(18, 17, 0)
	repo, branch := "yggdrasil", "feature/races"
	goal := RewindGoal{ID: 7, At: ms(18, 8, 0), Title: "Races", ClosedAt: &closed}
	goal.Tasks = append(goal.Tasks, struct {
		Agent     string  `json:"agent"`
		Repo      *string `json:"repo"`
		Branch    *string `json:"branch"`
		StartedAt int64   `json:"startedAt"`
		MergedAt  int64   `json:"mergedAt"`
	}{Agent: "a1", Repo: &repo, Branch: &branch, StartedAt: ms(18, 8, 30), MergedAt: ms(18, 16, 0)})
	goals := GoalEntries([]RewindGoal{goal}, from, to)
	SortEntries(goals)
	titles := []string{}
	for _, g := range goals {
		titles = append(titles, g.Kind+":"+g.Title)
	}
	if strings.Join(titles, "|") != "goal:New goal: Races|goal:Started: Races|merge:Merged: Races|goal:Closed: Races" || goals[2].Detail != "yggdrasil · feature/races" || goals[0].Link != "/dashboard/goals/7" {
		t.Fatalf("goals: %v %+v", titles, goals)
	}

	agents := AgentEntries([]RewindAgent{
		{ID: "a1", Label: "yggdrasil: in a goal", StartedAt: ms(18, 9, 0)},
		{ID: "a2", Label: "my-widget: dark mode", Cwd: "/w", StartedAt: ms(18, 9, 0), MergedAt: ms(18, 12, 0)},
	}, map[string]bool{"a1": true}, from, to)
	if len(agents) != 2 || agents[0].Title != "Agent: dark mode" || agents[1].Kind != "merge" || !strings.HasPrefix(agents[0].Link, "/dashboard/sessions/a2?") {
		t.Fatalf("agents: %+v", agents)
	}

	prompts := PromptEntries([]OwnPrompt{
		{At: ms(18, 10, 0), Session: "c1", Title: "Races", Cwd: "/p/ygg", Project: "ygg", Branch: "main", Prompt: "start races"},
		{At: ms(18, 10, 4), Session: "c1", Title: "Races", Cwd: "/p/ygg", Project: "ygg", Branch: "main", Prompt: "add tests"},
		{At: ms(18, 14, 0), Session: "c1", Title: "Races", Cwd: "/p/ygg", Project: "ygg", Branch: "main", Prompt: "ship it"},
	})
	SortEntries(prompts)
	if len(prompts) != 3 || prompts[0].Kind != "prompt" || prompts[1].Kind != "session" || prompts[1].End != ms(18, 14, 0) ||
		prompts[1].Detail != "ygg · main" || prompts[0].Detail != "… add tests" || prompts[2].Title != "ship it" {
		t.Fatalf("prompts: %+v", prompts)
	}

	notes := NoteEntries([]NoteChange{
		{Path: "a.md", Title: "a", Link: "obsidian://a", At: ms(18, 9, 0)},
		{Path: "a.md", Title: "a", Link: "obsidian://a", At: ms(18, 15, 0)},
		{Path: "b.md", Title: "b", At: ms(17, 9, 0)},
	}, from, to)
	if len(notes) != 1 || notes[0].At != ms(18, 15, 0) || notes[0].Link != "obsidian://a" {
		t.Fatalf("notes: %+v", notes)
	}
}

func TestResolveWhen(t *testing.T) {
	now := at(19, 10, 0) // a Thursday
	cases := []struct {
		text     string
		from, to time.Time
		narrow   bool
	}{
		{"what was I doing yesterday", at(18, 0, 0), at(19, 0, 0), false},
		{"Tuesday afternoon", at(17, 12, 0), at(17, 18, 0), true},
		{"yesterday at 3", at(18, 14, 0), at(18, 16, 0), true},
		{"igår kl 15:30", at(18, 14, 30), at(18, 16, 30), true},
		{"thursday", at(19, 0, 0), at(20, 0, 0), false},
		{"last thursday", at(12, 0, 0), at(13, 0, 0), false},
		{"i torsdags", at(12, 0, 0), at(13, 0, 0), false},
		{"3 days ago", at(16, 0, 0), at(17, 0, 0), false},
		{"last week", at(9, 0, 0), at(16, 0, 0), false},
		{"on 2026-11-02", at(2, 0, 0), at(3, 0, 0), false},
		{"24 nov", time.Date(2025, 11, 24, 0, 0, 0, 0, rewindZone), time.Date(2025, 11, 25, 0, 0, 0, 0, rewindZone), false},
		{"this morning", at(19, 6, 0), at(19, 12, 0), true},
	}
	for _, c := range cases {
		got, ok := ResolveWhen(c.text, now, rewindZone)
		if !ok || !got.From.Equal(c.from) || !got.To.Equal(c.to) || got.Narrow != c.narrow {
			t.Errorf("%q: got %v..%v narrow %v (%v), want %v..%v", c.text, got.From, got.To, got.Narrow, ok, c.from, c.to)
		}
	}
	if _, ok := ResolveWhen("when did I last work on races?", now, rewindZone); ok {
		t.Errorf("no day in it")
	}
	if words := SearchWords("When did I last work on races?"); strings.Join(words, ",") != "races" {
		t.Errorf("words: %v", words)
	}
}

func TestOutlookRangeReadsEveryPageWithoutCancelled(t *testing.T) {
	var asked []string
	get := func(_ context.Context, target, token string, _ map[string]string) (int, []byte, error) {
		asked = append(asked, target)
		if token != "tok" {
			return 401, nil, nil
		}
		if strings.Contains(target, "page=2") {
			return 200, []byte(`{"value":[{"id":"2","subject":"Retro","start":{"dateTime":"2026-10-01T13:00:00"},"end":{"dateTime":"2026-10-01T14:00:00"}}]}`), nil
		}
		return 200, []byte(`{"value":[
			{"id":"1","subject":"Planning","start":{"dateTime":"2026-10-01T08:00:00"},"end":{"dateTime":"2026-10-01T09:00:00"}},
			{"id":"x","subject":"Gone","isCancelled":true,"start":{"dateTime":"2026-10-01T10:00:00"},"end":{"dateTime":"2026-10-01T11:00:00"}}],
			"@odata.nextLink":"https://graph.microsoft.com/v1.0/me/calendarView?page=2"}`), nil
	}
	from, to := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	events, err := OutlookRange(context.Background(), get, "tok", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Subject != "Planning" || events[1].Subject != "Retro" {
		t.Fatalf("got %+v", events)
	}
	if !strings.Contains(asked[0], "startDateTime=2026-10-01T00%3A00%3A00Z") || !strings.Contains(asked[0], "endDateTime=2026-10-02T00%3A00%3A00Z") {
		t.Fatalf("asked %s", asked[0])
	}
	if _, err := OutlookRange(context.Background(), get, "bad", from, to); err == nil {
		t.Fatalf("a refusal is an error")
	}
	if got := inRange(events, from.Add(9*time.Hour).UnixMilli(), to.UnixMilli()); len(got) != 1 || got[0].Subject != "Retro" {
		t.Fatalf("in range: %+v", got)
	}
}

func TestGoogleRangeReadsEachCalendar(t *testing.T) {
	get := func(_ context.Context, target, _ string, _ map[string]string) (int, []byte, error) {
		switch {
		case strings.Contains(target, "calendarList"):
			return 200, []byte(`{"items":[{"id":"me@example.com","selected":true}]}`), nil
		case strings.Contains(target, "timeMin=2026-10-01"):
			return 200, []byte(`{"items":[{"id":"g1","summary":"Dentist","start":{"dateTime":"2026-10-01T10:00:00+02:00"},"end":{"dateTime":"2026-10-01T11:00:00+02:00"}}]}`), nil
		}
		return 404, nil, nil
	}
	events, err := GoogleRange(context.Background(), get, "tok", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	if err != nil || len(events) != 1 || events[0].Subject != "Dentist" {
		t.Fatalf("got %+v %v", events, err)
	}
}

func TestRangeCacheExpires(t *testing.T) {
	var c rangeCache
	now := time.Now()
	c.put("k", []CalendarEvent{{Subject: "a"}}, now)
	if got, ok := c.get("k", now.Add(time.Minute)); !ok || len(got) != 1 {
		t.Fatalf("kept: %v", got)
	}
	if _, ok := c.get("k", now.Add(rangeCacheFor+time.Second)); ok {
		t.Fatalf("expired")
	}
}

func TestAnswerRewindMarksWindowAndWords(t *testing.T) {
	entries := []RewindEntry{
		{At: ms(18, 13, 0), End: ms(18, 14, 30), Kind: "meeting", Title: "Sync"},
		{At: ms(18, 15, 0), Kind: "prompt", Title: "add the race picker"},
		{At: ms(18, 18, 0), Kind: "chat", Title: "dinner"},
	}
	when := RewindWhen{From: at(18, 14, 0), To: at(18, 16, 0), Narrow: true}
	marked, focus := AnswerRewind(entries, when, true, nil)
	if len(marked) != 3 || !marked[0].Match || !marked[1].Match || marked[2].Match || focus != ms(18, 13, 0) {
		t.Fatalf("window: %+v %d", marked, focus)
	}
	marked, focus = AnswerRewind(entries, RewindWhen{From: at(1, 0, 0), To: at(19, 0, 0)}, false, []string{"races"})
	if len(marked) != 1 || !marked[0].Match || focus != ms(18, 15, 0) {
		t.Fatalf("words: %+v %d", marked, focus)
	}
}

func TestRewindRoutesMeetTheContract(t *testing.T) {
	n, p := newNorn(t)
	ctx := context.Background()
	n.Store.AddTurn(ctx, "s1", "user", "look at the races page", nil, nil, nil)
	n.Brain.Remember(ctx, "The user likes races.", "manual")
	spec, err := contract.Load(Name, api.Spec(Name))
	if err != nil {
		t.Fatal(err)
	}
	check := func(method, path string, body any, want int) map[string]any {
		t.Helper()
		status, got := call(t, p, method, path, body)
		if status != want {
			t.Fatalf("%s %s: %d %v", method, path, status, got)
		}
		if schema, _, ok := spec.Response(method, strings.Split(path, "?")[0], status); ok {
			if err := spec.Validate(schema, got); err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
		}
		answer, _ := got.(map[string]any)
		return answer
	}
	today := check("GET", "/v1/rewind", nil, 200)
	entries := today["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("today: %v", today)
	}
	check("GET", "/v1/rewind?from=10&to=5", nil, 400)
	check("POST", "/v1/rewind/ask", map[string]string{"q": ""}, 400)

	// No model here: the question is read as it is.
	answer := check("POST", "/v1/rewind/ask", map[string]string{"q": "when did I last work on races?"}, 200)
	if answer["fromModel"] != false || len(answer["entries"].([]any)) != 2 || answer["focus"].(float64) == 0 {
		t.Fatalf("fallback: %v", answer)
	}

	// With the local model: it only splits the question.
	var asked []llm.Message
	n.Rewind.Model = func(_ context.Context, messages []llm.Message, options llm.Options) (string, error) {
		asked = messages
		if options.Format == nil {
			return "", errors.New("no format")
		}
		return `{"when":"today","words":["race"]}`, nil
	}
	answer = check("POST", "/v1/rewind/ask", map[string]string{"q": "what did I do on the race page today"}, 200)
	if answer["fromModel"] != true || len(asked) != 2 || !strings.Contains(answer["said"].(string), "looking for race") {
		t.Fatalf("model: %v", answer)
	}
	matched := 0
	for _, e := range answer["entries"].([]any) {
		if e.(map[string]any)["match"] == true {
			matched++
		}
	}
	if matched != 2 {
		t.Fatalf("two entries hold race: %v", answer["entries"])
	}
}

func TestTimelineReadsSindriAndTheVault(t *testing.T) {
	n, _ := newNorn(t)
	sindri := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/goals":
			_, _ = w.Write([]byte(`{"goals":[{"id":1,"at":` + msText(ms(18, 8, 0)) + `,"title":"Races","state":"draft","closedAt":null,"tasks":[]}]}`))
		case "/v1/agents":
			_, _ = w.Write([]byte(`[]`))
		case "/v1/sessions/prompts":
			if r.URL.Query().Get("from") != msText(ms(18, 0, 0)) {
				http.Error(w, "range", 400)
				return
			}
			_, _ = w.Write([]byte(`{"prompts":[{"at":` + msText(ms(18, 9, 0)) + `,"session":"c1","title":"Races","prompt":"go"}],"oldest":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer sindri.Close()
	n.Sindri.URL = sindri.URL
	n.Vault = testVault(t, map[string]string{"Planning.md": "secret text", "Notes/Meetings/Sync 2026-11-18.md": "more secret"})
	changed := at(18, 11, 0)
	_ = os.Chtimes(filepath.Join(n.Vault.Dir, "Planning.md"), changed, changed)
	ctx := context.Background()
	if err := n.logVault(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Changed again today: the log still has the earlier change.
	_ = os.Chtimes(filepath.Join(n.Vault.Dir, "Planning.md"), time.Now(), time.Now())

	entries, unavailable := n.Timeline(ctx, at(18, 0, 0), at(19, 0, 0))
	if len(unavailable) != 0 {
		t.Fatalf("unavailable: %v", unavailable)
	}
	kinds := []string{}
	for _, e := range entries {
		kinds = append(kinds, e.Kind+":"+e.Title)
		if strings.Contains(e.Title+e.Detail, "secret") {
			t.Fatalf("note text leaked: %+v", e)
		}
	}
	if strings.Join(kinds, "|") != "goal:New goal: Races|prompt:go|session:Races|note:Planning" {
		t.Fatalf("got %v", kinds)
	}
}

func msText(n int64) string { return strconv.FormatInt(n, 10) }
