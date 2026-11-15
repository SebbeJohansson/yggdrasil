package norn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

// slice16 cuts text by UTF-16 offsets, as Pennvass does.
func slice16(text string, offset, length int) string {
	units := utf16.Encode([]rune(text))
	return string(utf16.Decode(units[offset : offset+length]))
}

func TestReadLanguageTool(t *testing.T) {
	var body ltBody
	_ = json.Unmarshal([]byte(`{
		"language": {"code": "en-US", "detectedLanguage": {"code": "en-US"}},
		"matches": [
			{"message": "Possible spelling mistake", "offset": 23, "length": 8, "replacements": [{"value": "sentence"}], "rule": {"id": "MORFOLOGIK_RULE_EN_US", "issueType": "misspelling"}},
			{"message": "Use an", "offset": 11, "length": 1, "replacements": [{"value": "an"}], "rule": {"id": "EN_A_VS_AN", "issueType": "grammar"}},
			{"message": "Wordy", "offset": 0, "length": 4, "replacements": [], "rule": {"id": "X", "issueType": "style"}},
			{"message": "no offset"}
		]}`), &body)
	result := ReadLanguageTool(body)
	if result.Language != "en-US" || len(result.Issues) != 3 {
		t.Fatalf("got %+v", result)
	}
	kinds := []string{result.Issues[0].Kind, result.Issues[1].Kind, result.Issues[2].Kind}
	if !reflect.DeepEqual(kinds, []string{"spelling", "grammar", "style"}) {
		t.Errorf("kinds %v", kinds)
	}
}

func TestSentences(t *testing.T) {
	text := "Första är här.  Second, after two spaces!\nThird"
	got := []string{}
	for _, s := range Sentences(text) {
		if slice16(text, s.Offset, u16len(s.Text)) != s.Text {
			t.Errorf("offset %d does not point at %q", s.Offset, s.Text)
		}
		got = append(got, s.Text)
	}
	if !reflect.DeepEqual(got, []string{"Första är här.", "Second, after two spaces!", "Third"}) {
		t.Errorf("got %q", got)
	}
	if WorthImproving("Thanks, see you.") || !WorthImproving("Due to the fact that it was late, we waited.") {
		t.Error("worthImproving")
	}
}

func TestReadSuggestion(t *testing.T) {
	sentence := "Me and him goes to store yesterday."
	got := ReadSuggestion(sentence, `{"problems":["grammar","needless_passive_voice","grammar"],"better":"He and I went to the store yesterday."}`)
	if got == nil || got.Reason != "Grammar, needless passive voice" {
		t.Errorf("got %+v", got)
	}
	for _, reply := range []string{
		`{"problems":[],"better":"Me and him goes to store yesterday."}`,
		`{"problems":["x"],"better":""}`,
		`{"problems":["x"],"better":"me and him  goes to store yesterday"}`,
		`Sure! Here is`,
	} {
		if s := ReadSuggestion(sentence, reply); s != nil {
			t.Errorf("%s: kept %+v", reply, s)
		}
	}
	if ReadSuggestion("Tack för igår, det var jättekul att ses.", `{"problems":["wordy"],"better":"Tack för igår, det var jättekul att_meet_up."}`) != nil {
		t.Error("kept a made-up word")
	}
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"Anna is travelling to the centre.", "Anna is traveling to the center.", true},
		{"We are not able to organise it.", "We aren't able to organize it.", true},
		{"Thanks for the update, I will look.", "Thanks for the update; I'll look.", true},
		{"men jag vill gärna se dem.", "men jag vill gärna se den.", false},
	} {
		if OnlySpellingVariants(c.a, c.b) != c.same {
			t.Errorf("%q vs %q: want %v", c.a, c.b, c.same)
		}
	}
}

func TestImprove(t *testing.T) {
	bad := "Due to the fact that the report was late, the delays were caused."
	fine := "Thanks for the update, I will look at it tomorrow 🙂."
	text := fine + " " + bad
	result, err := Improve(text, NewSuggestionCache(10), func(_, sentence string) (string, error) {
		if sentence == bad {
			return `{"problems":["wordy phrase"],"better":"The late report caused the delays."}`, nil
		}
		return `{"problems":[],"better":""}`, nil
	})
	if err != nil || result.Pending != 0 || len(result.Issues) != 1 {
		t.Fatalf("got %+v, %v", result, err)
	}
	if got := slice16(text, result.Issues[0].Offset, result.Issues[0].Length); got != bad {
		t.Errorf("offset points at %q", got)
	}

	var many []string
	for n := range 5 {
		many = append(many, fmt.Sprintf("This is sentence number %d of the long test.", n))
	}
	cache, calls := NewSuggestionCache(10), 0
	complete := func(_, _ string) (string, error) { calls++; return `{"problems":[],"better":""}`, nil }
	first, _ := Improve(strings.Join(many, " "), cache, complete)
	second, _ := Improve(strings.Join(many, " "), cache, complete)
	if first.Pending != 2 || second.Pending != 0 || calls != 5 {
		t.Errorf("pending %d then %d, %d calls", first.Pending, second.Pending, calls)
	}
}

// A Swedish line over an English one: each is checked in its own language,
// and "deployment" in the Swedish line is no misspelling, being English.
func TestCheckPerLine(t *testing.T) {
	swedish := "Vi gör en deployment i morgon bitti, tror jag, om allt går bra."
	english := "Please review the changes before then, thanks."
	text := "Hej 👋\n" + swedish + "\n" + english
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		line, language := r.Form.Get("text"), r.Form.Get("language")
		answer := func(code string, matches ...string) {
			fmt.Fprintf(w, `{"language":{"code":%q},"matches":[%s]}`, code, strings.Join(matches, ","))
		}
		misspelt := func(word string) string {
			return fmt.Sprintf(`{"message":"x","offset":%d,"length":%d,"rule":{"issueType":"misspelling"}}`, u16len(line[:strings.Index(line, word)]), u16len(word))
		}
		switch {
		case language == "auto" && strings.HasPrefix(line, "Vi"):
			answer("sv", misspelt("deployment"), misspelt("bitti"))
		case language == "auto":
			answer("en-US")
		case language == "en-GB" && strings.Contains(line, "deployment"):
			answer("en-GB", misspelt("bitti"))
		case strings.HasPrefix(line, "Hej") && language == "sv":
			answer("sv", misspelt("bitti"))
		default:
			t.Errorf("unexpected check of %q in %s", line, language)
			answer(language)
		}
	}))
	defer server.Close()
	result, err := Check(context.Background(), server.Client(), server.URL, text, []string{"sv", "en-GB"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Issues) != 1 || slice16(text, result.Issues[0].Offset, result.Issues[0].Length) != "bitti" {
		t.Errorf("issues %+v", result.Issues)
	}
	if result.Language != "sv" {
		t.Errorf("language %s", result.Language)
	}
}
