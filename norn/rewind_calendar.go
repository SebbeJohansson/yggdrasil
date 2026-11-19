package norn

// Meetings for any range, for Rewind. The calendars Norn keeps in memory
// reach only from a day back (CalendarWindow); Graph's calendarView and
// Google Calendar answer any past range when asked, so a range outside the
// kept window is read on demand, with the same paging, cancelled meetings
// left out the same way, and kept for a few minutes per range.

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"
)

const (
	// rangeCacheFor is how long a calendar range read on demand is reused.
	rangeCacheFor = 5 * time.Minute
	// rangePages is how many pages one range read follows at most.
	rangePages = 20
)

// OutlookRange is the work calendar's meetings from `from` to `to`, read from
// Graph's calendarView with token. Cancelled meetings are left out.
func OutlookRange(ctx context.Context, get bearerGet, token string, from, to time.Time) ([]CalendarEvent, error) {
	events := []CalendarEvent{}
	next := GraphCalendarURL(from, to)
	for page := 0; next != "" && page < rangePages; page++ {
		status, body, err := get(ctx, next, token, map[string]string{"Prefer": `outlook.timezone="UTC", outlook.body-content-type="text"`})
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, apiError("Graph", status, body)
		}
		var found []CalendarEvent
		found, next = GraphEvents(body)
		events = append(events, found...)
	}
	return events, nil
}

// GoogleRange is the personal calendars' meetings from `from` to `to`.
func GoogleRange(ctx context.Context, get bearerGet, token string, from, to time.Time) ([]CalendarEvent, error) {
	body, err := getOK(ctx, get, googleCalendarAPI+"/users/me/calendarList?minAccessRole=reader", token)
	if err != nil {
		return nil, err
	}
	events := []CalendarEvent{}
	for _, id := range CalendarIDs(body) {
		pageToken := ""
		for page := 0; page < rangePages; page++ {
			body, err := getOK(ctx, get, googleEventsURL(id, from, to, pageToken), token)
			if err != nil {
				return nil, err
			}
			var found []CalendarEvent
			found, pageToken = GoogleEvents(body)
			events = append(events, found...)
			if pageToken == "" {
				break
			}
		}
	}
	slices.SortStableFunc(events, func(a, b CalendarEvent) int { return int(a.Start - b.Start) })
	return events, nil
}

// rangeCache keeps calendar ranges read on demand.
type rangeCache struct {
	mu   sync.Mutex
	kept map[string]cachedRange
}

type cachedRange struct {
	at     time.Time
	events []CalendarEvent
}

func (c *rangeCache) get(key string, now time.Time) ([]CalendarEvent, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	hit, ok := c.kept[key]
	if !ok || now.Sub(hit.at) > rangeCacheFor {
		return nil, false
	}
	return hit.events, true
}

func (c *rangeCache) put(key string, events []CalendarEvent, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.kept == nil {
		c.kept = map[string]cachedRange{}
	}
	for k, v := range c.kept {
		if now.Sub(v.at) > rangeCacheFor {
			delete(c.kept, k)
		}
	}
	c.kept[key] = cachedRange{at: now, events: events}
}

// inRange is the events that overlap from..to.
func inRange(events []CalendarEvent, from, to int64) []CalendarEvent {
	out := []CalendarEvent{}
	for _, e := range events {
		if e.Start < to && e.End > from {
			out = append(out, e)
		}
	}
	return out
}

// calendarRange is one calendar's meetings in from..to: the kept snapshot
// when it covers the range, else read on demand (cached briefly). Nil, nil
// when the calendar is off or not signed in.
func (n *Norn) calendarRange(ctx context.Context, name string, from, to time.Time) ([]CalendarEvent, error) {
	now := time.Now()
	var calendar *Calendar
	var read func() ([]CalendarEvent, error)
	switch name {
	case "work":
		status := n.Accounts.Microsoft.Status()
		if n.hidden(ctx, "outlook-calendar") || status.State != "signed-in" || status.Scopes == nil || !status.Scopes.Calendar {
			return nil, nil
		}
		calendar = n.Accounts.Work
		read = func() ([]CalendarEvent, error) {
			token, err := n.Accounts.Microsoft.Token(ctx)
			if err != nil {
				return nil, err
			}
			return OutlookRange(ctx, n.Accounts.get, token.AccessToken, from, to)
		}
	case "personal":
		if n.hidden(ctx, "google-calendar") || n.Accounts.Google.Status().State != "signed-in" || !n.Accounts.Google.Grants().Calendar {
			return nil, nil
		}
		calendar = n.Accounts.Personal
		read = func() ([]CalendarEvent, error) {
			token, err := n.Accounts.Google.Token(ctx)
			if err != nil {
				return nil, err
			}
			return GoogleRange(ctx, n.Accounts.get, token, from, to)
		}
	default:
		return nil, errors.New("no calendar " + name)
	}
	if snapshot := calendar.Current(now); snapshot != nil && snapshot.From <= from.UnixMilli() && snapshot.To >= to.UnixMilli() {
		return inRange(snapshot.Events, from.UnixMilli(), to.UnixMilli()), nil
	}
	key := name + ":" + from.UTC().Format(time.RFC3339) + ":" + to.UTC().Format(time.RFC3339)
	if events, ok := n.Rewind.calendars.get(key, now); ok {
		return events, nil
	}
	events, err := read()
	if errors.Is(err, ErrSignedOut) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	events = inRange(events, from.UnixMilli(), to.UnixMilli())
	n.Rewind.calendars.put(key, events, now)
	return events, nil
}
