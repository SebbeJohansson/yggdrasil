package norn

// Ranking: four rules (mentions you, sent to you, flagged, asks a
// question), who the sender is to you, age, and a judge's two answers (is
// somebody waiting, what happens if it waits until tomorrow). Mutes and the
// chats Teams is quiet about hide what you silenced. Ported from my-agent's
// tasks/inbox/urgency.ts, mutes.ts, quiet.ts, judge.ts and judgeFetch.ts.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/llm"
)

// Weights of each rule in the score.
var (
	relationWeight = map[string]float64{"manager": 2, "above": 2, "leadership": 1, "report": 1, "peer": 0}
)

const (
	weightMention     = 3
	weightDirect      = 2
	weightImportant   = 2
	weightQuestion    = 1
	weightAgePerHour  = 0.1
	maxAgePenalty     = 2
	weightAwaited     = 2
	weightConsequence = 2
	// UrgentAt and NotableAt are where the bands start.
	UrgentAt  = 4
	NotableAt = 2
)

// Judgement is a judge's two answers, each 0 to 1.
type Judgement struct {
	Awaited     float64 `json:"awaited"`
	Consequence float64 `json:"consequence"`
	Confidence  float64 `json:"confidence"`
}

// Ranked is an item with its score, band and why.
type Ranked struct {
	Item      Item       `json:"item"`
	Score     float64    `json:"score"`
	Band      string     `json:"band"` // urgent, notable, quiet, hidden
	Reasons   []string   `json:"reasons"`
	Judgement *Judgement `json:"judgement,omitempty"`
}

func swing(value, weight float64) float64 {
	return (value - 0.5) * 2 * weight
}

func bandFor(score float64) string {
	switch {
	case score >= UrgentAt:
		return "urgent"
	case score >= NotableAt:
		return "notable"
	}
	return "quiet"
}

// Rank scores one item.
func Rank(item Item, now time.Time, judgement *Judgement) Ranked {
	reasons := []string{}
	var score float64
	add := func(on bool, weight float64, reason string) {
		if on {
			score += weight
			reasons = append(reasons, reason)
		}
	}
	add(item.MentionsMe, weightMention, "mentions you")
	add(item.Direct, weightDirect, "sent to you")
	add(item.Important, weightImportant, "flagged important")
	add(strings.Contains(item.Preview, "?") || strings.Contains(item.Subject, "?"), weightQuestion, "asks a question")
	if weight := relationWeight[item.Relation]; weight > 0 {
		add(true, weight, "from "+relationWords[item.Relation])
	}
	age := math.Max(0, float64(now.UnixMilli()-item.At)/3_600_000)
	score -= math.Min(maxAgePenalty, age*weightAgePerHour)
	if judgement != nil {
		score += swing(judgement.Awaited, weightAwaited) + swing(judgement.Consequence, weightConsequence)
		if judgement.Awaited >= 0.7 {
			reasons = append(reasons, "somebody is waiting")
		}
		if judgement.Awaited <= 0.3 {
			reasons = append(reasons, "nobody is waiting")
		}
		if judgement.Consequence >= 0.7 {
			reasons = append(reasons, "costly to leave")
		}
	}
	band := bandFor(score)
	if item.Hidden != "" {
		band = "hidden"
		reasons = append(reasons, item.Hidden)
	}
	return Ranked{Item: item, Score: score, Band: band, Reasons: reasons, Judgement: judgement}
}

// RankAll scores every item, best first; ties go to the newer.
func RankAll(items []Item, now time.Time, judged map[string]Judgement) []Ranked {
	ranked := make([]Ranked, 0, len(items))
	for _, item := range items {
		var judgement *Judgement
		if j, ok := judged[item.ID]; ok {
			judgement = &j
		}
		ranked = append(ranked, Rank(item, now, judgement))
	}
	slices.SortStableFunc(ranked, func(a, b Ranked) int {
		switch {
		case a.Score != b.Score:
			if b.Score > a.Score {
				return 1
			}
			return -1
		case a.Item.At != b.Item.At:
			return int(b.Item.At - a.Item.At)
		}
		return strings.Compare(a.Item.ID, b.Item.ID)
	})
	return ranked
}

func visible(ranked []Ranked) []Ranked {
	shown := []Ranked{}
	for _, r := range ranked {
		if r.Band != "hidden" {
			shown = append(shown, r)
		}
	}
	return shown
}

// Why a muted or quiet chat is hidden. Something that names you gets through anyway.
const (
	MuteReason  = "a muted chat"
	QuietReason = "quiet in Teams"
)

