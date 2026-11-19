package norn

// What you sent, for the promise ledger (promises.go): your sent mail in
// Outlook (Mail.Read covers Sent Items) and Gmail (gmail.readonly covers
// Sent), read here; your own last message in each Teams chat and your own
// Slack messages, which the inbox already reads and hands over; and the
// meeting notes you save on the dashboard. Only your own words: a reply's
// quoted thread is cut off. Nothing here leaves Norn.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SentText is how much of one sent message the promise model reads.
const SentText = 4000

// Recipient is someone a message went to, as Norn knows them.
type Recipient struct {
	Name string `json:"name"`
	// Address is an email address, or a Teams or Slack user id; "" when unknown.
	Address string `json:"address,omitempty"`
}

// SentMessage is one message you sent, or meeting notes you saved.
type SentMessage struct {
	// ID is unique across sources: outlook:<id>, gmail:<id>, teams:<chat>:<message>,
	// slack:<channel>:<ts>, meeting:<start>:<hash>.
	ID string
	// Source is outlook, gmail, teams, slack or meeting.
	Source string
	// Key is what a promise is deduplicated by with its text: the message,
	// or the meeting (so notes saved twice give each promise once).
	Key     string
	To      []Recipient
	Subject string
	Text    string
	At      int64
	Link    string
	// Thread is whether a reply can go back where it was said (not meeting notes).
	Thread bool
}

func cutText(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return string(runes)
}

func graphRecipients(list ...[]graphAddress) []Recipient {
	var out []Recipient
	for _, recipients := range list {
		for _, r := range recipients {
			name := strings.TrimSpace(r.EmailAddress.Name)
			address := strings.ToLower(strings.TrimSpace(r.EmailAddress.Address))
			if name == "" {
				name = address
			}
			if name != "" {
				out = append(out, Recipient{Name: name, Address: address})
			}
		}
	}
	return out
}

// sentMailPath is Outlook's Sent Items since `since`, newest first, with the
// part of each body that is new in it (uniqueBody: no quoted thread).
func sentMailPath(since time.Time) string {
	return graphAPI + strings.ReplaceAll(fmt.Sprintf("/v1.0/me/mailFolders/sentitems/messages?$filter=sentDateTime ge %s&$top=25&$orderby=sentDateTime desc&$select=id,subject,toRecipients,ccRecipients,sentDateTime,uniqueBody,webLink",
		since.UTC().Format("2006-01-02T15:04:05Z")), " ", "%20")
}

// SentMailItems reads one page of Outlook's Sent Items.
func SentMailItems(body []byte) []SentMessage {
	var page struct {
		Value []struct {
			ID           string         `json:"id"`
			Subject      string         `json:"subject"`
			ToRecipients []graphAddress `json:"toRecipients"`
			CcRecipients []graphAddress `json:"ccRecipients"`
			SentDateTime string         `json:"sentDateTime"`
			WebLink      string         `json:"webLink"`
			UniqueBody   struct {
				ContentType string `json:"contentType"`
				Content     string `json:"content"`
			} `json:"uniqueBody"`
		} `json:"value"`
	}
	if json.Unmarshal(body, &page) != nil {
		return nil
	}
	var out []SentMessage
	for _, m := range page.Value {
		at, ok := when(m.SentDateTime)
		if m.ID == "" || !ok {
			continue
		}
		text := m.UniqueBody.Content
		if strings.EqualFold(m.UniqueBody.ContentType, "html") {
			text = StripHTML(text)
		}
		text = cutText(Unquote(text), SentText)
		if text == "" {
			continue
		}
		id := "outlook:" + m.ID
		out = append(out, SentMessage{ID: id, Key: id, Source: "outlook", To: graphRecipients(m.ToRecipients, m.CcRecipients),
			Subject: OneLine(m.Subject, 120), Text: text, At: at, Link: m.WebLink, Thread: true})
	}
	return out
}

