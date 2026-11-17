package norn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"yggdrasil/internal/llm"
)

func TestText(t *testing.T) {
	if got := OneLine(StripHTML("<p>Hi&nbsp;<b>there</b></p><script>x()</script>&#228;&#x1F600;"), 100); got != "Hi there ä😀" {
		t.Fatalf("got %q", got)
	}
	if got := OneLine("one two three four five six", 14); got != "one two three…" {
		t.Fatalf("got %q", got)
	}
	reply := "Sounds good.\n\nFrån: Ada\nSkickat: igår\nÄmne: x\n\nold text"
	if got := Unquote(reply); got != "Sounds good." {
		t.Fatalf("got %q", got)
	}
}

var me = Me{ID: "me-id", Address: "sebastian.johansson@example.com", DisplayName: "Sebastian Johansson"}

func TestMailItems(t *testing.T) {
	items := MailItems([]byte(`{"value":[
		{"id":"1","subject":"Q3?","from":{"emailAddress":{"name":"Ada","address":"ADA@example.com"}},"toRecipients":[{"emailAddress":{"address":"sebastian.johansson@example.com","name":"Me"}}],"receivedDateTime":"2026-10-08T07:00:00Z","bodyPreview":"Hi Sebastian, can you?","importance":"high"},
		{"id":"2","@odata.type":"#microsoft.graph.eventMessageResponse","subject":"Accepted: x","receivedDateTime":"2026-10-08T07:00:00Z"}
	]}`), me)
	if len(items) != 2 {
		t.Fatalf("got %+v", items)
	}
	if !items[0].Direct || !items[0].MentionsMe || !items[0].Important || items[0].Sender != "ada@example.com" {
		t.Fatalf("got %+v", items[0])
	}
	if items[1].Hidden != "a meeting response" {
		t.Fatalf("a meeting reply was not hidden: %+v", items[1])
	}
}

func TestChatItems(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	items := ChatItems([]byte(`{"value":[
		{"id":"c1","chatType":"oneOnOne","members":[{"displayName":"Ada","userId":"ada"},{"displayName":"Me","userId":"me-id"}],"viewpoint":{"lastMessageReadDateTime":"2026-10-08T10:00:00Z"},"lastMessagePreview":{"id":"m1","createdDateTime":"2026-10-08T11:00:00Z","body":{"content":"<p>ping Johansson</p>"},"from":{"user":{"id":"ada","displayName":"Ada"}}}},
		{"id":"c2","lastMessagePreview":{"createdDateTime":"2026-10-08T11:00:00Z","body":{"content":"mine"},"from":{"user":{"id":"me-id"}}}},
		{"id":"c3","viewpoint":{"lastMessageReadDateTime":"2026-10-08T11:00:30Z"},"lastMessagePreview":{"createdDateTime":"2026-10-08T11:00:00Z","body":{"content":"read"},"from":{"user":{"id":"x"}}}},
		{"id":"c4","lastMessagePreview":{"createdDateTime":"2026-10-08T06:00:00Z","body":{"content":"old, never read"},"from":{"user":{"id":"x"}}}}
	]}`), me, now)
	if len(items) != 1 || items[0].Subject != "Ada" || !items[0].Direct || !items[0].MentionsMe || items[0].Conversation != "c1" {
		t.Fatalf("got %+v", items)
	}
}

func TestOrgChart(t *testing.T) {
	people, _ := OrgPage([]byte(`{"value":[
		{"id":"ceo"},
		{"id":"boss","manager":{"id":"ceo"}},
		{"id":"me-id","manager":{"id":"boss"}},
		{"id":"peer","mail":"Peer@example.com","manager":{"id":"boss"}},
		{"id":"report","manager":{"id":"me-id"},"proxyAddresses":["SMTP:rep@example.com","x500:junk"]}
	]}`))
	chart := NewOrgChart(people)
	for sender, want := range map[string]string{"boss": "manager", "ceo": "above", "peer@example.com": "peer", "rep@example.com": "report", "me-id": "", "stranger": ""} {
		if got := chart.RelationOf("me-id", sender); got != want {
			t.Errorf("%s: got %q, want %q", sender, got, want)
		}
	}
}

