package norn

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"yggdrasil/internal/llm"
	"yggdrasil/internal/part"
)

func TestWhyWordsLeaveOutWhatSaysNothing(t *testing.T) {
	if got := WhyWords("Add a CSV export to the budget page"); !slices.Equal(got, []string{"csv", "export", "budget"}) {
		t.Fatalf("words %v", got)
	}
	if got := WhyWords("Fix it"); len(got) != 0 {
		t.Fatalf("words %v", got)
	}
	words := []string{"csv", "export", "budget"}
	if WhyScore("The budget exports should be CSV", words) != 3 || WhyScore("a reexport of nothing", words) != 0 {
		t.Fatal("scored wrong")
	}
	found := BestFound([]whyFound{
		{Kind: "mail", Ref: "a", At: 1, Text: "Budget only"},
		{Kind: "mail", Ref: "b", At: 2, Text: "Budget export, please"},
		{Kind: "mail", Ref: "c", At: 3, Text: "CSV budget export"},
		{Kind: "mail", Ref: "c", At: 4, Text: "CSV budget export again"},
	}, words)
	if len(found) != 2 || found[0].Ref != "c" || found[1].Ref != "b" {
		t.Fatalf("found %+v", found)
	}
}

func TestLooksSecret(t *testing.T) {
	for _, line := range []string{"Use token: abc", "The key is ghp_abcdefghij1234567890", "sk-ant-abcdefghijk123", "set it to Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5"} {
		if !looksSecret(line) {
			t.Errorf("%q passed", line)
		}
	}
	for _, line := range []string{"Person 1 wants monthly totals in the CSV export.", "Decided in the 2026-11-12 planning meeting."} {
		if looksSecret(line) {
			t.Errorf("%q dropped", line)
		}
	}
}

// whyModel is a local model that finds the names given and answers the why
// prompt with answer; asked records what the why prompt held.
type whyModel struct {
	mu     sync.Mutex
	asked  []string
	answer string
	// noNames makes the names prompt fail.
	noNames bool
}

func (m *whyModel) serve(t *testing.T, n *Norn, names ...string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []llm.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		last := body.Messages[len(body.Messages)-1].Content
		var content string
		switch body.Messages[0].Content {
		case whySystem:
			m.mu.Lock()
			m.asked = append(m.asked, last)
			m.mu.Unlock()
			content = m.answer
		case namesSystem:
			if m.noNames {
				http.Error(w, "down", http.StatusInternalServerError)
				return
			}
			found := []string{}
			for _, name := range names {
				if strings.Contains(last, name) {
					found = append(found, name)
				}
			}
			raw, _ := json.Marshal(map[string]any{"names": found})
			content = string(raw)
		default:
			http.Error(w, "not here", http.StatusInternalServerError)
			return
		}
		part.JSON(w, 200, map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": content}}}})
	}))
	t.Cleanup(server.Close)
	n.LLM = &llm.Client{PrimaryURL: server.URL + "/v1", OllamaURL: "http://127.0.0.1:1"}
}

func TestWhyPackIsSummarizedLocallyAndPseudonymized(t *testing.T) {
	n, p := newNorn(t)
	ctx := context.Background()
	_, _ = n.Store.AddTurn(ctx, "s1", "user", "Kalle Ek wants the budget export as CSV, monthly totals only", nil, nil, nil)
	_, _ = n.Store.AddTurn(ctx, "s2", "user", "lunch with Anna on Friday", nil, nil, nil)
	model := &whyModel{answer: `{"picked":[{"n":1,"summary":"Kalle Ek wants the budget export as CSV with monthly totals only."},` +
		`{"n":1,"summary":"twice"},{"n":9,"summary":"out of range"}]}`}
	model.serve(t, n, "Kalle Ek")

	status, body := call(t, p, "POST", "/v1/why", map[string]any{"goal": "Add a CSV export to the budget page", "repos": []string{"my-widget"}, "goalId": 7})
	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, body)
	}
	sources := body.(map[string]any)["sources"].([]any)
	if len(sources) != 1 {
		t.Fatalf("sources %v", sources)
	}
	source := sources[0].(map[string]any)
	summary := source["summary"].(string)
	if strings.Contains(summary, "Kalle") || !strings.Contains(summary, "Person 1") || source["kind"] != "rewind" || !strings.HasPrefix(source["ref"].(string), "rewind:chat:") {
		t.Fatalf("source %v", source)
	}
	if len(model.asked) != 1 || !strings.Contains(model.asked[0], "budget export") || strings.Contains(model.asked[0], "lunch") {
		t.Fatalf("the local model read %q", model.asked)
	}
}

func TestWhyDropsALineThatMayHoldASecret(t *testing.T) {
	n, _ := newNorn(t)
	_, _ = n.Store.AddTurn(context.Background(), "s1", "user", "the budget export needs the key ghp_abcdefghij1234567890abcdefghij123456", nil, nil, nil)
	model := &whyModel{answer: `{"picked":[{"n":1,"summary":"The budget export uses the key ghp_abcdefghij1234567890abcdefghij123456."}]}`}
	model.serve(t, n)
	if pack := n.Why(context.Background(), "Add a CSV export to the budget page", "", time.Now()); len(pack.Sources) != 0 {
		t.Fatalf("pack %+v", pack)
	}
}

func TestWhyIsEmptyWhenNothingMatchesSurely(t *testing.T) {
	n, _ := newNorn(t)
	_, _ = n.Store.AddTurn(context.Background(), "s1", "user", "the budget meeting moved", nil, nil, nil)
	model := &whyModel{answer: `{"picked":[{"n":1,"summary":"anything"}]}`}
	model.serve(t, n)
	pack := n.Why(context.Background(), "Add a CSV export to the budget page", "", time.Now())
	if len(pack.Sources) != 0 || len(model.asked) != 0 {
		t.Fatalf("pack %+v, model asked %d times", pack, len(model.asked))
	}
}

func TestWhyIsEmptyWhenNamesCannotBeSwapped(t *testing.T) {
	n, _ := newNorn(t)
	_, _ = n.Store.AddTurn(context.Background(), "s1", "user", "Kalle wants the budget export as CSV", nil, nil, nil)
	// The why prompt is answered, the names prompt is not: nothing may go.
	model := &whyModel{answer: `{"picked":[{"n":1,"summary":"Kalle wants a CSV budget export."}]}`, noNames: true}
	model.serve(t, n)
	if pack := n.Why(context.Background(), "Add a CSV export to the budget page", "", time.Now()); len(pack.Sources) != 0 || len(model.asked) != 1 {
		t.Fatalf("pack %+v, asked %d", pack, len(model.asked))
	}
}

func TestWhyAsksForAGoal(t *testing.T) {
	_, p := newNorn(t)
	if status, _ := call(t, p, "POST", "/v1/why", map[string]any{"goal": " "}); status != http.StatusBadRequest {
		t.Fatalf("status %d", status)
	}
}
