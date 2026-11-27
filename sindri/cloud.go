package sindri

// Cloud sessions: Claude Code on the web, started and followed through the
// real `claude` CLI on this machine, signed in as you. Anthropic offers no
// API that lists cloud sessions, so Sindri knows only the ones it started,
// and keeps them in its database.
//
//	start      `claude --cloud "<task>"` in a herdr pane in the repo (it is
//	           interactive), read until it prints the session's URL; without
//	           herdr, the same command with no terminal
//	follow up  `claude -p "<text>" --cloud <id> --output-format json`
//	           → {ok, session_id, url} or {ok: false, error}
//	bring home `claude --teleport <id>` in a herdr pane in the repo

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
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

const cloudSchema = `
CREATE TABLE IF NOT EXISTS cloud_sessions (
	id TEXT PRIMARY KEY,
	at INTEGER NOT NULL,
	cwd TEXT NOT NULL,
	task TEXT NOT NULL,
	url TEXT NOT NULL,
	pane TEXT NOT NULL DEFAULT '',
	last_sent_at INTEGER,
	last_error TEXT NOT NULL DEFAULT '',
	teleported_at INTEGER
);
`

// CloudSession is a cloud session Sindri started.
type CloudSession struct {
	ID   string `json:"id"`
	At   int64  `json:"at"`
	Cwd  string `json:"cwd"`
	Task string `json:"task"`
	URL  string `json:"url"`
	// Pane is the herdr pane it was started from, while it may still be open.
	Pane         string `json:"pane,omitempty"`
	LastSentAt   *int64 `json:"lastSentAt,omitempty"`
	LastError    string `json:"lastError,omitempty"`
	TeleportedAt *int64 `json:"teleportedAt,omitempty"`
}

// CloudStarted is what starting a cloud session gave back.
type CloudStarted struct {
	ID, URL, Pane string
}

// CloudCLI is the `claude` CLI as cloud sessions need it; a fake in tests.
type CloudCLI interface {
	// Start creates a cloud session for the repo at cwd with the task.
	Start(ctx context.Context, cwd, task string) (CloudStarted, error)
	// Send queues a message in a cloud session.
	Send(ctx context.Context, cwd, id, text string) error
	// Teleport resumes the session in a new herdr pane in cwd.
	Teleport(ctx context.Context, cwd, id string) (HerdrAgent, error)
}

// Cloud is Sindri's cloud sessions.
type Cloud struct {
	DB  *sql.DB
	CLI CloudCLI
	now func() time.Time
	mu  sync.Mutex // one start at a time: each reads its own pane
}

// NewCloud makes the table and the cloud sessions on store's database.
func NewCloud(store *Store, cli CloudCLI) (*Cloud, error) {
	if _, err := store.db.Exec(cloudSchema); err != nil {
		return nil, fmt.Errorf("sindri cloud schema: %w", err)
	}
	return &Cloud{DB: store.db, CLI: cli, now: time.Now}, nil
}

func scanCloud(row interface{ Scan(...any) error }) (CloudSession, error) {
	var c CloudSession
	var sent, teleported sql.NullInt64
	err := row.Scan(&c.ID, &c.At, &c.Cwd, &c.Task, &c.URL, &c.Pane, &sent, &c.LastError, &teleported)
	if sent.Valid {
		c.LastSentAt = &sent.Int64
	}
	if teleported.Valid {
		c.TeleportedAt = &teleported.Int64
	}
	return c, err
}

const cloudColumns = `id, at, cwd, task, url, pane, last_sent_at, last_error, teleported_at`

// List is every cloud session started here, newest first.
func (c *Cloud) List() ([]CloudSession, error) {
	rows, err := c.DB.Query(`SELECT ` + cloudColumns + ` FROM cloud_sessions ORDER BY at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CloudSession{}
	for rows.Next() {
		session, err := scanCloud(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

// Get is one cloud session, or nil.
func (c *Cloud) Get(id string) (*CloudSession, error) {
	session, err := scanCloud(c.DB.QueryRow(`SELECT `+cloudColumns+` FROM cloud_sessions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &session, err
}

// Start creates a cloud session and keeps it.
func (c *Cloud) Start(ctx context.Context, cwd, task string) (*CloudSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	started, err := c.CLI.Start(ctx, cwd, task)
	if err != nil {
		return nil, err
	}
	_, err = c.DB.Exec(`INSERT INTO cloud_sessions (id, at, cwd, task, url, pane) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET url = excluded.url, pane = excluded.pane`,
		started.ID, c.now().UnixMilli(), cwd, task, started.URL, started.Pane)
	if err != nil {
		return nil, err
	}
	return c.Get(started.ID)
}

func (c *Cloud) known(id string) (*CloudSession, error) {
	session, err := c.Get(id)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, httpErr(404, "no cloud session %s was started here", id)
	}
	return session, nil
}

