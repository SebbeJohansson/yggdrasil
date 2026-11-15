package norn

// Memory: recall, remember, and learn from what you say. Ported from
// my-agent's memory/brain.ts and memory/recall.ts.
//
// Learning reads only your own words, never the answer. A small model states
// things confidently that are not true, and a fact taken from its own answer
// would be recalled into the next prompt and repeated back as something you
// said.
//
// Recall is plain cosine similarity over every stored vector: a personal
// assistant holds hundreds of facts, not millions.

import (
	"context"
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"yggdrasil/internal/llm"
)

const (
	// RecallTop is how many memories a prompt gets at most.
	RecallTop = 5
	// RecallMin is the similarity below which a memory is not about the
	// question (tuned for nomic-embed-text).
	RecallMin = 0.5
	// DuplicateMin is the similarity at which a new fact says what an old one does.
	DuplicateMin = 0.9
	// FactsPerMessage is the most facts kept from one message.
	FactsPerMessage = 3
)

// Brain is what memory needs from the models.
type Brain struct {
	Store      *Store
	EmbedModel string
	Embed      func(ctx context.Context, texts []string) ([][]float64, error)
	Complete   func(ctx context.Context, messages []llm.Message, options llm.Options) (string, error)
	Warn       func(format string, args ...any)
}

func (b *Brain) warn(format string, args ...any) {
	if b.Warn != nil {
		b.Warn(format, args...)
	}
}

// Cosine is the cosine similarity of two vectors; 0 when they can't be compared.
func Cosine(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / math.Sqrt(normA*normB)
}

// Recall is the memories most like the question, best first. Without a
// query vector (the embedding model is not pulled, or Ollama failed) the
// newest few stand in.
func Recall(memories []StoredMemory, query []float64, model string) []Memory {
	if query == nil {
		var newest []Memory
		for i := 0; i < len(memories) && i < RecallTop; i++ {
			newest = append(newest, memories[i].Memory)
		}
		return newest
	}
	type scored struct {
		memory Memory
		score  float64
	}
	var candidates []scored
	for _, m := range memories {
		if m.Embedding == nil || m.Model != model {
			continue
		}
		if score := Cosine(query, m.Embedding); score >= RecallMin {
			candidates = append(candidates, scored{m.Memory, score})
		}
	}
	slices.SortStableFunc(candidates, func(a, b scored) int {
		switch {
		case a.score > b.score:
			return -1
		case a.score < b.score:
			return 1
		}
		return 0
	})
	var best []Memory
	for i := 0; i < len(candidates) && i < RecallTop; i++ {
		best = append(best, candidates[i].memory)
	}
	return best
}

var notWordOrNumber = regexp.MustCompile(`[^\p{L}\p{N}]+`)

func normalise(text string) string {
	return strings.TrimSpace(notWordOrNumber.ReplaceAllString(strings.ToLower(text), " "))
}

// IsKnown is whether text is already remembered: by meaning when there are
// vectors, by wording when not.
func IsKnown(memories []StoredMemory, text string, vector []float64, model string) bool {
	wording := normalise(text)
	for _, m := range memories {
		if normalise(m.Text) == wording {
			return true
		}
		if vector != nil && m.Embedding != nil && m.Model == model && Cosine(vector, m.Embedding) >= DuplicateMin {
			return true
		}
	}
	return false
}

func (b *Brain) vectorFor(ctx context.Context, text string) []float64 {
	if b.Embed == nil {
		return nil
	}
	vectors, err := b.Embed(ctx, []string{text})
	if err != nil || len(vectors) == 0 {
		b.warn("no embedding from %s; memory falls back to recency: %v", b.EmbedModel, err)
		return nil
	}
	return vectors[0]
}

// RecallFor is the memories worth putting in front of the model for a question.
func (b *Brain) RecallFor(ctx context.Context, question string) ([]Memory, error) {
	memories, err := b.Store.Memories(ctx)
	if err != nil || len(memories) == 0 {
		return nil, err
	}
	return Recall(memories, b.vectorFor(ctx, question), b.EmbedModel), nil
}

