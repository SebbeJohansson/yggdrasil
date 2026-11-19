package norn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"yggdrasil/internal/llm"
)

var weaveZone, _ = time.LoadLocation("Europe/Stockholm")

// weekday is a time in the week of Monday 23 November 2026, in Stockholm: day 0 is Monday.
func weekday(day, hour, minute int) time.Time {
	return time.Date(2026, 11, 23+day, hour, minute, 0, 0, weaveZone)
}

func meetingOn(day, fromH, fromM, toH, toM int) CalendarEvent {
	return CalendarEvent{Subject: "Meeting", Start: weekday(day, fromH, fromM).UnixMilli(), End: weekday(day, toH, toM).UnixMilli(), ShowAs: "busy", Response: "accepted"}
}

var nineToFive = WorkDay{From: 9 * 60, To: 17 * 60}

func planOf(events []CalendarEvent, from time.Time, uses ...WeaveUse) Weave {
	return PlanWeek(WeaveInput{Monday: weekday(0, 0, 0), From: from, Day: nineToFive, FocusMin: 90 * time.Minute, Events: events, Uses: uses})
}

func dayIn(t *testing.T, w Weave, date string) WeaveDay {
	t.Helper()
	for _, d := range w.Days {
		if d.Date == date {
			return d
		}
	}
	t.Fatalf("no %s in %+v", date, w.Days)
	return WeaveDay{}
}

func hhmmOf(ms int64) string { return time.UnixMilli(ms).In(weaveZone).Format("15:04") }

func blocksOf(d WeaveDay) string {
	var out []string
	for _, b := range d.Blocks {
		out = append(out, fmt.Sprintf("%s %s-%s", b.Kind, hhmmOf(b.Start), hhmmOf(b.End)))
	}
	return strings.Join(out, ", ")
}

func TestThePlanIsDueFromSundayEveningAndForTheRestOfAWeek(t *testing.T) {
	day := nineToFive
	monday := "2026-11-23"
	for _, c := range []struct {
		name string
		now  time.Time
		week string
		due  bool
	}{
		{"saturday", weekday(-2, 12, 0), monday, false},
		{"sunday before six", weekday(-1, 17, 59), monday, false},
		{"sunday at six", weekday(-1, 18, 0), monday, true},
		{"sunday night", weekday(-1, 23, 30), monday, true},
		{"monday morning, the machine was off on sunday", weekday(0, 7, 15), monday, true},
		{"wednesday", weekday(2, 11, 0), monday, true},
		{"friday afternoon", weekday(4, 16, 30), monday, true},
		{"friday evening: nothing left", weekday(4, 17, 0), monday, false},
		{"the next saturday", weekday(5, 9, 0), "2026-11-30", false},
	} {
		week, due := WeaveDue(c.now, weaveZone, 18*60, day)
		if week.Format(dayLayout) != c.week || due != c.due {
			t.Errorf("%s: week %s due %v, want %s %v", c.name, week.Format(dayLayout), due, c.week, c.due)
		}
	}
}

func TestAFocusBlockTakesTheLongestFreeStretchMorningsFirst(t *testing.T) {
	events := []CalendarEvent{
		meetingOn(0, 11, 0, 12, 0),                            // Monday: 09-11 free (2h) in the morning, 12-17 (5h) after
		meetingOn(1, 9, 0, 10, 0), meetingOn(1, 11, 0, 17, 0), // Tuesday: only an hour free
		meetingOn(2, 9, 0, 12, 0), // Wednesday: free from noon
	}
	w := planOf(events, weekday(-1, 18, 0))
	if len(w.Days) != 5 || w.Week != "2026-11-23" {
		t.Fatalf("days %+v", w.Days)
	}
	if got := blocksOf(dayIn(t, w, "2026-11-23")); got != "focus 09:00-11:00" {
		t.Errorf("Monday: %s", got)
	}
	tuesday := dayIn(t, w, "2026-11-24")
	if strings.Contains(blocksOf(tuesday), "focus") {
		t.Errorf("Tuesday has no 90 minutes free, and is packed: %s", blocksOf(tuesday))
	}
	if got := blocksOf(dayIn(t, w, "2026-11-25")); got != "break 12:00-12:30, focus 12:30-14:30" {
		t.Errorf("Wednesday: the afternoon when the morning is full, after a break, capped at two hours: %s", got)
	}
	if got := blocksOf(dayIn(t, w, "2026-11-26")); got != "focus 09:00-11:00" {
		t.Errorf("Thursday, a free day: %s", got)
	}
	if d := dayIn(t, w, "2026-11-26"); d.Words != "Thursday is clear: a focus block in the morning." {
		t.Errorf("Thursday words %q", d.Words)
	}
}

