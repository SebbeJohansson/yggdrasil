// Package alert is how a part talks to Gjallar: the shape of an alert and a
// client that raises, resolves, reads back what it raised and sends
// heartbeats.
//
// A sender never depends on Gjallar. Every call has a short timeout, and the
// caller logs a failure and moves on.
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Severity says how loudly Gjallar tells you.
type Severity string

const (
	// Info shows on the dashboard only.
	Info Severity = "info"
	// Warning adds a badge on the widget.
	Warning Severity = "warning"
	// Critical adds a toast on the widget and a push to the phone.
	Critical Severity = "critical"
)

// Valid reports whether s is one of the three severities.
func (s Severity) Valid() bool {
	return s == Info || s == Warning || s == Critical
}

// Rank orders severities: info 0, warning 1, critical 2.
func (s Severity) Rank() int {
	switch s {
	case Critical:
		return 2
	case Warning:
		return 1
	}
	return 0
}

// Raise is what a sender sends. The source is not in it: Gjallar takes it
// from the sender's token, so no part can raise or resolve another's alerts.
type Raise struct {
	// Key names the problem within the source, e.g. "down:norn". Sending the
	// same key again updates that alert instead of making a second one.
	Key       string     `json:"key"`
	Severity  Severity   `json:"severity"`
	Title     string     `json:"title"`
	Detail    string     `json:"detail,omitempty"`
	Link      string     `json:"link,omitempty"`
	TraceID   string     `json:"traceId,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// ErrOff is returned when no Gjallar is set up.
var ErrOff = errors.New("gjallar is not set up")

// Client calls Gjallar. A zero URL turns it off: every call returns ErrOff.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// NewClient makes a client with a 5 s timeout.
func NewClient(url, token string) *Client {
	return &Client{URL: strings.TrimRight(url, "/"), Token: token, HTTP: &http.Client{Timeout: 5 * time.Second}}
}

// Raise opens the alert, or updates the open one with the same key.
func (c *Client) Raise(ctx context.Context, raise Raise) error {
	return c.post(ctx, "/v1/alerts", raise)
}

// Resolve closes the sender's alert with this key: the condition is gone.
func (c *Client) Resolve(ctx context.Context, key string) error {
	return c.post(ctx, "/v1/alerts/resolve", map[string]string{"key": key})
}

// Heartbeat tells Gjallar the sender is alive.
func (c *Client) Heartbeat(ctx context.Context) error {
	return c.post(ctx, "/v1/heartbeat", map[string]string{})
}

// Held is the sender's own open and acknowledged alerts, as Gjallar holds
// them: what it raised before it restarted.
func (c *Client) Held(ctx context.Context) ([]Raise, error) {
	if c == nil || c.URL == "" {
		return nil, ErrOff
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/v1/alerts/mine", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		text, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return nil, fmt.Errorf("gjallar answered %d: %s", response.StatusCode, strings.TrimSpace(string(text)))
	}
	var body struct {
		Alerts []Raise `json:"alerts"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Alerts, nil
}

func (c *Client) post(ctx context.Context, path string, body any) error {
	if c == nil || c.URL == "" {
		return ErrOff
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.Token)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		text, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("gjallar answered %d: %s", response.StatusCode, strings.TrimSpace(string(text)))
	}
	return nil
}
