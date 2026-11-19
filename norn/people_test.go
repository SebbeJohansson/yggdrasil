package norn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yggdrasil/internal/contract/contracttest"
)

const annaTeams = "0b6f7c1e-2f3a-4b5c-8d9e-0f1a2b3c4d5e"

func peopleStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func observeAll(t *testing.T, s *Store, seen ...Observation) []Listed {
	t.Helper()
	if err := s.Observe(context.Background(), seen); err != nil {
		t.Fatal(err)
	}
	people, err := s.People(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return people
}

func suggestions(t *testing.T, s *Store) []MergeSuggestion {
	t.Helper()
	out, err := s.MergeSuggestions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// only is the one suggestion there is, else the test fails.
func only(t *testing.T, s *Store) MergeSuggestion {
	t.Helper()
	out := suggestions(t, s)
	if len(out) != 1 {
		t.Fatalf("want one suggestion, got %+v", out)
	}
	return out[0]
}

func TestAnAccountsAliasesAreOnePersonAndNamesAloneAreTheirOwn(t *testing.T) {
	s := peopleStore(t)
	people := observeAll(t, s,
		Observation{Name: "Anna Berg", Email: "Anna@Example.com", Teams: annaTeams, Source: "teams"},
		Observation{Name: "Anna Berg", Email: "anna@example.com", Source: "mail"},
		Observation{Name: annaTeams, Source: "teams"},
		Observation{Name: "Bo", Source: "promise"},
		Observation{Name: "Bo", Source: "calendar"},
		Observation{Name: "the team", Source: "promise"},
	)
	if len(people) != 2 {
		t.Fatalf("want Anna and Bo, got %+v", people)
	}
	anna := people[0]
	if anna.Name != "Anna Berg" || len(anna.Aliases) != 3 {
		t.Fatalf("Anna: %+v", anna)
	}
	if people[1].Name != "Bo" || len(people[1].Aliases) != 1 || people[1].Aliases[0].Source != "promise" {
		t.Fatalf("Bo: %+v", people[1])
	}
}

func TestEveryRuleGivesItsReason(t *testing.T) {
	for _, c := range []struct {
		rule string
		seen []Observation
	}{
		{"same-name", []Observation{{Name: "Anna Berg", Email: "anna@example.com", Source: "mail"}, {Name: "Berg, Anna", Teams: annaTeams, Source: "teams"}}},
		{"address-name", []Observation{{Email: "anna.berg@example.com", Source: "mail"}, {Name: "Anna Berg", Teams: annaTeams, Source: "teams"}}},
		{"short-name", []Observation{{Name: "Anna Berg", Email: "anna@example.com", Source: "mail"}, {Name: "Anna B.", Teams: annaTeams, Source: "teams"}}},
		{"first-name", []Observation{{Name: "Anna Berg", Email: "anna@example.com", Source: "mail"}, {Name: "Anna", Source: "promise"}}},
	} {
		s := peopleStore(t)
		observeAll(t, s, c.seen...)
		got := only(t, s)
		if got.Rule != c.rule || got.Reason == "" || got.Confidence < MergeConfidence || got.A.ID == got.B.ID {
			t.Errorf("%s: %+v", c.rule, got)
		}
	}
}

func TestOneMicrosoftAccountIsSuggestedNotMerged(t *testing.T) {
	s := peopleStore(t)
	observeAll(t, s, Observation{Email: "anna@example.com", Source: "mail"}, Observation{Teams: annaTeams, Source: "teams"})
	// Graph now says they are one account: both already listed, so suggested.
	people := observeAll(t, s, Observation{Name: "A. Berg", Email: "anna@example.com", Teams: annaTeams, Source: "teams"})
	if len(people) != 2 {
		t.Fatalf("merged by itself: %+v", people)
	}
	got := only(t, s)
	if got.Rule != "same-account" || got.Confidence != 0.99 {
		t.Fatalf("%+v", got)
	}
}

func TestTheSuggestionReadsAsWords(t *testing.T) {
	s := peopleStore(t)
	observeAll(t, s, Observation{Name: "Anna Berg", Email: "anna@example.com", Source: "mail"}, Observation{Name: "Anna B.", Teams: annaTeams, Source: "teams"})
	if words := SuggestionWords(only(t, s)); words != "Anna Berg (mail) and Anna B. (Teams) look like the same person: Anna B. looks like Anna Berg shortened" {
		t.Fatalf("%q", words)
	}
}

func TestWeakAndAmbiguousOnesAreNotSuggested(t *testing.T) {
	s := peopleStore(t)
	observeAll(t, s,
		// Two full names start with Anna: "Anna" alone could be either.
		Observation{Name: "Anna Berg", Email: "anna@example.com", Source: "mail"},
		Observation{Name: "Anna Lind", Email: "lind@example.com", Source: "mail"},
		Observation{Name: "Anna", Source: "promise"},
		// "Max" alone is a word as much as a name.
		Observation{Name: "Max Ek", Email: "max@example.com", Source: "mail"},
		Observation{Name: "Max", Source: "promise"},
		// Different people who share nothing.
		Observation{Name: "Bo Lind", Email: "bo@example.com", Source: "mail"},
	)
	if got := suggestions(t, s); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestNotTheSameIsRemembered(t *testing.T) {
	ctx := context.Background()
	s := peopleStore(t)
	seen := []Observation{{Name: "Anna Berg", Email: "anna@example.com", Source: "mail"}, {Name: "Anna Berg", Email: "anna.berg@other.com", Source: "mail"}}
	observeAll(t, s, seen...)
	got := only(t, s)
	if err := s.KeepApart(ctx, got.A.ID, got.B.ID); err != nil {
		t.Fatal(err)
	}
	observeAll(t, s, seen...)
	if out := suggestions(t, s); len(out) != 0 {
		t.Fatalf("suggested again: %+v", out)
	}
	// A third Anna Berg is still suggested with each of them.
	observeAll(t, s, Observation{Name: "Anna Berg", Teams: annaTeams, Source: "teams"})
	if out := suggestions(t, s); len(out) != 2 {
		t.Fatalf("the third: %+v", out)
	}
}

func TestCombineKeepsEveryAliasAndSplitUndoesIt(t *testing.T) {
	ctx := context.Background()
	s := peopleStore(t)
	observeAll(t, s, Observation{Name: "Anna Berg", Email: "anna@example.com", Source: "mail"}, Observation{Name: "Anna B.", Teams: annaTeams, Source: "teams"})
	got := only(t, s)
	combined, err := s.CombinePeople(ctx, got.A.ID, got.B.ID)
	if err != nil {
		t.Fatal(err)
	}
	if combined.Name != "Anna Berg" || len(combined.Aliases) != 4 {
		t.Fatalf("%+v", combined)
	}
	if people, _ := s.People(ctx); len(people) != 1 || len(suggestions(t, s)) != 0 {
		t.Fatalf("%+v", people)
	}
	if _, err := s.CombinePeople(ctx, got.A.ID, got.B.ID); err != ErrNoPerson {
		t.Fatalf("combined a person who is gone: %v", err)
	}

	left, split, err := s.SplitAlias(ctx, combined.ID, AliasTeams, strings.ToUpper(annaTeams))
	if err != nil {
		t.Fatal(err)
	}
	if len(left.Aliases) != 3 || len(split.Aliases) != 1 || split.Aliases[0].Kind != AliasTeams || split.ID == left.ID {
		t.Fatalf("%+v / %+v", left, split)
	}
	// Graph gives the two together again: the split stands, and it is not suggested again.
	people := observeAll(t, s, Observation{Name: "Anna B.", Email: "anna@example.com", Teams: annaTeams, Source: "teams"})
	if len(people) != 2 || len(suggestions(t, s)) != 0 {
		t.Fatalf("the split was undone: %+v %+v", people, suggestions(t, s))
	}
	if _, _, err := s.SplitAlias(ctx, split.ID, AliasTeams, annaTeams); err != ErrLastAlias {
		t.Fatalf("split the last alias: %v", err)
	}
	if _, _, err := s.SplitAlias(ctx, left.ID, AliasEmail, "nobody@example.com"); err != ErrNoPerson {
		t.Fatalf("split an alias they do not have: %v", err)
	}
}

func TestPeopleSeenLeavesYouOut(t *testing.T) {
	inbox := []Ranked{{Item: Item{Source: "outlook", From: "Anna Berg", Sender: "anna@example.com", Recipients: []string{"Me Myself", "Bo Lind"}}}}
	promises := []Promise{{To: "Cleo", ToAddress: ""}}
	events := []CalendarEvent{{Organizer: "dan@example.com", Attendees: []string{"Anna Berg"}}}
	seen := PeopleSeen(events, inbox, nil, promises, []string{"me@example.com", "Me Myself"})
	var got []string
	for _, o := range seen {
		got = append(got, o.Source+":"+firstOf(o.Name, o.Email))
	}
	want := "calendar:dan@example.com calendar:Anna Berg mail:Anna Berg mail:Bo Lind promise:Cleo"
	if strings.Join(got, " ") != want {
		t.Fatalf("%q", strings.Join(got, " "))
	}
}

func TestThePeopleRoutes(t *testing.T) {
	n, p := newNorn(t)
	ctx := context.Background()
	if err := n.Store.Observe(ctx, []Observation{{Name: "Anna Berg", Email: "anna@example.com", Source: "mail"}, {Name: "Anna B.", Teams: annaTeams, Source: "teams"},
		{Name: "Bo Lind", Email: "bo@example.com", Source: "mail"}, {Name: "Bo Lind", Email: "bo.lind@other.com", Source: "mail"}}); err != nil {
		t.Fatal(err)
	}
	contracttest.Run(t, p)

	_, body := call(t, p, "GET", "/v1/people/suggestions", nil)
	list := body.(map[string]any)["suggestions"].([]any)
	if len(list) != 2 {
		t.Fatalf("%v", body)
	}
	id := func(s any, side string) float64 { return s.(map[string]any)[side].(map[string]any)["id"].(float64) }

	// Only you change the list.
	request := httptest.NewRequest("POST", "/v1/people/combine", strings.NewReader(`{"keep":1,"merge":2}`))
	request.Header.Set("Authorization", "Bearer "+contracttest.Sender)
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("a part combined people: %d", recorder.Code)
	}

	var anna, bo any
	for _, s := range list {
		if s.(map[string]any)["rule"] == "short-name" {
			anna = s
		} else {
			bo = s
		}
	}
	if code, combined := call(t, p, "POST", "/v1/people/combine", map[string]any{"keep": id(anna, "a"), "merge": id(anna, "b")}); code != http.StatusOK ||
		len(combined.(map[string]any)["aliases"].([]any)) != 4 {
		t.Fatalf("%d %v", code, combined)
	}
	if code, _ := call(t, p, "POST", "/v1/people/apart", map[string]any{"a": id(bo, "a"), "b": id(bo, "b")}); code != http.StatusOK {
		t.Fatalf("apart: %d", code)
	}
	if _, body := call(t, p, "GET", "/v1/people/suggestions", nil); len(body.(map[string]any)["suggestions"].([]any)) != 0 {
		t.Fatalf("%v", body)
	}
	code, body := call(t, p, "POST", "/v1/people/1/split", map[string]any{"kind": "name", "value": "Anna B."})
	if code != http.StatusOK || body.(map[string]any)["split"].(map[string]any)["name"] != "Anna B." {
		t.Fatalf("%d %v", code, body)
	}
	if code, _ := call(t, p, "POST", "/v1/people/combine", map[string]any{"keep": 1, "merge": 1}); code != http.StatusBadRequest {
		t.Errorf("combine with themselves: %d", code)
	}
	if code, _ := call(t, p, "POST", "/v1/people/99/split", map[string]any{"kind": "name", "value": "X"}); code != http.StatusNotFound {
		t.Errorf("split nobody: %d", code)
	}
	if _, body := call(t, p, "GET", "/v1/people", nil); len(body.(map[string]any)["people"].([]any)) != 4 {
		t.Fatalf("%v", body)
	}
}
