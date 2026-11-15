package norn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/llm"
	"yggdrasil/internal/part"
)

func newNorn(t *testing.T) (*Norn, *part.Part) {
	t.Helper()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	n := New(Config{EmbedModel: "embed", LayaURL: "http://127.0.0.1:1", DataDir: t.TempDir(), TimeZone: "Europe/Stockholm"}, store, &llm.Client{OllamaURL: "http://127.0.0.1:1"}, t.Logf)
	// No models in tests: a fixed embedding per text, and no completions.
	n.Brain.Embed = func(_ context.Context, texts []string) ([][]float64, error) {
		vectors := make([][]float64, len(texts))
		for i, text := range texts {
			vectors[i] = []float64{float64(len(text)), 1}
		}
		return vectors, nil
	}
	n.Brain.Complete = func(context.Context, []llm.Message, llm.Options) (string, error) {
		return "", errors.New("no model in tests")
	}
	p := part.New(Name, contracttest.Tokens(), nil)
	n.Mount(p)
	return n, p
}

func call(t *testing.T, p *part.Part, method, path string, body any) (int, any) {
	t.Helper()
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer "+contracttest.Owner)
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, request)
	var decoded any
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return recorder.Code, decoded
}

func TestMeetsItsContract(t *testing.T) {
	n, p := newNorn(t)
	ctx := context.Background()
	n.Brain.Remember(ctx, "The user works at a clothing company.", "manual")
	n.Store.AddTurn(ctx, "s1", "user", "what is on my calendar about secrets", nil, nil, nil)
	n.Store.RateJoke(ctx, "a joke", nil, nil, 1)
	contracttest.Run(t, p)
}

func TestSayingAMemoryTwiceIsNotAnError(t *testing.T) {
	_, p := newNorn(t)
	if status, _ := call(t, p, "POST", "/v1/memory", map[string]string{"text": "My manager is Ada."}); status != http.StatusCreated {
		t.Fatalf("first: %d", status)
	}
	status, body := call(t, p, "POST", "/v1/memory", map[string]string{"text": "my manager is ada"})
	if status != http.StatusOK || body.(map[string]any)["known"] != true {
		t.Fatalf("second: %d %v", status, body)
	}
	_, list := call(t, p, "GET", "/v1/memory", nil)
	if len(list.([]any)) != 1 {
		t.Fatalf("got %v", list)
	}
	if status, _ := call(t, p, "DELETE", "/v1/memory/1", nil); status != http.StatusOK {
		t.Fatalf("delete: %d", status)
	}
	if status, _ := call(t, p, "DELETE", "/v1/memory/1", nil); status != http.StatusNotFound {
		t.Fatalf("delete again: %d", status)
	}
}

func TestRecall(t *testing.T) {
	memories := []StoredMemory{
		{Memory: Memory{ID: 3, Text: "c"}, Embedding: []float64{0, 1}, Model: "m"},
		{Memory: Memory{ID: 2, Text: "b"}, Embedding: []float64{1, 0.1}, Model: "m"},
		{Memory: Memory{ID: 1, Text: "a"}, Embedding: []float64{1, 0}, Model: "other"},
	}
	got := Recall(memories, []float64{1, 0}, "m")
	if len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("got %v: only b is close and from the same model", got)
	}
	if got := Recall(memories, nil, "m"); len(got) != 3 || got[0].ID != 3 {
		t.Fatalf("without a vector the newest stand in: %v", got)
	}
}

func TestNamesMustComeFromTheMessage(t *testing.T) {
	if NamesAreFromMessage("Mira is the user's team lead.", "who is my team lead?") {
		t.Fatalf("a copied name passed")
	}
	if !NamesAreFromMessage("The user's manager is Ada.", "my manager is ada") {
		t.Fatalf("a name from the message failed")
	}
}

func TestReadFacts(t *testing.T) {
	got := ReadFacts(`{"facts":["The user ships on Fridays.","short",42,"The user lives in Borås.","The user has two cats.","The user likes tea."]}`)
	want := []string{"The user ships on Fridays.", "The user lives in Borås.", "The user has two cats."}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if ReadFacts("not json") != nil {
		t.Fatalf("not json gave facts")
	}
}

func TestLearnKeepsOnlyNewFactsFromTheMessage(t *testing.T) {
	n, _ := newNorn(t)
	n.Brain.Complete = func(context.Context, []llm.Message, llm.Options) (string, error) {
		return `{"facts":["The user's manager is Ada.","Mira is the user's team lead."]}`, nil
	}
	learned := n.Brain.LearnFrom(context.Background(), "my manager is Ada")
	if len(learned) != 1 || learned[0].Text != "The user's manager is Ada." || learned[0].Source != "extracted" {
		t.Fatalf("got %v", learned)
	}
	if again := n.Brain.LearnFrom(context.Background(), "my manager is Ada"); len(again) != 0 {
		t.Fatalf("learned it twice: %v", again)
	}
}