// readSentMail reads Outlook's Sent Items since `since`.
func readSentMail(ctx context.Context, get bearerGet, token string, since time.Time) ([]SentMessage, error) {
	status, body, err := get(ctx, sentMailPath(since), token, map[string]string{"Prefer": `outlook.body-content-type="text"`, "Accept": "application/json"})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError("Microsoft Graph", status, body)
	}
	return SentMailItems(body), nil
}

type gmailPart struct {
	MimeType string `json:"mimeType"`
	Body     struct {
		Data string `json:"data"`
	} `json:"body"`
	Parts []gmailPart `json:"parts"`
}

// plainText is the first text/plain part, or the first text/html as text.
func (p gmailPart) plainText() string {
	decode := func(data string) string {
		raw, err := base64.URLEncoding.DecodeString(data)
		if err != nil {
			raw, err = base64.RawURLEncoding.DecodeString(data)
		}
		if err != nil {
			return ""
		}
		return string(raw)
	}
	if strings.HasPrefix(p.MimeType, "text/plain") && p.Body.Data != "" {
		return decode(p.Body.Data)
	}
	for _, part := range p.Parts {
		if text := part.plainText(); text != "" {
			return text
		}
	}
	if strings.HasPrefix(p.MimeType, "text/html") && p.Body.Data != "" {
		return StripHTML(decode(p.Body.Data))
	}
	return ""
}

// GmailSentItem reads one sent Gmail message (format=full); nil when it is not one.
func GmailSentItem(body []byte) *SentMessage {
	var message struct {
		ID           string    `json:"id"`
		ThreadID     string    `json:"threadId"`
		Snippet      string    `json:"snippet"`
		InternalDate string    `json:"internalDate"`
		Payload      gmailPart `json:"payload"`
	}
	var headers struct {
		Payload struct {
			Headers []struct{ Name, Value string } `json:"headers"`
		} `json:"payload"`
	}
	if json.Unmarshal(body, &message) != nil || json.Unmarshal(body, &headers) != nil {
		return nil
	}
	at, err := strconv.ParseInt(message.InternalDate, 10, 64)
	if message.ID == "" || err != nil || at <= 0 {
		return nil
	}
	header := func(name string) string {
		for _, h := range headers.Payload.Headers {
			if strings.EqualFold(h.Name, name) {
				return h.Value
			}
		}
		return ""
	}
	var to []Recipient
	for _, box := range append(Mailboxes(header("To")), Mailboxes(header("Cc"))...) {
		to = append(to, Recipient{Name: box.Name, Address: box.Address})
	}
	text := message.Payload.plainText()
	if text == "" {
		text = DecodeEntities(message.Snippet)
	}
	text = cutText(Unquote(text), SentText)
	if text == "" {
		return nil
	}
	link := ""
	if message.ThreadID != "" {
		link = "https://mail.google.com/mail/#sent/" + url.PathEscape(message.ThreadID)
	}
	id := "gmail:" + message.ID
	return &SentMessage{ID: id, Key: id, Source: "gmail", To: to, Subject: OneLine(header("Subject"), 120), Text: text, At: at, Link: link, Thread: true}
}

// readSentGmail reads Gmail's Sent since `since`, skipping the messages `known` says were read before.
func readSentGmail(ctx context.Context, get bearerGet, token string, since time.Time, known func(id string) bool) ([]SentMessage, error) {
	days := max(1, int(time.Since(since).Hours()/24)+1)
	list, err := getOK(ctx, get, gmailAPI+"/messages?"+url.Values{"q": {fmt.Sprintf("in:sent newer_than:%dd", days)}, "maxResults": {"25"}}.Encode(), token)
	if err != nil {
		return nil, err
	}
	var ids struct {
		Messages []struct{ ID string } `json:"messages"`
	}
	_ = json.Unmarshal(list, &ids)
	var out []SentMessage
	for _, m := range ids.Messages {
		if m.ID == "" || known("gmail:"+m.ID) {
			continue
		}
		body, err := getOK(ctx, get, gmailAPI+"/messages/"+url.PathEscape(m.ID)+"?format=full", token)
		if err != nil {
			return out, err
		}
		if sent := GmailSentItem(body); sent != nil && sent.At >= since.UnixMilli() {
			out = append(out, *sent)
		}
	}
	return out, nil
}