// Send queues a message in one, and keeps how it went.
func (c *Cloud) Send(ctx context.Context, id, text string) (*CloudSession, error) {
	session, err := c.known(id)
	if err != nil {
		return nil, err
	}
	sendErr := c.CLI.Send(ctx, session.Cwd, id, text)
	if sendErr != nil {
		_, err = c.DB.Exec(`UPDATE cloud_sessions SET last_error = ? WHERE id = ?`, sendErr.Error(), id)
	} else {
		_, err = c.DB.Exec(`UPDATE cloud_sessions SET last_sent_at = ?, last_error = '' WHERE id = ?`, c.now().UnixMilli(), id)
	}
	if err != nil {
		return nil, err
	}
	if sendErr != nil {
		return nil, sendErr
	}
	return c.Get(id)
}

// Teleport brings one home, into a herdr pane in its repo.
func (c *Cloud) Teleport(ctx context.Context, id string) (*CloudSession, HerdrAgent, error) {
	session, err := c.known(id)
	if err != nil {
		return nil, HerdrAgent{}, err
	}
	agent, err := c.CLI.Teleport(ctx, session.Cwd, id)
	if err != nil {
		return nil, HerdrAgent{}, err
	}
	if _, err := c.DB.Exec(`UPDATE cloud_sessions SET teleported_at = ? WHERE id = ?`, c.now().UnixMilli(), id); err != nil {
		return nil, HerdrAgent{}, err
	}
	session, err = c.Get(id)
	return session, agent, err
}

