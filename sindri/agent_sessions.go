package sindri

// Every coding-agent session on this machine, for the agents panel: Claude
// Code's (read on each request, cached), OpenCode's (a running `opencode
// serve`, read every three seconds) and herdr's panes (every two). Each
// source fails on its own: herdr not running leaves Claude Code's sessions
// listed, with a note saying why herdr's are not.

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/part"
)

// SourceView is one source's part of the view: its sessions, or why there are none.
type SourceView[T any] struct {
	// Sessions is null when the source is off or not answering.
	Sessions []T   `json:"sessions"`
	Error    string `json:"error,omitempty"`
	// From is where it reads from, or "off".
	From string `json:"from"`
}

// SessionsView is every source at one moment.
type SessionsView struct {
	Claude struct {
		SourceView[LiveClaudeSession]
		Degraded bool `json:"degraded"`
	} `json:"claude"`
	Opencode SourceView[OpencodeSession] `json:"opencode"`
	Herdr    SourceView[HerdrAgent]      `json:"herdr"`
	At       int64                       `json:"at"`
}

// AgentSessions keeps the sources read.
type AgentSessions struct {
	// Claude is off with an empty ClaudeDir; without SocketDir liveness is a guess.
	Claude ClaudeRoot
	// Opencode and Herdr are nil when off.
	Opencode *OpencodeClient
	Herdr    *Herdr

	cache    *LiveClaudeCache
	mu       sync.Mutex
	opencode SourceView[OpencodeSession]
	herdr    SourceView[HerdrAgent]
}

// NewAgentSessions starts with OpenCode and herdr "connecting".
func NewAgentSessions(claude ClaudeRoot, opencode *OpencodeClient, herdr *Herdr) *AgentSessions {
	s := &AgentSessions{Claude: claude, Opencode: opencode, Herdr: herdr, cache: NewLiveClaudeCache()}
	s.opencode = SourceView[OpencodeSession]{From: "off"}
	if opencode != nil {
		s.opencode = SourceView[OpencodeSession]{From: opencode.BaseURL, Error: "connecting"}
	}
	s.herdr = SourceView[HerdrAgent]{From: "off"}
	if herdr != nil {
		socket := herdr.Socket
		if socket == "" {
			socket = DefaultHerdrSocket()
		}
		s.herdr = SourceView[HerdrAgent]{From: socket, Error: "connecting"}
	}
	return s
}

// ReadOpencode reads OpenCode once; a server that has gone has no sessions.
func (s *AgentSessions) ReadOpencode(ctx context.Context) error {
	if s.Opencode == nil {
		return nil
	}
	sessions, err := s.Opencode.Sessions(ctx)
	view := SourceView[OpencodeSession]{Sessions: sessions, From: s.Opencode.BaseURL}
	if err != nil {
		view = SourceView[OpencodeSession]{Error: err.Error(), From: s.Opencode.BaseURL}
	}
	s.mu.Lock()
	s.opencode = view
	s.mu.Unlock()
	return nil
}

// ReadHerdr reads herdr's panes once.
func (s *AgentSessions) ReadHerdr(ctx context.Context) error {
	if s.Herdr == nil {
		return nil
	}
	agents, err := s.Herdr.List(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.herdr = SourceView[HerdrAgent]{Error: err.Error(), From: s.herdr.From}
		return nil
	}
	s.herdr = SourceView[HerdrAgent]{Sessions: agents, From: s.herdr.From}
	return nil
}

// View is everything, now.
func (s *AgentSessions) View(now time.Time) SessionsView {
	var view SessionsView
	view.At = now.UnixMilli()
	view.Claude.From = "off"
	if s.Claude.ClaudeDir != "" {
		view.Claude.From = s.Claude.ClaudeDir
		view.Claude.Sessions, view.Claude.Degraded = ReadLiveClaudeSessions(s.Claude, s.cache, now)
	}
	s.mu.Lock()
	view.Opencode, view.Herdr = s.opencode, s.herdr
	s.mu.Unlock()
	return view
}

func (s *AgentSessions) answerOpencode(ctx context.Context, requestID string, allow bool) error {
	if s.Opencode == nil {
		return httpErr(503, "OpenCode is off here (no OPENCODE_URL)")
	}
	s.mu.Lock()
	var found *OpencodePending
	for _, session := range s.opencode.Sessions {
		for _, p := range session.Pending {
			if p.RequestID == requestID {
				found = &p
			}
		}
	}
	s.mu.Unlock()
	if found == nil {
		return httpErr(404, "no open OpenCode request %s", requestID)
	}
	reply := "reject"
	if allow {
		reply = "once"
	}
	if err := s.Opencode.ReplyPermission(ctx, found.RequestID, found.Directory, reply); err != nil {
		return httpErr(502, "OpenCode did not take the answer: %s", err)
	}
	return s.ReadOpencode(ctx)
}

