package norn

// What a Claude meeting brief is written from, read here instead of by
// Claude: the invitation, your email with the people invited and your Teams
// chats with them, from the last 30 days. Claude gets it with the names
// swapped for placeholders (pseudo.go) and no tools, so it never reads your
// mail or chats itself.

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"yggdrasil/internal/pseudo"
)

const (
	// briefDays is how far back the brief's email and chats go.
	briefDays = 30
	// briefPeople is the most invitees whose email is searched.
	briefPeople = 8
	// briefChats is the most Teams chats read.
	briefChats = 6
	// briefChars is the most the digest may hold.
	briefChars = 14000
)

// BriefFormat is how a meeting brief is written, by either model.
const BriefFormat = "Write Markdown: at most five short bullets, each one concrete thing (an open question, a decision, " +
	"something I promised or was asked for, something to bring), naming who it is with. " +
	"Leave out who is invited: I can already see that. Leave out messages, notes and memories that are not " +
	"with these people or about this meeting's subject. No heading, no labels such as \"Open topics:\", no preamble. " +
	"If nothing bears on this meeting, say so in one line instead of filling it out. Write in the language the sources are in."

// BriefRules is how Claude writes a meeting brief from the digest it is given.
const BriefRules = "How to write this brief: use only the digest below, which Norn read for you from my calendar, " +
	"my email and my Teams chats with the people invited. You have no tools; do not try to look anything up. " +
	"Look for what is going on between me and the people invited. " + BriefFormat

// graphGet reads one Graph path into a value.
type graphGet func(ctx context.Context, path string, into any) error

// invitee is someone in the meeting, as the invitation says.
type invitee struct {
	Name, Address string
	Organizer     bool
}

// briefEvent is the meeting's invitation, as the brief needs it.
type briefEvent struct {
	Subject  string
	Body     string
	Invitees []invitee
}

// BriefMail is one email with an invitee.
type BriefMail struct {
	At            time.Time
	From, Subject string
	To            []string
	Preview       string
}

// BriefChatLine is one Teams message.
type BriefChatLine struct {
	At   time.Time
	From string
	Text string
}

// BriefChat is one Teams chat with an invitee, oldest message first.
type BriefChat struct {
	Name  string
	Lines []BriefChatLine
}

// findWorkEvent is the work meeting with that subject nearest to now.
func (n *Norn) findWorkEvent(subject string, now time.Time) (CalendarEvent, bool) {
	snapshot := n.Accounts.Work.Current(now)
	if snapshot == nil {
		return CalendarEvent{}, false
	}
	var found CalendarEvent
	best := int64(-1)
	for _, event := range snapshot.Events {
		if !strings.EqualFold(strings.TrimSpace(event.Subject), strings.TrimSpace(subject)) || event.ID == "" {
			continue
		}
		distance := event.Start - now.UnixMilli()
		if distance < 0 {
			distance = -distance
		}
		if best < 0 || distance < best {
			best, found = distance, event
		}
	}
	return found, best >= 0
}

// readInvitation is the meeting's invitation: its text and who is invited,
// you and rooms left out.
func readInvitation(ctx context.Context, get graphGet, id string, me Me) (briefEvent, error) {
	var raw struct {
		Subject string `json:"subject"`
		Body    struct {
			ContentType string `json:"contentType"`
			Content     string `json:"content"`
		} `json:"body"`
		Organizer graphAddress `json:"organizer"`
		Attendees []struct {
			Type         string `json:"type"`
			EmailAddress struct {
				Name    string `json:"name"`
				Address string `json:"address"`
			} `json:"emailAddress"`
		} `json:"attendees"`
	}
	path := "/v1.0/me/events/" + url.PathEscape(id) + "?$select=subject,body,organizer,attendees"
	if err := get(ctx, path, &raw); err != nil {
		return briefEvent{}, err
	}
	event := briefEvent{Subject: raw.Subject, Body: strings.TrimSpace(raw.Body.Content)}
	if strings.EqualFold(raw.Body.ContentType, "html") {
		event.Body = StripHTML(event.Body)
	}
	event.Body = strings.TrimSpace(whitespace.ReplaceAllString(event.Body, " "))
	add := func(name, address string, organizer bool) {
		address = strings.ToLower(strings.TrimSpace(address))
		if address == "" || address == me.Address {
			return
		}
		if slices.ContainsFunc(event.Invitees, func(i invitee) bool { return i.Address == address }) {
			return
		}
		event.Invitees = append(event.Invitees, invitee{Name: strings.TrimSpace(name), Address: address, Organizer: organizer})
	}
	add(raw.Organizer.EmailAddress.Name, raw.Organizer.EmailAddress.Address, true)
	for _, a := range raw.Attendees {
		if a.Type != "resource" {
			add(a.EmailAddress.Name, a.EmailAddress.Address, false)
		}
	}
	return event, nil
}

