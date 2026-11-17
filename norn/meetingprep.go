package norn

// How you want a meeting's brief written, kept per recurring series: what you
// add to the prompt, and whether the local model or Claude writes it. The
// meeting prep card reads and sets it; calendar.soon, .added and .moved carry
// the prompt, so Huginn's brief asks with it too.

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"yggdrasil/internal/part"
)

const meetingPrepSchema = `CREATE TABLE IF NOT EXISTS meeting_prep (
	key TEXT PRIMARY KEY,
	at INTEGER NOT NULL,
	prompt TEXT NOT NULL,
	model TEXT NOT NULL
);`

// maxPrepPrompt is how long your addition to a brief's prompt may be.
const maxPrepPrompt = 500

// MeetingPrep is how a meeting's brief is written. Model is "local",
// "claude", or "" when you never picked one for it.
type MeetingPrep struct {
	Key    string `json:"key"`
	Prompt string `json:"prompt"`
	Model  string `json:"model"`
}

// PrepKey is what a meeting's prep is kept by: its series when it recurs,
// so every occurrence shares it, else the meeting itself.
func PrepKey(event CalendarEvent) string {
	switch {
	case event.Series != "":
		return "series:" + event.Series
	case event.ID != "":
		return "event:" + event.ID
	}
	return "subject:" + strings.ToLower(strings.TrimSpace(event.Subject))
}

// MeetingPrep is the prep kept under key; empty when there is none.
func (s *Store) MeetingPrep(ctx context.Context, key string) (MeetingPrep, error) {
	prep := MeetingPrep{Key: key}
	err := s.db.QueryRowContext(ctx, `SELECT prompt, model FROM meeting_prep WHERE key = ?`, key).Scan(&prep.Prompt, &prep.Model)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return prep, err
}

// PutMeetingPrep keeps prep; one with neither a prompt nor a model is removed.
func (s *Store) PutMeetingPrep(ctx context.Context, prep MeetingPrep) error {
	if prep.Prompt == "" && prep.Model == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM meeting_prep WHERE key = ?`, prep.Key)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO meeting_prep (key, at, prompt, model) VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET at = excluded.at, prompt = excluded.prompt, model = excluded.model`,
		prep.Key, time.Now().UnixMilli(), prep.Prompt, prep.Model)
	return err
}

// ClaudeBriefRules is how Claude writes a meeting brief: from what it finds
// with and about the people in the meeting, not from your other work.
const ClaudeBriefRules = "How to write this brief: look for what is going on between me and the people invited, " +
	"with the Microsoft 365 tools. Search our Teams chats with each of them, one-to-one and group chats " +
	"(chat_message_search and teams_list_chats), and my email with them (outlook_email_search), from the last 30 days, " +
	"and read the meeting's invitation for an agenda (outlook_calendar_search). Use only what bears on this meeting " +
	"and these people: leave out my agent sessions, other projects and anything you cannot tie to them. " +
	"If nothing turns up, say so in one line instead of filling it out. " +
	"Write Markdown: at most five short bullets, each one concrete thing (an open question, a decision, " +
	"something I promised or was asked for), naming who it is with. No heading, no preamble. " +
	"Write in the language the sources are in."

// prepPrompt is your addition to the brief's prompt for event, for the
// calendar events Huginn briefs on.
func (n *Norn) prepPrompt(event CalendarEvent) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	prep, err := n.Store.MeetingPrep(ctx, PrepKey(event))
	if err != nil {
		n.logf("reading a meeting's prep: %v", err)
	}
	return prep.Prompt
}

func (n *Norn) mountMeetingPrep(p *part.Part) {
	p.Handle("GET /v1/meetings/prep", n.meetingPrep)
	p.Handle("PUT /v1/meetings/prep", n.putMeetingPrep)
}

func (n *Norn) meetingPrep(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		part.Error(w, http.StatusBadRequest, "`key` is the meeting's: its series, or the meeting")
		return
	}
	prep, err := n.Store.MeetingPrep(r.Context(), key)
	if err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, prep)
}

func (n *Norn) putMeetingPrep(w http.ResponseWriter, r *http.Request) {
	var prep MeetingPrep
	if err := part.Decode(r, &prep); err != nil || strings.TrimSpace(prep.Key) == "" {
		part.Error(w, http.StatusBadRequest, "`key` is the meeting's: its series, or the meeting")
		return
	}
	prep.Key = strings.TrimSpace(prep.Key)
	prep.Prompt = strings.Join(strings.Fields(prep.Prompt), " ")
	if len([]rune(prep.Prompt)) > maxPrepPrompt {
		part.Error(w, http.StatusBadRequest, "`prompt` is at most 500 characters")
		return
	}
	if prep.Model != "" && prep.Model != "local" && prep.Model != "claude" {
		part.Error(w, http.StatusBadRequest, "`model` is local or claude")
		return
	}
	if err := n.Store.PutMeetingPrep(r.Context(), prep); err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, prep)
}
