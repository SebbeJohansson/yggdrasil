// Package norn is Yggdrasil's assistant: what a human assistant does. Your
// inbox, calendars, mail and chat sign-ins, memory, profile and writing help,
// and the front desk: every message you type comes here first, and Norn
// answers it or hands it to the part that should (Sindri for code).
//
// Named for the Norns, who tend Yggdrasil every day, keep time and spin the
// threads. Ported from my-agent piece by piece; see docs/architecture.md for
// what has moved.
package norn

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"yggdrasil/internal/linear"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/llm"
	"yggdrasil/internal/part"
)

// Name is this part's name.
const Name = "norn"

// Config is what Norn reads from its environment.
type Config struct {
	ProfilePath string
	EmbedModel  string
	// Learn is whether Norn learns facts from your messages.
	Learn bool
	// Drafts is whether Norn drafts replies to the inbox's questions by itself (drafts.go).
	Drafts  bool
	LayaURL string
	// LayaKey is Laya's bearer key, for the inbox judge, the tone badge and
	// the /v1/systemone pass-through.
	LayaKey string
	// TimeZone is the reader's, for times in answers and the activity heatmap.
	TimeZone string
	// SummaryModel phrases the inbox's one-line summaries.
	SummaryModel string
	// AskModel answers locally; DispatchModel decides what each message is
	// ("off": the older rules). DispatchKeepAlive is how long Ollama keeps a
	// model loaded after a call; empty leaves it to Ollama (5m).
	AskModel, DispatchModel, DispatchKeepAlive string
	// Judge says who judges the inbox beside the four rules.
	Judge JudgeSettings
	// DataDir holds the sign-ins, beside the database.
	DataDir                            string
	GoogleClientID, GoogleClientSecret string
	// LayaShadowURL and LayaShadowKey are the shadow dispatcher's Laya; no key, no shadow.
	LayaShadowURL, LayaShadowKey string
	// DashboardURL is my-widget's dashboard, for links to read a session; off is none.
	DashboardURL string
	// Write is where Pennvass's writing checks run.
	Write WriteConfig
}

// Norn is the running assistant.
type Norn struct {
	Config Config
	Store  *Store
	LLM    *llm.Client
	Brain  *Brain
	HTTP   *http.Client
	// Gjallar is where Norn raises what is its to raise: a sign-in that ended.
	Gjallar *alert.Client
	// modelFailing is what the model-health loop last told Gjallar; modelChecked, that it has.
	modelFailing, modelChecked bool
	Accounts                   *Accounts
	Inbox                      *Inbox
	// Events is what just changed, for parts that react to it (Huginn).
	Events *Events
	Zone   *time.Location
	// Sindri is where coding work and questions that need tools go.
	Sindri *SindriClient
	// Lookup answers questions its MCP servers cover before an agent is
	// started; nil is off.
	Lookup *Lookup
	// Linear answers "is AI-141 done?"; off without LINEAR_KEY.
	Linear *linear.Client
	// suggestions remembers what the improve check said about each sentence.
	suggestions *SuggestionCache
	logf        func(format string, args ...any)
}

// New makes Norn over its store and model client.
func New(config Config, store *Store, client *llm.Client, logf func(string, ...any)) *Norn {
	zone, err := time.LoadLocation(config.TimeZone)
	if err != nil {
		zone = time.UTC
	}
	n := &Norn{Config: config, Store: store, LLM: client, HTTP: &http.Client{Timeout: 60 * time.Second}, logf: logf,
		Gjallar: alert.NewClient("", ""), Zone: zone, Sindri: &SindriClient{HTTP: &http.Client{Timeout: 60 * time.Second}},
		suggestions: NewSuggestionCache(1000),
		Accounts:    NewAccounts(config.DataDir, config.GoogleClientID, config.GoogleClientSecret)}
	n.Inbox = NewInbox(n, config.DataDir)
	n.Events = NewEvents(time.Now())
	n.Brain = &Brain{
		Store:      store,
		EmbedModel: config.EmbedModel,
		Embed: func(ctx context.Context, texts []string) ([][]float64, error) {
			return client.Embed(ctx, config.EmbedModel, texts)
		},
		Complete: client.Chat,
		Warn:     logf,
	}
	return n
}