// searchMail is your email with each invitee since since, newest first.
func searchMail(ctx context.Context, get graphGet, invitees []invitee, since time.Time) ([]BriefMail, error) {
	seen := map[string]bool{}
	var mails []BriefMail
	for i, person := range invitees {
		if i == briefPeople {
			break
		}
		var page struct {
			Value []graphMessage `json:"value"`
		}
		search := url.QueryEscape(`"participants:` + person.Address + `"`)
		if err := get(ctx, "/v1.0/me/messages?$search="+search+"&$top=15&$select=id,subject,from,toRecipients,ccRecipients,receivedDateTime,bodyPreview", &page); err != nil {
			return nil, err
		}
		for _, m := range page.Value {
			at, ok := when(m.ReceivedDateTime)
			if !ok || time.UnixMilli(at).Before(since) || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			from := ""
			if m.From != nil {
				from = strings.TrimSpace(m.From.EmailAddress.Name)
				if from == "" {
					from = m.From.EmailAddress.Address
				}
			}
			mails = append(mails, BriefMail{At: time.UnixMilli(at), From: from, Subject: m.Subject,
				To: recipientNames(append(m.ToRecipients, m.CcRecipients...)), Preview: OneLine(m.BodyPreview, 300)})
		}
	}
	sort.Slice(mails, func(a, b int) bool { return mails[a].At.After(mails[b].At) })
	return mails, nil
}

// readChats is your Teams chats with any invitee that were written in since
// since, each with its messages from then.
func readChats(ctx context.Context, get graphGet, invitees []invitee, me Me, since time.Time) ([]BriefChat, error) {
	var page struct {
		Value []struct {
			ID       string  `json:"id"`
			Topic    *string `json:"topic"`
			ChatType string  `json:"chatType"`
			Members  []struct {
				DisplayName string `json:"displayName"`
				UserID      string `json:"userId"`
				Email       string `json:"email"`
			} `json:"members"`
			LastMessagePreview *struct {
				CreatedDateTime string `json:"createdDateTime"`
			} `json:"lastMessagePreview"`
		} `json:"value"`
	}
	if err := get(ctx, chatsPath(50), &page); err != nil {
		return nil, err
	}
	isInvited := func(name, email string) bool {
		return slices.ContainsFunc(invitees, func(i invitee) bool {
			return (email != "" && strings.EqualFold(i.Address, email)) || (name != "" && i.Name != "" && strings.EqualFold(i.Name, name))
		})
	}
	var chats []BriefChat
	for _, chat := range page.Value {
		if len(chats) == briefChats {
			break
		}
		if chat.LastMessagePreview == nil {
			continue
		}
		if at, ok := when(chat.LastMessagePreview.CreatedDateTime); !ok || time.UnixMilli(at).Before(since) {
			continue
		}
		var others []string
		with := false
		for _, m := range chat.Members {
			if m.UserID == me.ID {
				continue
			}
			others = append(others, m.DisplayName)
			with = with || isInvited(m.DisplayName, m.Email)
		}
		if !with {
			continue
		}
		name := strings.Join(others, ", ")
		if chat.Topic != nil && strings.TrimSpace(*chat.Topic) != "" {
			name = strings.TrimSpace(*chat.Topic)
		}
		var messages struct {
			Value []struct {
				MessageType     string  `json:"messageType"`
				CreatedDateTime string  `json:"createdDateTime"`
				DeletedDateTime *string `json:"deletedDateTime"`
				From            *struct {
					User *struct {
						DisplayName string `json:"displayName"`
					} `json:"user"`
				} `json:"from"`
				Body struct {
					Content string `json:"content"`
				} `json:"body"`
			} `json:"value"`
		}
		if err := get(ctx, "/v1.0/me/chats/"+url.PathEscape(chat.ID)+"/messages?$top=30", &messages); err != nil {
			return nil, err
		}
		found := BriefChat{Name: name}
		for _, m := range messages.Value {
			at, ok := when(m.CreatedDateTime)
			if !ok || time.UnixMilli(at).Before(since) || m.MessageType != "message" || m.DeletedDateTime != nil {
				continue
			}
			text := OneLine(StripHTML(m.Body.Content), 400)
			if text == "" {
				continue
			}
			from := "someone"
			if m.From != nil && m.From.User != nil {
				from = m.From.User.DisplayName
			}
			found.Lines = append(found.Lines, BriefChatLine{At: time.UnixMilli(at), From: from, Text: text})
		}
		if len(found.Lines) == 0 {
			continue
		}
		sort.Slice(found.Lines, func(a, b int) bool { return found.Lines[a].At.Before(found.Lines[b].At) })
		chats = append(chats, found)
	}
	return chats, nil
}

