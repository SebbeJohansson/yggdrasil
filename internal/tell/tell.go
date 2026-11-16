// Package tell is how a part tells you something: through Hermóðr, which
// carries it to your phone, Slack and Discord and lists it for the widget.
// A part that must reach you when Hermóðr itself may be down (Gjallar,
// Ratatoskr) chains Hermóðr with a direct ntfy topic.
package tell

import (
	"context"
	"errors"
	"net/http"
	"time"

	"yggdrasil/internal/call"
)

// Message is what a part sends.
type Message struct {
	Title    string `json:"title"`
	Text     string `json:"text,omitempty"`
	Link     string `json:"link,omitempty"`
	Priority string `json:"priority,omitempty"`
	Hold     bool   `json:"hold,omitempty"`
	Push     *bool  `json:"push,omitempty"`
}

// Teller sends a message to you somehow.
type Teller interface {
	On() bool
	Tell(ctx context.Context, m Message) error
}

// Hermodr sends through Hermóðr's POST /v1/messages.
type Hermodr struct {
	Part *call.Part
}

// NewHermodr is Hermóðr at url with this part's token there; off without either.
func NewHermodr(url, token string) *Hermodr {
	return &Hermodr{Part: call.New("hermodr", url, token, 30*time.Second)}
}

func (h *Hermodr) On() bool { return h != nil && h.Part.On() }

func (h *Hermodr) Tell(ctx context.Context, m Message) error {
	return h.Part.Do(ctx, http.MethodPost, "/v1/messages", m, nil)
}

// Pusher is a direct phone push (ntfy.Topic).
type Pusher interface {
	On() bool
	Push(ctx context.Context, title, message string, priority int, link string) error
}

// Direct sends straight to a phone push, past Hermóðr.
type Direct struct {
	Push Pusher
}

var ntfyPriority = map[string]int{"low": 2, "normal": 3, "high": 4, "urgent": 5}

func (d Direct) On() bool { return d.Push != nil && d.Push.On() }

func (d Direct) Tell(ctx context.Context, m Message) error {
	priority := ntfyPriority[m.Priority]
	if priority == 0 {
		priority = 3
	}
	text := m.Text
	if text == "" {
		text = m.Title
	}
	return d.Push.Push(ctx, m.Title, text, priority, m.Link)
}

// Chain tries each teller that is on, in order, until one takes it.
type Chain []Teller

func (c Chain) On() bool {
	for _, t := range c {
		if t != nil && t.On() {
			return true
		}
	}
	return false
}

func (c Chain) Tell(ctx context.Context, m Message) error {
	var errs []error
	for _, t := range c {
		if t == nil || !t.On() {
			continue
		}
		err := t.Tell(ctx, m)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