// --- routes ----------------------------------------------------------------------

func (s *Sindri) herdrOn(w http.ResponseWriter) bool {
	if s.Sessions == nil || s.Sessions.Herdr == nil {
		part.Error(w, http.StatusServiceUnavailable, "herdr is off here")
		return false
	}
	return true
}

func (s *Sindri) mountSessions(p *part.Part) {
	p.Handle("GET /v1/sessions", func(w http.ResponseWriter, _ *http.Request) {
		part.JSON(w, http.StatusOK, s.Sessions.View(time.Now()))
	})
	p.Handle("POST /v1/sessions/opencode/permissions/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Allow *bool `json:"allow"`
		}
		if err := part.Decode(r, &body); err != nil || body.Allow == nil {
			part.Error(w, http.StatusBadRequest, "`allow` must be true or false")
			return
		}
		if err := s.Sessions.answerOpencode(r.Context(), r.PathValue("id"), *body.Allow); err != nil {
			fail(w, err)
			return
		}
		part.JSON(w, http.StatusOK, map[string]bool{"answered": true})
	})
	p.Handle("POST /v1/sessions/herdr", func(w http.ResponseWriter, r *http.Request) {
		if !s.herdrOn(w) {
			return
		}
		var body struct {
			Kind, Cwd, Name, Resume string
		}
		if err := part.Decode(r, &body); err != nil {
			part.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		if body.Kind == "" {
			part.Error(w, http.StatusBadRequest, "`kind` must name an agent, e.g. \"claude\"")
			return
		}
		if !strings.HasPrefix(body.Cwd, "/") {
			part.Error(w, http.StatusBadRequest, "`cwd` must be an absolute path")
			return
		}
		var args []string
		if body.Resume != "" {
			var err error
			if args, err = ResumeArgs(body.Kind, body.Resume); err != nil {
				fail(w, err)
				return
			}
		}
		agent, err := s.Sessions.Herdr.Start(r.Context(), HerdrStart{Kind: body.Kind, Cwd: body.Cwd, Name: body.Name, Args: args})
		if err != nil {
			fail(w, herdrHTTPError(err))
			return
		}
		part.JSON(w, http.StatusOK, map[string]any{"agent": agent})
	})
	p.Handle("POST /v1/sessions/herdr/{target}/prompt", func(w http.ResponseWriter, r *http.Request) {
		if !s.herdrOn(w) {
			return
		}
		var body struct{ Text string }
		if err := part.Decode(r, &body); err != nil || strings.TrimSpace(body.Text) == "" {
			part.Error(w, http.StatusBadRequest, "`text` must be the prompt to send")
			return
		}
		agent, err := s.Sessions.Herdr.Prompt(r.Context(), r.PathValue("target"), body.Text)
		if err != nil {
			fail(w, herdrHTTPError(err))
			return
		}
		part.JSON(w, http.StatusOK, map[string]any{"agent": agent})
	})
	p.Handle("POST /v1/sessions/herdr/{target}/keys", func(w http.ResponseWriter, r *http.Request) {
		if !s.herdrOn(w) {
			return
		}
		var body struct{ Keys []string }
		if err := part.Decode(r, &body); err != nil || len(body.Keys) == 0 || slices.Contains(body.Keys, "") {
			part.Error(w, http.StatusBadRequest, "`keys` must be a list of keys, e.g. [\"esc\"]")
			return
		}
		if err := s.Sessions.Herdr.Keys(r.Context(), r.PathValue("target"), body.Keys); err != nil {
			fail(w, herdrHTTPError(err))
			return
		}
		part.JSON(w, http.StatusOK, map[string]any{"sent": body.Keys})
	})
	p.Handle("POST /v1/sessions/herdr/{target}/choose", func(w http.ResponseWriter, r *http.Request) {
		if !s.herdrOn(w) {
			return
		}
		var body DialogAnswer
		if err := part.Decode(r, &body); err != nil || body.Question == "" || body.Key == "" || body.Label == "" {
			part.Error(w, http.StatusBadRequest, "`question`, `key` and `label` must be the dialog's question and the choice, as read")
			return
		}
		if err := s.Sessions.Herdr.Choose(r.Context(), r.PathValue("target"), body); err != nil {
			fail(w, herdrHTTPError(err))
			return
		}
		part.JSON(w, http.StatusOK, map[string]any{"sent": []string{body.Key}})
	})
	p.Handle("GET /v1/sessions/herdr/{target}/read", func(w http.ResponseWriter, r *http.Request) {
		if !s.herdrOn(w) {
			return
		}
		lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
		if lines <= 0 {
			lines = 120
		}
		text, err := s.Sessions.Herdr.Read(r.Context(), r.PathValue("target"), min(lines, 2000))
		if err != nil {
			fail(w, herdrHTTPError(err))
			return
		}
		part.JSON(w, http.StatusOK, map[string]string{"text": text})
	})
}
