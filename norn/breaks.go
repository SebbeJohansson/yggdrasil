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
	for i := range events {
		event := events[i]
		if event.Break && event.Start == slot.Start && event.End == slot.End {
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

// graphBreak is the event Graph is asked to make. transactionId makes a
// second POST of the same slot (a double click) the same event.
func graphBreak(slot BreakRequest) map[string]any {
	at := func(ms int64) map[string]string {
		return map[string]string{"dateTime": time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05"), "timeZone": "UTC"}
	}
	return map[string]any{
		"subject":       BreakSubject,
		"body":          map[string]string{"contentType": "text", "content": "Kept free by Yggdrasil, so this time stays a break."},
		"start":         at(slot.Start),
		"end":           at(slot.End),
		"showAs":        "busy",
		"categories":    []string{BreakCategory},
		"isReminderOn":  false,
		"transactionId": fmt.Sprintf("yggdrasil-break-%d-%d", slot.Start, slot.End),
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
	token, err := n.Accounts.Microsoft.Token(r.Context())
	if err != nil {
		part.Error(w, http.StatusConflict, "sign in to Microsoft first")
		return
	}
	// Read again first: the slot was free when the dashboard last looked.
	if err := n.readWorkCalendar(r.Context()); err != nil {
		n.logf("reading the calendar before keeping a break: %v", err)
	}
	snapshot := n.Accounts.Work.Current(time.Now())
	if snapshot == nil {
		part.Error(w, http.StatusConflict, "the work calendar has not been read yet")
		return
	}
	kept, taken := BreakSlot(append(slices.Clone(snapshot.Events), snapshot.Later...), slot)
	if kept != nil {
		part.JSON(w, http.StatusOK, map[string]any{"event": kept})
		return
	}
	if taken != nil {
		zone := n.Zone
		part.Error(w, http.StatusConflict, fmt.Sprintf("the slot is no longer free: something is booked %s-%s",
			time.UnixMilli(taken.Start).In(zone).Format("15:04"), time.UnixMilli(taken.End).In(zone).Format("15:04")))
		return
	}
	body, _ := json.Marshal(graphBreak(slot))
	status, answer, err := n.Accounts.post(r.Context(), "https://graph.microsoft.com/v1.0/me/events", token.AccessToken, body)
	switch {
	case err != nil:
		part.Error(w, http.StatusBadGateway, err.Error())
		return
	case status == http.StatusForbidden || status == http.StatusUnauthorized:
		part.Error(w, http.StatusConflict, "Norn may only read your calendar: sign in to Microsoft again to let it change it")
		return
	case status != http.StatusCreated && status != http.StatusOK:
		part.Error(w, http.StatusBadGateway, apiError("Graph", status, answer).Error())
		return
	}
	events, _ := GraphEvents([]byte(`{"value":[` + strings.TrimSpace(string(answer)) + `]}`))
	event := CalendarEvent{Subject: BreakSubject, Start: slot.Start, End: slot.End, ShowAs: "busy", Response: "organizer", Break: true}
	if len(events) == 1 {
		event = events[0]
		event.Break = true
	}
	// Read again, so the break shows everywhere Norn shows the calendar.
	if err := n.readWorkCalendar(r.Context()); err != nil {
		n.logf("reading the calendar after keeping a break: %v", err)
	}
	part.JSON(w, http.StatusCreated, map[string]any{"event": event})
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