func TestAPackedDayGetsNoFocusBlockAndABreakAfterALongRun(t *testing.T) {
	events := []CalendarEvent{
		// Tuesday: 09:00-12:00 and 12:10-13:00, one run of four hours with a short gap; then 13:00-13:20 free; 13:20-16:00 meetings.
		meetingOn(1, 9, 0, 12, 0), meetingOn(1, 12, 10, 13, 0), meetingOn(1, 13, 20, 16, 0),
	}
	w := planOf(events, weekday(-1, 18, 0))
	tuesday := dayIn(t, w, "2026-11-24")
	// 12:00-12:10 is too short to end the run, 13:00-13:20 ends it: four hours, then the break.
	if got := blocksOf(tuesday); got != "break 13:00-13:20" {
		t.Errorf("Tuesday: %s", got)
	}
	if tuesday.Words != "Tuesday is heavy: a break after lunch, no focus block." {
		t.Errorf("words %q", tuesday.Words)
	}
}

func TestABreakComesAfterEachLongRunAndNotWhenOneIsKept(t *testing.T) {
	events := []CalendarEvent{
		meetingOn(0, 9, 0, 12, 0), // Monday: three hours, then free: a break 12:00-12:30
		meetingOn(2, 9, 0, 12, 0), // Wednesday: the same, with a break already kept right after it
		{Subject: "Break", Start: weekday(2, 12, 0).UnixMilli(), End: weekday(2, 12, 15).UnixMilli(), ShowAs: "busy", Response: "organizer", Break: true},
		meetingOn(4, 9, 0, 11, 30), // Friday: two and a half hours, no break needed
	}
	w := planOf(events, weekday(-1, 18, 0))
	if got := blocksOf(dayIn(t, w, "2026-11-23")); got != "break 12:00-12:30, focus 12:30-14:30" {
		t.Errorf("Monday: %s", got)
	}
	if got := blocksOf(dayIn(t, w, "2026-11-25")); got != "focus 12:15-14:15" {
		t.Errorf("Wednesday, a break kept: %s", got)
	}
	if got := blocksOf(dayIn(t, w, "2026-11-27")); got != "focus 11:30-13:30" {
		t.Errorf("Friday: %s", got)
	}
}

func TestBreaksAndFocusBlocksAlreadyBookedCountAsBooked(t *testing.T) {
	events := []CalendarEvent{
		{Subject: "Focus", Start: weekday(0, 14, 0).UnixMilli(), End: weekday(0, 16, 0).UnixMilli(), ShowAs: "busy", Response: "organizer", Focus: true},
		{Subject: "Break", Start: weekday(1, 9, 0).UnixMilli(), End: weekday(1, 15, 0).UnixMilli(), ShowAs: "busy", Response: "organizer", Break: true},
		{Subject: "Holiday", Start: weekday(2, 0, 0).UnixMilli(), End: weekday(3, 0, 0).UnixMilli(), AllDay: true},
		{Subject: "Declined", Start: weekday(3, 9, 0).UnixMilli(), End: weekday(3, 17, 0).UnixMilli(), Response: "declined", ShowAs: "busy"},
	}
	w := planOf(events, weekday(-1, 18, 0))
	monday := dayIn(t, w, "2026-11-23")
	if len(monday.Blocks) != 0 || monday.Words != "Monday is clear: your focus block is already booked." {
		t.Errorf("Monday: %s %q", blocksOf(monday), monday.Words)
	}
	if got := blocksOf(dayIn(t, w, "2026-11-24")); got != "focus 15:00-17:00" {
		t.Errorf("Tuesday: a kept break is booked time: %s", got)
	}
	if got := blocksOf(dayIn(t, w, "2026-11-25")); got != "focus 09:00-11:00" {
		t.Errorf("Wednesday: an all-day event holds nothing: %s", got)
	}
	if got := blocksOf(dayIn(t, w, "2026-11-26")); got != "focus 09:00-11:00" {
		t.Errorf("Thursday: a declined meeting holds nothing: %s", got)
	}
}

