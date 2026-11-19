package norn

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/llm"
)

var recapNow = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

func ago(d time.Duration) int64 { return recapNow.Add(-d).UnixMilli() }

func TestWhoIsReadAsAnAddressAnIdOrAName(t *testing.T) {
	if p := ParseRecapPerson("Anna.Berg@Example.com", "Anna Berg"); p.Address != "anna.berg@example.com" || p.Name != "Anna Berg" || p.UserID != "" {
		t.Errorf("address: %+v", p)
	}
	if p := ParseRecapPerson("0b6f7c1e-2f3a-4b5c-8d9e-0f1a2b3c4d5e", ""); p.UserID == "" || p.Address != "" {
		t.Errorf("Teams id: %+v", p)
	}
	if p := ParseRecapPerson("Anna Berg", ""); p.Name != "Anna Berg" {
		t.Errorf("name: %+v", p)
	}
}

func TestSamePersonName(t *testing.T) {
	for _, same := range [][2]string{{"Anna Berg", "Berg, Anna"}, {"anna.berg@example.com", "Anna Berg"}, {"Anna", "anna"}, {"Anna Berg", "Anna Maria Berg"}} {
		if !SamePersonName(same[0], same[1]) {
			t.Errorf("%q and %q are one person", same[0], same[1])
		}
	}
	for _, other := range [][2]string{{"Anna", "Anna Berg"}, {"Anna Berg", "Anna Lind"}, {"", "Anna"}} {
		if SamePersonName(other[0], other[1]) {
			t.Errorf("%q and %q are not", other[0], other[1])
		}
	}
}

func TestTheRecapHoldsThePromisesToThem(t *testing.T) {
	anna := RecapPerson{Name: "Anna Berg", Address: "anna@example.com"}
	promises := []Promise{
		{ID: 1, What: "Send the deck", To: "Anna", ToAddress: "anna@example.com", State: "open", Source: "outlook", Due: "2026-11-20"},
		{ID: 2, What: "Check with legal", To: "Anna Berg", State: "open", Source: "meeting"},
		{ID: 3, What: "Book the room", To: "Bo Lind", ToAddress: "bo@example.com", State: "open", Source: "outlook"},
		{ID: 4, What: "Old one", To: "Anna Berg", ToAddress: "anna@example.com", State: "done", Source: "outlook"},
	}
	recap := BuildRecap(anna, nil, promises, nil, recapNow)
	if len(recap.Promises) != 2 || recap.Promises[0].ID != 1 || recap.Promises[1].ID != 2 || recap.Promises[0].Due != "2026-11-20" {
		t.Fatalf("%+v", recap.Promises)
	}
	if !recap.On || recap.Who != "anna@example.com" {
		t.Errorf("%+v", recap)
	}
}

func TestAnEmptyPersonGetsAnEmptyRecap(t *testing.T) {
	recap := BuildRecap(RecapPerson{Address: "nobody@example.com"}, nil, nil, nil, recapNow)
	if recap.Last != nil || recap.Promises == nil || recap.Open == nil || len(recap.Promises)+len(recap.Open) != 0 {
		t.Fatalf("%+v", recap)
	}
}

