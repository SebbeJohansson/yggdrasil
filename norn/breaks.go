package norn

// Breaks: a free slot between meetings that you ask Yggdrasil to keep free.
// Norn puts a "Break" event in the work calendar (Outlook), shown as busy so
// nobody books over it, with nobody invited, and marked with BreakCategory so
// Yggdrasil knows it as its own: the calendar payload says break: true,
// Ratatoskr does not count it as a meeting, and Hermóðr says "on a break".

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"yggdrasil/internal/part"
)

const (
	// BreakCategory marks a break Yggdrasil kept free. Outlook keeps an
	// event's categories, and calendarView reads them back.
	BreakCategory = "Yggdrasil break"
	// BreakSubject is the break's title in the calendar.
	BreakSubject = "Break"
	// MinBreak and MaxBreak are the shortest and longest break kept free.
	MinBreak = 15 * time.Minute
	MaxBreak = 2 * time.Hour
)

// BreakRequest is POST /v1/calendar/breaks: the slot, epoch ms.
type BreakRequest struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// CheckBreak is what is wrong with a slot asked for at now, before the
// calendar is looked at; empty when nothing is.
func CheckBreak(slot BreakRequest, now time.Time) string {
	length := time.Duration(slot.End-slot.Start) * time.Millisecond
	switch {
	case slot.Start <= 0 || slot.End <= 0:
		return "start and end are epoch ms"
	case length < MinBreak || length > MaxBreak:
		return "a break is 15 minutes to 2 hours"
	case slot.End <= now.UnixMilli():
		return "the break is over already"
	case slot.Start > now.Add(CalendarDays*24*time.Hour).UnixMilli():
		return "a break is at most 7 days ahead"
	}
	return ""
}

// blocks is whether an event takes the time a break would: a meeting you
// did not decline, not all day, not shown as free or working elsewhere.
func blocks(event CalendarEvent) bool {
	switch {
	case event.AllDay, event.Response == "declined", event.ShowAs == "free", event.ShowAs == "workingElsewhere":
		return false
	}
	return true
}

// BreakSlot is the break already kept for exactly this slot, or the event in
// its way. Both nil: the slot is free.
func BreakSlot(events []CalendarEvent, slot BreakRequest) (kept, taken *CalendarEvent) {
	return KeptSlot(events, slot, breakKind)
}

// SlotKind is what Norn keeps a slot free as: a break here, a focus block
// for the week's plan (weave.go). Each has its own subject and category.
type SlotKind struct {
	Subject, Category, Body string
	// Focus is a focus block; otherwise a break.
	Focus bool
}

var breakKind = SlotKind{Subject: BreakSubject, Category: BreakCategory, Body: "Kept free by Yggdrasil, so this time stays a break."}

// isKind is whether an event is one Yggdrasil kept as kind.
func (kind SlotKind) isKind(event CalendarEvent) bool {
	if kind.Focus {
		return event.Focus
	}
	return event.Break
}

// KeptSlot is the event of kind already kept for exactly this slot, or the
// event in its way. Both nil: the slot is free.
func KeptSlot(events []CalendarEvent, slot BreakRequest, kind SlotKind) (kept, taken *CalendarEvent) {
	for i := range events {
		event := events[i]
		if kind.isKind(event) && event.Start == slot.Start && event.End == slot.End {
			return &event, nil
		}
	}
	for i := range events {
		event := events[i]
		if blocks(event) && event.Start < slot.End && slot.Start < event.End {
			return nil, &event
		}
	}
	return nil, nil
}