func TestMadeMidweekItPlansTheRestOfTheWeekOnly(t *testing.T) {
	w := planOf(nil, weekday(2, 10, 5))
	if len(w.Days) != 3 || w.Days[0].Date != "2026-11-25" {
		t.Fatalf("days %+v", w.Days)
	}
	if got := blocksOf(w.Days[0]); got != "focus 10:15-12:15" {
		t.Errorf("Wednesday from now: %s", got)
	}
	if !strings.HasPrefix(w.Words, "The rest of the week: focus time on Wednesday, Thursday and Friday") {
		t.Errorf("week %q", w.Words)
	}
	if w := planOf(nil, weekday(-1, 18, 0)); w.Words != "The week ahead: focus time on Monday, Tuesday, Wednesday, Thursday and Friday." {
		t.Errorf("week ahead %q", w.Words)
	}
}

func TestEachFocusBlockIsForWhatWaits(t *testing.T) {
	uses := []WeaveUse{
		{Kind: "review", For: "the review of races", Link: "/dashboard/goals/4?part=a&view=review", Minutes: 20, Since: 1},
		{Kind: "promise", For: "the promise to Anna{due}: send the deck", Link: PromisesLink, Minutes: 10, Due: "2026-11-26"},
		{Kind: "promise", For: "the promise to Bo{due}: check the numbers", Link: PromisesLink, Minutes: 10, Due: "2026-11-23"},
		{Kind: "night", For: "what ran overnight on Login", Link: "/dashboard/goals/9", Minutes: 20},
	}
	w := planOf(nil, weekday(-1, 18, 0), uses...)
	var got []string
	for _, d := range w.Days {
		got = append(got, d.Blocks[0].For)
	}
	want := []string{"the promise to Bo due Monday: check the numbers", "the promise to Anna due Thursday: send the deck", "the review of races", "", ""}
	// Monday: Bo's promise (due Monday) comes before what ran overnight; the night's goal fits Monday only.
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("uses\n got %q\nwant %q", got, want)
	}
	// Monday is packed: a promise due Monday is not given to Tuesday, where it would be late.
	packed := planOf([]CalendarEvent{meetingOn(0, 9, 0, 17, 0)}, weekday(-1, 18, 0), uses[2])
	if got := packed.Days[1].Blocks[0].For; got != "" {
		t.Errorf("late promise offered: %q", got)
	}
	// Made on Friday, one due Thursday is late already, and offered first.
	if late := planOf(nil, weekday(4, 9, 0), uses[1]); late.Days[0].Blocks[0].For != "the promise to Anna due Thursday: send the deck" {
		t.Errorf("a late promise: %q", late.Days[0].Blocks[0].For)
	}
}

func TestTheWarmerLineKeepsToThePlainOne(t *testing.T) {
	plain := "The week ahead: focus time on Monday and Wednesday; Tuesday is heavy."
	for _, c := range []struct{ reply, want string }{
		{"A kind week: quiet focus on Monday and Wednesday, and a full Tuesday to go easy on.", "A kind week: quiet focus on Monday and Wednesday, and a full Tuesday to go easy on."},
		{"Focus on Monday! Tuesday is heavy.", ""},
		{"Focus on Monday, Tuesday is heavy.", ""},
		{"Focus for 2 hours on Monday and Wednesday, Tuesday is heavy.", ""},
		{"", ""},
	} {
		if got := warmer(plain, c.reply); got != c.want {
			t.Errorf("%q: %q", c.reply, got)
		}
	}
}

