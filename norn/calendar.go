package norn

// Your meetings from a day back to seven days ahead: work from Outlook
// (Graph), personal from Google Calendar with Google Tasks beside it. Read
// every two minutes while signed in, kept in memory only. Ported from
// my-agent's microsoft/calendar.ts, google/calendar.ts and
// tasks/calendarContext.ts.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // the reader's time zone, without the OS's tz files
)

const (
	// CalendarDays is how far ahead the snapshot reaches.
	CalendarDays = 7
	// SuggestDays is how far ahead Outlook is read for calendar suggestions:
	// a meeting copied from an email is often weeks out.
	SuggestDays = 30
	// CalendarEvery is how often the calendars are read.
	CalendarEvery = 2 * time.Minute
	// CalendarStale is when a snapshot is no longer "your calendar".
	CalendarStale = 30 * time.Minute
	maxPages      = 5
)

// CalendarEvent is one meeting.
type CalendarEvent struct {
	Subject string `json:"subject"`
	// Start and End are epoch ms.
	Start    int64  `json:"start"`
	End      int64  `json:"end"`
	AllDay   bool   `json:"allDay"`
	Location string `json:"location,omitempty"`
	// Organizer is left out when you organize it.
	Organizer string `json:"organizer,omitempty"`
	// Response is accepted, tentativelyAccepted, declined, notResponded, organizer or none.
	Response string `json:"response,omitempty"`
	// ShowAs is free, busy, tentative, oof or workingElsewhere.
	ShowAs string `json:"showAs,omitempty"`
	Online bool   `json:"online"`
	// ID stays the same when the meeting moves.
	ID string `json:"id,omitempty"`
	// Attendees is who is invited, by name (or address when there is no
	// name), you left out where the calendar says which is you, rooms left out. At most maxAttendees.
	Attendees []string `json:"attendees,omitempty"`
	// Series is the recurring meeting it is one of, the same for every
	// occurrence; empty for a one-off.
	Series string `json:"series,omitempty"`
	// Rooms is the rooms booked for it, by name.
	Rooms []string `json:"rooms,omitempty"`
	// Join is its Teams or Google Meet link: the calendar's own, else the
	// first one in the invitation's text.
	Join string `json:"join,omitempty"`
	// Break is a break Yggdrasil kept free in the work calendar
	// (POST /v1/calendar/breaks), not a meeting.
	Break bool `json:"break,omitempty"`
}

// joinLink is the first Teams or Google Meet link in text.
var joinLink = regexp.MustCompile(`https://(?:teams\.microsoft\.com/(?:l/meetup-join|meet)/[^\s<>"')\]]+|teams\.live\.com/meet/[^\s<>"')\]]+|meet\.google\.com/[a-z]{3}-[a-z]{4}-[a-z]{3}(?:\?[^\s<>"')\]]*)?)`)

// JoinLink is the meeting link to use: the first of links that is set, else
// the first Teams or Meet link in text.
func JoinLink(text string, links ...string) string {
	for _, link := range links {
		if link = strings.TrimSpace(link); strings.HasPrefix(link, "https://") {
			return link
		}
	}
	return joinLink.FindString(text)
}

// maxAttendees keeps an all-hands from filling the snapshot.
const maxAttendees = 30

// addAttendee adds a name once, up to maxAttendees.
func addAttendee(names []string, name string) []string {
	name = strings.TrimSpace(name)
	if name == "" || len(names) >= maxAttendees || slices.Contains(names, name) {
		return names
	}
	return append(names, name)
}

// DueTask is a Google task with a due date. Google keeps only the date.
type DueTask struct {
	Title string `json:"title"`
	// Due is YYYY-MM-DD.
	Due   string `json:"due"`
	List  string `json:"list,omitempty"`
	Notes string `json:"notes,omitempty"`
	Link  string `json:"link,omitempty"`
}

