package norn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/llm"
)

func stockholm(t *testing.T) *time.Location {
	t.Helper()
	zone, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	return zone
}

func TestParsePromisesDropsBlanksAndRepeats(t *testing.T) {
	got, err := ParsePromises("```json\n" + `{"promises":[{"what":"Send the Q3 deck.","to":"Anna","when":"Friday"},{"what":" ","to":"x","when":""},{"what":"send the  Q3 deck","to":"Anna","when":""},{"what":"Kolla med juristen","to":"","when":"i morgon"}]}` + "\n```")
	if err != nil || len(got) != 2 || got[0].What != "Send the Q3 deck" || got[0].When != "Friday" || got[1].What != "Kolla med juristen" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := ParsePromises("I found none."); err == nil {
		t.Fatal("prose passed as promises")
	}
	many := `{"promises":[`
	for i := range 8 {
		if i > 0 {
			many += ","
		}
		many += `{"what":"Thing ` + string(rune('a'+i)) + `","to":"","when":""}`
	}
	if got, _ := ParsePromises(many + "]}"); len(got) != maxPromisesPerMessage {
		t.Fatalf("kept %d", len(got))
	}
}

func TestResolveDueTurnsWordsIntoADay(t *testing.T) {
	zone := stockholm(t)
	thursday := time.Date(2026, 11, 19, 15, 0, 0, 0, zone)
	friday := time.Date(2026, 11, 20, 9, 0, 0, 0, zone)
	saturday := time.Date(2026, 11, 21, 9, 0, 0, 0, zone)
	cases := []struct {
		when string
		sent time.Time
		want string
	}{
		{"Friday", thursday, "2026-11-20"},
		{"by Friday", thursday, "2026-11-20"},
		{"på fredag", thursday, "2026-11-20"},
		{"Friday", friday, "2026-11-27"},
		{"next Friday", thursday, "2026-11-27"},
		{"nästa måndag", thursday, "2026-11-23"},
		{"Monday", thursday, "2026-11-23"},
		{"tomorrow", thursday, "2026-11-20"},
		{"i morgon", thursday, "2026-11-20"},
		{"imorgon bitti", thursday, "2026-11-20"},
		{"day after tomorrow", thursday, "2026-11-21"},
		{"today", thursday, "2026-11-19"},
		{"end of day", thursday, "2026-11-19"},
		{"by end of week", thursday, "2026-11-20"},
		{"i veckan", saturday, "2026-11-27"},
		{"end of next week", thursday, "2026-11-27"},
		{"24/11", thursday, "2026-11-24"},
		{"Nov 24", thursday, "2026-11-24"},
		{"the 24th of November", thursday, "2026-11-24"},
		{"den 3 december", thursday, "2026-12-03"},
		{"2 jan", thursday, "2027-01-02"},
		{"2026-12-01", thursday, "2026-12-01"},
		{"soon", thursday, ""},
		{"next week", thursday, ""},
		{"", thursday, ""},
		{"31/2", thursday, ""},
	}
	for _, c := range cases {
		if got := ResolveDue(c.when, c.sent, zone); got != c.want {
			t.Errorf("%q on %s: got %q, want %q", c.when, c.sent.Format("Mon"), got, c.want)
		}
	}
	// Late on a Thursday in Stockholm is still Thursday, whatever UTC says.
	late := time.Date(2026, 11, 19, 23, 30, 0, 0, zone)
	if got := ResolveDue("tomorrow", late.UTC(), zone); got != "2026-11-20" {
		t.Fatalf("late: %s", got)
	}
}