func TestTheGapSeesAFocusBlockAsBusy(t *testing.T) {
	focus := CalendarEvent{Subject: "Focus", Start: weekday(0, 10, 0).UnixMilli(), End: weekday(0, 11, 0).UnixMilli(), ShowAs: "busy", Response: "organizer", Focus: true}
	if _, free := FreeUntil([]CalendarEvent{focus}, weekday(0, 10, 30), weaveZone, nineToFive); free {
		t.Error("a gap offered inside a focus block")
	}
	until, free := FreeUntil([]CalendarEvent{focus}, weekday(0, 9, 10), weaveZone, nineToFive)
	if !free || !until.Equal(weekday(0, 10, 0)) {
		t.Errorf("before it: %v %v", until, free)
	}
	if SavePointMeeting(CalendarEvent{ID: "f", Start: focus.Start, End: focus.End, Focus: true}) {
		t.Error("a focus block is not a meeting to keep your place through")
	}
}

func TestAFocusBlockIsKnownByItsCategory(t *testing.T) {
	start := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	page, _ := json.Marshal(map[string]any{"value": []any{graphEvent("f", "Focus", start, start.Add(2*time.Hour), FocusCategory)}})
	events, _ := GraphEvents(page)
	if len(events) != 1 || !events[0].Focus || events[0].Break {
		t.Fatalf("got %+v", events)
	}
}

// weaveNorn is Norn with a work calendar read at now, no goals, and a model that is not there.
func weaveNorn(t *testing.T, now time.Time, events []CalendarEvent) *Norn {
	n, _ := newNorn(t)
	n.Weave.now = func() time.Time { return now }
	n.Weave.Rephrase = func(context.Context, []llm.Message, llm.Options) (string, error) { return "", errors.New("no model") }
	n.Gap.Goals = func(context.Context) []GapGoal { return nil }
	n.Gap.Day = nineToFive
	n.Accounts.Work.Set(CalendarSnapshot{Events: events, At: now.UnixMilli()})
	return n
}

func published(n *Norn, kind string) int {
	count := 0
	for _, e := range n.Events.kept {
		if e.Kind == kind {
			count++
		}
	}
	return count
}