// Silence hides items in muted chats and in chats Teams is quiet about,
// unless they mention you.
func Silence(items []Item, muted, quiet map[string]bool) []Item {
	silenced := make([]Item, len(items))
	for i, item := range items {
		silenced[i] = item
		if item.Hidden != "" || item.Conversation == "" || item.MentionsMe {
			continue
		}
		switch {
		case muted[item.Conversation]:
			silenced[i].Hidden = MuteReason
		case quiet[item.Conversation]:
			silenced[i].Hidden = QuietReason
		}
	}
	return silenced
}

// --- the judge ---------------------------------------------------------------------

// ConsequenceLevels are the judge's answers to "what happens if the reader
// waits until tomorrow?", from nothing to something breaking.
var ConsequenceLevels = []string{
	"Nothing happens. Nobody expects anything back: an announcement, a newsletter, an automated notification, a thread the reader is only copied on, or a remark in a channel that was not addressed to anyone in particular.",
	"Somebody waits a little longer for something they did not ask for urgently: a question with no stated deadline, a review request on work that is not blocking anyone, or a request that another person could equally answer.",
	"Somebody stays blocked. A named person has stopped and cannot continue until the reader answers, or a time the sender stated falls later today.",
	"Something breaks or a commitment is missed: a deadline the reader owns passes, a production problem goes unattended, a customer is left without an answer, or a decision the reader has to make is taken without them.",
}

const (
	awaitedInstructions = "Is a specific person waiting for `message` to be answered or acted on by `reader` before they can carry on? Weigh the subject, the text, who sent it and who else it went to: an ask sent to many people is rarely waiting on the reader in particular. Judge what the message is doing, not whether it is punctuated as a question."
	awaitedTrue         = "The message asks the reader for something: an answer, a decision, a review, an approval, or an action. It counts whether or not it ends in a question mark, and it counts when the ask is phrased politely as a statement (\"let me know if that works\")."
	awaitedFalse        = "Nobody is waiting on the reader. Announcements, notifications, replies that close a thread, acknowledgements, and rhetorical or to-the-room questions that anybody could answer. A question mark on its own does not make this true."
	consequenceText     = "If `reader` does not look at `message` until tomorrow, what happens? Weigh the subject, the text, who sent it and who else it went to. Judge only from what the message says; do not assume a deadline that is not stated."
	judgeConcurrency    = 4
	judgeCacheLimit     = 500
)

var relationForJudge = map[string]string{
	"manager": "the reader's manager", "above": "above the reader's manager in the reader's reporting line",
	"leadership": "company leadership", "report": "reports directly to the reader", "peer": "on the same team as the reader",
}

func ageWords(d time.Duration) string {
	minutes := int(math.Max(0, math.Round(d.Minutes())))
	if minutes < 60 {
		return fmt.Sprintf("%d minutes old", minutes)
	}
	hours := int(math.Round(float64(minutes) / 60))
	if hours < 48 {
		return fmt.Sprintf("%d hours old", hours)
	}
	return fmt.Sprintf("%d days old", int(math.Round(float64(hours)/24)))
}

// JudgeState is what the judge reads about one message.
func JudgeState(item Item, reader string, now time.Time) map[string]any {
	message := map[string]any{
		"from": item.From, "subject": item.Subject, "source": item.Source, "preview": item.Preview,
		"age": ageWords(time.Duration(now.UnixMilli()-item.At) * time.Millisecond), "sentDirectlyToReader": item.Direct,
		"namesTheReader": item.MentionsMe, "senderFlaggedImportant": item.Important,
	}
	if len(item.Recipients) > 1 {
		message["recipients"] = item.Recipients
	}
	if item.Context != "" {
		message["context"] = item.Context
	}
	if item.Relation != "" {
		message["senderIs"] = relationForJudge[item.Relation]
	}
	return map[string]any{"reader": reader, "message": message}
}

func judgeQuestions(neutralLabels bool) map[string]any {
	awaited := map[string]any{"type": "noul", "instructions": awaitedInstructions, "criteria": map[string]string{"true": awaitedTrue, "false": awaitedFalse}}
	if neutralLabels {
		awaited["labels"] = map[string]string{"true": "A", "false": "B"}
	}
	return map[string]any{
		"awaited":     awaited,
		"consequence": map[string]any{"type": "score", "instructions": consequenceText, "criteria": ConsequenceLevels},
	}
}

func unit(value any) (float64, bool) {
	n, ok := value.(float64)
	return n, ok && n >= 0 && n <= 1 && !math.IsNaN(n)
}

