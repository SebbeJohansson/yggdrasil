package sindri

// What each OpenCode session is doing, ported from agent-conduit
// (opencode/status.ts, opencode/client.ts). OpenCode says it outright:
//
//	session.status busy | retry    working
//	session.status idle            finished; your move
//	open permission or question    waiting on you
//
// A subagent runs as a child session with a parentID, and everything it does
// is credited to the root session you started.
//
// agent-conduit follows the server's event stream; Sindri reads a fresh
// snapshot every few seconds instead (the "opencode" loop). A snapshot is
// the whole truth each time, so a dropped connection never leaves stale
// state behind.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// OpencodePending is a permission or question waiting on you.
type OpencodePending struct {
	RequestID string `json:"requestId"`
	Kind      string `json:"kind"` // permission or question
	// SessionID is the session that asked: the root or one of its subagents.
	SessionID string `json:"sessionId"`
	// Summary is what it wants in one line: "bash git push", or the question.
	Summary string `json:"summary"`
	// Directory is the instance it was asked in, which a reply names.
	Directory string `json:"directory"`
}

// OpencodeSession is one root session, its subagents folded in.
type OpencodeSession struct {
	SessionID string `json:"sessionId"`
	Directory string `json:"directory"`
	Title     string `json:"title"`
	// Activity is approval, working or idle.
	Activity    string            `json:"activity"`
	Pending     []OpencodePending `json:"pending"`
	TimeCreated int64             `json:"timeCreated"`
	TimeUpdated int64             `json:"timeUpdated"`
}

type opencodeRecord struct {
	id, parentID, directory, title string
	created, updated               int64
}

// OpencodeSnapshot is everything a server knows, raw, from every directory.
type OpencodeSnapshot struct {
	Sessions    []any
	Statuses    map[string]any
	Permissions []any
	Questions   []any
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asText(v any) string {
	s, _ := v.(string)
	return s
}

func asNumber(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

func opencodeSession(v any) *opencodeRecord {
	info := asMap(v)
	id := asText(info["id"])
	if id == "" {
		return nil
	}
	t := asMap(info["time"])
	return &opencodeRecord{id: id, parentID: asText(info["parentID"]), directory: asText(info["directory"]), title: asText(info["title"]),
		created: asNumber(t["created"]), updated: asNumber(t["updated"])}
}

// opencodeRunning: busy and retry both mean still going.
func opencodeRunning(status any) bool {
	kind := asText(status)
	if kind == "" {
		kind = asText(asMap(status)["type"])
	}
	kind = strings.ToLower(kind)
	return kind == "busy" || kind == "retry"
}

func opencodeRequest(v any, kind string) *OpencodePending {
	r := asMap(v)
	id, session := asText(r["id"]), asText(r["sessionID"])
	if id == "" || session == "" {
		return nil
	}
	p := &OpencodePending{RequestID: id, Kind: kind, SessionID: session}
	if kind == "permission" {
		parts := []string{asText(r["permission"])}
		if parts[0] == "" {
			parts[0] = "permission"
		}
		if patterns, ok := r["patterns"].([]any); ok {
			for _, pattern := range patterns {
				if s := asText(pattern); s != "" {
					parts = append(parts, s)
				}
			}
		}
		p.Summary = strings.Join(parts, " ")
		return p
	}
	var first map[string]any
	if questions, ok := r["questions"].([]any); ok && len(questions) > 0 {
		first = asMap(questions[0])
	}
	p.Summary = asText(first["question"])
	if p.Summary == "" {
		p.Summary = asText(first["header"])
	}
	if p.Summary == "" {
		p.Summary = "question"
	}
	return p
}

// ReadOpencodeSessions folds a snapshot into root sessions.
func ReadOpencodeSessions(snapshot OpencodeSnapshot) []OpencodeSession {
	records := map[string]*opencodeRecord{}
	for _, raw := range snapshot.Sessions {
		if r := opencodeSession(raw); r != nil {
			records[r.id] = r
		}
	}
	rootOf := func(id string) string {
		seen := map[string]bool{}
		for !seen[id] {
			seen[id] = true
			r := records[id]
			if r == nil || r.parentID == "" || records[r.parentID] == nil {
				return id
			}
			id = r.parentID
		}
		return id
	}
	running := map[string]bool{}
	for id, status := range snapshot.Statuses {
		if opencodeRunning(status) {
			running[rootOf(id)] = true
		}
	}
	pending := map[string][]OpencodePending{}
	seen := map[string]bool{}
	add := func(list []any, kind string) {
		for _, raw := range list {
			p := opencodeRequest(raw, kind)
			if p == nil || seen[p.RequestID] {
				continue
			}
			seen[p.RequestID] = true
			// A snapshot's requests do not say which instance they came from; the session that asked does.
			if r := records[p.SessionID]; r != nil {
				p.Directory = r.directory
			}
			root := rootOf(p.SessionID)
			pending[root] = append(pending[root], *p)
		}
	}
	add(snapshot.Permissions, "permission")
	add(snapshot.Questions, "question")

	sessions := []OpencodeSession{}
	for id, r := range records {
		if rootOf(id) != id {
			continue
		}
		activity := "idle"
		switch {
		case len(pending[id]) > 0:
			activity = "approval"
		case running[id]:
			activity = "working"
		}
		list := pending[id]
		if list == nil {
			list = []OpencodePending{}
		}
		sessions = append(sessions, OpencodeSession{SessionID: id, Directory: r.directory, Title: r.title, Activity: activity, Pending: list, TimeCreated: r.created, TimeUpdated: r.updated})
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].TimeUpdated > sessions[j].TimeUpdated })
	return sessions
}