func TestSlack(t *testing.T) {
	names := map[string]string{"U1": "Ada"}
	if got := SlackToText("<@U1> see <https://x.y|this> &amp; <!here> in <#C1|general>", names); got != "@Ada see this & @here in #general" {
		t.Fatalf("got %q", got)
	}
	var conversations []SlackChannel
	for i := range 10 {
		conversations = append(conversations, SlackChannel{ID: "d" + itoa(i), IsIM: true}, SlackChannel{ID: "c" + itoa(i)})
	}
	order := SweepOrder(conversations, 8)
	channels := 0
	for _, c := range order {
		if !c.IsIM {
			channels++
		}
	}
	if len(order) != 8 || channels != 2 {
		t.Fatalf("a quarter of the budget is kept for channels: %d of %d", channels, len(order))
	}
	me := SlackMe{UserID: "UME", URL: "https://team.slack.com"}
	items := SlackItems(SlackChannel{ID: "D1", IsIM: true, User: "U1"}, []SlackMessage{
		{User: "U1", Text: "hi <@UME>", TS: "1700000000.000100"},
		{User: "UME", Text: "mine", TS: "1700000001.000100"},
		{User: "U1", Subtype: "channel_join", Text: "joined", TS: "1700000002.000100"},
		{User: "U1", TS: "1700000003.000100", Files: []struct{ Name, Title, Filetype string }{{Title: "image.png"}}},
	}, me, names, nil, false)
	if len(items) != 2 || items[0].From != "Ada" || !items[0].MentionsMe || !items[0].Direct || items[0].Subject != "Ada" || items[1].Context != "image.png" {
		t.Fatalf("got %+v", items)
	}
	if items[0].Link != "https://team.slack.com/archives/D1/p1700000000000100" {
		t.Fatalf("got %q", items[0].Link)
	}
}

func TestGmail(t *testing.T) {
	boxes := Mailboxes(`"Berg, Anna" <Anna@Example.com>, bo@example.com`)
	if !reflect.DeepEqual(boxes, []Mailbox{{"Berg, Anna", "anna@example.com"}, {"bo@example.com", "bo@example.com"}}) {
		t.Fatalf("got %+v", boxes)
	}
	item := GmailItem([]byte(`{"id":"m","threadId":"t","labelIds":["CATEGORY_PROMOTIONS"],"snippet":"50% &amp; more","internalDate":"1700000000000",
		"payload":{"headers":[{"name":"From","value":"Shop <shop@x.com>"},{"name":"To","value":"me@gmail.com"},{"name":"Subject","value":"Sale"},{"name":"X-Priority","value":"1 (Highest)"}]}}`), "me@gmail.com")
	if item == nil || item.From != "Shop" || !item.Direct || !item.Important || item.Hidden != "a Gmail promotion" || item.Preview != "50% & more" {
		t.Fatalf("got %+v", item)
	}
}

func TestRankAndSilence(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	items := []Item{
		{ID: "a", Source: "teams", From: "Ada", Subject: "x", Preview: "can you?", At: now.UnixMilli(), Direct: true, Relation: "manager", Conversation: "c1"},
		{ID: "b", Source: "teams", From: "Bo", Subject: "x", Preview: "fyi", At: now.UnixMilli(), Conversation: "c2"},
		{ID: "c", Source: "teams", From: "Cy", Subject: "x", Preview: "hey Seb", At: now.UnixMilli(), MentionsMe: true, Conversation: "c2"},
	}
	ranked := RankAll(Silence(items, map[string]bool{"c2": true}, nil), now, map[string]Judgement{"a": {Awaited: 1, Consequence: 1}})
	if ranked[0].Item.ID != "a" || ranked[0].Band != "urgent" || !strings.Contains(strings.Join(ranked[0].Reasons, ","), "from your manager") {
		t.Fatalf("got %+v", ranked[0])
	}
	bands := map[string]string{}
	for _, r := range ranked {
		bands[r.Item.ID] = r.Band
	}
	if bands["b"] != "hidden" || bands["c"] == "hidden" {
		t.Fatalf("a muted chat hides all but what names you: %v", bands)
	}
}

