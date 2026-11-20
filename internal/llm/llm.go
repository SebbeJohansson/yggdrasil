// Package llm talks to the language models: an OpenAI-compatible server
// first (XeStrata, llama.cpp, LM Studio…), Ollama when it is off, slow or
// broken. Ollama loads its models only when asked, so as a fallback it costs
// no memory until needed. Without the OpenAI-compatible server, Ollama alone
// answers everything, tool calls too.
//
// Every attempt, failed ones too, is handed to Record, for the chat's debug
// view. The conversation and message it belongs to ride on the context
// (WithExchange), so the code in between need not pass them along.
//
// Ported from my-agent's llm.ts, ollama.ts and openai.ts.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Message is one chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Options are one chat call's settings.
type Options struct {
	// Purpose is what the call is for, as the debug view names it: dispatch, answer…
	Purpose     string
	Model       string
	Temperature float64
	TopP        float64
	MaxTokens   int
	// Format is a JSON schema the reply must match (structured output).
	Format map[string]any
	// Think switches a thinking model's reasoning pass; nil leaves it off on
	// both backends, since every caller was written for models that do not think.
	Think *bool
	// KeepAlive is how long Ollama keeps the model loaded after this call.
	KeepAlive string
	Timeout   time.Duration
	// NoFallback skips Ollama when XeStrata fails: Ollama runs on the CPU
	// here, too slow for a call you are waiting on that has a rule to fall
	// back to instead (dispatch, route).
	NoFallback bool
}

// Usage is what a backend says about one answer; nil where it says nothing.
type Usage struct {
	PromptTokens *int     `json:"promptTokens"`
	CachedTokens *int     `json:"cachedTokens"`
	OutputTokens *int     `json:"outputTokens"`
	PromptMS     *float64 `json:"promptMs"`
	OutputMS     *float64 `json:"outputMs"`
}

// Call is one attempt at an answer, as the debug view shows it.
type Call struct {
	At int64 `json:"at"`
	// Session is the conversation it was made for; empty outside one.
	Session string `json:"session"`
	// Exchange is when the message it answers came in (epoch ms); 0 outside one.
	Exchange int64  `json:"exchange"`
	Purpose  string `json:"purpose"`
	Backend  string `json:"backend"`
	Model    string `json:"model"`
	Usage
	TotalMS  int64     `json:"totalMs"`
	Error    string    `json:"error"`
	Messages []Message `json:"messages"`
	Response string    `json:"response"`
	Thinking string    `json:"thinking"`
	// Placeholders is the names swapped out of what was sent (Claude
	// handovers), with the real names: shown on this machine only.
	Placeholders []Placeholder `json:"placeholders,omitempty"`
}

// Placeholder is one name swapped for a placeholder, and how it was found.
type Placeholder struct {
	Token string `json:"token"`
	Name  string `json:"name"`
	Found string `json:"found"`
}

// MaxCallText is the longest text kept per message, reply or thinking, and
// MaxCallMessages the most messages kept per call (the newest).
const (
	MaxCallText     = 20_000
	MaxCallMessages = 40
)

func clip(text string) string {
	if len(text) <= MaxCallText {
		return text
	}
	return fmt.Sprintf("%s\n… [cut: %d more characters]", text[:MaxCallText], len(text)-MaxCallText)
}

func clipMessages(messages []Message) []Message {
	if len(messages) > MaxCallMessages {
		messages = messages[len(messages)-MaxCallMessages:]
	}
	clipped := make([]Message, len(messages))
	for i, message := range messages {
		clipped[i] = Message{Role: message.Role, Content: clip(message.Content)}
	}
	return clipped
}

type exchangeKey struct{}

type exchange struct {
	session string
	at      int64
}

// WithExchange marks every call made with ctx as part of one message in a
// conversation, for the debug view.
func WithExchange(ctx context.Context, session string, at time.Time) context.Context {
	return context.WithValue(ctx, exchangeKey{}, exchange{session: session, at: at.UnixMilli()})
}

