// Package ntfy pushes a message to your phone through an ntfy topic.
//
// Gjallar uses it for critical alerts. Ratatoskr uses it directly only when
// Gjallar itself is the problem.
package ntfy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Topic is one ntfy topic URL, e.g. https://ntfy.sh/<secret-topic>. A zero
// URL turns pushing off.
type Topic struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// New makes a topic with a 10 s timeout.
func New(url, token string) *Topic {
	return &Topic{URL: url, Token: token, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// On reports whether a topic is set.
func (t *Topic) On() bool {
	return t != nil && t.URL != ""
}

// Push sends one message. priority is ntfy's 1 (min) to 5 (max).
func (t *Topic) Push(ctx context.Context, title, message string, priority int, link string) error {
	if !t.On() {
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, strings.NewReader(message))
	if err != nil {
		return err
	}
	request.Header.Set("Title", title)
	request.Header.Set("Priority", fmt.Sprint(priority))
	if link != "" {
		request.Header.Set("Click", link)
	}
	if t.Token != "" {
		request.Header.Set("Authorization", "Bearer "+t.Token)
	}
	response, err := t.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		return fmt.Errorf("ntfy answered %d", response.StatusCode)
	}
	return nil
}