// Remember stores a fact; nil when it is already known.
func (b *Brain) Remember(ctx context.Context, text, source string) (*Memory, error) {
	vector := b.vectorFor(ctx, text)
	memories, err := b.Store.Memories(ctx)
	if err != nil {
		return nil, err
	}
	if IsKnown(memories, text, vector, b.EmbedModel) {
		return nil, nil
	}
	memory, err := b.Store.AddMemory(ctx, text, source, vector, b.EmbedModel)
	if err != nil {
		return nil, err
	}
	return &memory, nil
}

var extractSystem = strings.Join([]string{
	"You pick out lasting facts about the user from one message they wrote to their assistant.",
	"A lasting fact is still true next week: who they are, their role, the people they work with",
	"and how, their projects, their preferences, their recurring commitments.",
	"Not questions, not requests, not anything only about today.",
	"Write each fact as one short sentence about the user. Use only what the message says.",
	"If there are none, return an empty list.",
}, " ")

var factsSchema = map[string]any{
	"type":       "object",
	"properties": map[string]any{"facts": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}},
	"required":   []string{"facts"},
}

// ExtractMessages is the prompt that picks facts out of a message. Two
// examples, one each way: without the empty one a small model finds a
// "fact" in every message.
func ExtractMessages(message string) []llm.Message {
	return []llm.Message{
		{Role: "system", Content: extractSystem},
		{Role: "user", Content: "What did Mira want? She is my team lead, and we ship every Friday."},
		{Role: "assistant", Content: `{"facts":["Mira is the user's team lead.","The user's team ships every Friday."]}`},
		{Role: "user", Content: "Can you rewrite this paragraph to sound less formal?"},
		{Role: "assistant", Content: `{"facts":[]}`},
		{Role: "user", Content: message},
	}
}

var word = regexp.MustCompile(`[\p{L}\x{2019}'-]+`)

// factWords are capitalised in a fact because they open it or the fact is about you.
var factWords = map[string]bool{"the": true, "a": true, "an": true, "user": true, "user's": true, "they": true, "their": true,
	"he": true, "she": true, "his": true, "her": true, "it": true, "its": true}

// NamesAreFromMessage is whether every name in a fact is in the message it
// came from. A 1B model copies the worked example: asked "who is my team
// lead?" it answers with the example's fact and puts Mira in charge.
func NamesAreFromMessage(fact, message string) bool {
	said := map[string]bool{}
	for _, w := range word.FindAllString(message, -1) {
		said[strings.ToLower(w)] = true
	}
	for _, w := range word.FindAllString(fact, -1) {
		lower := strings.ToLower(w)
		first := []rune(w)[0]
		if unicode.IsUpper(first) && !factWords[lower] && !said[lower] {
			return false
		}
	}
	return true
}

// ReadFacts is the facts in a reply; none when the reply is not what was asked for.
func ReadFacts(raw string) []string {
	var parsed struct {
		Facts []any `json:"facts"`
	}
	if json.Unmarshal([]byte(raw), &parsed) != nil {
		return nil
	}
	var facts []string
	for _, item := range parsed.Facts {
		fact, ok := item.(string)
		if !ok {
			continue
		}
		fact = strings.TrimSpace(fact)
		if length := len([]rune(fact)); length >= 8 && length <= 200 {
			facts = append(facts, fact)
		}
		if len(facts) == FactsPerMessage {
			break
		}
	}
	return facts
}

// LearnFrom extracts and stores what is new in one of your messages. It
// never fails: what it can't learn it leaves.
func (b *Brain) LearnFrom(ctx context.Context, message string) []Memory {
	raw, err := b.Complete(ctx, ExtractMessages(message), llm.Options{Purpose: "learn", Temperature: 0, TopP: 1, MaxTokens: 200, Format: factsSchema})
	if err != nil {
		b.warn("could not learn from that message: %v", err)
		return nil
	}
	var learned []Memory
	for _, fact := range ReadFacts(raw) {
		if !NamesAreFromMessage(fact, message) {
			b.warn("not remembering a fact the message did not say: %s", fact)
			continue
		}
		memory, err := b.Remember(ctx, fact, "extracted")
		if err != nil {
			b.warn("could not store a fact: %v", err)
			continue
		}
		if memory != nil {
			learned = append(learned, *memory)
		}
	}
	return learned
}