// Forget drops one from the list. The session itself goes on at claude.ai.
func (c *Cloud) Forget(id string) (bool, error) {
	result, err := c.DB.Exec(`DELETE FROM cloud_sessions WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// --- the real CLI ---------------------------------------------------------------

// cloudURL is a cloud session's link as the CLI prints it.
var cloudURL = regexp.MustCompile(`https://claude\.ai/code/(session_[A-Za-z0-9_-]+)`)

// FindCloudURL is the first cloud session link in text, and its id.
func FindCloudURL(text string) (url, id string) {
	m := cloudURL.FindStringSubmatch(text)
	if m == nil {
		return "", ""
	}
	return m[0], m[1]
}

// ClaudeCloud runs the real `claude`.
type ClaudeCloud struct {
	Bin string
	// Herdr, when on, holds the interactive commands: start and teleport.
	Herdr *Herdr
	// StartWait is how long a start may take to print the session's link.
	StartWait time.Duration
	// RemoteControl: a session brought home joins Remote Control (SINDRI_REMOTE_CONTROL).
	RemoteControl bool
}

func (c *ClaudeCloud) on() error {
	if c.Bin == "" || c.Bin == "off" {
		return httpErr(409, "Claude is off here (CLAUDE_BIN=off)")
	}
	return nil
}

func (c *ClaudeCloud) wait() time.Duration {
	if c.StartWait > 0 {
		return c.StartWait
	}
	return 90 * time.Second
}

// Start runs `claude --cloud "<task>"` and waits for the session's link.
func (c *ClaudeCloud) Start(ctx context.Context, cwd, task string) (CloudStarted, error) {
	if err := c.on(); err != nil {
		return CloudStarted{}, err
	}
	if c.Herdr != nil {
		started, err := c.startInHerdr(ctx, cwd, task)
		var down *HerdrDown
		if !errors.As(err, &down) {
			return started, err
		}
		// herdr is set up but not running: run it here, with no terminal.
	}
	return c.startHere(ctx, cwd, task)
}

// startHere runs `claude --cloud` here and reads its output. The CLI refuses
// --cloud without a terminal, so it gets one from util-linux's `script`
// (-e: its exit status, -f: output as it comes); it prints the session's link
// and exits by itself.
// Every task and text goes in as "--cloud=<task>" or after "--", never as
// its own argument, so text starting with "-" is never read as an option.
func (c *ClaudeCloud) startHere(ctx context.Context, cwd, task string) (CloudStarted, error) {
	ctx, cancel := context.WithTimeout(ctx, c.wait())
	defer cancel()
	var cmd *exec.Cmd
	if script, err := exec.LookPath("script"); err == nil {
		cmd = exec.CommandContext(ctx, script, "-qfec", shellQuote(c.Bin)+" "+shellQuote("--cloud="+task), "/dev/null")
	} else {
		cmd = exec.CommandContext(ctx, c.Bin, "--cloud="+task)
	}
	cmd.Dir = cwd
	// An open stdin: `script` ends when its input does.
	stdin, hold, err := os.Pipe()
	if err != nil {
		return CloudStarted{}, err
	}
	defer stdin.Close()
	defer hold.Close()
	cmd.Stdin = stdin
	out, err := cmd.StdoutPipe()
	if err != nil {
		return CloudStarted{}, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return CloudStarted{}, httpErr(502, "claude did not start: %s", err)
	}
	var seen strings.Builder
	scanner := bufio.NewScanner(io.LimitReader(out, 1<<20))
	for scanner.Scan() {
		line := scanner.Text()
		seen.WriteString(line + "\n")
		if url, id := FindCloudURL(line); id != "" {
			// The session exists once its link is printed; what the CLI does after is not ours to wait for.
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return CloudStarted{ID: id, URL: url}, nil
		}
	}
	_ = cmd.Wait()
	return CloudStarted{}, httpErr(502, "claude --cloud printed no session link: %s", lastLines(seen.String(), 3))
}

func (c *ClaudeCloud) startInHerdr(ctx context.Context, cwd, task string) (CloudStarted, error) {
	agent, err := c.Herdr.Start(ctx, HerdrStart{Kind: "claude", Cwd: cwd, Name: PrefixedHerdrName("cloud-", cwd), Args: []string{"--cloud=" + task}})
	if err != nil {
		var down *HerdrDown
		if errors.As(err, &down) {
			return CloudStarted{}, err // the caller runs it here instead
		}
		return CloudStarted{}, herdrHTTPError(err)
	}
	deadline := time.Now().Add(c.wait())
	var text string
	for time.Now().Before(deadline) {
		text, err = c.Herdr.Read(ctx, agent.PaneID, 400)
		if err == nil {
			if url, id := FindCloudURL(text); id != "" {
				return CloudStarted{ID: id, URL: url, Pane: agent.PaneID}, nil
			}
			if trustFolder.MatchString(text) {
				return CloudStarted{}, httpErr(409, "Claude asks whether to trust %s (pane %s): answer it there, or run claude in that folder once and pick Yes, then start again", cwd, agent.PaneID)
			}
		}
		select {
		case <-ctx.Done():
			return CloudStarted{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return CloudStarted{}, httpErr(504, "claude --cloud showed no session link in %s (pane %s): %s", c.wait(), agent.PaneID, lastLines(text, 3))
}

// cloudSent is what `claude -p … --cloud <id> --output-format json` prints.
type cloudSent struct {
	OK        bool   `json:"ok"`
	SessionID string `json:"session_id"`
	URL       string `json:"url"`
	Error     string `json:"error"`
}

// Send runs `claude -p "<text>" --cloud <id> --output-format json`.
func (c *ClaudeCloud) Send(ctx context.Context, cwd, id, text string) error {
	if err := c.on(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Bin, "-p", "--cloud="+id, "--output-format", "json", "--", text)
	cmd.Dir = cwd
	out, err := cmd.Output()
	var sent cloudSent
	if json.Unmarshal(out, &sent) == nil && (sent.OK || sent.Error != "") {
		if !sent.OK {
			return httpErr(502, "the cloud session did not take it: %s", sent.Error)
		}
		return nil
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return httpErr(502, "claude failed: %s", lastLines(string(exitErr.Stderr)+string(out), 3))
		}
		return httpErr(502, "claude did not run: %s", err)
	}
	return httpErr(502, "claude answered something other than JSON: %s", lastLines(string(out), 3))
}

// Teleport runs `claude --teleport <id>` in a new herdr pane in cwd.
func (c *ClaudeCloud) Teleport(ctx context.Context, cwd, id string) (HerdrAgent, error) {
	if err := c.on(); err != nil {
		return HerdrAgent{}, err
	}
	if c.Herdr == nil {
		return HerdrAgent{}, httpErr(503, "herdr is off here, and --teleport needs a terminal: run `claude --teleport %s` in %s", id, cwd)
	}
	args := []string{"--teleport", id}
	if c.RemoteControl {
		args = WithRemoteControl(args)
	}
	agent, err := c.Herdr.Start(ctx, HerdrStart{Kind: "claude", Cwd: cwd, Args: args})
	var down *HerdrDown
	if errors.As(err, &down) {
		return HerdrAgent{}, &HTTPError{Status: 503, Code: "not_running",
			Message: fmt.Sprintf("herdr is not running, and --teleport needs a terminal: run `claude --teleport %s` in %s", id, cwd)}
	}
	if err != nil {
		return HerdrAgent{}, herdrHTTPError(err)
	}
	return agent, nil
}

func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, " / "))
}

// --- routes ----------------------------------------------------------------------

// mayDrive: you, or Sága doing what you asked on another device.
func mayDrive(r *http.Request) bool {
	caller := part.Caller(r)
	return caller == part.Owner || caller == "saga"
}

func (s *Sindri) mountCloud(p *part.Part) {
	p.Handle("GET /v1/cloud", func(w http.ResponseWriter, _ *http.Request) {
		if s.Cloud == nil {
			part.JSON(w, http.StatusOK, map[string]any{"sessions": []CloudSession{}, "off": "cloud sessions are off here"})
			return
		}
		sessions, err := s.Cloud.List()
		if err != nil {
			fail(w, err)
			return
		}
		part.JSON(w, http.StatusOK, map[string]any{"sessions": sessions})
	})
	guard := func(w http.ResponseWriter, r *http.Request) bool {
		if !mayDrive(r) {
			part.Error(w, http.StatusForbidden, "only the owner may do this")
			return false
		}
		if s.Cloud == nil {
			part.Error(w, http.StatusServiceUnavailable, "cloud sessions are off here")
			return false
		}
		return true
	}
	p.Handle("POST /v1/cloud", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		var body struct{ Cwd, Task string }
		if err := part.Decode(r, &body); err != nil {
			part.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		if !strings.HasPrefix(body.Cwd, "/") {
			part.Error(w, http.StatusBadRequest, "`cwd` must be the repo's absolute path")
			return
		}
		if strings.TrimSpace(body.Task) == "" {
			part.Error(w, http.StatusBadRequest, "`task` must say what the cloud session should do")
			return
		}
		session, err := s.Cloud.Start(r.Context(), body.Cwd, strings.TrimSpace(body.Task))
		if err != nil {
			fail(w, err)
			return
		}
		part.JSON(w, http.StatusCreated, map[string]any{"session": session})
	})
	p.Handle("POST /v1/cloud/{id}/message", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		var body struct{ Text string }
		if err := part.Decode(r, &body); err != nil || strings.TrimSpace(body.Text) == "" {
			part.Error(w, http.StatusBadRequest, "`text` must be the message to send")
			return
		}
		session, err := s.Cloud.Send(r.Context(), r.PathValue("id"), body.Text)
		if err != nil {
			fail(w, err)
			return
		}
		part.JSON(w, http.StatusOK, map[string]any{"session": session})
	})
	p.Handle("POST /v1/cloud/{id}/teleport", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		session, agent, err := s.Cloud.Teleport(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		part.JSON(w, http.StatusOK, map[string]any{"session": session, "agent": agent})
	})
	p.Handle("DELETE /v1/cloud/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		gone, err := s.Cloud.Forget(r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		if !gone {
			part.Error(w, http.StatusNotFound, "no such cloud session here")
			return
		}
		part.JSON(w, http.StatusOK, map[string]bool{"forgotten": true})
	})
}
