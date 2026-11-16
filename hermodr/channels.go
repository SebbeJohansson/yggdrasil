package hermodr

import (
	"context"
	"net/http"
	"time"

	"yggdrasil/internal/ntfy"
)

// Priorities, lowest first.
var Priorities = []string{"low", "normal", "high", "urgent"}

// Rank orders a priority: low 0 … urgent 3; unknown is normal.
func Rank(priority string) int {
	for i, p := range Priorities {
		if p == priority {
			return i
		}
	}
	return 1
}

// Out is one message as a channel sends it.
type Out struct {
	Title, Text, Link, Priority, Source string
}

// Channel is one place messages go out to.
type Channel interface {
	Name() string
	On() bool
	Send(ctx context.Context, out Out) error
}

// Ntfy pushes to your phone.
type Ntfy struct {
	Topic *ntfy.Topic
}

func (n Ntfy) Name() string { return "ntfy" }
func (n Ntfy) On() bool     { return n.Topic.On() }

// ntfy's 1 to 5.
var ntfyPriority = map[string]int{"low": 2, "normal": 3, "high": 4, "urgent": 5}

func (n Ntfy) Send(ctx context.Context, out Out) error {
	return n.Topic.Push(ctx, out.Title, orTitle(out), ntfyPriority[out.Priority], out.Link)
}

func orTitle(out Out) string {
	if out.Text == "" {
		return out.Title
	}
	return out.Text
}

// NewHTTP is the client channels use.
func NewHTTP() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}
