// Package sse follows another part's server-sent event stream with a token,
// reconnecting after a drop and sending Last-Event-ID so the part can send
// what was missed (Norn does; Gjallar starts fresh).
package sse

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Event is one event from the stream.
type Event struct {
	ID   string
	Kind string
	Data string
}

// Stream is one stream being followed.
type Stream struct {
	Part  string
	URL   string
	Token string
	HTTP  *http.Client

	mu        sync.Mutex
	connected bool
	since     time.Time
	lastID    string
	lastAt    time.Time
	lastError string
}

// Status is how the stream is doing.
type Status struct {
	Part        string     `json:"part"`
	Connected   bool       `json:"connected"`
	Since       *time.Time `json:"since,omitempty"`
	LastEventID string     `json:"lastEventId,omitempty"`
	LastEventAt *time.Time `json:"lastEventAt,omitempty"`
	Error       string     `json:"error,omitempty"`
}

// New makes a stream; it starts with Follow. The client has no timeout: a
// stream stays open.
func New(part, url, token string, started time.Time) *Stream {
	return &Stream{Part: part, URL: url, Token: token, HTTP: &http.Client{}, since: started}
}

// Resume makes the first connection ask for what came after id: the last
// event handled before this process started, so a restart misses nothing the
// part still keeps. Call it before Follow.
func (s *Stream) Resume(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastID = id
}

// Status is the stream's state now. Since is when it last connected or, while
// down, when it went down.
func (s *Stream) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := Status{Part: s.Part, Connected: s.connected, LastEventID: s.lastID, Error: s.lastError}
	since := s.since
	status.Since = &since
	if !s.lastAt.IsZero() {
		at := s.lastAt
		status.LastEventAt = &at
	}
	return status
}

// DownFor is how long the stream has been down; zero while connected.
func (s *Stream) DownFor(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connected {
		return 0
	}
	return now.Sub(s.since)
}

func (s *Stream) set(connected bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if connected != s.connected {
		s.since = time.Now()
	}
	s.connected = connected
	if err != nil {
		s.lastError = err.Error()
	} else if connected {
		s.lastError = ""
	}
}

// Follow reads the stream until ctx ends, calling handle for each event and
// reconnecting after a drop, waiting 2 s and doubling up to a minute.
func (s *Stream) Follow(ctx context.Context, handle func(Event)) {
	wait := 2 * time.Second
	for ctx.Err() == nil {
		err := s.once(ctx, handle, func() { wait = 2 * time.Second })
		s.set(false, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Minute)
	}
}

func (s *Stream) once(ctx context.Context, handle func(Event), connected func()) error {
	if s.URL == "" || s.Token == "" {
		return fmt.Errorf("%s is not set up", s.Part)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+s.Token)
	request.Header.Set("Accept", "text/event-stream")
	s.mu.Lock()
	if s.lastID != "" {
		request.Header.Set("Last-Event-ID", s.lastID)
	}
	s.mu.Unlock()
	response, err := s.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", s.Part, response.StatusCode)
	}
	s.set(true, nil)
	connected()
	return Read(bufio.NewScanner(response.Body), func(event Event) {
		s.mu.Lock()
		if event.ID != "" {
			s.lastID = event.ID
		}
		s.lastAt = time.Now()
		s.mu.Unlock()
		handle(event)
	})
}

// Read parses events from the scanner until it ends. Comments (": …") are
// skipped, and data lines are joined with newlines.
func Read(scanner *bufio.Scanner, handle func(Event)) error {
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	var event Event
	var data []string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if len(data) > 0 {
				event.Data = strings.Join(data, "\n")
				handle(event)
			}
			event, data = Event{}, nil
		case strings.HasPrefix(line, ":"):
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				event.Kind = value
			case "id":
				event.ID = value
			case "data":
				data = append(data, value)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("the stream ended")
}