func TestTheLastExchangeAndWhatIsOpen(t *testing.T) {
	anna := RecapPerson{Name: "Anna Berg", Address: "anna@example.com", UserID: "u-anna"}
	exchanges := []Exchange{
		{ID: "meeting:m1", Kind: "meeting", Source: "calendar", At: ago(26 * time.Hour), Title: "Monday sync"},
		{ID: "outlook:a", Kind: "mail", Source: "outlook", At: ago(3 * time.Hour), Title: "Q3 numbers", Thread: "outlook:c1", FromThem: true},
		{ID: "outlook:b", Kind: "mail", Source: "outlook", At: ago(5 * time.Hour), Title: "Q3 numbers", Thread: "outlook:c1", FromThem: false},
		{ID: "outlook:c", Kind: "mail", Source: "outlook", At: ago(2 * time.Hour), Title: "Lunch?", Thread: "outlook:c2", FromThem: false},
		{ID: "teams:chat1:9", Kind: "chat", Source: "teams", At: ago(time.Hour), Title: "Anna Berg", Thread: "teams:chat1", FromThem: true},
		{ID: "outlook:old", Kind: "mail", Source: "outlook", At: ago(20 * 24 * time.Hour), Title: "Old", Thread: "outlook:c3", FromThem: true},
	}
	inbox := []Ranked{
		{Item: Item{ID: "teams:chat1:9", Source: "teams", From: "Anna Berg", Sender: "u-anna", At: ago(time.Hour)}, Band: "notable"},
		{Item: Item{ID: "outlook:z", Source: "outlook", From: "Bo Lind", Sender: "bo@example.com"}, Band: "notable"},
	}
	recap := BuildRecap(anna, exchanges, nil, inbox, recapNow)
	if recap.Last == nil || recap.Last.Kind != "chat" || !recap.Last.FromThem {
		t.Fatalf("last: %+v", recap.Last)
	}
	// The chat from the inbox, then the Q3 thread where she wrote last; not
	// lunch (you wrote last), not the old thread, not Bo's message.
	if len(recap.Open) != 2 || recap.Open[0].ID != "teams:chat1:9" || !recap.Open[0].InInbox || recap.Open[1].ID != "outlook:a" || recap.Open[1].InInbox {
		t.Fatalf("open: %+v", recap.Open)
	}
}

func TestOnlyTheNewestInAThreadSaysWhoWroteLast(t *testing.T) {
	exchanges := []Exchange{
		{ID: "outlook:a", Kind: "mail", At: ago(5 * time.Hour), Title: "Q3", Thread: "outlook:c1", FromThem: true},
		{ID: "outlook:b", Kind: "mail", At: ago(time.Hour), Title: "Re: Q3", Thread: "outlook:c1", FromThem: false},
	}
	if recap := BuildRecap(RecapPerson{Address: "anna@example.com"}, exchanges, nil, nil, recapNow); len(recap.Open) != 0 {
		t.Fatalf("answered thread is open: %+v", recap.Open)
	}
}