func TestReminderDueOnTheWorkingDayBeforeOrOnTheDay(t *testing.T) {
	zone := stockholm(t)
	at := func(day, hour int) time.Time { return time.Date(2026, 11, day, hour, 0, 0, 0, zone) }
	// Due Monday the 23rd, made Wednesday the 18th.
	p := Promise{State: "open", Due: "2026-11-23", CreatedAt: at(18, 10).UnixMilli()}
	if _, ok := ReminderDue(p, at(19, 9), zone); ok {
		t.Fatal("Thursday is too early")
	}
	if _, ok := ReminderDue(p, at(20, 7), zone); ok {
		t.Fatal("before eight")
	}
	if by, ok := ReminderDue(p, at(20, 9), zone); !ok || by != "Monday" {
		t.Fatalf("Friday: %q %v", by, ok)
	}
	if by, ok := ReminderDue(p, at(23, 9), zone); !ok || by != "today" {
		t.Fatalf("missed Friday, Monday: %q %v", by, ok)
	}
	if _, ok := ReminderDue(p, at(24, 9), zone); ok {
		t.Fatal("after its day")
	}
	p.RemindedAt = at(20, 9).UnixMilli()
	if _, ok := ReminderDue(p, at(23, 9), zone); ok {
		t.Fatal("reminded twice")
	}
	// Due Friday the 20th, made Wednesday: Thursday says tomorrow.
	q := Promise{State: "open", Due: "2026-11-20", CreatedAt: at(18, 10).UnixMilli()}
	if by, ok := ReminderDue(q, at(19, 8), zone); !ok || by != "tomorrow" {
		t.Fatalf("Thursday: %q %v", by, ok)
	}
	// Made on the day before: not at once, but on the day.
	q.CreatedAt = at(19, 14).UnixMilli()
	if _, ok := ReminderDue(q, at(19, 15), zone); ok {
		t.Fatal("reminded right after it was made")
	}
	if by, ok := ReminderDue(q, at(20, 9), zone); !ok || by != "today" {
		t.Fatalf("on the day: %q %v", by, ok)
	}
	// Made on its own day: two hours first.
	q.CreatedAt = at(20, 9).UnixMilli()
	if _, ok := ReminderDue(q, at(20, 10), zone); ok {
		t.Fatal("same day, too soon")
	}
	if _, ok := ReminderDue(Promise{State: "open", CreatedAt: at(18, 10).UnixMilli()}, at(20, 9), zone); ok {
		t.Fatal("no day, no reminder")
	}
	if _, ok := ReminderDue(Promise{State: "done", Due: "2026-11-20", CreatedAt: at(18, 10).UnixMilli()}, at(19, 9), zone); ok {
		t.Fatal("done is not reminded")
	}
}

// fakeModel answers each call by what it asks: a kept question or a promise question.
type fakeModel struct {
	promises, kept string
	asked          []string
	fail           bool
}

func (f *fakeModel) chat(_ context.Context, messages []llm.Message, options llm.Options) (string, error) {
	f.asked = append(f.asked, options.Purpose+": "+messages[len(messages)-1].Content)
	if f.fail {
		return "", errors.New("down")
	}
	if options.Purpose == "promise kept" {
		return f.kept, nil
	}
	return f.promises, nil
}

func promisesNorn(t *testing.T, now time.Time) (*Norn, *fakeModel) {
	t.Helper()
	n, _ := newNorn(t)
	model := &fakeModel{promises: `{"promises":[]}`, kept: `{"done":[]}`}
	n.Promises.Model = model.chat
	n.Promises.now = func() time.Time { return now }
	return n, model
}