func TestThePlanIsMadeOnceAWeekAndSaysSo(t *testing.T) {
	ctx := context.Background()
	now := weekday(-1, 17, 0)
	n := weaveNorn(t, now, []CalendarEvent{meetingOn(0, 9, 0, 10, 0)})
	if err := n.weaveTick(ctx); err != nil {
		t.Fatal(err)
	}
	if w, _ := n.loadWeave(ctx); w != nil {
		t.Fatalf("made before Sunday 18:00: %+v", w)
	}
	now = weekday(-1, 18, 2)
	n.Weave.now = func() time.Time { return now }
	n.Accounts.Work.Set(CalendarSnapshot{Events: []CalendarEvent{meetingOn(0, 9, 0, 10, 0)}, At: now.UnixMilli()})
	n.Weave.Rephrase = func(context.Context, []llm.Message, llm.Options) (string, error) {
		return "A gentle week ahead, with room to focus on Monday, Tuesday, Wednesday, Thursday and Friday.", nil
	}
	if err := n.weaveTick(ctx); err != nil {
		t.Fatal(err)
	}
	w, _ := n.loadWeave(ctx)
	if w == nil || w.Week != "2026-11-23" || len(w.Days) != 5 || !w.Waiting {
		t.Fatalf("plan %+v", w)
	}
	if w.Words != "A gentle week ahead, with room to focus on Monday, Tuesday, Wednesday, Thursday and Friday." {
		t.Errorf("words %q", w.Words)
	}
	if published(n, "weave.ready") != 1 {
		t.Fatalf("weave.ready sent %d times", published(n, "weave.ready"))
	}
	var ready WeaveReady
	_ = json.Unmarshal(n.Events.kept[len(n.Events.kept)-1].Data, &ready)
	if ready.Week != "2026-11-23" || ready.Link != WeaveLink || ready.Words != w.Words {
		t.Errorf("ready %+v", ready)
	}

	// Monday morning: the same week, never twice.
	now = weekday(0, 8, 0)
	n.Accounts.Work.Set(CalendarSnapshot{Events: []CalendarEvent{meetingOn(0, 9, 0, 12, 0)}, At: now.UnixMilli()})
	if err := n.weaveTick(ctx); err != nil {
		t.Fatal(err)
	}
	if again, _ := n.loadWeave(ctx); again.MadeAt != w.MadeAt || published(n, "weave.ready") != 1 {
		t.Errorf("made twice: %d, %d events", again.MadeAt, published(n, "weave.ready"))
	}

	// Off: nothing is made.
	n2 := weaveNorn(t, weekday(-1, 19, 0), nil)
	_ = n2.Store.SetSetting(ctx, weaveSettingsKey, WeaveSettings{On: false, MakeAt: "18:00", FocusMinutes: 90})
	_ = n2.weaveTick(ctx)
	if w, _ := n2.loadWeave(ctx); w != nil {
		t.Error("made while off")
	}
}

func TestOnMondayItCatchesUpAndWaitsForTheCalendar(t *testing.T) {
	ctx := context.Background()
	now := weekday(0, 8, 30)
	n := weaveNorn(t, now, nil)
	n.Accounts.Work.Clear()
	if err := n.weaveTick(ctx); err != nil {
		t.Fatalf("no calendar is no failure: %v", err)
	}
	if w, _ := n.loadWeave(ctx); w != nil {
		t.Fatal("made without a calendar")
	}
	n.Accounts.Work.Set(CalendarSnapshot{Events: []CalendarEvent{}, At: now.UnixMilli()})
	_ = n.weaveTick(ctx)
	w, _ := n.loadWeave(ctx)
	if w == nil || w.Week != "2026-11-23" || len(w.Days) != 5 || !strings.HasPrefix(w.Words, "This week:") {
		t.Fatalf("plan %+v", w)
	}
}