// ReadJudgement reads a SystemOne answer; false when it is not one.
func ReadJudgement(body []byte) (Judgement, bool) {
	var parsed struct {
		Answers map[string]map[string]any `json:"answers"`
	}
	if json.Unmarshal(body, &parsed) != nil || parsed.Answers == nil {
		return Judgement{}, false
	}
	awaited, ok := unit(parsed.Answers["awaited"]["noul"])
	level, isNumber := parsed.Answers["consequence"]["score"].(float64)
	if !ok || !isNumber || math.IsNaN(level) || math.IsInf(level, 0) {
		return Judgement{}, false
	}
	confidence, _ := unit(parsed.Answers["consequence"]["confidence"])
	top := float64(len(ConsequenceLevels) - 1)
	return Judgement{Awaited: awaited, Consequence: math.Min(1, math.Max(0, level/top)), Confidence: confidence}, true
}

// JudgeBackend is who answers: Laya or TypeSafe (SystemOne), XeStrata or Ollama.
type JudgeBackend struct {
	ID    string
	Label string
	URL   string
	Model string
	Key   string
	// Chat asks XeStrata (Norn's model client), for ID xestrata.
	Chat func(context.Context, []llm.Message, llm.Options) (string, error)
	// NeutralLabels hides "true"/"false" behind A/B, which Laya answers better.
	NeutralLabels bool
}

// Judge answers for items, keeping answers by item id.
type Judge struct {
	mu    sync.Mutex
	cache map[string]Judgement
	order []string
	HTTP  *http.Client
}

// NewJudge makes a judge with an empty cache.
func NewJudge() *Judge {
	return &Judge{cache: map[string]Judgement{}, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// Judged is every answer so far.
func (j *Judge) Judged() map[string]Judgement {
	j.mu.Lock()
	defer j.mu.Unlock()
	copied := make(map[string]Judgement, len(j.cache))
	for id, judgement := range j.cache {
		copied[id] = judgement
	}
	return copied
}

func (j *Judge) remember(id string, judgement Judgement) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, known := j.cache[id]; !known {
		j.order = append(j.order, id)
	}
	j.cache[id] = judgement
	for len(j.order) > judgeCacheLimit {
		delete(j.cache, j.order[0])
		j.order = j.order[1:]
	}
}

func (j *Judge) post(ctx context.Context, target, key string, payload any) ([]byte, error) {
	body, _ := json.Marshal(payload)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := j.HTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if response.StatusCode >= 300 {
		return nil, fmt.Errorf("answered %d", response.StatusCode)
	}
	return data, nil
}

// judgePrompt is the system prompt and the message, for a judge that is a
// plain model (XeStrata, Ollama) rather than SystemOne.
func judgePrompt(item Item, reader string, now time.Time) (system, state string) {
	levels := make([]string, len(ConsequenceLevels))
	for i, level := range ConsequenceLevels {
		levels[i] = fmt.Sprintf("%d: %s", i, level)
	}
	system = strings.Join([]string{
		"You judge one unread message for its reader. Answer only with JSON.", "",
		"awaited: " + awaitedInstructions, "Yes means: " + awaitedTrue, "No means: " + awaitedFalse,
		"Give the probability (0 to 1) that the answer is yes.", "",
		"consequence: " + consequenceText + " Pick the level that fits:", strings.Join(levels, "\n"),
	}, "\n")
	encoded, _ := json.Marshal(JudgeState(item, reader, now))
	return system, string(encoded)
}

// judgeFormat is the JSON a plain model answers with.
func judgeFormat() map[string]any {
	return map[string]any{"type": "object", "required": []string{"awaited", "consequence"}, "properties": map[string]any{
		"awaited":     map[string]any{"type": "number", "minimum": 0, "maximum": 1},
		"consequence": map[string]any{"type": "integer", "minimum": 0, "maximum": len(ConsequenceLevels) - 1}}}
}

// readPlainJudgement reads a plain model's {awaited, consequence} as SystemOne's shape.
func readPlainJudgement(content string) (Judgement, bool) {
	var answer struct {
		Awaited     any `json:"awaited"`
		Consequence any `json:"consequence"`
	}
	if json.Unmarshal([]byte(content), &answer) != nil {
		return Judgement{}, false
	}
	wrapped, _ := json.Marshal(map[string]any{"answers": map[string]any{"awaited": map[string]any{"noul": answer.Awaited}, "consequence": map[string]any{"score": answer.Consequence, "confidence": 0}}})
	return ReadJudgement(wrapped)
}

func (j *Judge) one(ctx context.Context, backend JudgeBackend, item Item, reader string, now time.Time) (Judgement, bool) {
	if backend.ID == "xestrata" {
		system, state := judgePrompt(item, reader, now)
		content, err := backend.Chat(ctx, []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: state}},
			llm.Options{Purpose: "judge", Model: backend.Model, Temperature: 0, TopP: 1, MaxTokens: 60, Format: judgeFormat(), NoFallback: true})
		if err != nil {
			return Judgement{}, false
		}
		return readPlainJudgement(content)
	}
	if backend.ID == "ollama" {
		system, state := judgePrompt(item, reader, now)
		body, err := j.post(ctx, backend.URL+"/api/chat", "", map[string]any{
			"model": backend.Model, "stream": false, "think": false, "keep_alive": "30m",
			"format":   judgeFormat(),
			"messages": []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": state}},
			"options":  map[string]any{"temperature": 0},
		})
		if err != nil {
			return Judgement{}, false
		}
		var reply struct {
			Message struct{ Content string } `json:"message"`
		}
		if json.Unmarshal(body, &reply) != nil {
			return Judgement{}, false
		}
		return readPlainJudgement(reply.Message.Content)
	}
	payload := map[string]any{"state": JudgeState(item, reader, now), "questions": judgeQuestions(backend.NeutralLabels)}
	if backend.Model != "" {
		payload["model"] = backend.Model
	}
	body, err := j.post(ctx, backend.URL, backend.Key, payload)
	if err != nil {
		return Judgement{}, false
	}
	return ReadJudgement(body)
}