// Note hands Record a call made some other way (a Claude agent through
// Sindri), in the conversation and message ctx is marked with.
func (c *Client) Note(ctx context.Context, call Call) {
	if ex, ok := ctx.Value(exchangeKey{}).(exchange); ok && call.Session == "" {
		call.Session, call.Exchange = ex.session, ex.at
	}
	call.Messages, call.Response = clipMessages(call.Messages), clip(call.Response)
	if c.Record != nil {
		c.Record(call)
	}
}

// Client chats with the OpenAI-compatible server first and Ollama second,
// and embeds with the embedding server or Ollama.
type Client struct {
	// PrimaryURL is the OpenAI-compatible base URL, ending in /v1. Empty: Ollama only.
	PrimaryURL string
	// OllamaURL is Ollama's base URL. Empty: no Ollama at all, for answers or embeddings.
	OllamaURL string
	// EmbedURL is an OpenAI-compatible base URL, ending in /v1, that makes
	// embeddings (llama-server --embeddings). Empty: Ollama makes them.
	EmbedURL string
	// Record is handed every attempt.
	Record func(Call)
	Warn   func(format string, args ...any)
	HTTP   *http.Client
	// NoFallback never asks Ollama when XeStrata fails, whatever a call's
	// options say: a failed answer beats a 5 GB model loading on the CPU.
	NoFallback bool
	// lastAnswer is when XeStrata last answered, in Unix ms, for KeepWarm.
	lastAnswer atomic.Int64
	// Cloud answers instead of XeStrata and Ollama while cloud mode is on
	// (SetCloud); Guard looks at each call first (cloud.go).
	Cloud   CloudFunc
	Guard   CloudGuard
	cloudOn atomic.Bool
}

// ErrNoModel is a client with neither XeStrata nor Ollama.
var ErrNoModel = errors.New("no model server: CHAT_URL and OLLAMA_URL are both off")

// ErrNoEmbeddings is a client with neither an embedding server nor Ollama.
var ErrNoEmbeddings = errors.New("no embedding server: EMBED_URL and OLLAMA_URL are both off")

// Chat answers, from XeStrata or, when it fails, from Ollama.
func (c *Client) Chat(ctx context.Context, messages []Message, options Options) (string, error) {
	if c.CloudOn() {
		return c.cloudAttempt(ctx, messages, options)
	}
	if c.PrimaryURL == "" {
		if c.OllamaURL == "" {
			return "", ErrNoModel
		}
		return c.attempt(ctx, "ollama", messages, options)
	}
	content, err := c.attempt(ctx, "xestrata", messages, options)
	if err == nil {
		return content, nil
	}
	if options.NoFallback || c.NoFallback || c.OllamaURL == "" {
		return "", err
	}
	if c.Warn != nil {
		c.Warn("%s failed (%v); asking Ollama instead", c.PrimaryURL, err)
	}
	return c.attempt(ctx, "ollama", messages, options)
}

// KeepWarm asks XeStrata for one token when it has not answered for `idle`.
// Its model sits in the iGPU's shared memory, and Windows pages it out when
// left alone: the first message after that waits 30 s or more for the model
// to come back (0.8 prompt tokens a second, against 40 when warm). The ask
// is not recorded; the debug view is for your messages.
func (c *Client) KeepWarm(ctx context.Context, idle time.Duration) error {
	if c.PrimaryURL == "" || c.CloudOn() || time.Since(time.UnixMilli(c.lastAnswer.Load())) < idle {
		return nil
	}
	_, err := c.openaiChat(ctx, []Message{{Role: "user", Content: "hi"}}, Options{MaxTokens: 1, Timeout: 2 * time.Minute})
	if err != nil {
		return fmt.Errorf("could not keep %s warm: %w", c.PrimaryURL, err)
	}
	c.lastAnswer.Store(time.Now().UnixMilli())
	return nil
}