// CalendarSnapshot is one read of a calendar.
type CalendarSnapshot struct {
	Events []CalendarEvent `json:"events"`
	Tasks  []DueTask       `json:"tasks,omitempty"`
	// Cancelled is the cancelled meetings still in the calendar (Outlook
	// keeps an invite you haven't removed), and the ones that left it while
	// still ahead (Calendar.gone), for calendar suggestions only.
	Cancelled []CalendarEvent `json:"-"`
	// Later is the meetings after To, up to SuggestDays ahead, for calendar
	// suggestions only.
	Later []CalendarEvent `json:"-"`
	From  int64           `json:"from"`
	To    int64           `json:"to"`
	At    int64           `json:"at"`
}

// CalendarRead is whether the last read worked: refused is usually the
// calendar scope never granted.
type CalendarRead struct {
	State string `json:"state"` // off, pending, ok, refused
	Error string `json:"error,omitempty"`
}

// Calendar holds one calendar's last snapshot and read.
type Calendar struct {
	mu       sync.Mutex
	snapshot *CalendarSnapshot
	last     CalendarRead
	// gone is meetings that left the calendar while still ahead (an invite
	// cancelled and removed), by id, until they would have ended. In memory
	// only: a restart forgets them.
	gone map[string]CalendarEvent
}

// Set keeps a snapshot unless a newer one is already there, noting the
// meetings that left the calendar since the last one.
func (c *Calendar) Set(snapshot CalendarSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = CalendarRead{State: "ok"}
	if c.snapshot != nil && snapshot.At < c.snapshot.At {
		return
	}
	if c.gone == nil {
		c.gone = map[string]CalendarEvent{}
	}
	still := map[string]bool{}
	for _, event := range append(slices.Clone(snapshot.Events), snapshot.Later...) {
		still[event.ID] = true
	}
	if c.snapshot != nil {
		for _, old := range append(slices.Clone(c.snapshot.Events), c.snapshot.Later...) {
			if old.ID != "" && !still[old.ID] && old.Start > snapshot.At {
				c.gone[old.ID] = old
			}
		}
	}
	for id, event := range c.gone {
		if still[id] || event.End <= snapshot.At {
			delete(c.gone, id)
			continue
		}
		snapshot.Cancelled = append(snapshot.Cancelled, event)
	}
	c.snapshot = &snapshot
}

// Clear forgets the snapshot: the calendar was switched off where you are.
func (c *Calendar) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot = nil
}

func (c *Calendar) setRead(read CalendarRead) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = read
}

// Read is the last read's state.
func (c *Calendar) Read() CalendarRead {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last.State == "" {
		return CalendarRead{State: "pending"}
	}
	return c.last
}

// Current is the snapshot while fresh; nil when there is none.
func (c *Calendar) Current(now time.Time) *CalendarSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snapshot == nil || now.Sub(time.UnixMilli(c.snapshot.At)) > CalendarStale {
		return nil
	}
	snapshot := *c.snapshot
	return &snapshot
}

// CalendarWindow is from a day ago, so the dashboard's "Today" still lists
// the meetings that are over, to CalendarDays ahead.
func CalendarWindow(now time.Time) (from, to time.Time) {
	return now.Add(-24 * time.Hour), now.Add(CalendarDays * 24 * time.Hour)
}

type bearerGet func(ctx context.Context, url, token string, header map[string]string) (int, []byte, error)