func TestSnippet(t *testing.T) {
	text := strings.Repeat("a ", 100) + "the SECRETS are here " + strings.Repeat("b ", 100)
	got := Snippet(text, "secrets")
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") || !strings.Contains(got, "SECRETS") {
		t.Fatalf("got %q", got)
	}
	if got := Snippet("short text", "nowhere"); got != "short text" {
		t.Fatalf("got %q", got)
	}
	if got := Snippet("åäö ÅÄÖ match", "match"); got != "åäö ÅÄÖ match" {
		t.Fatalf("got %q", got)
	}
}

func TestCallsGoToTheFirstMessageAtOrAfterTheirExchange(t *testing.T) {
	agent := "a1"
	turns := []Turn{{ID: 1, Role: "user", At: 100}, {ID: 2, Role: "assistant", At: 150}, {ID: 3, Role: "assistant", Author: &agent, At: 160}, {ID: 4, Role: "user", At: 200}}
	exchange := func(at int64) *int64 { return &at }
	calls := []StoredCall{{Exchange: exchange(90), Purpose: "a"}, {Exchange: exchange(100), Purpose: "b"}, {Exchange: exchange(170), Purpose: "c"}, {Purpose: "none"}}
	got := CallsByTurn(turns, calls)
	if len(got) != 2 || got[0].Turn != 1 || len(got[0].Calls) != 2 || got[1].Turn != 4 || got[1].Calls[0].Purpose != "c" {
		t.Fatalf("got %+v", got)
	}
}

func TestWidgetSettings(t *testing.T) {
	_, p := newNorn(t)
	status, body := call(t, p, "PATCH", "/v1/settings/widgets", map[string]any{"place": "away", "hideAtHome": []string{"teams"}, "hideAway": []string{"gmail", "gmail"}})
	if status != http.StatusOK || !reflect.DeepEqual(body.(map[string]any)["hideAway"], []any{"gmail"}) {
		t.Fatalf("got %d %v", status, body)
	}
	if status, _ := call(t, p, "PATCH", "/v1/settings/widgets", map[string]any{"place": "moon", "hideAtHome": []string{}, "hideAway": []string{}}); status != http.StatusBadRequest {
		t.Fatalf("a bad place: %d", status)
	}
	if status, _ := call(t, p, "PATCH", "/v1/settings/widgets", map[string]any{"place": "home", "hideAtHome": []string{"radio"}, "hideAway": []string{}}); status != http.StatusBadRequest {
		t.Fatalf("a bad feature: %d", status)
	}
	settings := WidgetSettings{Place: "away", HideAway: []string{"gmail"}}
	if !settings.Hidden("gmail") || settings.Hidden("teams") {
		t.Fatalf("hidden is wrong")
	}
}

func TestProfile(t *testing.T) {
	got := CleanProfile("<!-- for the editor -->\n# Me\n\n\n\nSwedish, short answers.\n")
	if got != "# Me\n\nSwedish, short answers." {
		t.Fatalf("got %q", got)
	}
	if got := CleanProfile(strings.Repeat("å", 2000)); len([]rune(got)) != ProfileMaxChars {
		t.Fatalf("not cut: %d", len([]rune(got)))
	}
}

func TestDecisionsView(t *testing.T) {
	n, _ := newNorn(t)
	ctx := context.Background()
	id, _ := n.Store.LogDecision(ctx, "s1", "is the build done?", "", "model", &Decision{Action: "answer"},
		map[string]any{"action": "status", "agent": "build", "project": nil, "agentId": "a1"}, []string{"answer needs tools"}, 300)
	_ = n.Store.SetShadow(ctx, id, ShadowDecision{Action: "status", AgentID: strPtr("a1"), MS: 40,
		Probabilities: struct {
			Action map[string]float64 `json:"action"`
			Agent  map[string]float64 `json:"agent"`
		}{map[string]float64{"status": 0.9}, map[string]float64{"build": 0.8, "none": 0.1}}})
	_, _ = n.Store.Correct(ctx, id, "status:a1")
	rows, _ := n.Store.Decisions(ctx, 10)
	view := ViewDecision(rows[0])
	if view.Live.Action != "status" || *view.Live.Target != "build" || view.BeforeGuards == nil || view.BeforeGuards.Action != "answer" {
		t.Errorf("live %+v before %+v", view.Live, view.BeforeGuards)
	}
	if view.Shadow == nil || *view.Shadow.Target != "build" || view.Shadow.Confidence != 0.9 || !*view.Agrees || !*view.ShadowRight {
		t.Errorf("shadow %+v", view.Shadow)
	}
	if view.Correction.Action != "status" || *view.Correction.Target != "a1" || *view.LiveRef != "status:a1" || *view.ShadowRef != "status:a1" {
		t.Errorf("correction %+v refs %v %v", view.Correction, *view.LiveRef, *view.ShadowRef)
	}
	summary := SummariseDecisions([]DecisionView{view})
	if summary.Shadowed != 1 || summary.Agree != 1 || summary.ShadowRightOnCorrected != 1 || *summary.MedianMS.Live != 300 {
		t.Errorf("summary %+v", summary)
	}
}