// OpencodeClient talks to a running `opencode serve`.
type OpencodeClient struct {
	BaseURL string
	// Authorization is sent as is, for a server started with a password.
	Authorization string
	HTTP          *http.Client
}

func (c *OpencodeClient) url(path, directory string) string {
	u := strings.TrimRight(c.BaseURL, "/") + path
	if directory != "" {
		u += "?" + url.Values{"directory": {directory}}.Encode()
	}
	return u
}

func (c *OpencodeClient) do(ctx context.Context, method, path, directory string, body any, into any) error {
	var reader io.Reader
	if body != nil {
		payload, _ := json.Marshal(body)
		reader = bytes.NewReader(payload)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, c.url(path, directory), reader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.Authorization != "" {
		request.Header.Set("Authorization", c.Authorization)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		return fmt.Errorf("%s failed (%d)", path, response.StatusCode)
	}
	if into == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, 32<<20)).Decode(into)
}

// directories are the server's instances: the default one plus every
// project's worktree and sandboxes. Each keeps its own sessions.
func (c *OpencodeClient) directories(ctx context.Context) ([]string, error) {
	var projects []map[string]any
	if err := c.do(ctx, http.MethodGet, "/project", "", nil, &projects); err != nil {
		return nil, err
	}
	out, seen := []string{""}, map[string]bool{}
	addDir := func(d string) {
		if d != "" && d != "/" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, p := range projects {
		addDir(asText(p["worktree"]))
		if sandboxes, ok := p["sandboxes"].([]any); ok {
			for _, s := range sandboxes {
				addDir(asText(s))
			}
		}
	}
	return out, nil
}

// Snapshot reads everything the server knows right now, from every directory.
func (c *OpencodeClient) Snapshot(ctx context.Context) (OpencodeSnapshot, error) {
	snapshot := OpencodeSnapshot{Statuses: map[string]any{}}
	directories, err := c.directories(ctx)
	if err != nil {
		return snapshot, err
	}
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		first error
	)
	seenSession := map[string]bool{}
	for _, directory := range directories {
		wg.Add(1)
		go func(directory string) {
			defer wg.Done()
			var sessions, permissions, questions []any
			var statuses map[string]any
			errSessions := c.do(ctx, http.MethodGet, "/session", directory, nil, &sessions)
			errStatuses := c.do(ctx, http.MethodGet, "/session/status", directory, nil, &statuses)
			// Older servers have no question endpoint; neither is worth failing over.
			_ = c.do(ctx, http.MethodGet, "/permission", directory, nil, &permissions)
			_ = c.do(ctx, http.MethodGet, "/question", directory, nil, &questions)
			mu.Lock()
			defer mu.Unlock()
			for _, err := range []error{errSessions, errStatuses} {
				if err != nil && first == nil {
					first = err
				}
			}
			for _, s := range sessions {
				if id := asText(asMap(s)["id"]); id != "" && !seenSession[id] {
					seenSession[id] = true
					snapshot.Sessions = append(snapshot.Sessions, s)
				}
			}
			for id, status := range statuses {
				snapshot.Statuses[id] = status
			}
			snapshot.Permissions = append(snapshot.Permissions, permissions...)
			snapshot.Questions = append(snapshot.Questions, questions...)
		}(directory)
	}
	wg.Wait()
	return snapshot, first
}

// Sessions is one read of every root session and what it is doing.
func (c *OpencodeClient) Sessions(ctx context.Context) ([]OpencodeSession, error) {
	snapshot, err := c.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return ReadOpencodeSessions(snapshot), nil
}

// ReplyPermission answers a permission: once allows this call, always allows
// calls like it for the session, reject refuses it.
func (c *OpencodeClient) ReplyPermission(ctx context.Context, requestID, directory, reply string) error {
	return c.do(ctx, http.MethodPost, "/permission/"+url.PathEscape(requestID)+"/reply", directory, map[string]string{"reply": reply}, nil)
}