func TestBookTheWeek(t *testing.T) {
	n, p := newNorn(t)
	now := time.Now().Truncate(time.Minute)
	n.Weave.Rephrase = nil
	meetingStart := now.Add(2 * time.Hour)
	calendar := []any{graphEvent("m1", "Planning", meetingStart, meetingStart.Add(time.Hour))}
	n.Accounts.Microsoft.stored = &microsoftStored{Scopes: MicrosoftScopes{Calendar: true}, RefreshToken: "r", AccessToken: "a", ExpiresAt: now.Add(time.Hour).UnixMilli()}
	n.Accounts.get = func(context.Context, string, string, map[string]string) (int, []byte, error) {
		page, _ := json.Marshal(map[string]any{"value": calendar})
		return http.StatusOK, page, nil
	}
	var sent []map[string]any
	n.Accounts.post = func(_ context.Context, _, _ string, body []byte) (int, []byte, error) {
		var event map[string]any
		_ = json.Unmarshal(body, &event)
		sent = append(sent, event)
		start, _ := time.Parse("2006-01-02T15:04:05", event["start"].(map[string]any)["dateTime"].(string))
		end, _ := time.Parse("2006-01-02T15:04:05", event["end"].(map[string]any)["dateTime"].(string))
		made := graphEvent(fmt.Sprintf("e%d", len(sent)), event["subject"].(string), start, end, event["categories"].([]any)[0].(string))
		calendar = append(calendar, made)
		answer, _ := json.Marshal(made)
		return http.StatusCreated, answer, nil
	}
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	weave := Weave{Week: "2026-11-23", MadeAt: now.UnixMilli(), Words: "This week.", Days: []WeaveDay{{Date: now.Format(dayLayout), Words: "Today.", Blocks: []WeaveBlock{
		{ID: "focus-a", Kind: "focus", Start: ms(4 * time.Hour), End: ms(6 * time.Hour), For: "the review of races"},
		{ID: "break-b", Kind: "break", Start: ms(3 * time.Hour), End: ms(3*time.Hour + 30*time.Minute)},
		{ID: "focus-taken", Kind: "focus", Start: ms(2*time.Hour + 30*time.Minute), End: ms(4 * time.Hour)},
		{ID: "break-skip", Kind: "break", Start: ms(7 * time.Hour), End: ms(7*time.Hour + 15*time.Minute)},
	}}}}
	if err := n.Store.SetSetting(context.Background(), weaveKey, weave); err != nil {
		t.Fatal(err)
	}

	if status, _ := call(t, p, http.MethodPost, "/v1/weave/blocks/focus-a/skip", nil); status != http.StatusOK {
		t.Fatalf("skip: %d", status)
	}
	if status, _ := call(t, p, http.MethodPost, "/v1/weave/blocks/nope/skip", nil); status != http.StatusNotFound {
		t.Fatalf("skip unknown: %d", status)
	}
	// Booking one by name books it even after a skip; all books the rest that is open.
	status, body := call(t, p, http.MethodPost, "/v1/weave/book", map[string]any{"ids": []string{"focus-a"}})
	if status != http.StatusOK || len(sent) != 1 {
		t.Fatalf("book one: %d %v", status, body)
	}
	if sent[0]["subject"] != FocusSubject || sent[0]["showAs"] != "busy" || !strings.Contains(fmt.Sprint(sent[0]["categories"]), FocusCategory) ||
		!strings.HasPrefix(sent[0]["transactionId"].(string), "yggdrasil-focus-") {
		t.Errorf("asked Graph for %v", sent[0])
	}
	if status, _ := call(t, p, http.MethodPost, "/v1/weave/blocks/break-skip/skip", nil); status != http.StatusOK {
		t.Fatalf("skip: %d", status)
	}
	status, body = call(t, p, http.MethodPost, "/v1/weave/book", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("book all: %d %v", status, body)
	}
	results := map[string]map[string]any{}
	for _, r := range body.(map[string]any)["results"].([]any) {
		results[r.(map[string]any)["id"].(string)] = r.(map[string]any)
	}
	if results["break-b"]["booked"] != true || results["break-b"]["eventId"] != "e2" {
		t.Errorf("break: %v", results["break-b"])
	}
	if results["focus-taken"]["booked"] == true || !strings.Contains(fmt.Sprint(results["focus-taken"]["error"]), "no longer free") {
		t.Errorf("a slot a meeting took: %v", results["focus-taken"])
	}
	if _, asked := results["break-skip"]; asked || len(sent) != 2 || sent[1]["subject"] != BreakSubject {
		t.Errorf("a skipped block was booked, or the break was not: %v %d", results, len(sent))
	}
	// A double click: the booked ones are not sent to Graph again.
	status, body = call(t, p, http.MethodPost, "/v1/weave/book", map[string]any{"ids": []string{"focus-a", "break-b"}})
	if status != http.StatusOK || len(sent) != 2 {
		t.Fatalf("again: %d %v, %d posts", status, body, len(sent))
	}
	// The calendar now says focus.
	_, cal := call(t, p, http.MethodGet, "/v1/calendar", nil)
	focus := false
	for _, e := range cal.(map[string]any)["events"].([]any) {
		focus = focus || e.(map[string]any)["focus"] == true
	}
	if !focus {
		t.Errorf("no focus block in the calendar: %v", cal)
	}
	_, got := call(t, p, http.MethodGet, "/v1/weave", nil)
	plan := got.(map[string]any)
	if plan["waiting"] != true { // focus-taken is still open
		t.Errorf("waiting %v", plan["waiting"])
	}
	if status, _ := call(t, p, http.MethodPost, "/v1/weave/blocks/break-b/skip", nil); status != http.StatusConflict {
		t.Errorf("skipping a booked block: %d", status)
	}
	if status, _ := call(t, p, http.MethodPost, "/v1/weave/blocks/focus-taken/skip", nil); status != http.StatusOK {
		t.Fatal("skip")
	}
	if _, got := call(t, p, http.MethodGet, "/v1/weave", nil); got.(map[string]any)["waiting"] != false {
		t.Errorf("still waiting after all booked or skipped")
	}
}

