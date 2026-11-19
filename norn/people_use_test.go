package norn

import (
	"context"
	"strings"
	"testing"
	"time"

	"yggdrasil/internal/pseudo"
)

// combinedAnna is a Norn whose list holds Anna Berg's mail and her Teams
// account as "Anna B.", combined into one person.
func combinedAnna(t *testing.T) *Norn {
	t.Helper()
	n, _ := newNorn(t)
	ctx := context.Background()
	if err := n.Store.Observe(ctx, []Observation{{Name: "Anna Berg", Email: "anna@example.com", Source: "mail"}, {Name: "Anna B.", Teams: annaTeams, Source: "teams"}}); err != nil {
		t.Fatal(err)
	}
	got, err := n.Store.MergeSuggestions(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %+v", err, got)
	}
	if _, err := n.Store.CombinePeople(ctx, got[0].A.ID, got[0].B.ID); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTheRecapFindsEveryAliasOnceCombined(t *testing.T) {
	n := combinedAnna(t)
	ctx := context.Background()
	var asked RecapPerson
	n.Recaps.Exchanges = func(_ context.Context, person RecapPerson, _ time.Time) ([]Exchange, RecapPerson) {
		asked = person
		return nil, person
	}
	if err := n.Promises.init(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	n.Promises.keep(ctx, Promise{What: "Send the deck", To: "Anna B.", ToAddress: annaTeams, Source: "teams", Message: "teams:1",
		CreatedAt: now, ChangedAt: now, Key: "teams:1", Norm: "send the deck"})
	recap, err := n.Recaps.For(ctx, ParseRecapPerson("anna@example.com", "Anna Berg"))
	if err != nil {
		t.Fatal(err)
	}
	if len(recap.Promises) != 1 || asked.UserID != annaTeams {
		t.Fatalf("the promise to Anna B. or her Teams id was missed: %+v %+v", recap, asked)
	}
	// Her Teams chats are hers: the chat's member is her Teams account.
	me := Me{ID: "u-me"}
	chat := recapChat{ID: "c1", ChatType: "oneOnOne"}
	chat.Members = append(chat.Members, struct {
		DisplayName string `json:"displayName"`
		UserID      string `json:"userId"`
		Email       string `json:"email"`
	}{"Anna B.", annaTeams, ""})
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
	if found, _ := ChatExchanges([]recapChat{chat}, RecapPerson{Address: "anna@example.com", Name: "Anna Berg"}, me); len(found) != 0 {
		t.Fatalf("found before expanding: %+v", found)
	}
	expanded := n.Book(ctx).Expand(RecapPerson{Address: "anna@example.com", Name: "Anna Berg"})
	if found, _ := ChatExchanges([]recapChat{chat}, expanded, me); len(found) != 1 {
		t.Fatalf("her Teams chat was missed: %+v", expanded)
	}
	if !expanded.Is("", "Anna B.") || expanded.Is("", "Bo Lind") {
		t.Fatalf("%+v", expanded)
	}
}

func TestAPromiseIsKeptByAMessageToAnotherAliasOnceCombined(t *testing.T) {
	n := combinedAnna(t)
	promise := Promise{To: "Anna B.", ToAddress: annaTeams}
	to := []Recipient{{Name: "Anna Berg", Address: "anna@example.com"}}
	if sameRecipient(promise, to, nil) {
		t.Fatal("matched without the list")
	}
	if !sameRecipient(promise, to, n.Book(context.Background())) {
		t.Fatal("the list's aliases were not used")
	}
	if sameRecipient(promise, []Recipient{{Name: "Bo Lind", Address: "bo@example.com"}}, n.Book(context.Background())) {
		t.Fatal("someone else matched")
	}
}

func TestOnePlaceholderPerCombinedPerson(t *testing.T) {
	n := combinedAnna(t)
	ctx := context.Background()
	namesModel(t, n)
	now := time.Now()
	n.Accounts.Work.Set(CalendarSnapshot{From: now.Add(-time.Hour).UnixMilli(), To: now.Add(24 * time.Hour).UnixMilli(), At: now.UnixMilli(),
		Events: []CalendarEvent{{Subject: "Sync", Start: now.Add(time.Hour).UnixMilli(), Attendees: []string{"Anna Berg"}}}})
	texts, m, err := n.pseudonymize(ctx, "", nil, "Anna B. wrote in Teams; did Anna Berg (anna@example.com) get the deck?")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 1 || strings.Contains(texts[0], "Anna") || strings.Contains(texts[0], "example.com") || strings.Count(texts[0], "Person 1") != 3 {
		t.Fatalf("%q %+v", texts[0], m.Entries)
	}
	// Without the list, two placeholders.
	apart := NewPeopleBook(nil).Group([]pseudo.Person{{Name: "Anna Berg"}, {Name: "Anna B."}})
	if len(apart) != 2 {
		t.Fatalf("%+v", apart)
	}
}
