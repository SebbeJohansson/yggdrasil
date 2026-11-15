// Package mcp is a small client for MCP servers over streamable HTTP: list
// the tools, call one. Enough for Norn's read-only lookups.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Protocol is the MCP version spoken.
const Protocol = "2025-06-18"

// Tool is one tool a server offers.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Client talks to one MCP server with a bearer token.
type Client struct {
	URL, Token string
	HTTP       *http.Client
	Timeout    time.Duration

	mu      sync.Mutex
	session *string
	nextID  int
}

type status struct{ code int }

func (s status) Error() string { return fmt.Sprintf("%d", s.code) }

// Tools lists the server's tools.
func (c *Client) Tools(ctx context.Context) ([]Tool, error) {
	var result struct {
		Tools []Tool `json:"tools"`
	}
	return result.Tools, c.request(ctx, "tools/list", map[string]any{}, &result)
}

// Call runs a tool and returns its text; a tool error comes back as "Error: …".
func (c *Client) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := c.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &result); err != nil {
		return "", err
	}
	var parts []string
	for _, p := range result.Content {
		if p.Type == "text" {
			parts = append(parts, p.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if result.IsError {
		return "Error: " + text, nil
	}
	return text, nil
}

func (c *Client) id() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return c.nextID
}

func (c *Client) sessionID(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.session != nil {
		defer c.mu.Unlock()
		return *c.session, nil
	}
	c.mu.Unlock()
	response, err := c.send(ctx, map[string]any{"jsonrpc": "2.0", "id": c.id(), "method": "initialize", "params": map[string]any{
		"protocolVersion": Protocol, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "yggdrasil", "version": "1"},
	}}, "")
	if err != nil {
		return "", err
	}
	if err := reply(response, nil); err != nil {
		return "", err
	}
	session := response.Header.Get("Mcp-Session-Id")
	if r, err := c.send(ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, session); err == nil {
		r.Body.Close()
	}
	c.mu.Lock()
	c.session = &session
	c.mu.Unlock()
	return session, nil
}

func (c *Client) request(ctx context.Context, method string, params map[string]any, into any) error {
	for attempt := 0; ; attempt++ {
		session, err := c.sessionID(ctx)
		if err != nil {
			return err
		}
		response, err := c.send(ctx, map[string]any{"jsonrpc": "2.0", "id": c.id(), "method": method, "params": params}, session)
		var gone status
		if errors.As(err, &gone) && gone.code == http.StatusNotFound && attempt == 0 {
			// The session expired: open a new one, once.
			c.mu.Lock()
			c.session = nil
			c.mu.Unlock()
			continue
		}
		if err != nil {
			return err
		}
		return reply(response, into)
	}
}

func (c *Client) send(ctx context.Context, body map[string]any, session string) (*http.Response, error) {
	payload, _ := json.Marshal(body)
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(payload))
	if err != nil {
		cancel()
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Authorization", "Bearer "+c.Token)
	request.Header.Set("Mcp-Protocol-Version", Protocol)
	if session != "" {
		request.Header.Set("Mcp-Session-Id", session)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		cancel()
		return nil, err
	}
	data, _ := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	response.Body.Close()
	cancel()
	response.Body = io.NopCloser(bytes.NewReader(data))
	if response.StatusCode >= 300 {
		if response.StatusCode == http.StatusNotFound {
			return nil, status{response.StatusCode}
		}
		return nil, fmt.Errorf("%d from %s: %.200s", response.StatusCode, c.URL, data)
	}
	return response, nil
}

// reply reads the JSON-RPC result, from plain JSON or an event stream.
func reply(response *http.Response, into any) error {
	data, _ := io.ReadAll(response.Body)
	var messages []json.RawMessage
	if strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 0, 64<<10), len(data)+1)
		for scanner.Scan() {
			if line, ok := strings.CutPrefix(scanner.Text(), "data:"); ok {
				messages = append(messages, json.RawMessage(strings.TrimSpace(line)))
			}
		}
	} else {
		messages = append(messages, data)
	}
	for _, raw := range messages {
		var m struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &m) != nil || (m.Result == nil && m.Error == nil) {
			continue
		}
		if m.Error != nil {
			return errors.New("MCP error: " + m.Error.Message)
		}
		if into == nil {
			return nil
		}
		return json.Unmarshal(m.Result, into)
	}
	return errors.New("the MCP server answered without a result")
}