func TestAMessageGivesItsPromisesOnceWithTheDayAndThePerson(t *testing.T) {
	zone := stockholm(t)
	now := time.Date(2026, 11, 19, 15, 0, 0, 0, zone)
	n, model := promisesNorn(t, now)
	ctx := context.Background()
	model.promises = `{"promises":[{"what":"Send the Q3 deck","to":"Anna","when":"Friday"},{"what":"Check with legal","to":"","when":"soon"}]}`
	m := SentMessage{ID: "outlook:1", Key: "outlook:1", Source: "outlook", To: []Recipient{{Name: "Anna Berg", Address: "anna@example.com"}, {Name: "Bo Ek", Address: "bo@example.com"}},
		Subject: "Q3", Text: "Thanks! I'll send the deck Friday, and I'll check with legal soon.", At: now.Add(-time.Hour).UnixMilli(), Link: "https://outlook/1", Thread: true}
	if _, err := n.Promises.Read(ctx, m); err != nil {
		t.Fatal(err)
	}
	if len(model.asked) != 1 || !strings.Contains(model.asked[0], "I'll send the deck Friday") || !strings.Contains(model.asked[0], "Anna Berg <anna@example.com>") {
		t.Fatalf("asked %q", model.asked)
	}
	// The same message read again (a restart before it was marked) adds nothing.
	if _, err := n.Promises.Read(ctx, m); err != nil {
		t.Fatal(err)
	}
	open, err := n.Promises.List(ctx, "open")
	if err != nil || len(open) != 2 {
		t.Fatalf("%+v %v", open, err)
	}
	deck := open[0]
	if deck.What != "Send the Q3 deck" || deck.Due != "2026-11-20" || deck.To != "Anna Berg" || deck.ToAddress != "anna@example.com" || !deck.Thread || deck.Link != "https://outlook/1" {
		t.Fatalf("deck %+v", deck)
	}
	if legal := open[1]; legal.Due != "" || legal.To != "" {
		t.Fatalf("legal %+v", legal)
	}
	if !n.Promises.seen(ctx, "outlook:1") {
		t.Fatal("not marked read")
	}
}

