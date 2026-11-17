package huginn

// A meeting brief written by Claude, for a meeting whose series you set to
// Claude on the meeting prep card. It is asked in the same Norn conversation
// the card reads (ClaudeSession), so the card shows it without asking again.

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"yggdrasil/internal/call"
)

var (
	// ClaudeBriefWait is how long a Claude brief may take.
	ClaudeBriefWait = 10 * time.Minute
	// claudeBriefPoll is how often its conversation is read meanwhile.
	claudeBriefPoll = 10 * time.Second
)

// ClaudeSession is the Norn conversation a meeting's Claude brief is asked
// in: my-widget's claudeSession (utils/meetingPrep.ts), the same hash of the
// meeting's id (its subject when it has none) and start.
func ClaudeSession(meeting CalendarEvent) string {
	key := meeting.ID
	if key == "" {
		key = meeting.Subject
	}
	key += "@" + strconv.FormatInt(meeting.Start, 10)
	a, b := uint32(0x811C9DC5), uint32(0x01000193)
	for _, unit := range utf16.Encode([]rune(key)) {
		a = (a ^ uint32(unit)) * 0x01000193
		b = (b ^ uint32(unit)) * 0x5BD1E995
	}
	return "meeting-prep-claude-" + strconv.FormatUint(uint64(a), 36) + strconv.FormatUint(uint64(b), 36)
}

// lastAnswer is the last agent message after your last question, and
// whether a question was asked at all: my-widget's claudeAnswer.
func lastAnswer(turns []call.Turn) (string, bool) {
	asked := int64(-1)
	for _, t := range turns {
		if t.Role == "user" {
			asked = t.ID
		}
	}
	answer := ""
	for _, t := range turns {
		if text := strings.TrimSpace(t.Content); t.ID > asked && t.Role == "assistant" && t.Author != nil && text != "" {
			answer = text
		}
	}
	return answer, asked >= 0
}

// meetingPeople is the organizer and the attendees, each once.
func meetingPeople(meeting CalendarEvent) []string {
	var people []string
	for _, name := range append([]string{meeting.Organizer}, meeting.Attendees...) {
		if name != "" && !slices.Contains(people, name) {
			people = append(people, name)
		}
	}
	return people
}

// claudeBrief hands the brief to a Claude agent through Norn and waits for
// its answer. One already asked for this meeting (by the card) is waited for
// instead of asked again.
func (h *Huginn) claudeBrief(ctx context.Context, question string, meeting CalendarEvent) (string, error) {
	session := ClaudeSession(meeting)
	turns, err := h.Norn.Chat(ctx, session, 0)
	if err != nil {
		return "", err
	}
	answer, asked := lastAnswer(turns)
	if answer != "" {
		return answer, nil
	}
	if !asked {
		handed, err := h.Norn.AskClaude(ctx, question, session)
		if err != nil {
			return "", err
		}
		if handed.Delegated == nil {
			return "", fmt.Errorf("norn did not hand it to Claude: %s", handed.Answer)
		}
	}
	until := h.now().Add(ClaudeBriefWait)
	for h.now().Before(until) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(claudeBriefPoll):
		}
		turns, err := h.Norn.Chat(ctx, session, 0)
		if err != nil {
			continue
		}
		if answer, _ := lastAnswer(turns); answer != "" {
			return answer, nil
		}
	}
	return "", fmt.Errorf("claude did not answer within %s", ClaudeBriefWait)
}