// OwnChatMessages is your own last message in each Teams chat of one page of
// /me/chats, the page the inbox reads (it skips these as nothing to answer).
func OwnChatMessages(body []byte, me Me) []SentMessage {
	var page struct {
		Value []graphChat `json:"value"`
	}
	if me.ID == "" || json.Unmarshal(body, &page) != nil {
		return nil
	}
	var out []SentMessage
	for _, chat := range page.Value {
		preview := chat.LastMessagePreview
		if chat.ID == "" || preview == nil || preview.From == nil || preview.From.User == nil || preview.From.User.ID != me.ID {
			continue
		}
		at, ok := when(preview.CreatedDateTime)
		text := cutText(StripHTML(preview.Body.Content), SentText)
		if !ok || text == "" {
			continue
		}
		var to []Recipient
		for _, m := range chat.Members {
			if m.UserID != me.ID && strings.TrimSpace(m.DisplayName) != "" {
				to = append(to, Recipient{Name: strings.TrimSpace(m.DisplayName), Address: m.UserID})
			}
		}
		id := preview.ID
		if id == "" {
			id = fmt.Sprint(at)
		}
		key := "teams:" + chat.ID + ":" + id
		out = append(out, SentMessage{ID: key, Key: key, Source: "teams", To: to, Subject: chatName(chat, me), Text: text, At: at, Link: chat.WebURL, Thread: true})
	}
	return out
}

// OwnSlackMessages is your own messages in one conversation's history.
func OwnSlackMessages(channel SlackChannel, messages []SlackMessage, me SlackMe, names, emails map[string]string) []SentMessage {
	if channel.ID == "" || me.UserID == "" {
		return nil
	}
	var to []Recipient
	switch {
	case channel.IsIM && channel.User != "":
		name := names[channel.User]
		if name == "" {
			name = channel.User
		}
		to = []Recipient{{Name: name, Address: emails[channel.User]}}
	case channel.IsMPIM:
		for _, member := range groupMembers(channel) {
			to = append(to, Recipient{Name: member})
		}
	}
	var out []SentMessage
	for _, m := range messages {
		at, ok := tsToMillis(m.TS)
		if !ok || m.User != me.UserID || !isPost(m) {
			continue
		}
		text := cutText(SlackToText(m.Text, names), SentText)
		if text == "" {
			continue
		}
		key := "slack:" + channel.ID + ":" + m.TS
		out = append(out, SentMessage{ID: key, Key: key, Source: "slack", To: to, Subject: channelLabel(channel, names), Text: text, At: at,
			Link: SlackPermalink(me.URL, channel.ID, m.TS, m.ThreadTS), Thread: true})
	}
	return out
}

// TakeSent hands over your own messages the last sweep saw, once.
func (s *Slack) TakeSent() []SentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	sent := s.sent
	s.sent = nil
	return sent
}

// MeetingNotesSent is meeting notes you saved, as one "message" to the people
// in the meeting.
func MeetingNotesSent(meeting Meeting, text, link string, now time.Time) SentMessage {
	var to []Recipient
	for _, person := range meeting.Attendees {
		if person = strings.TrimSpace(person); person != "" {
			to = append(to, Recipient{Name: person})
		}
	}
	key := "meeting:" + strconv.FormatInt(meeting.Start, 10)
	return SentMessage{ID: key + ":" + shortHash(text), Key: key, Source: "meeting", To: to, Subject: OneLine(meeting.Subject, 120),
		Text: cutText(text, maxLiveNote), At: now.UnixMilli(), Link: link}
}