func TestAMessageWithoutPromiseWordsNeverReachesTheModel(t *testing.T) {
	now := time.Now()
	n, model := promisesNorn(t, now)
	if _, err := n.Promises.Read(context.Background(), SentMessage{ID: "slack:c:1", Source: "slack", Text: "Thanks, looks good!", At: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if len(model.asked) != 0 {
		t.Fatalf("asked %q", model.asked)
	}
}

func TestMeetingNotesSavedTwiceGiveEachPromiseOnce(t *testing.T) {
	now := time.Date(2026, 11, 19, 15, 0, 0, 0, stockholm(t))
	n, model := promisesNorn(t, now)
	model.promises = `{"promises":[{"what":"Skicka offerten","to":"Peter","when":"på fredag"}]}`
	meeting := Meeting{Subject: "Budget", Start: now.Add(-2 * time.Hour).UnixMilli(), Attendees: []string{"Peter Lund", "Ada Ek"}}
	n.OfferNotes(meeting, "- Jag skickar offerten till Peter på fredag", "")
	n.OfferNotes(meeting, "- Jag skickar offerten till Peter på fredag\n- Ada tar budgeten", "obsidian://x")
	if err := n.Promises.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(model.asked) != 2 {
		t.Fatalf("asked %d times", len(model.asked))
	}
	open, _ := n.Promises.List(context.Background(), "open")
	if len(open) != 1 || open[0].Source != "meeting" || open[0].To != "Peter Lund" || open[0].Due != "2026-11-20" || open[0].Thread {
		t.Fatalf("%+v", open)
	}
}

func TestALaterMessageToTheSamePersonKeepsAPromise(t *testing.T) {
	zone := stockholm(t)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, zone)
	n, model := promisesNorn(t, now)
	ctx := context.Background()
	anna := []Recipient{{Name: "Anna Berg", Address: "anna@example.com"}}
	model.promises = `{"promises":[{"what":"Send the Q3 deck","to":"Anna","when":"Friday"}]}`
	if _, err := n.Promises.Read(ctx, SentMessage{ID: "outlook:1", Key: "outlook:1", Source: "outlook", To: anna, Text: "I'll send it Friday", At: now.Add(-24 * time.Hour).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	open, _ := n.Promises.List(ctx, "open")
	id := open[0].ID
	model.asked = nil
	// To someone else: not asked about Anna's promise.
	if _, err := n.Promises.Read(ctx, SentMessage{ID: "outlook:2", Key: "outlook:2", Source: "outlook", To: []Recipient{{Name: "Bo", Address: "bo@example.com"}}, Text: "Here it is", At: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if len(model.asked) != 0 {
		t.Fatalf("asked %q", model.asked)
	}
	// To Anna: asked, and an id it was not asked about is ignored.
	model.kept = `{"done":[{"id":` + jsonInt(id) + `,"why":"The deck is attached."},{"id":999,"why":"x"}]}`
	if _, err := n.Promises.Read(ctx, SentMessage{ID: "outlook:3", Key: "outlook:3", Source: "outlook", To: anna, Text: "Here is the deck.", At: now.UnixMilli(), Link: "https://outlook/3"}); err != nil {
		t.Fatal(err)
	}
	if len(model.asked) != 1 || !strings.HasPrefix(model.asked[0], "promise kept:") || !strings.Contains(model.asked[0], "Send the Q3 deck") {
		t.Fatalf("asked %q", model.asked)
	}
	done, _ := n.Promises.List(ctx, "done")
	if len(done) != 1 || done[0].DoneBy != "norn" || done[0].DoneWhy != "The deck is attached." || done[0].DoneLink != "https://outlook/3" {
		t.Fatalf("%+v", done)
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestAFailingModelLeavesTheMessageForTheNextPass(t *testing.T) {
	now := time.Now()
	n, model := promisesNorn(t, now)
	model.fail = true
	n.Promises.Offer(SentMessage{ID: "teams:c:1", Key: "teams:c:1", Source: "teams", Text: "I'll fix it", At: now.UnixMilli()},
		SentMessage{ID: "teams:c:2", Key: "teams:c:2", Source: "teams", Text: "I'll fix that too", At: now.UnixMilli() + 1})
	if err := n.Promises.pass(context.Background()); err == nil {
		t.Fatal("a failed model call passed")
	}
	if len(model.asked) != 1 {
		t.Fatalf("asked %d times after a failure", len(model.asked))
	}
	model.fail = false
	if err := n.Promises.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !n.Promises.seen(context.Background(), "teams:c:1") || !n.Promises.seen(context.Background(), "teams:c:2") {
		t.Fatal("the offered messages were not read on the next pass")
	}
}

func promiseEvents(n *Norn) []PromiseDue {
	n.Events.mu.Lock()
	defer n.Events.mu.Unlock()
	var out []PromiseDue
	for _, e := range n.Events.kept {
		if e.Kind == "promise.due" {
			var due PromiseDue
			_ = json.Unmarshal(e.Data, &due)
			out = append(out, due)
		}
	}
	return out
}

func TestAPromiseIsRemindedOnce(t *testing.T) {
	zone := stockholm(t)
	made := time.Date(2026, 11, 18, 10, 0, 0, 0, zone)
	n, _ := promisesNorn(t, made)
	n.Config.DashboardURL = "http://localhost:17420"
	ctx := context.Background()
	if err := n.Promises.init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Promises.keep(ctx, Promise{What: "Send the Q3 deck", To: "Anna Berg", Due: "2026-11-20", Source: "meeting", Message: "meeting:1:x",
		CreatedAt: made.UnixMilli(), ChangedAt: made.UnixMilli(), Key: "meeting:1", Norm: "send the q3 deck"}); err != nil {
		t.Fatal(err)
	}
	thursday := time.Date(2026, 11, 19, 9, 0, 0, 0, zone)
	for _, at := range []time.Time{made, thursday, thursday.Add(10 * time.Minute), thursday.Add(24 * time.Hour)} {
		n.Promises.now = func() time.Time { return at }
		if err := n.Promises.remind(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got := promiseEvents(n)
	if len(got) != 1 || got[0].By != "tomorrow" || got[0].To != "Anna Berg" || got[0].What != "Send the Q3 deck" || got[0].Draft ||
		got[0].Link != "http://localhost:17420/dashboard/inbox#promises" {
		t.Fatalf("%+v", got)
	}
	open, _ := n.Promises.List(ctx, "open")
	if open[0].RemindedAt == 0 {
		t.Fatal("not kept as reminded")
	}
}

func TestFollowUpItemAsksForAFollowUp(t *testing.T) {
	item := FollowUpItem(Promise{ID: 7, What: "Send the Q3 deck", To: "Anna", Due: "2026-11-20", Source: "teams", Subject: "Anna"})
	if item.ID != "promise:7" || item.Source != "teams" || item.From != "Anna" || !strings.Contains(item.Text, "Send the Q3 deck, by 2026-11-20") {
		t.Fatalf("%+v", item)
	}
	if gone := (&Inbox{feedAt: 1, sources: []SourceStatus{{Source: "teams", OK: true}}}).Gone([]string{item.ID}); len(gone) != 0 {
		t.Fatal("a follow-up draft would be thrown away as answered")
	}
}

func TestPromiseRoutes(t *testing.T) {
	n, p := newNorn(t)
	ctx := context.Background()
	now := time.Now()
	if err := n.Promises.init(ctx); err != nil {
		t.Fatal(err)
	}
	n.Promises.keep(ctx, Promise{What: "Send the deck", To: "Anna", Source: "outlook", Message: "outlook:1", CreatedAt: now.UnixMilli(), ChangedAt: now.UnixMilli(), Key: "outlook:1", Norm: "send the deck"})
	contracttest.Run(t, p)

	code, body := call(t, p, "GET", "/v1/promises", nil)
	list := body.(map[string]any)["promises"].([]any)
	if code != http.StatusOK || len(list) != 1 {
		t.Fatalf("%d %v", code, body)
	}
	id := jsonInt(int64(list[0].(map[string]any)["id"].(float64)))
	if code, _ := call(t, p, "GET", "/v1/promises?state=later", nil); code != http.StatusBadRequest {
		t.Fatalf("bad state: %d", code)
	}
	if code, _ := call(t, p, "PATCH", "/v1/promises/"+id, map[string]any{"due": "Friday"}); code != http.StatusBadRequest {
		t.Fatalf("bad due: %d", code)
	}
	if code, _ := call(t, p, "PATCH", "/v1/promises/999", map[string]any{"state": "done"}); code != http.StatusNotFound {
		t.Fatalf("unknown: %d", code)
	}
	code, body = call(t, p, "PATCH", "/v1/promises/"+id, map[string]any{"state": "done", "due": "2026-11-27"})
	changed := body.(map[string]any)
	if code != http.StatusOK || changed["state"] != "done" || changed["doneBy"] != "you" || changed["due"] != "2026-11-27" {
		t.Fatalf("%d %v", code, body)
	}
	if _, body := call(t, p, "GET", "/v1/promises?state=done", nil); len(body.(map[string]any)["promises"].([]any)) != 1 {
		t.Fatalf("done: %v", body)
	}
	code, body = call(t, p, "PATCH", "/v1/promises/"+id, map[string]any{"state": "open"})
	if code != http.StatusOK || body.(map[string]any)["doneBy"] != nil {
		t.Fatalf("reopen: %d %v", code, body)
	}
	code, body = call(t, p, "PUT", "/v1/promises/"+id+"/dev", map[string]any{"dev": true})
	if code != http.StatusOK || body.(map[string]any)["dev"] != true {
		t.Fatalf("dev: %d %v", code, body)
	}
	// Only the owner changes a promise.
	request := httptest.NewRequest("PATCH", "/v1/promises/"+id, strings.NewReader(`{"state":"dropped"}`))
	request.Header.Set("Authorization", "Bearer "+contracttest.Sender)
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("a part changed it: %d", recorder.Code)
	}
}

func TestSentMessagesAreReadAsYourOwnWords(t *testing.T) {
	me := Me{ID: "u-me", Address: "me@example.com", DisplayName: "Me Myself"}
	chats := `{"value":[
		{"id":"c1","chatType":"oneOnOne","webUrl":"https://teams/c1","members":[{"displayName":"Me Myself","userId":"u-me"},{"displayName":"Anna Berg","userId":"u-anna"}],
		 "lastMessagePreview":{"id":"m1","createdDateTime":"2026-11-19T10:00:00Z","body":{"content":"<p>I&#39;ll send it Friday</p>"},"from":{"user":{"id":"u-me","displayName":"Me Myself"}}}},
		{"id":"c2","chatType":"oneOnOne","members":[],"lastMessagePreview":{"id":"m2","createdDateTime":"2026-11-19T10:00:00Z","body":{"content":"hi"},"from":{"user":{"id":"u-anna"}}}}]}`
	own := OwnChatMessages([]byte(chats), me)
	if len(own) != 1 || own[0].ID != "teams:c1:m1" || own[0].Text != "I'll send it Friday" || len(own[0].To) != 1 || own[0].To[0].Address != "u-anna" || own[0].Subject != "Anna Berg" {
		t.Fatalf("%+v", own)
	}
	if len(ChatItems([]byte(chats), me, time.Date(2026, 11, 19, 10, 5, 0, 0, time.UTC))) != 1 {
		t.Fatal("the inbox should still see only the other chat")
	}

	mail := `{"value":[{"id":"a1","subject":"Q3","sentDateTime":"2026-11-19T10:00:00Z","webLink":"https://outlook/a1",
		"toRecipients":[{"emailAddress":{"name":"Anna Berg","address":"Anna@Example.com"}}],
		"uniqueBody":{"contentType":"text","content":"I'll send the deck Friday.\n\nFrom: Anna Berg\nSent: Thursday\nCan you send the deck?"}}]}`
	sent := SentMailItems([]byte(mail))
	if len(sent) != 1 || sent[0].Text != "I'll send the deck Friday." || sent[0].To[0].Address != "anna@example.com" || !sent[0].Thread {
		t.Fatalf("%+v", sent)
	}
	if !strings.Contains(sentMailPath(time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)), "sentitems/messages?$filter=sentDateTime%20ge%202026-11-16T00:00:00Z") {
		t.Fatal(sentMailPath(time.Now()))
	}

	plain := base64.URLEncoding.EncodeToString([]byte("Jag skickar den på fredag.\n\nDen tors 19 nov. 2026 skrev Anna <anna@example.com>:\n> Kan du skicka?"))
	gmail := `{"id":"g1","threadId":"t1","internalDate":"1795600000000","snippet":"x","payload":{"mimeType":"multipart/alternative",
		"headers":[{"name":"To","value":"\"Anna Berg\" <anna@example.com>"},{"name":"Subject","value":"Offert"}],
		"parts":[{"mimeType":"text/html","body":{"data":""}},{"mimeType":"text/plain","body":{"data":"` + plain + `"}}]}}`
	got := GmailSentItem([]byte(gmail))
	if got == nil || got.Text != "Jag skickar den på fredag." || got.To[0].Name != "Anna Berg" || got.Link != "https://mail.google.com/mail/#sent/t1" {
		t.Fatalf("%+v", got)
	}

	slack := OwnSlackMessages(SlackChannel{ID: "D1", IsIM: true, User: "U2"}, []SlackMessage{{User: "U1", Text: "I'll check with <@U2>", TS: "1795600000.000100"}, {User: "U2", Text: "ok", TS: "1795600001.000100"}},
		SlackMe{UserID: "U1", URL: "https://x.slack.com"}, map[string]string{"U2": "Anna"}, map[string]string{"U2": "anna@example.com"})
	if len(slack) != 1 || slack[0].To[0].Address != "anna@example.com" || !strings.Contains(slack[0].Text, "@Anna") {
		t.Fatalf("%+v", slack)
	}
}

func TestMatchRecipient(t *testing.T) {
	to := []Recipient{{Name: "Anna Berg", Address: "anna@example.com"}, {Name: "Anna Lund", Address: "al@example.com"}, {Name: "Bo Ek", Address: "bo@example.com"}}
	if r, ok := matchRecipient("Bo", to); !ok || r.Address != "bo@example.com" {
		t.Fatal("first name")
	}
	if _, ok := matchRecipient("Anna", to); ok {
		t.Fatal("two Annas is not one")
	}
	if r, ok := matchRecipient("anna berg", to); !ok || r.Address != "anna@example.com" {
		t.Fatal("full name")
	}
	if r, ok := matchRecipient("", to[:1]); !ok || r.Name != "Anna Berg" {
		t.Fatal("the only one")
	}
}

func TestAFollowUpCanBeDraftedOnlyForAnOpenPromiseMadeInAThread(t *testing.T) {
	n, _ := newNorn(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if err := n.Promises.init(ctx); err != nil {
		t.Fatal(err)
	}
	n.Promises.keep(ctx, Promise{What: "Send the deck", To: "Anna", Source: "teams", Thread: true, Message: "teams:c:1", CreatedAt: now, ChangedAt: now, Key: "teams:c:1", Norm: "send the deck"})
	n.Promises.keep(ctx, Promise{What: "Ring Peter", To: "Peter", Source: "meeting", Message: "meeting:1:x", CreatedAt: now, ChangedAt: now, Key: "meeting:1", Norm: "ring peter"})
	if item, ok := n.Promises.FollowUp(ctx, "promise:1"); !ok || item.ID != "promise:1" || item.From != "Anna" {
		t.Fatalf("%+v %v", item, ok)
	}
	for _, id := range []string{"promise:2", "promise:9", "promise:x", "teams:c:1"} {
		if _, ok := n.Promises.FollowUp(ctx, id); ok {
			t.Fatalf("%s drafted", id)
		}
	}
}

func TestThePromiseLedgerTurnedOffReadsAndRemindsNothing(t *testing.T) {
	now := time.Date(2026, 11, 19, 15, 0, 0, 0, stockholm(t))
	n, p := newNorn(t)
	model := &fakeModel{promises: `{"promises":[{"what":"Skicka offerten","to":"Peter","when":"på fredag"}]}`, kept: `{"done":[]}`}
	n.Promises.Model = model.chat
	n.Promises.now = func() time.Time { return now }
	ctx := context.Background()
	meeting := Meeting{Subject: "Budget", Start: now.Add(-2 * time.Hour).UnixMilli(), Attendees: []string{"Peter Lund"}}

	if code, body := call(t, p, "GET", "/v1/settings/promises", nil); code != http.StatusOK || body.(map[string]any)["on"] != true {
		t.Fatalf("on until turned off: %d %v", code, body)
	}
	n.OfferNotes(meeting, "- Jag skickar offerten till Peter på fredag", "")
	if code, body := call(t, p, "PUT", "/v1/settings/promises", map[string]any{"on": false}); code != http.StatusOK || body.(map[string]any)["on"] != false {
		t.Fatalf("turning off: %d %v", code, body)
	}
	n.OfferNotes(meeting, "- Jag skickar offerten till Peter på fredag", "")
	if err := n.Promises.pass(ctx); err != nil {
		t.Fatal(err)
	}
	if len(model.asked) != 0 {
		t.Fatalf("read while off: %q", model.asked)
	}
	// Notes offered before it was turned off are not read once it is on again.
	if code, _ := call(t, p, "PUT", "/v1/settings/promises", map[string]any{"on": true}); code != http.StatusOK {
		t.Fatalf("turning on: %d", code)
	}
	if err := n.Promises.pass(ctx); err != nil {
		t.Fatal(err)
	}
	if len(model.asked) != 0 {
		t.Fatalf("read what was offered before it was off: %q", model.asked)
	}
	request := httptest.NewRequest("PUT", "/v1/settings/promises", strings.NewReader(`{"on":false}`))
	request.Header.Set("Authorization", "Bearer "+contracttest.Sender)
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("a part turned it off: %d", recorder.Code)
	}
}