type result struct {
	content  string
	thinking string
	usage    Usage
}

func (c *Client) attempt(ctx context.Context, backend string, messages []Message, options Options) (string, error) {
	start := time.Now()
	call := Call{At: start.UnixMilli(), Purpose: options.Purpose, Backend: backend, Model: options.Model, Messages: clipMessages(messages)}
	if call.Purpose == "" {
		call.Purpose = "chat"
	}
	if ex, ok := ctx.Value(exchangeKey{}).(exchange); ok {
		call.Session, call.Exchange = ex.session, ex.at
	}
	var answer result
	var err error
	if backend == "xestrata" {
		answer, err = c.openaiChat(ctx, messages, options)
	} else {
		answer, err = c.ollamaChat(ctx, messages, options)
	}
	call.TotalMS = time.Since(start).Milliseconds()
	if err == nil && backend == "xestrata" {
		c.lastAnswer.Store(time.Now().UnixMilli())
	}
	if err != nil {
		call.Error = err.Error()
	} else {
		call.Usage, call.Response, call.Thinking = answer.usage, clip(answer.content), clip(answer.thinking)
	}
	if c.Record != nil {
		c.Record(call)
	}
	return answer.content, err
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) post(ctx context.Context, url string, payload any, timeout time.Duration, into any) error {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return err
	}
	if response.StatusCode >= 300 {
		text := string(data)
		if len(text) > 200 {
			text = text[:200]
		}
		return fmt.Errorf("%s answered %d: %s", url[strings.LastIndex(url, "/"):], response.StatusCode, text)
	}
	return json.Unmarshal(data, into)
}

func intPtr(value *float64) *int {
	if value == nil {
		return nil
	}
	n := int(*value)
	return &n
}

func (c *Client) ollamaChat(ctx context.Context, messages []Message, options Options) (result, error) {
	payload := map[string]any{
		"model":    options.Model,
		"stream":   false,
		"messages": messages,
		"options":  map[string]any{"temperature": options.Temperature, "top_p": options.TopP, "num_predict": options.MaxTokens},
	}
	if options.Format != nil {
		payload["format"] = options.Format
	}
	ollamaShared(payload, options)
	var body struct {
		Message *struct {
			Content  *string `json:"content"`
			Thinking *string `json:"thinking"`
		} `json:"message"`
		PromptEvalCount    *float64 `json:"prompt_eval_count"`
		PromptEvalDuration *float64 `json:"prompt_eval_duration"`
		EvalCount          *float64 `json:"eval_count"`
		EvalDuration       *float64 `json:"eval_duration"`
	}
	if err := c.post(ctx, c.OllamaURL+"/api/chat", payload, options.Timeout, &body); err != nil {
		return result{}, fmt.Errorf("ollama: %w", err)
	}
	if body.Message == nil || body.Message.Content == nil {
		return result{}, fmt.Errorf("ollama answered without message.content")
	}
	answer := result{content: *body.Message.Content, usage: ollamaUsage(body.PromptEvalCount, body.PromptEvalDuration, body.EvalCount, body.EvalDuration)}
	if body.Message.Thinking != nil {
		answer.thinking = *body.Message.Thinking
	}
	return answer, nil
}

// ollamaShared sets what every Ollama /api/chat call carries: thinking off
// unless asked for (a Qwen3 thinking pass spends a small num_predict before
// any answer), and how long the model stays loaded.
func ollamaShared(payload map[string]any, options Options) {
	payload["think"] = options.Think != nil && *options.Think
	if options.KeepAlive != "" {
		payload["keep_alive"] = options.KeepAlive
	}
}

