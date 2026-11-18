package norn

// Calendar suggestions: a meeting you put in your own calendar (you organize
// it, nobody else is invited), usually copied from an email, that an invite
// from someone else now covers. Either the invite is the same thing
// (duplicate: delete your copy) or the invite was cancelled (stale: your copy
// is left over). Norn only suggests; deleting your copy is one call, and
// never touches a meeting someone else is in.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

	"yggdrasil/internal/part"
)

const (
	// keptSuggestionsKey is the suggestions you said to keep your copy for.
	keptSuggestionsKey = "calendar-suggestions-kept"
	// toldSuggestionsKey is the suggestions Huginn was told about, as id:kind.
	toldSuggestionsKey = "calendar-suggestions-told"
)

// CalendarSuggestion is one of your copies and the invite that covers it.
type CalendarSuggestion struct {
	// ID is short and stable for the copy, safe in a URL (Graph ids are not).
	ID string `json:"id"`
	// Kind is duplicate (the invite is the same thing) or stale (the invite
	// it matches was cancelled).
	Kind   string        `json:"kind"`
	Copy   CalendarEvent `json:"copy"`
	Invite CalendarEvent `json:"invite"`
}

// suggestionID is the copy's id, hashed short.
func suggestionID(copyID string) string {
	sum := sha256.Sum256([]byte(copyID))
	return hex.EncodeToString(sum[:6])
}

// yourCopy is a meeting only you are in, that you made.
func yourCopy(event CalendarEvent) bool {
	return event.Response == "organizer" && len(event.Attendees) == 0 && !event.AllDay && event.ID != ""
}

// anInvite is a meeting someone else organizes, that you didn't decline.
func anInvite(event CalendarEvent) bool {
	return event.Response != "organizer" && event.Response != "declined" && !event.AllDay
}

// little words that say nothing about what a meeting is.
var littleWords = map[string]bool{"and": true, "the": true, "with": true, "for": true, "och": true, "med": true, "för": true}

// subjectMarks is the words (lowercase, three letters or more) and the
// emoji in a subject.
func subjectMarks(subject string) map[string]bool {
	marks := map[string]bool{}
	word := strings.Builder{}
	flush := func() {
		if w := word.String(); len([]rune(w)) >= 3 && !littleWords[w] {
			marks[w] = true
		}
		word.Reset()
	}
	for _, r := range strings.ToLower(subject) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			word.WriteRune(r)
		case r > 0x2000 && unicode.Is(unicode.So, r):
			flush()
			marks[string(r)] = true
		default:
			flush()
		}
	}
	flush()
	return marks
}

// alike is whether two subjects share a word or an emoji.
func alike(a, b string) bool {
	marksB := subjectMarks(b)
	for mark := range subjectMarks(a) {
		if marksB[mark] {
			return true
		}
	}
	return false
}

func overlaps(a, b CalendarEvent) bool {
	return a.Start < b.End && b.Start < a.End
}

// CalendarSuggestions is your copies, still ahead, that an invite covers:
// it overlaps in time and the subjects share a word or an emoji. A live
// invite makes it a duplicate; only a cancelled one makes it stale. kept
// is the suggestions you said to leave alone.
func CalendarSuggestions(events, cancelled []CalendarEvent, kept []string, now time.Time) []CalendarSuggestion {
	suggestions := []CalendarSuggestion{}
	nowMs := now.UnixMilli()
	match := func(own CalendarEvent, among []CalendarEvent) (CalendarEvent, bool) {
		for _, invite := range among {
			if invite.ID != own.ID && anInvite(invite) && overlaps(own, invite) && alike(own.Subject, invite.Subject) {
				return invite, true
			}
		}
		return CalendarEvent{}, false
	}
	for _, own := range events {
		if !yourCopy(own) || own.Start <= nowMs {
			continue
		}
		id := suggestionID(own.ID)
		if slices.Contains(kept, id) {
			continue
		}
		if invite, ok := match(own, events); ok {
			suggestions = append(suggestions, CalendarSuggestion{ID: id, Kind: "duplicate", Copy: own, Invite: invite})
		} else if invite, ok := match(own, cancelled); ok {
			suggestions = append(suggestions, CalendarSuggestion{ID: id, Kind: "stale", Copy: own, Invite: invite})
		}
	}
	return suggestions
}

// Suggestions sends calendar.duplicate or calendar.stale for each
// suggestion not told yet, and is told with them added (the last 200).
// told outlives a restart (toldSuggestionsKey), so each is told once, and
// one already there when this came in is told too.
func (e *Events) Suggestions(calendar string, suggestions []CalendarSuggestion, told []string) []string {
	for _, s := range suggestions {
		key := s.ID + ":" + s.Kind
		if slices.Contains(told, key) {
			continue
		}
		invite := s.Invite
		e.Publish("calendar."+s.Kind, CalendarChange{Calendar: calendar, Event: s.Copy, Invite: &invite})
		told = append(told, key)
	}
	if len(told) > 200 {
		told = told[len(told)-200:]
	}
	return told
}

