package norn

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWantsDraftForQuestionsToYouThatMatter(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	entry := func(band string, item Item) Ranked {
		if item.At == 0 {
			item.At = now.Add(-time.Hour).UnixMilli()
		}
		return Ranked{Band: band, Item: item}
	}
	cases := []struct {
		name  string
		entry Ranked
		want  bool
	}{
		{"a question to you", entry("urgent", Item{Direct: true, Preview: "Can you look at the Q4 numbers?"}), true},
		{"a Swedish request", entry("notable", Item{MentionsMe: true, Preview: "Kan du kolla på det här innan fredag"}), true},
		{"quiet", entry("quiet", Item{Direct: true, Preview: "Can you?"}), false},
		{"not to you", entry("urgent", Item{Preview: "Can anyone look?"}), false},
		{"asks nothing", entry("urgent", Item{Direct: true, Preview: "FYI: the build is green."}), false},
		{"old", entry("urgent", Item{Direct: true, Preview: "Can you?", At: now.Add(-72 * time.Hour).UnixMilli()}), false},
	}
	for _, c := range cases {
		if got := WantsDraft(c.entry, now); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDraftPromptCarriesWhoAndWhereAndOnlyWhatIsKnown(t *testing.T) {
	item := Item{Source: "teams", From: "Anna Berg", Subject: "Q4", Preview: "Can you look at the numbers?", Text: "Can you look at the numbers before Friday? The sheet is in the channel."}
	messages := DraftMessages(item, "manager", "I lead the AI team.", []Memory{{Text: "Anna owns the Q4 budget"}}, []string{"Tue 14:00-15:00 Sprint review"})
	system, user := messages[0].Content, messages[1].Content
	for _, want := range []string{"Teams chat", "language of the message", "[square brackets]", "I lead the AI team.", "Anna owns the Q4 budget", "Sprint review"} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	if !strings.Contains(user, "Anna Berg (the owner's manager)") || !strings.Contains(user, "before Friday? The sheet") {
		t.Errorf("user prompt: %s", user)
	}
	if strings.Contains(user, "Only the start") {
		t.Error("the whole text was read; it should not say only the start was")
	}
	mail := DraftMessages(Item{Source: "gmail", From: "x", Preview: strings.Repeat("a", PreviewChars)}, "", "", nil, nil)
	if !strings.Contains(mail[0].Content, "an email") || !strings.Contains(mail[1].Content, "Only the start") {
		t.Errorf("mail prompt: %v", mail)
	}
}

func TestCleanDraftTakesOffWhatModelsAdd(t *testing.T) {
	if got := cleanDraft("Here's a draft:\n\"Hej Anna, jag kollar i morgon.\""); got != "Hej Anna, jag kollar i morgon." {
		t.Fatalf("%q", got)
	}
}

func TestDraftsAreKeptListedAndThrownAway(t *testing.T) {
	n, p := newNorn(t)
	ctx := context.Background()
	if err := n.keepDraft(ctx, Draft{Item: "teams:c:1", Source: "teams", Text: "Ja, jag tittar.", Auto: true, At: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	code, body := call(t, p, "GET", "/v1/drafts", nil)
	drafts := body.(map[string]any)["drafts"].([]any)
	if code != http.StatusOK || len(drafts) != 1 || drafts[0].(map[string]any)["text"] != "Ja, jag tittar." {
		t.Fatalf("%d %v", code, body)
	}
	if code, _ := call(t, p, "POST", "/v1/drafts", map[string]string{"item": "teams:nope"}); code != http.StatusNotFound {
		t.Fatalf("unknown item: %d", code)
	}
	if code, _ := call(t, p, "DELETE", "/v1/drafts/teams:c:1", nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := call(t, p, "DELETE", "/v1/drafts/teams:c:1", nil); code != http.StatusNotFound {
		t.Fatalf("delete again: %d", code)
	}
}