// ollamaUsage turns Ollama's nanosecond counts into a Usage.
func ollamaUsage(promptCount, promptNS, evalCount, evalNS *float64) Usage {
	ms := func(ns *float64) *float64 {
		if ns == nil {
			return nil
		}
		value := *ns / 1e6
		return &value
	}
	return Usage{PromptTokens: intPtr(promptCount), OutputTokens: intPtr(evalCount), PromptMS: ms(promptNS), OutputMS: ms(evalNS)}
}

type openaiUsage struct {
	PromptTokens        *float64 `json:"prompt_tokens"`
	CompletionTokens    *float64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens *float64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type openaiTimings struct {
	PromptMS    *float64 `json:"prompt_ms"`
	PredictedMS *float64 `json:"predicted_ms"`
}

func (u *openaiUsage) usage(timings *openaiTimings) Usage {
	var usage Usage
	if u != nil {
		usage.PromptTokens, usage.OutputTokens = intPtr(u.PromptTokens), intPtr(u.CompletionTokens)
		if u.PromptTokensDetails != nil {
			usage.CachedTokens = intPtr(u.PromptTokensDetails.CachedTokens)
		}
	}
	if timings != nil {
		usage.PromptMS, usage.OutputMS = timings.PromptMS, timings.PredictedMS
	}
	return usage
}

func (c *Client) openaiPayload(messages any, options Options) map[string]any {
	payload := map[string]any{
		"model":       options.Model,
		"stream":      false,
		"messages":    messages,
		"temperature": options.Temperature,
		"top_p":       options.TopP,
		"max_tokens":  options.MaxTokens,
	}
	// Off unless asked for: every caller was written for models that do not
	// think, and a Qwen3 thinking pass spends a small max_tokens before any answer.
	if options.Think == nil || !*options.Think {
		payload["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
	}
	return payload
}

func (c *Client) openaiChat(ctx context.Context, messages []Message, options Options) (result, error) {
	payload := c.openaiPayload(messages, options)
	if options.Format != nil {
		payload["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "reply", "schema": options.Format}}
	}
	var body struct {
		Choices []struct {
			Message *struct {
				Content          *string `json:"content"`
				ReasoningContent string  `json:"reasoning_content"`
				Reasoning        string  `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
		Usage   *openaiUsage   `json:"usage"`
		Timings *openaiTimings `json:"timings"`
	}
	if err := c.post(ctx, c.PrimaryURL+"/chat/completions", payload, options.Timeout, &body); err != nil {
		return result{}, err
	}
	if len(body.Choices) == 0 || body.Choices[0].Message == nil || body.Choices[0].Message.Content == nil {
		return result{}, fmt.Errorf("chat/completions answered without choices[0].message.content")
	}
	message := body.Choices[0].Message
	thinking := message.ReasoningContent
	if thinking == "" {
		thinking = message.Reasoning
	}
	return result{content: *message.Content, thinking: thinking, usage: body.Usage.usage(body.Timings)}, nil
}

// Embed is one vector per input, in order, from Ollama.
func (c *Client) Embed(ctx context.Context, model string, inputs []string) ([][]float64, error) {
	if c.EmbedURL != "" {
		return c.openaiEmbed(ctx, model, inputs)
	}
	if c.OllamaURL == "" {
		return nil, ErrNoEmbeddings
	}
	var body struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := c.post(ctx, c.OllamaURL+"/api/embed", map[string]any{"model": model, "input": inputs}, 30*time.Second, &body); err != nil {
		return nil, fmt.Errorf("ollama: %w", err)
	}
	if len(body.Embeddings) != len(inputs) {
		return nil, fmt.Errorf("ollama answered without one embedding per input")
	}
	return body.Embeddings, nil
}

// CanEmbed reports whether the client has somewhere to make embeddings.
func (c *Client) CanEmbed() bool {
	return c.EmbedURL != "" || c.OllamaURL != ""
}

// openaiEmbed asks an OpenAI-compatible /v1/embeddings, one vector per input, in order.
func (c *Client) openaiEmbed(ctx context.Context, model string, inputs []string) ([][]float64, error) {
	var body struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := c.post(ctx, c.EmbedURL+"/embeddings", map[string]any{"model": model, "input": inputs}, 30*time.Second, &body); err != nil {
		return nil, fmt.Errorf("embeddings: %w", err)
	}
	if len(body.Data) != len(inputs) {
		return nil, fmt.Errorf("embeddings answered %d vectors for %d inputs", len(body.Data), len(inputs))
	}
	vectors := make([][]float64, len(inputs))
	for _, item := range body.Data {
		if item.Index < 0 || item.Index >= len(inputs) || len(item.Embedding) == 0 {
			return nil, fmt.Errorf("embeddings answered a vector out of place")
		}
		vectors[item.Index] = item.Embedding
	}
	return vectors, nil
}

// ToolCall is a function the model wants called.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ToolMessage is a message in a tool conversation: a plain one, an
// assistant's tool calls, or a tool's result.
type ToolMessage struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Tool is a tool as the model sees it, in OpenAI's function format.
type Tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

// ToolTurn is one turn: the model's text, or the tools it wants called.
type ToolTurn struct {
	Content   string
	Thinking  string
	ToolCalls []ToolCall
	Usage     Usage
}

// ToolChat is one turn of a tool conversation, on the OpenAI-compatible
// server, or on Ollama when there is none. Ollama is never a fallback for a
// failed tool turn: the small models it runs beside XeStrata pick tools too
// poorly for that.
func (c *Client) ToolChat(ctx context.Context, messages []ToolMessage, tools []Tool, options Options) (ToolTurn, error) {
	if c.CloudOn() {
		return c.cloudToolChat(ctx, messages, tools, options)
	}
	start := time.Now()
	plain := make([]Message, len(messages))
	for i, message := range messages {
		text := ""
		if message.Content != nil {
			text = *message.Content
		}
		if len(message.ToolCalls) > 0 {
			calls, _ := json.Marshal(message.ToolCalls)
			text += "\n" + string(calls)
		}
		plain[i] = Message{Role: message.Role, Content: text}
	}
	backend := "xestrata"
	if c.PrimaryURL == "" {
		backend = "ollama"
	}
	call := Call{At: start.UnixMilli(), Purpose: options.Purpose, Backend: backend, Model: options.Model, Messages: clipMessages(plain)}
	if ex, ok := ctx.Value(exchangeKey{}).(exchange); ok {
		call.Session, call.Exchange = ex.session, ex.at
	}
	var turn ToolTurn
	var err error
	if backend == "ollama" {
		turn, err = c.ollamaToolChat(ctx, messages, tools, options)
	} else {
		turn, err = c.toolChat(ctx, messages, tools, options)
	}
	call.TotalMS = time.Since(start).Milliseconds()
	if err != nil {
		call.Error = err.Error()
	} else {
		call.Usage, call.Thinking = turn.Usage, clip(turn.Thinking)
		call.Response = clip(turn.Content)
		if call.Response == "" {
			calls, _ := json.Marshal(turn.ToolCalls)
			call.Response = clip(string(calls))
		}
	}
	if c.Record != nil {
		c.Record(call)
	}
	return turn, err
}

func (c *Client) toolChat(ctx context.Context, messages []ToolMessage, tools []Tool, options Options) (ToolTurn, error) {
	payload := c.openaiPayload(messages, options)
	payload["tools"] = tools
	var body struct {
		Choices []struct {
			Message *struct {
				Content          *string    `json:"content"`
				ToolCalls        []ToolCall `json:"tool_calls"`
				ReasoningContent string     `json:"reasoning_content"`
				Reasoning        string     `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
		Usage   *openaiUsage   `json:"usage"`
		Timings *openaiTimings `json:"timings"`
	}
	if err := c.post(ctx, c.PrimaryURL+"/chat/completions", payload, options.Timeout, &body); err != nil {
		return ToolTurn{}, err
	}
	if len(body.Choices) == 0 || body.Choices[0].Message == nil {
		return ToolTurn{}, fmt.Errorf("chat/completions answered without choices[0].message")
	}
	message := body.Choices[0].Message
	turn := ToolTurn{ToolCalls: message.ToolCalls, Usage: body.Usage.usage(body.Timings), Thinking: message.ReasoningContent}
	if turn.Thinking == "" {
		turn.Thinking = message.Reasoning
	}
	if message.Content != nil {
		turn.Content = *message.Content
	}
	return turn, nil
}

// ollamaToolChat is one tool turn on Ollama's own /api/chat, which takes and
// gives a call's arguments as a JSON object rather than OpenAI's string, and
// gives calls no ids: they are numbered here, in order.
func (c *Client) ollamaToolChat(ctx context.Context, messages []ToolMessage, tools []Tool, options Options) (ToolTurn, error) {
	if c.OllamaURL == "" {
		return ToolTurn{}, ErrNoModel
	}
	type ollamaCall struct {
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	type ollamaMessage struct {
		Role      string       `json:"role"`
		Content   string       `json:"content"`
		ToolCalls []ollamaCall `json:"tool_calls,omitempty"`
		ToolName  string       `json:"tool_name,omitempty"`
	}
	names := map[string]string{}
	sent := make([]ollamaMessage, len(messages))
	for i, message := range messages {
		out := ollamaMessage{Role: message.Role}
		if message.Content != nil {
			out.Content = *message.Content
		}
		for _, call := range message.ToolCalls {
			names[call.ID] = call.Function.Name
			var oc ollamaCall
			oc.Function.Name = call.Function.Name
			oc.Function.Arguments = json.RawMessage(call.Function.Arguments)
			if !json.Valid(oc.Function.Arguments) {
				oc.Function.Arguments = json.RawMessage("{}")
			}
			out.ToolCalls = append(out.ToolCalls, oc)
		}
		if message.ToolCallID != "" {
			out.ToolName = names[message.ToolCallID]
		}
		sent[i] = out
	}
	payload := map[string]any{
		"model":    options.Model,
		"stream":   false,
		"messages": sent,
		"tools":    tools,
		"options":  map[string]any{"temperature": options.Temperature, "top_p": options.TopP, "num_predict": options.MaxTokens},
	}
	ollamaShared(payload, options)
	var body struct {
		Message *struct {
			Content   string       `json:"content"`
			Thinking  string       `json:"thinking"`
			ToolCalls []ollamaCall `json:"tool_calls"`
		} `json:"message"`
		PromptEvalCount    *float64 `json:"prompt_eval_count"`
		PromptEvalDuration *float64 `json:"prompt_eval_duration"`
		EvalCount          *float64 `json:"eval_count"`
		EvalDuration       *float64 `json:"eval_duration"`
	}
	if err := c.post(ctx, c.OllamaURL+"/api/chat", payload, options.Timeout, &body); err != nil {
		return ToolTurn{}, fmt.Errorf("ollama: %w", err)
	}
	if body.Message == nil {
		return ToolTurn{}, fmt.Errorf("ollama answered without message")
	}
	turn := ToolTurn{
		Content:  body.Message.Content,
		Thinking: body.Message.Thinking,
		Usage:    ollamaUsage(body.PromptEvalCount, body.PromptEvalDuration, body.EvalCount, body.EvalDuration),
	}
	for i, oc := range body.Message.ToolCalls {
		call := ToolCall{ID: fmt.Sprintf("call_%d", i), Type: "function"}
		call.Function.Name = oc.Function.Name
		call.Function.Arguments = string(oc.Function.Arguments)
		if call.Function.Arguments == "" || call.Function.Arguments == "null" {
			call.Function.Arguments = "{}"
		}
		turn.ToolCalls = append(turn.ToolCalls, call)
	}
	return turn, nil
}