// Mount adds Norn's routes and loops to the part.
func (n *Norn) Mount(p *part.Part) {
	p.Handle("GET /v1/memory", n.listMemories)
	p.Handle("POST /v1/memory", n.addMemory)
	p.Handle("DELETE /v1/memory/{id}", n.deleteMemory)
	p.Handle("GET /v1/jokes", n.jokes)
	p.Handle("POST /v1/jokes/rating", n.rateJoke)
	p.Handle("GET /v1/settings/widgets", n.widgetSettings)
	p.Handle("PATCH /v1/settings/widgets", n.setWidgetSettings)
	p.Handle("GET /v1/conversations", n.conversations)
	p.Handle("GET /v1/search", n.search)
	p.Handle("GET /v1/activity", n.activity)
	p.Handle("GET /v1/chat/llm-calls", n.llmCalls)
	p.Handle("GET /v1/models/health", n.modelHealth)
	p.Handle("POST /v1/systemone", n.systemOne)
	n.mountAccounts(p)
	n.mountInbox(p)
	n.mountChat(p)
	n.mountWrite(p)
	n.mountEvents(p)
	n.mountDrafts(p)
	p.Loop("model-health", healthEvery, n.checkHealth)
}

func (n *Norn) fail(w http.ResponseWriter, err error) {
	n.logf("request failed: %v", err)
	part.Error(w, http.StatusInternalServerError, "internal error")
}

// --- memory --------------------------------------------------------------------

func (n *Norn) listMemories(w http.ResponseWriter, r *http.Request) {
	stored, err := n.Store.Memories(r.Context())
	if err != nil {
		n.fail(w, err)
		return
	}
	memories := make([]Memory, len(stored))
	for i, m := range stored {
		memories[i] = m.Memory
	}
	part.JSON(w, http.StatusOK, memories)
}

func (n *Norn) addMemory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := part.Decode(r, &body); err != nil || strings.TrimSpace(body.Text) == "" || len([]rune(body.Text)) > 500 {
		part.Error(w, http.StatusBadRequest, "`text` must be a non-empty string of at most 500 characters")
		return
	}
	memory, err := n.Brain.Remember(r.Context(), strings.TrimSpace(body.Text), "manual")
	if err != nil {
		n.fail(w, err)
		return
	}
	// 200 with `known` rather than a 409: saying it twice is not an error.
	if memory == nil {
		part.JSON(w, http.StatusOK, map[string]bool{"known": true})
		return
	}
	part.JSON(w, http.StatusCreated, memory)
}

func (n *Norn) deleteMemory(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		part.Error(w, http.StatusNotFound, "no such memory")
		return
	}
	deleted, err := n.Store.DeleteMemory(r.Context(), id)
	if err != nil {
		n.fail(w, err)
		return
	}
	if !deleted {
		part.Error(w, http.StatusNotFound, "no memory #"+r.PathValue("id"))
		return
	}
	part.JSON(w, http.StatusOK, map[string]int64{"deleted": id})
}

// --- jokes ---------------------------------------------------------------------

func (n *Norn) jokes(w http.ResponseWriter, r *http.Request) {
	ratings, err := n.Store.JokeRatings(r.Context(), 500)
	if err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, ratings)
}

func (n *Norn) rateJoke(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text   string  `json:"text"`
		Topic  *string `json:"topic"`
		Form   *string `json:"form"`
		Rating *int    `json:"rating"`
	}
	if err := part.Decode(r, &body); err != nil || strings.TrimSpace(body.Text) == "" || len([]rune(body.Text)) > 1000 {
		part.Error(w, http.StatusBadRequest, "`text` must be the joke, at most 1000 characters")
		return
	}
	if body.Rating == nil || (*body.Rating != 1 && *body.Rating != -1 && *body.Rating != 0) {
		part.Error(w, http.StatusBadRequest, "`rating` must be 1, -1 or 0")
		return
	}
	if err := n.Store.RateJoke(r.Context(), strings.TrimSpace(body.Text), body.Topic, body.Form, *body.Rating); err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, map[string]int{"rated": *body.Rating})
}

// --- conversations and search ----------------------------------------------------

func (n *Norn) conversations(w http.ResponseWriter, r *http.Request) {
	conversations, err := n.Store.Conversations(r.Context(), 100)
	if err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, conversations)
}

// Search limits, and how much text a hit shows either side of the match.
const (
	QueryMin      = 2
	QueryMax      = 200
	SnippetRadius = 90
)

var whitespace = regexp.MustCompile(`\s+`)

