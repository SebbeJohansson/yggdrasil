package norn

// Gmail, your personal mail: unread in the inbox, promotions and social
// hidden, each message fetched once. Ported from my-agent's
// tasks/inbox/sources/gmail.ts and gmailFetch.ts.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

const gmailAPI = "https://gmail.googleapis.com/gmail/v1/users/me"

// Mailbox is one address in a header, with its name.
type Mailbox struct {
	Name    string
	Address string
}

var (
	angleAddress = regexp.MustCompile(`^(.*?)<([^>]+)>\s*$`)
	quotedName   = regexp.MustCompile(`^"(.*)"$`)
)

// Mailboxes splits a From/To/Cc header, minding commas inside quotes and <>.
func Mailboxes(header string) []Mailbox {
	var parts []string
	var current strings.Builder
	quoted, angled := false, false
	for _, char := range header {
		switch {
		case char == '"':
			quoted = !quoted
		case char == '<' && !quoted:
			angled = true
		case char == '>' && !quoted:
			angled = false
		}
		if char == ',' && !quoted && !angled {
			parts = append(parts, current.String())
			current.Reset()
			continue
		}
		current.WriteRune(char)
	}
	parts = append(parts, current.String())
	var boxes []Mailbox
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if match := angleAddress.FindStringSubmatch(part); match != nil {
			name := strings.TrimSpace(strings.ReplaceAll(quotedName.ReplaceAllString(strings.TrimSpace(match[1]), "$1"), `\"`, `"`))
			address := strings.ToLower(strings.TrimSpace(match[2]))
			if name == "" {
				name = address
			}
			boxes = append(boxes, Mailbox{Name: name, Address: address})
			continue
		}
		address := strings.ToLower(strings.TrimSpace(quotedName.ReplaceAllString(part, "$1")))
		boxes = append(boxes, Mailbox{Name: address, Address: address})
	}
	return boxes
}

var hiddenCategories = map[string]string{"CATEGORY_PROMOTIONS": "a Gmail promotion", "CATEGORY_SOCIAL": "a Gmail social update"}

var highPriority = regexp.MustCompile(`^\s*[12]\b`)

// GmailItem reads one message's metadata; nil when it is not one.
func GmailItem(body []byte, address string) *Item {
	var message struct {
		ID           string   `json:"id"`
		ThreadID     string   `json:"threadId"`
		LabelIDs     []string `json:"labelIds"`
		Snippet      string   `json:"snippet"`
		InternalDate string   `json:"internalDate"`
		Payload      struct {
			Headers []struct{ Name, Value string } `json:"headers"`
		} `json:"payload"`
	}
	if json.Unmarshal(body, &message) != nil {
		return nil
	}
	at, err := strconv.ParseInt(message.InternalDate, 10, 64)
	if message.ID == "" || err != nil || at <= 0 {
		return nil
	}
	header := func(name string) string {
		for _, h := range message.Payload.Headers {
			if strings.EqualFold(h.Name, name) {
				return h.Value
			}
		}
		return ""
	}
	senders := Mailboxes(header("From"))
	to, cc := Mailboxes(header("To")), Mailboxes(header("Cc"))
	item := &Item{ID: "gmail:" + message.ID, Source: "gmail", From: "unknown sender", At: at}
	if len(senders) > 0 {
		item.From, item.Sender = senders[0].Name, senders[0].Address
	}
	for _, box := range append(append([]Mailbox{}, to...), cc...) {
		item.Recipients = append(item.Recipients, box.Name)
	}
	subject := strings.TrimSpace(header("Subject"))
	if subject == "" {
		subject = "(no subject)"
	}
	item.Subject = OneLine(subject, 120)
	item.Preview = OneLine(DecodeEntities(message.Snippet), PreviewChars)
	item.Direct = address != "" && slices.ContainsFunc(to, func(box Mailbox) bool { return box.Address == address })
	item.Important = strings.EqualFold(strings.TrimSpace(header("Importance")), "high") || highPriority.MatchString(header("X-Priority"))
	if message.ThreadID != "" {
		account := ""
		if address != "" {
			account = "?authuser=" + url.QueryEscape(address)
		}
		item.Link = "https://mail.google.com/mail/" + account + "#inbox/" + url.PathEscape(message.ThreadID)
	}
	for _, label := range message.LabelIDs {
		if why, ok := hiddenCategories[label]; ok {
			item.Hidden = why
			break
		}
	}
	return item
}

// Gmail reads the unread inbox, fetching each message once.
type Gmail struct {
	mu      sync.Mutex
	address string
	seen    map[string]*Item
	get     bearerGet
}

// NewGmail makes a reader.
func NewGmail() *Gmail {
	return &Gmail{seen: map[string]*Item{}, get: defaultBearerGet}
}

// Reset forgets everything: a different account may sign in next.
func (g *Gmail) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.address = ""
	g.seen = map[string]*Item{}
}

// Items is the unread inbox, newest first.
func (g *Gmail) Items(ctx context.Context, token string, max int) ([]Item, error) {
	g.mu.Lock()
	address := g.address
	g.mu.Unlock()
	if address == "" {
		body, err := getOK(ctx, g.get, gmailAPI+"/profile", token)
		if err != nil {
			return nil, err
		}
		var profile struct {
			EmailAddress string `json:"emailAddress"`
		}
		_ = json.Unmarshal(body, &profile)
		address = strings.ToLower(profile.EmailAddress)
		g.mu.Lock()
		g.address = address
		g.mu.Unlock()
	}
	list, err := getOK(ctx, g.get, gmailAPI+"/messages?"+url.Values{"q": {"in:inbox is:unread"}, "maxResults": {strconv.Itoa(max)}}.Encode(), token)
	if err != nil {
		return nil, err
	}
	var ids struct {
		Messages []struct{ ID string } `json:"messages"`
	}
	_ = json.Unmarshal(list, &ids)
	params := url.Values{"format": {"metadata"}, "metadataHeaders": {"From", "To", "Cc", "Subject", "Importance", "X-Priority"}}
	var current []string
	for _, message := range ids.Messages {
		if message.ID == "" {
			continue
		}
		current = append(current, message.ID)
		g.mu.Lock()
		_, known := g.seen[message.ID]
		g.mu.Unlock()
		if known {
			continue
		}
		status, body, err := g.get(ctx, gmailAPI+"/messages/"+url.PathEscape(message.ID)+"?"+params.Encode(), token, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, apiError("Gmail", status, body)
		}
		g.mu.Lock()
		g.seen[message.ID] = GmailItem(body, address)
		g.mu.Unlock()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for id := range g.seen {
		if !slices.Contains(current, id) {
			delete(g.seen, id)
		}
	}
	items := []Item{}
	for _, id := range current {
		if item := g.seen[id]; item != nil {
			items = append(items, *item)
		}
	}
	slices.SortStableFunc(items, func(a, b Item) int { return int(b.At - a.At) })
	return items, nil
}