// JudgeAll answers for the items not yet judged, four at a time.
func (j *Judge) JudgeAll(ctx context.Context, backend JudgeBackend, items []Item, reader string, now time.Time, progress func(done, total int)) {
	j.mu.Lock()
	var unjudged []Item
	for _, item := range items {
		if _, ok := j.cache[item.ID]; !ok {
			unjudged = append(unjudged, item)
		}
	}
	j.mu.Unlock()
	if len(unjudged) == 0 {
		return
	}
	var done int
	var mu sync.Mutex
	progress(0, len(unjudged))
	work := make(chan Item)
	var wg sync.WaitGroup
	for range min(judgeConcurrency, len(unjudged)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range work {
				if judgement, ok := j.one(ctx, backend, item, reader, now); ok {
					j.remember(item.ID, judgement)
				}
				mu.Lock()
				done++
				progress(done, len(unjudged))
				mu.Unlock()
			}
		}()
	}
	for _, item := range unjudged {
		work <- item
	}
	close(work)
	wg.Wait()
}

// JudgeSettings say who may judge.
type JudgeSettings struct {
	// Provider is laya, typesafe, xestrata, ollama or off.
	Provider, LayaURL, LayaKey, TypesafeKey, OllamaURL, OllamaModel string
	// Chat asks XeStrata; nil when there is no XeStrata (CHAT_URL off).
	Chat func(context.Context, []llm.Message, llm.Options) (string, error)
}

var embedModel = regexp.MustCompile(`(?i)embed`)

// PickJudge is Laya when it answers, TypeSafe when asked for, XeStrata when
// asked for, else Ollama with whatever it has pulled, or XeStrata when Ollama
// is off; nil means the four rules alone.
func PickJudge(ctx context.Context, settings JudgeSettings, client *http.Client) *JudgeBackend {
	provider := settings.Provider
	if provider == "" {
		provider = "laya"
	}
	if provider == "off" {
		return nil
	}
	reach := func(target string) bool {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		response, err := client.Do(request)
		if err != nil {
			return false
		}
		response.Body.Close()
		return true
	}
	if provider == "laya" && settings.LayaKey != "" {
		target := strings.TrimRight(settings.LayaURL, "/") + "/v1/systemone"
		if reach(target) {
			return &JudgeBackend{ID: "laya", Label: "Laya", URL: target, Key: settings.LayaKey, NeutralLabels: true}
		}
	}
	if provider == "typesafe" && settings.TypesafeKey != "" {
		return &JudgeBackend{ID: "typesafe", Label: "TypeSafe", URL: "https://api.typesafe.ai/v1/systemone", Model: "jev-latest", Key: settings.TypesafeKey}
	}
	xestrata := &JudgeBackend{ID: "xestrata", Label: "XeStrata", Model: settings.OllamaModel, Chat: settings.Chat}
	if provider == "xestrata" || settings.OllamaURL == "" {
		if settings.Chat == nil {
			return nil
		}
		return xestrata
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, settings.OllamaURL+"/api/tags", nil)
	response, err := client.Do(request)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	var tags struct {
		Models []struct{ Name string } `json:"models"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&tags) != nil {
		return nil
	}
	var names []string
	for _, model := range tags.Models {
		if model.Name != "" && !embedModel.MatchString(model.Name) {
			names = append(names, model.Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	model := names[0]
	if slices.Contains(names, settings.OllamaModel) {
		model = settings.OllamaModel
	}
	return &JudgeBackend{ID: "ollama", Label: "Ollama (" + model + ")", URL: settings.OllamaURL, Model: model}
}