func defaultBearerGet(ctx context.Context, target, token string, header map[string]string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	for name, value := range header {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	return response.StatusCode, body, err
}

func apiError(service string, status int, body []byte) error {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	if parsed.Error.Message != "" {
		return fmt.Errorf("%s answered %d: %s", service, status, parsed.Error.Message)
	}
	return fmt.Errorf("%s answered %d", service, status)
}

// --- Outlook ---------------------------------------------------------------------

// GraphCalendarURL is calendarView for a window.
func GraphCalendarURL(from, to time.Time) string {
	params := url.Values{
		"startDateTime": {from.UTC().Format(time.RFC3339Nano)},
		"endDateTime":   {to.UTC().Format(time.RFC3339Nano)},
		"$select":       {"id,subject,start,end,isAllDay,location,organizer,attendees,responseStatus,showAs,isOnlineMeeting,onlineMeeting,body,locations,isCancelled,seriesMasterId,categories"},
		"$orderby":      {"start/dateTime"},
		"$top":          {"200"},
	}
	return "https://graph.microsoft.com/v1.0/me/calendarView?" + params.Encode()
}

var hasZone = regexp.MustCompile(`([zZ]|[+-]\d\d:\d\d)$`)

// graphUTC reads a Graph UTC time ("2026-10-08T11:00:00.0000000", no zone).
func graphUTC(value string) (int64, bool) {
	if value == "" {
		return 0, false
	}
	if !hasZone.MatchString(value) {
		value += "Z"
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}

// GraphEvents reads one page of calendarView. Cancelled meetings are left out.
func GraphEvents(body []byte) (events []CalendarEvent, next string) {
	events, _, next = graphPage(body)
	return events, next
}

// graphPage is one page of calendarView, the cancelled meetings apart.
func graphPage(body []byte) (events, cancelled []CalendarEvent, next string) {
	var page struct {
		Value []struct {
			ID              string                       `json:"id"`
			SeriesMasterID  string                       `json:"seriesMasterId"`
			Subject         string                       `json:"subject"`
			Start           struct{ DateTime string }    `json:"start"`
			End             struct{ DateTime string }    `json:"end"`
			IsAllDay        bool                         `json:"isAllDay"`
			IsCancelled     bool                         `json:"isCancelled"`
			IsOnlineMeeting bool                         `json:"isOnlineMeeting"`
			Location        struct{ DisplayName string } `json:"location"`
			Locations       []struct {
				DisplayName  string `json:"displayName"`
				LocationType string `json:"locationType"`
			} `json:"locations"`
			OnlineMeeting *struct {
				JoinURL string `json:"joinUrl"`
			} `json:"onlineMeeting"`
			Body struct {
				Content string `json:"content"`
			} `json:"body"`
			Organizer struct {
				EmailAddress struct{ Name string } `json:"emailAddress"`
			} `json:"organizer"`
			Attendees []struct {
				Type         string                         `json:"type"`
				EmailAddress struct{ Name, Address string } `json:"emailAddress"`
			} `json:"attendees"`
			ResponseStatus struct{ Response string } `json:"responseStatus"`
			ShowAs         string                    `json:"showAs"`
			Categories     []string                  `json:"categories"`
		} `json:"value"`
		Next string `json:"@odata.nextLink"`
	}
	_ = json.Unmarshal(body, &page)
	events = []CalendarEvent{}
	for _, raw := range page.Value {
		start, okStart := graphUTC(raw.Start.DateTime)
		end, okEnd := graphUTC(raw.End.DateTime)
		if !okStart || !okEnd {
			continue
		}
		subject := strings.TrimSpace(raw.Subject)
		if subject == "" {
			subject = "(no title)"
		}
		var attendees, rooms []string
		for _, location := range raw.Locations {
			if location.LocationType == "conferenceRoom" {
				rooms = addAttendee(rooms, location.DisplayName)
			}
		}
		for _, attendee := range raw.Attendees {
			// A booked room is an attendee too, not a person.
			if attendee.Type == "resource" {
				rooms = addAttendee(rooms, attendee.EmailAddress.Name)
				continue
			}
			name := attendee.EmailAddress.Name
			if name == "" {
				name = attendee.EmailAddress.Address
			}
			attendees = addAttendee(attendees, name)
		}
		event := CalendarEvent{
			Subject: subject, Start: start, End: end, AllDay: raw.IsAllDay, Online: raw.IsOnlineMeeting,
			Location: strings.TrimSpace(raw.Location.DisplayName), Organizer: raw.Organizer.EmailAddress.Name,
			Response: raw.ResponseStatus.Response, ShowAs: raw.ShowAs, ID: raw.ID, Attendees: attendees, Series: raw.SeriesMasterID,
			Rooms: rooms, Join: JoinLink(raw.Body.Content, joinURL(raw.OnlineMeeting)),
			Break: slices.Contains(raw.Categories, BreakCategory),
		}
		if raw.IsCancelled {
			cancelled = append(cancelled, event)
		} else {
			events = append(events, event)
		}
	}
	return events, cancelled, page.Next
}

func joinURL(meeting *struct {
	JoinURL string `json:"joinUrl"`
}) string {
	if meeting == nil {
		return ""
	}
	return meeting.JoinURL
}

// ReadOutlook is one read of the week. ErrSignedOut when there is no sign-in.
func ReadOutlook(ctx context.Context, auth *MicrosoftAuth, get bearerGet, now time.Time) (CalendarSnapshot, error) {
	token, err := auth.Token(ctx)
	if err != nil {
		return CalendarSnapshot{}, err
	}
	from, to := CalendarWindow(now)
	snapshot := CalendarSnapshot{Events: []CalendarEvent{}, From: from.UnixMilli(), To: to.UnixMilli(), At: now.UnixMilli()}
	next := GraphCalendarURL(from, now.Add(SuggestDays*24*time.Hour))
	for page := 0; next != "" && page < maxPages; page++ {
		status, body, err := get(ctx, next, token.AccessToken, map[string]string{"Prefer": `outlook.timezone="UTC", outlook.body-content-type="text"`})
		if err != nil {
			return CalendarSnapshot{}, err
		}
		if status != http.StatusOK {
			return CalendarSnapshot{}, apiError("Graph", status, body)
		}
		var events, cancelled []CalendarEvent
		events, cancelled, next = graphPage(body)
		for _, event := range events {
			if event.Start < snapshot.To {
				snapshot.Events = append(snapshot.Events, event)
			} else {
				snapshot.Later = append(snapshot.Later, event)
			}
		}
		snapshot.Cancelled = append(snapshot.Cancelled, cancelled...)
	}
	return snapshot, nil
}

// --- Google ----------------------------------------------------------------------

const (
	googleCalendarAPI = "https://www.googleapis.com/calendar/v3"
	googleTasksAPI    = "https://tasks.googleapis.com/tasks/v1"
)

// generatedCalendar are Google's own calendars (holidays, birthdays): plans of nobody's.
var generatedCalendar = regexp.MustCompile(`#(holiday|contacts|weeknum)@group\.v\.calendar\.google\.com$`)

// CalendarIDs is the calendars to read: the ones ticked in Google
// Calendar's sidebar, without the generated ones.
func CalendarIDs(body []byte) []string {
	var list struct {
		Items []struct {
			ID       string `json:"id"`
			Selected bool   `json:"selected"`
			Hidden   bool   `json:"hidden"`
			Deleted  bool   `json:"deleted"`
		} `json:"items"`
	}
	_ = json.Unmarshal(body, &list)
	var ids []string
	for _, item := range list.Items {
		if item.ID != "" && item.Selected && !item.Hidden && !item.Deleted && !generatedCalendar.MatchString(item.ID) {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

func googleEventsURL(id string, from, to time.Time, pageToken string) string {
	params := url.Values{
		"timeMin": {from.UTC().Format(time.RFC3339Nano)}, "timeMax": {to.UTC().Format(time.RFC3339Nano)},
		// One event per occurrence, so a weekly standup is seven of them, not one rule.
		"singleEvents": {"true"}, "orderBy": {"startTime"}, "maxResults": {"250"},
	}
	if pageToken != "" {
		params.Set("pageToken", pageToken)
	}
	return googleCalendarAPI + "/calendars/" + url.PathEscape(id) + "/events?" + params.Encode()
}

type googleTime struct {
	DateTime string `json:"dateTime"`
	Date     string `json:"date"`
}

// at is epoch ms and whether it is all day. An all-day date is midnight UTC,
// the way Graph's UTC calendarView gives Outlook's.
func (t googleTime) at() (int64, bool, bool) {
	if t.DateTime != "" {
		parsed, err := time.Parse(time.RFC3339, t.DateTime)
		return parsed.UnixMilli(), false, err == nil
	}
	if t.Date != "" {
		parsed, err := time.Parse("2006-01-02", t.Date)
		return parsed.UnixMilli(), true, err == nil
	}
	return 0, false, false
}

var googleResponses = map[string]string{"accepted": "accepted", "tentative": "tentativelyAccepted", "declined": "declined", "needsAction": "notResponded"}

// GoogleEvents reads one page of events. Cancelled ones are left out.
func GoogleEvents(body []byte) (events []CalendarEvent, next string) {
	var page struct {
		Items []struct {
			ID             string          `json:"id"`
			RecurringEvent string          `json:"recurringEventId"`
			Status         string          `json:"status"`
			Summary        string          `json:"summary"`
			Start          googleTime      `json:"start"`
			End            googleTime      `json:"end"`
			Location       string          `json:"location"`
			Transparency   string          `json:"transparency"`
			HangoutLink    string          `json:"hangoutLink"`
			ConferenceData json.RawMessage `json:"conferenceData"`
			Description    string          `json:"description"`
			Organizer      struct {
				DisplayName string `json:"displayName"`
				Email       string `json:"email"`
				Self        bool   `json:"self"`
			} `json:"organizer"`
			Attendees []struct {
				Self           bool   `json:"self"`
				ResponseStatus string `json:"responseStatus"`
				DisplayName    string `json:"displayName"`
				Email          string `json:"email"`
				Resource       bool   `json:"resource"`
			} `json:"attendees"`
		} `json:"items"`
		NextPageToken string `json:"nextPageToken"`
	}
	_ = json.Unmarshal(body, &page)
	events = []CalendarEvent{}
	for _, raw := range page.Items {
		start, allDay, okStart := raw.Start.at()
		end, _, okEnd := raw.End.at()
		if raw.Status == "cancelled" || !okStart || !okEnd {
			continue
		}
		event := CalendarEvent{Subject: strings.TrimSpace(raw.Summary), Start: start, End: end, AllDay: allDay,
			Online: raw.HangoutLink != "" || len(raw.ConferenceData) > 0, Location: strings.TrimSpace(raw.Location), ShowAs: "busy", ID: raw.ID, Series: raw.RecurringEvent}
		if event.Subject == "" {
			event.Subject = "(no title)"
		}
		if raw.Transparency == "transparent" {
			event.ShowAs = "free"
		}
		if raw.Organizer.Self {
			event.Response = "organizer"
		} else {
			event.Organizer = raw.Organizer.DisplayName
			if event.Organizer == "" {
				event.Organizer = raw.Organizer.Email
			}
			for _, attendee := range raw.Attendees {
				if attendee.Self {
					event.Response = googleResponses[attendee.ResponseStatus]
				}
			}
		}
		var conference struct {
			EntryPoints []struct {
				Type string `json:"entryPointType"`
				URI  string `json:"uri"`
			} `json:"entryPoints"`
		}
		_ = json.Unmarshal(raw.ConferenceData, &conference)
		video := ""
		for _, entry := range conference.EntryPoints {
			if entry.Type == "video" && video == "" {
				video = entry.URI
			}
		}
		event.Join = JoinLink(raw.Description+"\n"+raw.Location, raw.HangoutLink, video)
		for _, attendee := range raw.Attendees {
			if attendee.Resource {
				name := attendee.DisplayName
				if name == "" {
					name = attendee.Email
				}
				event.Rooms = addAttendee(event.Rooms, name)
				continue
			}
			if attendee.Self {
				continue
			}
			name := attendee.DisplayName
			if name == "" {
				name = attendee.Email
			}
			event.Attendees = addAttendee(event.Attendees, name)
		}
		events = append(events, event)
	}
	return events, page.NextPageToken
}

var dueDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)

// DueTasks reads one page of tasks, those with a due date only.
func DueTasks(body []byte, list string) (tasks []DueTask, next string) {
	var page struct {
		Items []struct {
			Title, Notes, Due, Status, WebViewLink string
			Deleted, Hidden                        bool
		} `json:"items"`
		NextPageToken string `json:"nextPageToken"`
	}
	_ = json.Unmarshal(body, &page)
	for _, raw := range page.Items {
		due := dueDate.FindString(raw.Due)
		title := strings.TrimSpace(raw.Title)
		if due == "" || raw.Status == "completed" || raw.Deleted || raw.Hidden || title == "" {
			continue
		}
		task := DueTask{Title: title, Due: due, List: list, Link: raw.WebViewLink}
		if notes := strings.TrimSpace(raw.Notes); notes != "" {
			if runes := []rune(notes); len(runes) > 300 {
				notes = string(runes[:300])
			}
			task.Notes = notes
		}
		tasks = append(tasks, task)
	}
	return tasks, page.NextPageToken
}

func getOK(ctx context.Context, get bearerGet, target, token string) ([]byte, error) {
	status, body, err := get(ctx, target, token, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError("Google", status, body)
	}
	return body, nil
}

func readGoogleTasks(ctx context.Context, get bearerGet, token string, to time.Time) ([]DueTask, error) {
	body, err := getOK(ctx, get, googleTasksAPI+"/users/@me/lists?maxResults=100", token)
	if err != nil {
		return nil, err
	}
	var lists struct {
		Items []struct{ ID, Title string } `json:"items"`
	}
	_ = json.Unmarshal(body, &lists)
	var tasks []DueTask
	for _, list := range lists.Items {
		if list.ID == "" {
			continue
		}
		name := ""
		if len(lists.Items) > 1 {
			name = strings.TrimSpace(list.Title)
			if name == "" {
				name = "Tasks"
			}
		}
		pageToken := ""
		for page := 0; page < maxPages; page++ {
			params := url.Values{"showCompleted": {"false"}, "dueMax": {to.UTC().Format(time.RFC3339Nano)}, "maxResults": {"100"}}
			if pageToken != "" {
				params.Set("pageToken", pageToken)
			}
			body, err := getOK(ctx, get, googleTasksAPI+"/lists/"+url.PathEscape(list.ID)+"/tasks?"+params.Encode(), token)
			if err != nil {
				return nil, err
			}
			var found []DueTask
			found, pageToken = DueTasks(body, name)
			tasks = append(tasks, found...)
			if pageToken == "" {
				break
			}
		}
	}
	slices.SortFunc(tasks, func(a, b DueTask) int {
		if c := strings.Compare(a.Due, b.Due); c != 0 {
			return c
		}
		return strings.Compare(a.Title, b.Title)
	})
	return tasks, nil
}

// ReadGoogle is one read of the week across your calendars, with your tasks
// when the sign-in covers them. A Tasks API that fails costs the tasks, not
// the events.
func ReadGoogle(ctx context.Context, auth *GoogleAuth, get bearerGet, now time.Time, warn func(string, ...any)) (CalendarSnapshot, error) {
	token, err := auth.Token(ctx)
	if err != nil {
		return CalendarSnapshot{}, err
	}
	from, to := CalendarWindow(now)
	body, err := getOK(ctx, get, googleCalendarAPI+"/users/me/calendarList?minAccessRole=reader", token)
	if err != nil {
		return CalendarSnapshot{}, err
	}
	snapshot := CalendarSnapshot{Events: []CalendarEvent{}, From: from.UnixMilli(), To: to.UnixMilli(), At: now.UnixMilli()}
	for _, id := range CalendarIDs(body) {
		pageToken := ""
		for page := 0; page < maxPages; page++ {
			body, err := getOK(ctx, get, googleEventsURL(id, from, to, pageToken), token)
			if err != nil {
				return CalendarSnapshot{}, err
			}
			var events []CalendarEvent
			events, pageToken = GoogleEvents(body)
			snapshot.Events = append(snapshot.Events, events...)
			if pageToken == "" {
				break
			}
		}
	}
	slices.SortStableFunc(snapshot.Events, func(a, b CalendarEvent) int { return int(a.Start - b.Start) })
	if auth.Grants().Tasks {
		tasks, err := readGoogleTasks(ctx, get, token, to)
		if err != nil {
			warn("could not read Google Tasks: %v", err)
		} else {
			snapshot.Tasks = tasks
		}
	}
	return snapshot, nil
}

// --- what the model is told --------------------------------------------------------

var calendarWords = regexp.MustCompile(`(?i)\b(calendar|meetings?|schedule|agenda|booked|busy|free (at|on|between|tomorrow|today|this|next)|am i free|available|availability|standup|1:1|one-on-one|invite|kalender|möten?|mötet|bokad|upptagen|ledig|schema|to-?dos?|due|att göra)\b`)

// AsksAboutCalendar is whether a question is about your calendar. "today"
// alone is not enough: "what's the weather tomorrow?" is not one.
func AsksAboutCalendar(question string) bool {
	return calendarWords.MatchString(question)
}

var beyondFeed = regexp.MustCompile(`(?i)\b(invit\w*|attendees?|participants?|who('?s| is| are)? (coming|joining|attending|in it|there)|agenda|about\b|description|notes|minutes|join link|teams link|reschedul\w*|move|cancel\w*|decline|accept|book|set up a meeting|inbjud\w*|deltagare|vilka kommer|dagordning|flytta|avboka|boka|tacka (ja|nej))\b`)

// BeyondCalendar is a meeting question the feed can't answer: who is
// invited, what it is about, or changing it. Those go to an agent with
// Microsoft 365 before the local model invents an answer.
func BeyondCalendar(question string) bool {
	return beyondFeed.MatchString(question)
}

func inWords(d time.Duration) string {
	minutes := int(d.Round(time.Minute) / time.Minute)
	if minutes < 0 {
		minutes = 0
	}
	if minutes < 60 {
		return fmt.Sprintf("in %d min", minutes)
	}
	hours, rest := minutes/60, minutes%60
	if hours >= 24 {
		days := (hours + 12) / 24
		if days == 1 {
			return "in 1 day"
		}
		return fmt.Sprintf("in %d days", days)
	}
	if rest > 0 {
		return fmt.Sprintf("in %d h %d min", hours, rest)
	}
	return fmt.Sprintf("in %d h", hours)
}

var responseWords = map[string]string{"accepted": "accepted", "tentativelyAccepted": "tentative", "declined": "declined", "notResponded": "not answered", "organizer": "you organize"}

// CalendarLines is the calendar as prompt lines: "now", then one line per
// meeting in the reader's time zone. Declined meetings are left out. The
// next meeting and any overlap are said outright: asked "what's my next
// meeting?" with two listed in order, qwen2.5:7b answered with the second.
func CalendarLines(snapshot CalendarSnapshot, now time.Time, zone *time.Location) []string {
	day := func(ms int64) string { return time.UnixMilli(ms).In(zone).Format("Mon 2 Jan") }
	clock := func(ms int64) string { return time.UnixMilli(ms).In(zone).Format("15:04") }
	nowMS := now.UnixMilli()
	var events []CalendarEvent
	for _, event := range snapshot.Events {
		if event.End > nowMS && event.Response != "declined" {
			events = append(events, event)
		}
	}
	slices.SortStableFunc(events, func(a, b CalendarEvent) int { return int(a.Start - b.Start) })
	lines := []string{fmt.Sprintf("Now: %s %s (%s). The calendar below covers until %s.", day(nowMS), clock(nowMS), zone, day(snapshot.To))}
	today := day(nowMS)
	var next *CalendarEvent
	left := 0
	for i := range events {
		event := &events[i]
		if event.AllDay {
			continue
		}
		if event.Start <= nowMS {
			lines = append(lines, fmt.Sprintf("Happening now: %s, until %s.", event.Subject, clock(event.End)))
		} else if next == nil {
			next = event
		}
		if day(event.Start) == today {
			left++
		}
	}
	if next != nil {
		when := day(next.Start)
		if when == today {
			when = "today"
		}
		lines = append(lines, fmt.Sprintf("Next meeting: %s, %s at %s (%s).", next.Subject, when, clock(next.Start), inWords(time.Duration(next.Start-nowMS)*time.Millisecond)))
	} else {
		lines = append(lines, "Next meeting: none in this window.")
	}
	lines = append(lines, fmt.Sprintf("Meetings left today: %d.", left), "All meetings:")
	for i, event := range events {
		when := fmt.Sprintf("%s %s-%s", day(event.Start), clock(event.Start), clock(event.End))
		if event.AllDay {
			when = day(event.Start) + " all day"
		}
		var details []string
		if event.Location != "" {
			details = append(details, event.Location)
		}
		if event.Organizer != "" {
			details = append(details, "organizer "+event.Organizer)
		}
		if words := responseWords[event.Response]; words != "" {
			details = append(details, words)
		}
		if event.Break {
			details = append(details, "a break you kept free, not a meeting")
		}
		switch event.ShowAs {
		case "free":
			details = append(details, "shown as free")
		case "tentative":
			details = append(details, "shown as tentative")
		}
		if event.Online || event.Join != "" {
			details = append(details, "online")
		}
		if len(event.Rooms) > 0 {
			details = append(details, "in "+strings.Join(event.Rooms, " or "))
		}
		line := when + ": " + event.Subject
		if len(details) > 0 {
			line += " (" + strings.Join(details, "; ") + ")"
		}
		for j, other := range events {
			if j != i && !other.AllDay && !event.AllDay && other.Start < event.End && event.Start < other.End {
				line += " [overlaps another meeting]"
				break
			}
		}
		lines = append(lines, line)
	}
	if len(events) == 0 {
		lines = append(lines, "No meetings left in this window.")
	}
	if len(snapshot.Tasks) > 0 {
		todayDate := now.In(zone).Format("2006-01-02")
		lines = append(lines, "Tasks with a due date (no time of day; Google keeps only the date):")
		for _, task := range snapshot.Tasks {
			when := "due " + task.Due
			switch {
			case task.Due < todayDate:
				when = "overdue since " + task.Due
			case task.Due == todayDate:
				when = "due today"
			}
			line := when + ": " + task.Title
			if task.List != "" {
				line += " (list " + task.List + ")"
			}
			lines = append(lines, line)
		}
	}
	return lines
}

// ForQuestion is the lines for a calendar question; nil when it is not one
// or there is no fresh calendar.
func (c *Calendar) ForQuestion(question string, now time.Time, zone *time.Location) []string {
	snapshot := c.Current(now)
	if snapshot == nil || !AsksAboutCalendar(question) {
		return nil
	}
	return CalendarLines(*snapshot, now, zone)
}

// readInto reads a calendar into c, keeping the read's state.
func (c *Calendar) readInto(read func() (CalendarSnapshot, error)) error {
	snapshot, err := read()
	switch {
	case errors.Is(err, ErrSignedOut):
		c.setRead(CalendarRead{State: "pending"})
		return nil
	case err != nil:
		c.setRead(CalendarRead{State: "refused", Error: err.Error()})
		return err
	}
	c.Set(snapshot)
	return nil
}