func TestRemakeKeepsWhatIsBookedAndSettingsAreKept(t *testing.T) {
	n, p := newNorn(t)
	now := time.Now()
	n.Weave.Rephrase = nil
	n.Gap.Goals = func(context.Context) []GapGoal { return nil }
	if status, _ := call(t, p, http.MethodPost, "/v1/weave/remake", nil); status != http.StatusConflict {
		t.Fatalf("without a calendar: %d", status)
	}
	if status, body := call(t, p, http.MethodGet, "/v1/weave", nil); status != http.StatusOK || body.(map[string]any)["week"] != "" {
		t.Fatalf("no plan yet: %d %v", status, body)
	}
	n.Accounts.Work.Set(CalendarSnapshot{Events: []CalendarEvent{}, At: now.UnixMilli()})
	monday, _ := WeaveDue(now, n.Zone, 0, n.Gap.Day)
	booked := WeaveBlock{ID: "focus-kept", Kind: "focus", Start: now.Add(time.Hour).UnixMilli(), End: now.Add(2 * time.Hour).UnixMilli(), Booked: true, EventID: "e1"}
	old := Weave{Week: monday.Format(dayLayout), Days: []WeaveDay{}}
	day := now.In(n.Zone)
	if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday {
		old.Days = append(old.Days, WeaveDay{Date: day.Format(dayLayout), Blocks: []WeaveBlock{booked}})
	}
	_ = n.Store.SetSetting(context.Background(), weaveKey, old)
	status, body := call(t, p, http.MethodPost, "/v1/weave/remake", nil)
	if status != http.StatusOK || body.(map[string]any)["week"] != monday.Format(dayLayout) {
		t.Fatalf("remake: %d %v", status, body)
	}

	if status, body := call(t, p, http.MethodPut, "/v1/settings/weave", map[string]any{"makeAt": "19:30", "focusMinutes": 120}); status != http.StatusOK ||
		body.(map[string]any)["makeAt"] != "19:30" || body.(map[string]any)["on"] != true {
		t.Fatalf("settings: %d %v", status, body)
	}
	if status, _ := call(t, p, http.MethodPut, "/v1/settings/weave", map[string]any{"makeAt": "late"}); status != http.StatusBadRequest {
		t.Fatalf("bad time: %d", status)
	}
	if got := n.WeaveSettingsNow(context.Background()); got.FocusMinutes != 120 || got.MakeAt != "19:30" {
		t.Errorf("kept %+v", got)
	}
}

func TestRemakeCarriesBookedBlocks(t *testing.T) {
	w := Weave{Week: "2026-11-23", Days: []WeaveDay{{Date: "2026-11-23", Blocks: []WeaveBlock{{ID: "focus-x", Start: 2}}}}}
	old := Weave{Week: "2026-11-23", Days: []WeaveDay{{Date: "2026-11-23", Blocks: []WeaveBlock{
		{ID: "break-y", Start: 1, Booked: true, EventID: "e"}, {ID: "focus-z", Start: 3}}}}}
	w.keepBooked(&old)
	if got := w.Days[0].Blocks; len(got) != 2 || got[0].ID != "break-y" || got[1].ID != "focus-x" {
		t.Errorf("blocks %+v", got)
	}
}
