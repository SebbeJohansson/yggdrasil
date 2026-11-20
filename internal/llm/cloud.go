package llm

// Cloud mode: for a machine too slow to run a model, every call that would
// go to XeStrata or Ollama goes to Claude Code or Copilot CLI instead,
// through Sindri's POST /v1/complete. A call there is one plain answer: the
// conversation is written out as one prompt, a JSON schema becomes an
// instruction to answer in JSON, and tools become an instruction to answer
// with the calls to make as JSON. Embeddings stay where they are.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CloudAnswer is what Cloud gives back: the text, and which engine and model.
type CloudAnswer struct {
	Text, Engine, Model string
}

// CloudFunc asks Claude Code or Copilot CLI for one answer.
type CloudFunc func(ctx context.Context, system, prompt string) (CloudAnswer, error)

// CloudGuard looks at a call before it leaves for the cloud. It may refuse
// it (ErrKeptLocal), or change its messages (names swapped for
// placeholders), and gives back what turns the answer back (names restored).
type CloudGuard func(ctx context.Context, purpose string, messages []Message) ([]Message, func(string) string, error)

// ErrKeptLocal is a call the privacy setting keeps off the cloud.
var ErrKeptLocal = errors.New("kept on this machine in cloud mode")

// SetCloud turns cloud mode on or off.
func (c *Client) SetCloud(on bool) { c.cloudOn.Store(on) }

// CloudOn is whether calls go to the cloud now.
func (c *Client) CloudOn() bool { return c.Cloud != nil && c.cloudOn.Load() }

// cloudPrompt writes messages out as a system prompt and one prompt.
func cloudPrompt(messages []Message, options Options) (string, string) {
	var system []string
	var turns []Message
	for _, m := range messages {
		if m.Role == "system" {
			system = append(system, m.Content)
		} else {
			turns = append(turns, m)
		}
	}
	if options.Format != nil {
		schema, _ := json.Marshal(options.Format)
		system = append(system, "Answer with one JSON value only, matching this JSON schema. No prose, no code fences.\n"+string(schema))
	}
	if options.MaxTokens > 0 && options.MaxTokens <= 400 {
		system = append(system, "Keep the answer short.")
	}
	var prompt string
	if len(turns) == 1 && turns[0].Role == "user" {
		prompt = turns[0].Content
	} else {
		var b strings.Builder
		b.WriteString("The conversation so far. Write the assistant's next message only.\n")
		for _, m := range turns {
			fmt.Fprintf(&b, "\n[%s]\n%s\n", m.Role, m.Content)
		}
		prompt = b.String()
	}
	return strings.Join(system, "\n\n"), prompt
}

// unfence takes the JSON out of an answer that wrapped it in prose or fences.
func unfence(text string) string {
	t := strings.TrimSpace(text)
	if json.Valid([]byte(t)) {
		return t
	}
	t = strings.TrimPrefix(strings.TrimPrefix(t, "```json"), "```")
	t = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(t), "```"))
	if json.Valid([]byte(t)) {
		return t
	}
	for _, pair := range [][2]string{{"{", "}"}, {"[", "]"}} {
		start, end := strings.Index(t, pair[0]), strings.LastIndex(t, pair[1])
		if start >= 0 && end > start && json.Valid([]byte(t[start:end+1])) {
			return t[start : end+1]
		}
	}
	return text
}

// cloudAttempt is one call to the cloud, recorded like any other.
func (c *Client) cloudAttempt(ctx context.Context, messages []Message, options Options) (string, error) {
	start := time.Now()
	call := Call{At: start.UnixMilli(), Purpose: options.Purpose, Backend: "cloud", Model: options.Model, Messages: clipMessages(messages)}
	if call.Purpose == "" {
		call.Purpose = "chat"
	}
	if ex, ok := ctx.Value(exchangeKey{}).(exchange); ok {
		call.Session, call.Exchange = ex.session, ex.at
	}
	text, err := c.cloudAsk(ctx, call.Purpose, messages, options, &call)
	call.TotalMS = time.Since(start).Milliseconds()
	if err != nil {
		call.Error = err.Error()
	} else {
		call.Response = clip(text)
	}
	if c.Record != nil {
		c.Record(call)
	}
	return text, err
}

