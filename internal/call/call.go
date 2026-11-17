// Package call is how a part calls another part's JSON routes with its own
// token. Huginn and Dellingr use it to ask Norn, read Sindri's agents and
// draft goals.
package call

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Part is another part, at its URL, with this part's token there. A zero URL
// or token means it isn't set up: every call returns ErrOff.
type Part struct {
	Name  string
	URL   string
	Token string
	HTTP  *http.Client
}

// ErrOff is returned when the part isn't set up.
var ErrOff = fmt.Errorf("not set up")

// New makes a part with the timeout given.
func New(name, url, token string, timeout time.Duration) *Part {
	return &Part{Name: name, URL: strings.TrimRight(url, "/"), Token: token, HTTP: &http.Client{Timeout: timeout}}
}

// On reports whether it is set up.
func (p *Part) On() bool {
	return p != nil && p.URL != "" && p.Token != ""
}

// Do sends body (nil for none) as JSON and decodes the answer into out (nil
// to ignore it). An answer of 300 or more is an error that holds its text.
func (p *Part) Do(ctx context.Context, method, path string, body, out any) error {
	if !p.On() {
		return fmt.Errorf("%s: %w", p.Name, ErrOff)
	}
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, p.URL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+p.Token)
	response, err := p.HTTP.Do(request)
	if err != nil {
		return fmt.Errorf("%s: %w", p.Name, err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		text, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("%s answered %d: %s", p.Name, response.StatusCode, strings.TrimSpace(string(text)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(out)
}

// Ask sends a question to Norn's POST /v1/ask with `to: norn` and `/local`
// in front, so Norn answers it itself and never hands it to an agent, and
// returns the answer.
func (p *Part) Ask(ctx context.Context, question, session string) (string, error) {
	var answer struct {
		Answer string `json:"answer"`
	}
	err := p.Do(ctx, http.MethodPost, "/v1/ask", map[string]string{"question": "/local " + question, "to": "norn", "session": session}, &answer)
	return strings.TrimSpace(answer.Answer), err
}

// Handed is what Norn did with a question sent on to Claude: Delegated is
// false when nothing was handed over, and Answer says why.
type Handed struct {
	Answer    string `json:"answer"`
	Delegated *struct {
		AgentID string `json:"agentId"`
	} `json:"delegated"`
}

// AskClaude sends a question to Norn with `/claude` in front, so Norn hands
// it to a Claude agent, which answers later in session (read it with Chat).
func (p *Part) AskClaude(ctx context.Context, question, session string) (Handed, error) {
	var handed Handed
	err := p.Do(ctx, http.MethodPost, "/v1/ask", map[string]string{"question": "/claude " + question, "to": "norn", "session": session}, &handed)
	return handed, err
}

// Turn is one message in a Norn conversation.
type Turn struct {
	ID      int64   `json:"id"`
	Role    string  `json:"role"`
	Content string  `json:"content"`
	Author  *string `json:"author"`
}

// Chat is a Norn conversation's messages after the id given.
func (p *Part) Chat(ctx context.Context, session string, after int64) ([]Turn, error) {
	var chat struct {
		Messages []Turn `json:"messages"`
	}
	err := p.Do(ctx, http.MethodGet, fmt.Sprintf("/v1/chat?session=%s&after=%d", url.QueryEscape(session), after), nil, &chat)
	return chat.Messages, err
}