// BriefDigest is what Claude writes the brief from, in words, at most
// briefChars long.
func BriefDigest(subject string, start time.Time, organizer string, invited []string, body string, mails []BriefMail, chats []BriefChat, zone *time.Location) string {
	day := func(t time.Time) string { return t.In(zone).Format("2006-01-02") }
	lines := []string{"The meeting: " + subject}
	if !start.IsZero() {
		lines[0] += ", " + start.In(zone).Format("Mon 2006-01-02 15:04")
	}
	if organizer != "" {
		lines = append(lines, "Organizer: "+organizer)
	}
	if len(invited) > 0 {
		lines = append(lines, "Invited: "+strings.Join(invited, ", "))
	}
	if body != "" {
		lines = append(lines, "", "The invitation's text:", clipRunes(body, 2000))
	}
	lines = append(lines, "", fmt.Sprintf("Email with the people invited, last %d days, newest first:", briefDays))
	if len(mails) == 0 {
		lines = append(lines, "- none")
	}
	for _, m := range mails {
		to := ""
		if len(m.To) > 0 {
			to = " to " + strings.Join(m.To, ", ")
		}
		lines = append(lines, fmt.Sprintf("- %s, from %s%s: %s — %s", day(m.At), m.From, to, m.Subject, m.Preview))
	}
	lines = append(lines, "", fmt.Sprintf("Teams chats with the people invited, last %d days:", briefDays))
	if len(chats) == 0 {
		lines = append(lines, "- none")
	}
	for _, c := range chats {
		lines = append(lines, fmt.Sprintf("Chat %q:", c.Name))
		for _, l := range c.Lines {
			lines = append(lines, fmt.Sprintf("- %s %s: %s", day(l.At), l.From, l.Text))
		}
	}
	return clipRunes(strings.Join(lines, "\n"), briefChars)
}

// briefDigest reads, on this machine, what a brief asked by question is
// written from, and the people in it with their addresses, for the
// placeholders.
func (n *Norn) briefDigest(ctx context.Context, question string, now time.Time) (string, []pseudo.Person, error) {
	match := briefAsk.FindStringSubmatch(strings.TrimSpace(question))
	if match == nil {
		return "", nil, fmt.Errorf("not a meeting brief")
	}
	event, ok := n.findWorkEvent(match[1], now)
	if !ok {
		return "", nil, fmt.Errorf("found no work meeting called %q in the calendar", match[1])
	}
	token, err := n.Accounts.Microsoft.Token(ctx)
	if err != nil || n.Inbox == nil {
		return "", nil, fmt.Errorf("the brief needs the Microsoft sign-in to read the meeting, email and chats")
	}
	get := func(ctx context.Context, path string, into any) error {
		return graphJSON(ctx, token.AccessToken, path, into)
	}
	me, err := n.Inbox.Graph.Me(ctx, token.AccessToken)
	if err != nil {
		return "", nil, err
	}
	invitation, err := readInvitation(ctx, get, event.ID, me)
	if err != nil {
		return "", nil, fmt.Errorf("reading the invitation: %w", err)
	}
	since := now.AddDate(0, 0, -briefDays)
	mails, err := searchMail(ctx, get, invitation.Invitees, since)
	if err != nil {
		return "", nil, fmt.Errorf("searching your email: %w", err)
	}
	chats, err := readChats(ctx, get, invitation.Invitees, me, since)
	if err != nil {
		return "", nil, fmt.Errorf("reading your Teams chats: %w", err)
	}
	var people []pseudo.Person
	var organizer string
	var invited []string
	for _, i := range invitation.Invitees {
		name := i.Name
		if name == "" {
			name = i.Address
		}
		role := "invited to the meeting"
		if i.Organizer {
			role, organizer = "organizes the meeting", name
		} else {
			invited = append(invited, name)
		}
		people = append(people, pseudo.Person{Name: i.Name, Emails: []string{i.Address}, Role: role})
	}
	subject := invitation.Subject
	if subject == "" {
		subject = event.Subject
	}
	return BriefDigest(subject, time.UnixMilli(event.Start), organizer, invited, invitation.Body, mails, chats, n.Zone), people, nil
}
