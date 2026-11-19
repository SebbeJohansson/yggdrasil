package pseudo

import (
	"encoding/json"
	"strings"
	"testing"
)

var people = []Person{
	{Name: "Anna Svensson", Emails: []string{"anna.svensson@example.com"}, Role: "invited to the meeting"},
	{Name: "Björn Öberg", Emails: []string{"bjorn@example.com"}},
	{Name: "Per Holm"},
}

func TestPseudonymizeKnownPeople(t *testing.T) {
	var m Map
	out := m.Pseudonymize("Anna Svensson wrote to bjorn@example.com. Annas deck is late; Björn agreed. ANNA SVENSSON again.", people, nil)
	// Addresses are numbered first: bjorn@ before Anna's name.
	want := "Person 2 wrote to Person 1. Person 2s deck is late; Person 1 agreed. Person 2 again."
	if out != want {
		t.Fatalf("got  %q\nwant %q", out, want)
	}
	if leaks := m.Leaks(out, people); len(leaks) != 0 {
		t.Fatalf("leaks: %v", leaks)
	}
}

func TestFirstNamesStandAlone(t *testing.T) {
	var m Map
	out := m.Pseudonymize("Annabel met Anna. Per månad betalar Per Holm.", people, nil)
	if !strings.Contains(out, "Annabel") || strings.Contains(out, "Anna.") {
		t.Fatalf("Anna swapped inside a word, or not alone: %q", out)
	}
	if !strings.HasPrefix(strings.SplitN(out, ". ", 2)[1], "Per månad") {
		t.Fatalf("the word Per was taken for a name: %q", out)
	}
}

func TestFoundNamesAndUnknownEmails(t *testing.T) {
	var m Map
	out := m.Pseudonymize("Ask Kalle Ek, or mail stranger@other.se.", nil, []string{"Kalle Ek", "Nobody Here"})
	if out != "Ask Person 2, or mail Person 1." {
		t.Fatalf("got %q", out)
	}
	if len(m.Entries) != 2 {
		t.Fatalf("a name not in the text got a placeholder: %+v", m.Entries)
	}
}

func TestRestoreAndStableAcrossTurns(t *testing.T) {
	var m Map
	first := m.Pseudonymize("Anna Svensson asked.", people, nil)
	raw, _ := json.Marshal(m)
	var again Map
	_ = json.Unmarshal(raw, &again)
	second := again.Pseudonymize("And Björn Öberg, with Anna.", people, nil)
	if first != "Person 1 asked." || second != "And Person 2, with Person 1." {
		t.Fatalf("got %q then %q", first, second)
	}
	if got := again.Restore("Tell person 1 that Person 2 agreed; Person 9 is not ours."); got != "Tell Anna Svensson that Björn Öberg agreed; Person 9 is not ours." {
		t.Fatalf("restored %q", got)
	}
}

func TestLeaksCatchesWhatSlippedThrough(t *testing.T) {
	var m Map
	if leaks := m.Leaks("from someone@x.io about anna svensson", people); len(leaks) != 2 {
		t.Fatalf("leaks %v", leaks)
	}
}

func TestLegend(t *testing.T) {
	var m Map
	out := m.Pseudonymize("Anna Svensson and Per Holm", people, nil)
	legend := m.Legend(out)
	if !strings.Contains(legend, "- Person 1: invited to the meeting") || !strings.Contains(legend, "- Person 2\n") && !strings.HasSuffix(legend, "- Person 2") {
		t.Fatalf("legend %q", legend)
	}
	if m.Legend("no one here") != "" {
		t.Fatal("a legend for text with no placeholders")
	}
}

func TestAPersonsOtherNamesShareTheirPlaceholder(t *testing.T) {
	var m Map
	known := []Person{{Name: "Anna Berg", Aliases: []string{"Anna B."}, Emails: []string{"anna@example.com"}}}
	out := m.Pseudonymize("Anna B. said Anna Berg (anna@example.com) has it.", known, nil)
	if out != "Person 1 said Person 1 (Person 1) has it." || len(m.Entries) != 1 {
		t.Fatalf("%q %+v", out, m.Entries)
	}
	// Only the other name in the text is enough to find them.
	var again Map
	if out := again.Pseudonymize("Ask Anna B. first.", known, nil); out != "Ask Person 1 first." || again.Entries[0].Name != "Anna Berg" {
		t.Fatalf("%q %+v", out, again.Entries)
	}
	if leaks := again.Leaks("Anna B. still here", known); len(leaks) != 1 {
		t.Fatalf("the other name is not a leak: %v", leaks)
	}
}