func TestChatAndMailExchanges(t *testing.T) {
	me := Me{ID: "u-me"}
	var chats []recapChat
	chat := recapChat{ID: "c1", ChatType: "oneOnOne", WebURL: "https://teams/c1"}
	chat.Members = append(chat.Members, struct {
		DisplayName string `json:"displayName"`
		UserID      string `json:"userId"`
		Email       string `json:"email"`
	}{"Me", "u-me", "me@example.com"}, struct {
		DisplayName string `json:"displayName"`
		UserID      string `json:"userId"`
		Email       string `json:"email"`
	}{"Anna Berg", "u-anna", "Anna@Example.com"})
	chat.LastMessagePreview = &struct {
		ID              string `json:"id"`
		CreatedDateTime string `json:"createdDateTime"`
		Body            struct {
			Content string `json:"content"`
		} `json:"body"`
		From *struct {
			User *struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"from"`
	}{ID: "9", CreatedDateTime: "2026-11-19T09:00:00Z"}
	chat.LastMessagePreview.Body.Content = "<p>Can you look at the draft?</p>"
	chats = append(chats, chat)
	got, person := ChatExchanges(chats, RecapPerson{UserID: "u-anna"}, me)
	if len(got) != 1 || got[0].ID != "teams:c1:9" || got[0].Text != "Can you look at the draft?" || person.Address != "anna@example.com" || person.Name != "Anna Berg" {
		t.Fatalf("%+v %+v", got, person)
	}
	from := &graphAddress{}
	from.EmailAddress.Address = "anna@example.com"
	mails := []recapMail{{ID: "m1", Subject: "Q3", ConversationID: "conv", From: from, ReceivedDateTime: "2026-11-18T09:00:00Z", BodyPreview: "Numbers attached"},
		{ID: "m2", Subject: "Old", ReceivedDateTime: "2026-09-01T09:00:00Z"}}
	exchanges := MailExchanges(mails, person, recapNow.AddDate(0, 0, -30))
	if len(exchanges) != 1 || !exchanges[0].FromThem || exchanges[0].Thread != "outlook:conv" {
		t.Fatalf("%+v", exchanges)
	}
	meetings := MeetingExchanges([]CalendarEvent{
		{ID: "x", Subject: "Monday sync", Start: ago(26 * time.Hour), End: ago(25 * time.Hour), Attendees: []string{"Berg, Anna"}},
		{ID: "y", Subject: "Later", Start: recapNow.Add(time.Hour).UnixMilli(), End: recapNow.Add(2 * time.Hour).UnixMilli(), Attendees: []string{"Anna Berg"}},
	}, person, recapNow)
	if len(meetings) != 1 || meetings[0].Title != "Monday sync" {
		t.Fatalf("%+v", meetings)
	}
}

func TestTheLineIsTheLocalModelsAndNothingElseIsCalled(t *testing.T) {
	n, _ := newNorn(t)
	n.Recaps.now = func() time.Time { return recapNow }
	var asked []llm.Options
	var read []llm.Message
	n.Recaps.Model = func(_ context.Context, messages []llm.Message, options llm.Options) (string, error) {
		asked, read = append(asked, options), messages
		return "  \"The Q3 numbers she sent\"  ", nil
	}
	n.Recaps.Exchanges = func(_ context.Context, person RecapPerson, _ time.Time) ([]Exchange, RecapPerson) {
		return []Exchange{{ID: "outlook:a", Kind: "mail", Source: "outlook", At: ago(time.Hour), Title: "Q3", Text: "Here are the numbers.", FromThem: true}}, person
	}
	recap, err := n.Recaps.For(context.Background(), ParseRecapPerson("anna@example.com", "Anna Berg"))
	if err != nil {
		t.Fatal(err)
	}
	if recap.Last == nil || recap.Last.Words != "The Q3 numbers she sent" {
		t.Fatalf("%+v", recap.Last)
	}
	if len(asked) != 1 || asked[0].Purpose != "person recap" || !strings.Contains(read[1].Content, "Here are the numbers.") {
		t.Fatalf("%+v", asked)
	}
	// Kept: the same person again is not read or asked again.
	if _, err := n.Recaps.For(context.Background(), ParseRecapPerson("anna@example.com", "Anna Berg")); err != nil || len(asked) != 1 {
		t.Fatalf("asked again: %d %v", len(asked), err)
	}
}

func TestWithoutTheModelTheLineIsTheSubject(t *testing.T) {
	n, _ := newNorn(t)
	n.Recaps.Model = func(context.Context, []llm.Message, llm.Options) (string, error) { return "", errors.New("down") }
	n.Recaps.Exchanges = func(_ context.Context, person RecapPerson, _ time.Time) ([]Exchange, RecapPerson) {
		return []Exchange{{Kind: "chat", Source: "teams", At: time.Now().Add(-time.Hour).UnixMilli(), Title: "Anna Berg", Text: "hej"}}, person
	}
	recap, _ := n.Recaps.For(context.Background(), ParseRecapPerson("Anna Berg", ""))
	if recap.Last == nil || recap.Last.Words != "Anna Berg" {
		t.Fatalf("%+v", recap.Last)
	}
	if RecapLine("one\ntwo") != "" || RecapLine(strings.Repeat("x", 200)) != "" {
		t.Error("a line that is not one line was kept")
	}
}

func TestTheRecapRoute(t *testing.T) {
	n, p := newNorn(t)
	ctx := context.Background()
	n.Recaps.Exchanges = func(_ context.Context, person RecapPerson, _ time.Time) ([]Exchange, RecapPerson) { return nil, person }
	if err := n.Promises.init(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	n.Promises.keep(ctx, Promise{What: "Send the deck", To: "Anna Berg", ToAddress: "anna@example.com", Source: "outlook", Message: "outlook:1",
		CreatedAt: now.UnixMilli(), ChangedAt: now.UnixMilli(), Key: "outlook:1", Norm: "send the deck"})
	contracttest.Run(t, p)

	code, body := call(t, p, "GET", "/v1/people/recap?who=anna@example.com&name=Anna%20Berg", nil)
	if code != http.StatusOK || len(body.(map[string]any)["promises"].([]any)) != 1 {
		t.Fatalf("%d %v", code, body)
	}
	if code, _ := call(t, p, "GET", "/v1/people/recap", nil); code != http.StatusBadRequest {
		t.Errorf("no who: %d", code)
	}
}