// Snippet is the text around the first match, whitespace collapsed, with
// "…" where it was cut.
func Snippet(text, query string) string {
	flat := []rune(strings.TrimSpace(whitespace.ReplaceAllString(text, " ")))
	lower := []rune(strings.ToLower(string(flat)))
	at := strings.Index(string(lower), strings.ToLower(query))
	if at == -1 {
		if len(flat) > SnippetRadius*2 {
			return strings.TrimRight(string(flat[:SnippetRadius*2]), " ") + "…"
		}
		return string(flat)
	}
	at = len([]rune(string(lower)[:at]))
	from := max(0, at-SnippetRadius)
	to := min(len(flat), at+len([]rune(query))+SnippetRadius)
	snippet := strings.TrimSpace(string(flat[from:to]))
	if from > 0 {
		snippet = "…" + snippet
	}
	if to < len(flat) {
		snippet += "…"
	}
	return snippet
}

// SearchHit is one turn that holds the searched text.
type SearchHit struct {
	Session string  `json:"session"`
	Title   *string `json:"title"`
	Role    string  `json:"role"`
	Author  *string `json:"author"`
	Label   *string `json:"label"`
	At      int64   `json:"at"`
	Snippet string  `json:"snippet"`
}

func (n *Norn) search(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(whitespace.ReplaceAllString(r.URL.Query().Get("q"), " "))
	if length := len([]rune(query)); length < QueryMin || length > QueryMax {
		part.Error(w, http.StatusBadRequest, "`q` must be 2-200 characters")
		return
	}
	turns, err := n.Store.SearchTurns(r.Context(), query, 60)
	if err != nil {
		n.fail(w, err)
		return
	}
	hits := []SearchHit{}
	for _, t := range turns {
		hits = append(hits, SearchHit{Session: t.Session, Title: t.Title, Role: t.Role, Author: t.Author, Label: t.Label, At: t.At, Snippet: Snippet(t.Content, query)})
	}
	part.JSON(w, http.StatusOK, hits)
}

// --- the debug view ------------------------------------------------------------

var sessionID = regexp.MustCompile(`^[\w-]{1,64}$`)

// TurnCalls is one of your messages with the model calls it caused.
type TurnCalls struct {
	Turn  int64        `json:"turn"`
	Calls []StoredCall `json:"calls"`
}

// CallsByTurn groups calls by the message they were made for: a call carries
// the time its message came in, and your message is stored during that
// request, so a call belongs to your first message at or after it.
func CallsByTurn(turns []Turn, calls []StoredCall) []TurnCalls {
	var yours []Turn
	for _, t := range turns {
		if t.Role == "user" && t.Author == nil {
			yours = append(yours, t)
		}
	}
	groups := []TurnCalls{}
	index := map[int64]int{}
	for _, call := range calls {
		if call.Exchange == nil {
			continue
		}
		for _, t := range yours {
			if t.At >= *call.Exchange {
				if i, ok := index[t.ID]; ok {
					groups[i].Calls = append(groups[i].Calls, call)
				} else {
					index[t.ID] = len(groups)
					groups = append(groups, TurnCalls{Turn: t.ID, Calls: []StoredCall{call}})
				}
				break
			}
		}
	}
	return groups
}

func (n *Norn) llmCalls(w http.ResponseWriter, r *http.Request) {
	session := r.URL.Query().Get("session")
	if !sessionID.MatchString(session) {
		part.Error(w, http.StatusBadRequest, "`session` must be 1-64 letters, digits, - or _")
		return
	}
	turns, err := n.Store.Chat(r.Context(), session, 0, 10_000)
	if err != nil {
		n.fail(w, err)
		return
	}
	calls, err := n.Store.Calls(r.Context(), session)
	if err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"turns": CallsByTurn(turns, calls)})
}

// --- Laya ------------------------------------------------------------------------

// systemOne forwards to Laya unchanged, so the widget can point its judge
// here too. The Authorization header carries Norn's token, so Laya's key
// travels in X-Laya-Authorization, or is Norn's own LAYA_KEY. Norn's token is
// never passed on.
func (n *Norn) systemOne(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, part.MaxBody))
	if err != nil {
		part.Error(w, http.StatusBadRequest, "could not read the body")
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(n.Config.LayaURL, "/")+"/v1/systemone", strings.NewReader(string(body)))
	if err != nil {
		n.fail(w, err)
		return
	}
	request.Header.Set("Content-Type", "application/json")
	if key := r.Header.Get("X-Laya-Authorization"); key != "" {
		request.Header.Set("Authorization", key)
	} else if n.Config.LayaKey != "" {
		request.Header.Set("Authorization", "Bearer "+n.Config.LayaKey)
	}
	response, err := n.HTTP.Do(request)
	if err != nil {
		part.Error(w, http.StatusBadGateway, "laya did not answer")
		return
	}
	defer response.Body.Close()
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}