// announceSuggestions tells Huginn about the work calendar's new suggestions.
func (n *Norn) announceSuggestions(ctx context.Context) {
	var told []string
	if _, err := n.Store.Setting(ctx, toldSuggestionsKey, &told); err != nil {
		n.logf("reading the calendar suggestions told: %v", err)
		return
	}
	last := ""
	if len(told) > 0 {
		last = told[len(told)-1]
	}
	told = n.Events.Suggestions("work", n.workSuggestions(ctx), told)
	if len(told) > 0 && told[len(told)-1] != last {
		if err := n.Store.SetSetting(ctx, toldSuggestionsKey, told); err != nil {
			n.logf("keeping the calendar suggestions told: %v", err)
		}
	}
}

// workSuggestions is the work calendar's suggestions now, without the ones you kept.
func (n *Norn) workSuggestions(ctx context.Context) []CalendarSuggestion {
	snapshot := n.Accounts.Work.Current(time.Now())
	if snapshot == nil {
		return []CalendarSuggestion{}
	}
	var kept []string
	_, _ = n.Store.Setting(ctx, keptSuggestionsKey, &kept)
	return CalendarSuggestions(snapshot.Events, snapshot.Cancelled, kept, time.Now())
}

func (n *Norn) mountSuggestions(p *part.Part) {
	p.Handle("GET /v1/calendar/suggestions", func(w http.ResponseWriter, r *http.Request) {
		part.JSON(w, http.StatusOK, map[string]any{"suggestions": n.workSuggestions(r.Context())})
	})
	p.Handle("POST /v1/calendar/suggestions/{id}/apply", n.applySuggestion)
	p.Handle("POST /v1/calendar/suggestions/{id}/keep", n.keepSuggestion)
}

func (n *Norn) findSuggestion(w http.ResponseWriter, r *http.Request) (CalendarSuggestion, bool) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only you can change your calendar")
		return CalendarSuggestion{}, false
	}
	id := r.PathValue("id")
	for _, s := range n.workSuggestions(r.Context()) {
		if s.ID == id {
			return s, true
		}
	}
	part.Error(w, http.StatusNotFound, "no calendar suggestion "+id)
	return CalendarSuggestion{}, false
}

// applySuggestion deletes your copy from Outlook. Only a current suggestion's
// copy, so only a meeting you made with nobody else in it.
func (n *Norn) applySuggestion(w http.ResponseWriter, r *http.Request) {
	s, ok := n.findSuggestion(w, r)
	if !ok {
		return
	}
	token, err := n.Accounts.Microsoft.Token(r.Context())
	if err != nil {
		part.Error(w, http.StatusConflict, "sign in to Microsoft first")
		return
	}
	status, body, err := n.Accounts.del(r.Context(), "https://graph.microsoft.com/v1.0/me/events/"+url.PathEscape(s.Copy.ID), token.AccessToken)
	switch {
	case err != nil:
		part.Error(w, http.StatusBadGateway, err.Error())
		return
	case status == http.StatusForbidden || status == http.StatusUnauthorized:
		part.Error(w, http.StatusConflict, "Norn may only read your calendar: sign in to Microsoft again to let it change it")
		return
	case status != http.StatusNoContent && status != http.StatusNotFound:
		part.Error(w, http.StatusBadGateway, apiError("Graph", status, body).Error())
		return
	}
	// Read again, so the copy is gone everywhere Norn shows the calendar.
	if err := n.readWorkCalendar(r.Context()); err != nil {
		n.logf("reading the calendar after deleting %q: %v", s.Copy.Subject, err)
	}
	part.JSON(w, http.StatusOK, map[string]string{"deleted": s.ID})
}

// keepSuggestion is "leave my copy": it is not suggested again.
func (n *Norn) keepSuggestion(w http.ResponseWriter, r *http.Request) {
	s, ok := n.findSuggestion(w, r)
	if !ok {
		return
	}
	var kept []string
	if _, err := n.Store.Setting(r.Context(), keptSuggestionsKey, &kept); err != nil {
		n.fail(w, err)
		return
	}
	// Only the last 200: copies of meetings long past need not be remembered.
	kept = append(kept, s.ID)
	if len(kept) > 200 {
		kept = kept[len(kept)-200:]
	}
	if err := n.Store.SetSetting(r.Context(), keptSuggestionsKey, kept); err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, map[string]string{"kept": s.ID})
}

// bearerDelete is an authorized DELETE.
type bearerDelete func(ctx context.Context, url, token string) (int, []byte, error)

func defaultBearerDelete(ctx context.Context, target, token string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, target, nil)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, nil, fmt.Errorf("deleting the event: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil && !errors.Is(err, io.EOF) {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, body, nil
}