// graphSlot is the event Graph is asked to make. transactionId makes a
// second POST of the same slot (a double click) the same event.
func graphSlot(kind SlotKind, slot BreakRequest) map[string]any {
	at := func(ms int64) map[string]string {
		return map[string]string{"dateTime": time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05"), "timeZone": "UTC"}
	}
	prefix := "yggdrasil-break"
	if kind.Focus {
		prefix = "yggdrasil-focus"
	}
	return map[string]any{
		"subject":       kind.Subject,
		"body":          map[string]string{"contentType": "text", "content": kind.Body},
		"start":         at(slot.Start),
		"end":           at(slot.End),
		"showAs":        "busy",
		"categories":    []string{kind.Category},
		"isReminderOn":  false,
		"transactionId": fmt.Sprintf("%s-%d-%d", prefix, slot.Start, slot.End),
	}
}

// keepBreak is POST /v1/calendar/breaks: keep a free slot free as a Break
// event. 201 with the new event, 200 with the one already there, 409 when
// the slot is taken or Norn may not write to the calendar.
func (n *Norn) keepBreak(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only you can change your calendar")
		return
	}
	var slot BreakRequest
	if err := part.Decode(r, &slot); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if problem := CheckBreak(slot, time.Now()); problem != "" {
		part.Error(w, http.StatusBadRequest, problem)
		return
	}
	token, events, status, problem := n.calendarToWrite(r.Context())
	if problem != "" {
		part.Error(w, status, problem)
		return
	}
	event, status, problem := n.keepSlot(r.Context(), token, events, breakKind, slot)
	if problem != "" {
		part.Error(w, status, problem)
		return
	}
	if status == http.StatusCreated {
		// Read again, so the break shows everywhere Norn shows the calendar.
		if err := n.readWorkCalendar(r.Context()); err != nil {
			n.logf("reading the calendar after keeping a break: %v", err)
		}
	}
	part.JSON(w, status, map[string]any{"event": event})
}

// calendarToWrite is the Microsoft token and the work calendar read again
// (the slot was free when the dashboard last looked); a status and why not
// when Norn cannot write to it now.
func (n *Norn) calendarToWrite(ctx context.Context) (string, []CalendarEvent, int, string) {
	token, err := n.Accounts.Microsoft.Token(ctx)
	if err != nil {
		return "", nil, http.StatusConflict, "sign in to Microsoft first"
	}
	if err := n.readWorkCalendar(ctx); err != nil {
		n.logf("reading the calendar before keeping a slot free: %v", err)
	}
	snapshot := n.Accounts.Work.Current(time.Now())
	if snapshot == nil {
		return "", nil, http.StatusConflict, "the work calendar has not been read yet"
	}
	return token.AccessToken, append(slices.Clone(snapshot.Events), snapshot.Later...), 0, ""
}

// keepSlot keeps a slot free as kind, among the calendar's events: 201 with
// the new event, 200 with the one already there, else a status and why not.
func (n *Norn) keepSlot(ctx context.Context, token string, events []CalendarEvent, kind SlotKind, slot BreakRequest) (CalendarEvent, int, string) {
	kept, taken := KeptSlot(events, slot, kind)
	if kept != nil {
		return *kept, http.StatusOK, ""
	}
	if taken != nil {
		zone := n.Zone
		return CalendarEvent{}, http.StatusConflict, fmt.Sprintf("the slot is no longer free: something is booked %s-%s",
			time.UnixMilli(taken.Start).In(zone).Format("15:04"), time.UnixMilli(taken.End).In(zone).Format("15:04"))
	}
	body, _ := json.Marshal(graphSlot(kind, slot))
	status, answer, err := n.Accounts.post(ctx, "https://graph.microsoft.com/v1.0/me/events", token, body)
	switch {
	case err != nil:
		return CalendarEvent{}, http.StatusBadGateway, err.Error()
	case status == http.StatusForbidden || status == http.StatusUnauthorized:
		return CalendarEvent{}, http.StatusConflict, "Norn may only read your calendar: sign in to Microsoft again to let it change it"
	case status != http.StatusCreated && status != http.StatusOK:
		return CalendarEvent{}, http.StatusBadGateway, apiError("Graph", status, answer).Error()
	}
	parsed, _ := GraphEvents([]byte(`{"value":[` + strings.TrimSpace(string(answer)) + `]}`))
	event := CalendarEvent{Subject: kind.Subject, Start: slot.Start, End: slot.End, ShowAs: "busy", Response: "organizer"}
	if len(parsed) == 1 {
		event = parsed[0]
	}
	event.Break, event.Focus = !kind.Focus, kind.Focus
	return event, http.StatusCreated, ""
}

// bearerPost is an authorized POST of JSON.
type bearerPost func(ctx context.Context, url, token string, body []byte) (int, []byte, error)

func defaultBearerPost(ctx context.Context, target, token string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Prefer", `outlook.timezone="UTC", outlook.body-content-type="text"`)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, nil, fmt.Errorf("making the event: %w", err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, answer, err
}