func (c *Client) cloudAsk(ctx context.Context, purpose string, messages []Message, options Options, call *Call) (string, error) {
	restore := func(s string) string { return s }
	if c.Guard != nil {
		guarded, back, err := c.Guard(ctx, purpose, messages)
		if err != nil {
			return "", err
		}
		messages, restore = guarded, back
		call.Messages = clipMessages(messages)
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	// The CLI starts up first: give it room over the local model's budget.
	ctx, cancel := context.WithTimeout(ctx, max(timeout, 45*time.Second)+15*time.Second)
	defer cancel()
	system, prompt := cloudPrompt(messages, options)
	answer, err := c.Cloud(ctx, system, prompt)
	if answer.Engine != "" {
		call.Backend, call.Model = answer.Engine, answer.Model
	}
	if err != nil {
		return "", err
	}
	text := answer.Text
	if options.Format != nil {
		text = unfence(text)
	}
	return restore(text), nil
}

// cloudToolChat is one tool turn in the cloud: the tools are described in
// the system prompt, and the answer is JSON naming the calls or the answer.
func (c *Client) cloudToolChat(ctx context.Context, messages []ToolMessage, tools []Tool, options Options) (ToolTurn, error) {
	var b strings.Builder
	b.WriteString("You can call these tools. To call any, answer with only this JSON, no prose or fences:\n")
	b.WriteString(`{"tool_calls":[{"name":"<tool>","arguments":{...}}]}`)
	b.WriteString("\nWhen you have what you need, answer with only this JSON instead:\n")
	b.WriteString(`{"answer":"<your answer to the user>"}`)
	b.WriteString("\n\nTools:\n")
	for _, tool := range tools {
		params, _ := json.Marshal(tool.Function.Parameters)
		fmt.Fprintf(&b, "- %s: %s\n  parameters: %s\n", tool.Function.Name, tool.Function.Description, params)
	}
	plain := []Message{{Role: "system", Content: b.String()}}
	for _, m := range messages {
		text := ""
		if m.Content != nil {
			text = *m.Content
		}
		switch {
		case len(m.ToolCalls) > 0:
			calls, _ := json.Marshal(m.ToolCalls)
			text = strings.TrimSpace(text + "\ncalled: " + string(calls))
		case m.Role == "tool":
			plain = append(plain, Message{Role: "user", Content: "Result of tool call " + m.ToolCallID + ":\n" + text})
			continue
		}
		plain = append(plain, Message{Role: m.Role, Content: text})
	}
	options.Format = nil
	answer, err := c.cloudAttempt(ctx, plain, options)
	if err != nil {
		return ToolTurn{}, err
	}
	var parsed struct {
		ToolCalls []struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"tool_calls"`
		Answer *string `json:"answer"`
	}
	if json.Unmarshal([]byte(unfence(answer)), &parsed) != nil {
		return ToolTurn{Content: answer}, nil
	}
	if len(parsed.ToolCalls) == 0 {
		if parsed.Answer != nil {
			return ToolTurn{Content: *parsed.Answer}, nil
		}
		return ToolTurn{Content: answer}, nil
	}
	turn := ToolTurn{}
	for i, tc := range parsed.ToolCalls {
		call := ToolCall{ID: fmt.Sprintf("call_%d_%d", len(messages), i+1), Type: "function"}
		call.Function.Name = tc.Name
		call.Function.Arguments = string(tc.Arguments)
		if call.Function.Arguments == "" || call.Function.Arguments == "null" {
			call.Function.Arguments = "{}"
		}
		turn.ToolCalls = append(turn.ToolCalls, call)
	}
	return turn, nil
}
