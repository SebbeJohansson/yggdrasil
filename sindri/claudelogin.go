package sindri

// Claude Code's sign-in, run from the dashboard. When an agent's turn or a
// cloud-mode answer fails because the sign-in ran out, Sindri remembers it.
// POST /v1/claude-login starts `claude auth login`, which prints a page to
// open; that page shows a code, and POST /v1/claude-login/code hands it to
// the waiting login. No terminal needed: Sindri runs on the host, with the
// same ~/.claude the agents use.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/part"
)

// LoginTimeout is how long a started sign-in waits for its code.
const LoginTimeout = 10 * time.Minute

// loginURLWait is how long Start waits for the CLI to print its page.
const loginURLWait = 20 * time.Second

// loginCodeWait is how long Finish waits for the CLI to take the code.
const loginCodeWait = time.Minute

var loginURL = regexp.MustCompile(`https://\S*/oauth/authorize\?\S+`)

// ClaudeLoginState is what the dashboard shows about Claude Code's sign-in.
type ClaudeLoginState struct {
	// State is "ok", "expired" (a turn failed for it), "linking" (waiting
	// for the code) or "error" (the last sign-in did not finish).
	State string `json:"state"`
	// URL is the page to sign in on, while linking.
	URL string `json:"url,omitempty"`
	// Error is why the last sign-in failed, or what Claude said when it ran out.
	Error      string `json:"error,omitempty"`
	ExpiredAt  int64  `json:"expiredAt,omitempty"`
	SignedInAt int64  `json:"signedInAt,omitempty"`
}

// ClaudeLogin runs one `claude auth login` at a time.
type ClaudeLogin struct {
	bin string
	now func() time.Time

	mu         sync.Mutex
	expiredAt  int64
	expiredWhy string
	signedInAt int64
	url        string
	failure    string
	stdin      io.WriteCloser
	output     strings.Builder
	running    bool
	done       chan struct{}
}

// NewClaudeLogin is the sign-in for the Claude Code at bin.
func NewClaudeLogin(bin string) *ClaudeLogin {
	return &ClaudeLogin{bin: bin, now: time.Now}
}

func (l *ClaudeLogin) ms() int64 { return l.now().UnixMilli() }

// MarkExpired records that Claude failed because its sign-in ran out.
func (l *ClaudeLogin) MarkExpired(why string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.expiredAt = l.ms()
	l.expiredWhy = Clip(strings.TrimSpace(why), TextLimit)
}

// State is the sign-in as the dashboard shows it.
func (l *ClaudeLogin) State() ClaudeLoginState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stateLocked()
}

func (l *ClaudeLogin) stateLocked() ClaudeLoginState {
	s := ClaudeLoginState{State: "ok", ExpiredAt: l.expiredAt, SignedInAt: l.signedInAt}
	switch {
	case l.running:
		s.State, s.URL = "linking", l.url
	case l.failure != "":
		s.State, s.Error = "error", l.failure
	case l.expiredAt > l.signedInAt:
		s.State, s.Error = "expired", l.expiredWhy
	}
	return s
}

// Start begins a sign-in, or returns the one already waiting for its code.
func (l *ClaudeLogin) Start() (ClaudeLoginState, error) {
	l.mu.Lock()
	if l.running {
		defer l.mu.Unlock()
		return l.stateLocked(), nil
	}
	if l.bin == "" || l.bin == "off" {
		l.mu.Unlock()
		return ClaudeLoginState{}, httpErr(409, "Claude Code is off in Sindri (CLAUDE_BIN=off)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), LoginTimeout)
	command := exec.CommandContext(ctx, l.bin, "auth", "login", "--claudeai")
	// The dashboard opens the page; Sindri has no screen to open it on.
	command.Env = append(os.Environ(), "BROWSER=true")
	stdin, err := command.StdinPipe()
	if err != nil {
		l.mu.Unlock()
		cancel()
		return ClaudeLoginState{}, err
	}
	reader, writer := io.Pipe()
	command.Stdout, command.Stderr = writer, writer
	if err := command.Start(); err != nil {
		l.mu.Unlock()
		cancel()
		return ClaudeLoginState{}, fmt.Errorf("could not start claude auth login: %w", err)
	}
	done := make(chan struct{})
	l.running, l.stdin, l.url, l.failure, l.done = true, stdin, "", "", done
	l.output.Reset()
	l.mu.Unlock()

	read := make(chan struct{})
	go func() {
		defer close(read)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			l.mu.Lock()
			if l.output.Len() < 16<<10 {
				l.output.WriteString(line + "\n")
			}
			if l.url == "" {
				l.url = loginURL.FindString(line)
			}
			l.mu.Unlock()
		}
	}()
	go func() {
		err := command.Wait()
		_ = writer.Close()
		<-read
		cancel()
		l.mu.Lock()
		l.running, l.stdin = false, nil
		switch {
		case err == nil:
			l.signedInAt, l.failure = l.ms(), ""
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			l.failure = "the sign-in was not finished within 10 minutes; start it again"
		default:
			l.failure = loginFailure(l.output.String(), err)
		}
		l.mu.Unlock()
		close(done)
	}()

	deadline := time.Now().Add(loginURLWait)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		state := l.stateLocked()
		l.mu.Unlock()
		if state.State != "linking" || state.URL != "" {
			return state, nil
		}
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
		}
	}
	return l.State(), nil
}

// Finish hands the code from the sign-in page to the waiting login.
func (l *ClaudeLogin) Finish(code string) (ClaudeLoginState, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return ClaudeLoginState{}, httpErr(400, "`code` is required")
	}
	l.mu.Lock()
	if !l.running || l.stdin == nil {
		l.mu.Unlock()
		return ClaudeLoginState{}, httpErr(409, "no sign-in is waiting for a code; start one first")
	}
	stdin, done := l.stdin, l.done
	l.mu.Unlock()
	if _, err := io.WriteString(stdin, code+"\n"); err != nil {
		return ClaudeLoginState{}, fmt.Errorf("could not hand the code to claude: %w", err)
	}
	select {
	case <-done:
	case <-time.After(loginCodeWait):
	}
	return l.State(), nil
}

// loginFailure is the CLI's own words for a failed sign-in, without the page link.
func loginFailure(output string, err error) string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(loginURL.ReplaceAllString(line, ""))
		if _, after, found := strings.Cut(line, "Paste code here if prompted >"); found {
			line = strings.TrimSpace(after)
		}
		if line == "" || strings.HasPrefix(line, "Opening browser") || strings.HasPrefix(line, "If the browser didn't open") {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "claude auth login failed: " + err.Error()
	}
	return Clip(strings.Join(lines, " "), TextLimit)
}

func (s *Sindri) claudeLoginState(w http.ResponseWriter, _ *http.Request) {
	part.JSON(w, http.StatusOK, s.Agents.Login().State())
}

func (s *Sindri) claudeLoginStart(w http.ResponseWriter, _ *http.Request) {
	state, err := s.Agents.Login().Start()
	if err != nil {
		fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, state)
}

func (s *Sindri) claudeLoginCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if err := part.Decode(r, &body); err != nil {
		part.Error(w, http.StatusBadRequest, "`code` is required")
		return
	}
	state, err := s.Agents.Login().Finish(body.Code)
	if err != nil {
		fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, state)
}