func TestSummaryGuards(t *testing.T) {
	if got := CleanSummary("Here is a summary:\n\"Ada wants the numbers.\""); got != "Ada wants the numbers." {
		t.Fatalf("got %q", got)
	}
	top := []Ranked{{Item: Item{From: "Ada Lovelace", Subject: "Q3 numbers", Preview: "send them on Friday"}, Band: "urgent"}}
	if got := InventedNames("Ada wants the Q3 numbers by Friday.", top); len(got) != 0 {
		t.Fatalf("flagged %v", got)
	}
	if got := InventedNames("Ada and Mira want the numbers. Anna too.", top); !reflect.DeepEqual(got, []string{"Mira", "Anna"}) {
		t.Fatalf("got %v", got)
	}
	if got := PlainSummary(top, 3); got != "Ada Lovelace in Q3 numbers, and 2 more" {
		t.Fatalf("got %q", got)
	}
	if got := PlainSummary(nil, 0); got != "nothing unread" {
		t.Fatalf("got %q", got)
	}
}

func TestReadJudgement(t *testing.T) {
	got, ok := ReadJudgement([]byte(`{"answers":{"awaited":{"noul":0.8},"consequence":{"score":2,"confidence":0.6}}}`))
	if !ok || got.Awaited != 0.8 || got.Consequence != 2.0/3 || got.Confidence != 0.6 {
		t.Fatalf("got %+v %v", got, ok)
	}
	if _, ok := ReadJudgement([]byte(`{"answers":{"awaited":{"noul":2}}}`)); ok {
		t.Fatalf("a bad answer was read")
	}
}

func TestAsksAboutInbox(t *testing.T) {
	items := []Ranked{{Item: Item{From: "Ada Lovelace"}}}
	for question, want := range map[string]bool{"anything unread?": true, "what did ada say": true, "vad är vädret": false} {
		if AsksAboutInbox(question, items) != want {
			t.Errorf("%q: want %v", question, want)
		}
	}
}

func TestAPollWithNothingSignedInSaysSo(t *testing.T) {
	n, _ := newNorn(t)
	n.Config.Judge = JudgeSettings{Provider: "off"}
	if err := n.Inbox.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	view := n.Inbox.View(context.Background())
	if len(view.Items) != 0 || view.At == nil {
		t.Fatalf("got %+v", view)
	}
	for _, s := range view.Sources {
		if s.OK && s.Source != "slack" {
			t.Errorf("%s is ok without a sign-in", s.Source)
		}
	}
}

func TestAnUnchangedSectionIsNotPhrasedAgain(t *testing.T) {
	asked := 0
	fail := false
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		if fail {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Kim needs the numbers."}}]}`))
	}))
	defer model.Close()
	n, _ := newNorn(t)
	n.LLM = &llm.Client{PrimaryURL: model.URL + "/v1"}
	now := time.Now()
	top := []Ranked{{Item: Item{ID: "m1", Source: "outlook", From: "Kim", Subject: "numbers"}, Band: "urgent"}}
	for range 2 {
		if text, fromModel := n.Summarise(context.Background(), top, 1, now, "email"); text != "Kim needs the numbers." || !fromModel {
			t.Fatalf("got %q %v", text, fromModel)
		}
	}
	if asked != 1 {
		t.Fatalf("asked %d times for the same section", asked)
	}
	top[0].Band = "notable"
	fail = true
	for range 2 {
		if _, fromModel := n.Summarise(context.Background(), top, 1, now, "email"); fromModel {
			t.Fatal("phrased by a failing model")
		}
	}
	if asked != 2 {
		t.Fatalf("asked %d times, want a failure kept for a while", asked)
	}
	if _, fromModel := n.Summarise(context.Background(), top, 1, now.Add(summaryRetry+time.Minute), "email"); fromModel || asked != 3 {
		t.Fatalf("not tried again later: asked %d", asked)
	}
}
