package norn

// Outlook and Teams from Microsoft Graph, and the org chart senders are
// ranked by. Ported from my-agent's tasks/inbox/sources/graph.ts,
// graphFetch.ts and org.ts.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

const graphAPI = "https://graph.microsoft.com"

// Me is the signed-in user, as Graph knows them.
type Me struct {
	ID          string
	Address     string
	DisplayName string
}

func graphJSON(ctx context.Context, token, path string, into any) error {
	target := path
	if !strings.HasPrefix(path, "https://") {
		// The $filter and $orderby values hold spaces; Graph's own nextLinks are encoded already.
		target = graphAPI + strings.ReplaceAll(path, " ", "%20")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if response.StatusCode >= 300 {
		detail := strings.TrimSpace(string(body))
		if len(detail) > 200 {
			detail = detail[:200]
		}
		return fmt.Errorf("Microsoft Graph answered %d: %s", response.StatusCode, detail)
	}
	return json.Unmarshal(body, into)
}

// MeFrom reads /me.
func MeFrom(body []byte) Me {
	var raw struct {
		ID, Mail, UserPrincipalName, DisplayName string
	}
	_ = json.Unmarshal(body, &raw)
	address := raw.Mail
	if address == "" {
		address = raw.UserPrincipalName
	}
	name := raw.DisplayName
	if name == "" {
		name = address
	}
	if name == "" {
		name = "you"
	}
	return Me{ID: raw.ID, Address: strings.ToLower(address), DisplayName: name}
}

func mailPath(top int) string {
	return fmt.Sprintf("/v1.0/me/mailFolders/inbox/messages?$filter=isRead eq false&$top=%d&$orderby=receivedDateTime desc&$select=id,subject,from,toRecipients,ccRecipients,receivedDateTime,bodyPreview,importance,webLink", top)
}

func chatsPath(top int) string {
	return fmt.Sprintf("/v1.0/me/chats?$top=%d&$expand=lastMessagePreview,members&$orderby=lastMessagePreview/createdDateTime desc", top)
}

type graphAddress struct {
	EmailAddress struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	} `json:"emailAddress"`
}

type graphMessage struct {
	ID                 string         `json:"id"`
	Subject            string         `json:"subject"`
	MeetingMessageType string         `json:"meetingMessageType"`
	ODataType          string         `json:"@odata.type"`
	From               *graphAddress  `json:"from"`
	ToRecipients       []graphAddress `json:"toRecipients"`
	CcRecipients       []graphAddress `json:"ccRecipients"`
	ReceivedDateTime   string         `json:"receivedDateTime"`
	BodyPreview        string         `json:"bodyPreview"`
	Importance         string         `json:"importance"`
	WebLink            string         `json:"webLink"`
}

var meetingResponses = map[string]string{
	"meetingAccepted":            "a meeting acceptance",
	"meetingTentativelyAccepted": "a tentative meeting acceptance",
	"meetingDeclined":            "a meeting decline",
}

// meetingResponse is why a meeting reply is hidden: it answers nothing.
func meetingResponse(message graphMessage) string {
	if words, ok := meetingResponses[message.MeetingMessageType]; ok {
		return words
	}
	if strings.Contains(message.ODataType, "eventMessageResponse") {
		return "a meeting response"
	}
	return ""
}

func recipientNames(recipients []graphAddress) []string {
	var names []string
	for _, r := range recipients {
		name := strings.TrimSpace(r.EmailAddress.Name)
		if name == "" {
			name = strings.TrimSpace(r.EmailAddress.Address)
		}
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func when(iso string) (int64, bool) {
	if iso == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339Nano, iso)
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}

// MailItems reads one page of unread Outlook mail.
func MailItems(body []byte, me Me) []Item {
	var page struct {
		Value []graphMessage `json:"value"`
	}
	if json.Unmarshal(body, &page) != nil {
		return nil
	}
	names := MentionNames(me)
	var items []Item
	for _, message := range page.Value {
		at, ok := when(message.ReceivedDateTime)
		if message.ID == "" || !ok {
			continue
		}
		preview := OneLine(Unquote(message.BodyPreview), PreviewChars)
		subject := strings.TrimSpace(message.Subject)
		if subject == "" {
			subject = "(no subject)"
		}
		subject = OneLine(subject, 120)
		from, sender := "unknown sender", ""
		if message.From != nil {
			from = message.From.EmailAddress.Name
			if from == "" {
				from = message.From.EmailAddress.Address
			}
			if from == "" {
				from = "unknown sender"
			}
			sender = strings.ToLower(message.From.EmailAddress.Address)
		}
		direct := false
		for _, r := range message.ToRecipients {
			if strings.ToLower(r.EmailAddress.Address) == me.Address {
				direct = true
			}
		}
		items = append(items, Item{
			ID: "outlook:" + message.ID, Source: "outlook", From: from, Sender: sender,
			Recipients: append(recipientNames(message.ToRecipients), recipientNames(message.CcRecipients)...),
			Subject:    subject, Preview: preview, At: at, Direct: direct,
			MentionsMe: NamesMe(subject+" "+preview, names),
			Important:  strings.EqualFold(message.Importance, "high"),
			Link:       message.WebLink, Hidden: meetingResponse(message),
		})
	}
	return items
}

const (
	// readGrace: Teams sets "read" a moment before the message's own time.
	readGrace = time.Minute
	// freshUnread: a chat that never says when you read it counts as unread
	// only while its last message is this new.
	freshUnread = 2 * time.Hour
)

var nameSeparator = regexp.MustCompile(`[._-]+`)

// MentionNames is how your name may appear in a message: the full display
// name, and the parts of your address, spelled as in your display name.
func MentionNames(me Me) []string {
	full := strings.TrimSpace(me.DisplayName)
	var displayTokens []string
	for _, token := range strings.Fields(full) {
		if len([]rune(token)) >= 3 {
			displayTokens = append(displayTokens, token)
		}
	}
	local, _, _ := strings.Cut(me.Address, "@")
	var localTokens []string
	for _, token := range nameSeparator.Split(local, -1) {
		if len([]rune(token)) >= 3 {
			localTokens = append(localTokens, token)
		}
	}
	candidates := displayTokens
	if len(localTokens) > 0 {
		candidates = nil
		for _, token := range localTokens {
			spelled := token
			for _, display := range displayTokens {
				if strings.EqualFold(display, token) {
					spelled = display
					break
				}
			}
			candidates = append(candidates, spelled)
		}
	}
	seen := map[string]bool{}
	var names []string
	for _, candidate := range append([]string{full}, candidates...) {
		name := strings.TrimSpace(candidate)
		key := strings.ToLower(name)
		if len([]rune(name)) >= 3 && !seen[key] {
			seen[key] = true
			names = append(names, name)
		}
	}
	return names
}

func isWordByte(b byte) bool {
	return b == '_' || b == '\'' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// NamesMe is whether text names you as a whole word.
func NamesMe(text string, names []string) bool {
	haystack := strings.ToLower(text)
	for _, name := range names {
		needle := strings.ToLower(name)
		at := strings.Index(haystack, needle)
		if at < 0 {
			continue
		}
		before := at > 0 && isWordByte(haystack[at-1])
		end := at + len(needle)
		after := end < len(haystack) && isWordByte(haystack[end])
		if !before && !after {
			return true
		}
	}
	return false
}

type graphChat struct {
	ID        string  `json:"id"`
	Topic     *string `json:"topic"`
	ChatType  string  `json:"chatType"`
	WebURL    string  `json:"webUrl"`
	Viewpoint *struct {
		LastMessageReadDateTime string `json:"lastMessageReadDateTime"`
		IsHidden                bool   `json:"isHidden"`
	} `json:"viewpoint"`
	Members []struct {
		DisplayName string `json:"displayName"`
		UserID      string `json:"userId"`
	} `json:"members"`
	LastMessagePreview *struct {
		ID              string `json:"id"`
		CreatedDateTime string `json:"createdDateTime"`
		Body            struct {
			Content string `json:"content"`
		} `json:"body"`
		From *struct {
			User *struct {
				ID          string `json:"id"`
				DisplayName string `json:"displayName"`
			} `json:"user"`
		} `json:"from"`
	} `json:"lastMessagePreview"`
}

func chatName(chat graphChat, me Me) string {
	if chat.Topic != nil && strings.TrimSpace(*chat.Topic) != "" {
		return strings.TrimSpace(*chat.Topic)
	}
	var others []string
	for _, m := range chat.Members {
		if m.UserID != me.ID && strings.TrimSpace(m.DisplayName) != "" {
			others = append(others, strings.TrimSpace(m.DisplayName))
		}
	}
	switch {
	case len(others) == 0 && chat.ChatType == "meeting":
		return "meeting chat"
	case len(others) == 0:
		return "chat"
	case len(others) <= 2:
		return strings.Join(others, ", ")
	}
	return fmt.Sprintf("%s +%d", strings.Join(others[:2], ", "), len(others)-2)
}

// ChatItems reads one page of Teams chats: those whose last message, from
// someone else, is newer than when you last read the chat.
func ChatItems(body []byte, me Me, now time.Time) []Item {
	var page struct {
		Value []graphChat `json:"value"`
	}
	if json.Unmarshal(body, &page) != nil {
		return nil
	}
	names := MentionNames(me)
	var items []Item
	for _, chat := range page.Value {
		preview := chat.LastMessagePreview
		if chat.ID == "" || preview == nil || (chat.Viewpoint != nil && chat.Viewpoint.IsHidden) {
			continue
		}
		at, ok := when(preview.CreatedDateTime)
		if !ok {
			continue
		}
		senderID, senderName := "", ""
		if preview.From != nil && preview.From.User != nil {
			senderID, senderName = preview.From.User.ID, strings.TrimSpace(preview.From.User.DisplayName)
		}
		if senderID != "" && senderID == me.ID {
			continue
		}
		readAt := ""
		if chat.Viewpoint != nil {
			readAt = chat.Viewpoint.LastMessageReadDateTime
		}
		if read, ok := when(readAt); ok {
			if read >= at-readGrace.Milliseconds() {
				continue
			}
		} else if now.UnixMilli()-at > freshUnread.Milliseconds() {
			continue
		}
		text := OneLine(StripHTML(preview.Body.Content), PreviewChars)
		if text == "" {
			continue
		}
		var recipients []string
		for _, m := range chat.Members {
			if (senderID == "" || m.UserID != senderID) && strings.TrimSpace(m.DisplayName) != "" {
				recipients = append(recipients, strings.TrimSpace(m.DisplayName))
			}
		}
		if senderName == "" {
			senderName = "someone"
		}
		id := preview.ID
		if id == "" {
			id = fmt.Sprint(at)
		}
		items = append(items, Item{
			ID: "teams:" + chat.ID + ":" + id, Source: "teams", From: senderName, Sender: senderID,
			Recipients: recipients, Subject: chatName(chat, me), Preview: text, At: at,
			Conversation: chat.ID, Direct: chat.ChatType == "oneOnOne", MentionsMe: NamesMe(text, names), Link: chat.WebURL,
		})
	}
	return items
}

// --- the org chart ---------------------------------------------------------------

// Relation words, as the ranking reasons say them.
var relationWords = map[string]string{
	"manager": "your manager", "above": "above you in the org", "leadership": "company leadership",
	"report": "reports to you", "peer": "on your team",
}

// Person is one directory user.
type Person struct {
	ID        string
	Addresses []string
	ManagerID string
}

// OrgChart is the directory, by id and by address, with its top.
type OrgChart struct {
	byID      map[string]Person
	byAddress map[string]string
	rootID    string
}

const orgPath = "/v1.0/users?$select=id,mail,userPrincipalName,proxyAddresses&$expand=manager($select=id)&$top=100"

// OrgPage reads one page of users.
func OrgPage(body []byte) (people []Person, next string) {
	var page struct {
		Value []struct {
			ID                string   `json:"id"`
			Mail              *string  `json:"mail"`
			UserPrincipalName *string  `json:"userPrincipalName"`
			ProxyAddresses    []string `json:"proxyAddresses"`
			Manager           *struct {
				ID string `json:"id"`
			} `json:"manager"`
		} `json:"value"`
		Next string `json:"@odata.nextLink"`
	}
	_ = json.Unmarshal(body, &page)
	for _, user := range page.Value {
		if user.ID == "" {
			continue
		}
		var candidates []string
		if user.Mail != nil {
			candidates = append(candidates, *user.Mail)
		}
		if user.UserPrincipalName != nil {
			candidates = append(candidates, *user.UserPrincipalName)
		}
		for _, proxy := range user.ProxyAddresses {
			if len(proxy) > 5 && strings.EqualFold(proxy[:5], "smtp:") {
				candidates = append(candidates, proxy[5:])
			}
		}
		var addresses []string
		for _, address := range candidates {
			address = strings.ToLower(strings.TrimSpace(address))
			if address != "" && !slices.Contains(addresses, address) {
				addresses = append(addresses, address)
			}
		}
		person := Person{ID: user.ID, Addresses: addresses}
		if user.Manager != nil {
			person.ManagerID = user.Manager.ID
		}
		people = append(people, person)
	}
	return people, page.Next
}

func (c *OrgChart) chainAbove(id string) []string {
	var chain []string
	at := c.byID[id].ManagerID
	for at != "" && !slices.Contains(chain, at) && len(chain) < len(c.byID) {
		chain = append(chain, at)
		at = c.byID[at].ManagerID
	}
	return chain
}

// NewOrgChart indexes people. The root is the person without a manager
// with the most people under them.
func NewOrgChart(people []Person) *OrgChart {
	chart := &OrgChart{byID: map[string]Person{}, byAddress: map[string]string{}}
	for _, person := range people {
		chart.byID[person.ID] = person
		for _, address := range person.Addresses {
			chart.byAddress[address] = person.ID
		}
	}
	headcount := map[string]int{}
	for _, person := range people {
		for _, id := range chart.chainAbove(person.ID) {
			headcount[id]++
		}
	}
	most := 0
	for _, person := range people {
		if count := headcount[person.ID]; chart.byID[person.ID].ManagerID == "" && count > most {
			chart.rootID, most = person.ID, count
		}
	}
	return chart
}

// RelationOf is what the sender is to you: your manager, above you, company
// leadership, your report, or on your team.
func (c *OrgChart) RelationOf(meID, sender string) string {
	them := sender
	if _, ok := c.byID[them]; !ok {
		them = c.byAddress[strings.ToLower(sender)]
	}
	if _, ok := c.byID[meID]; them == "" || them == meID || !ok {
		return ""
	}
	mine := c.chainAbove(meID)
	switch {
	case len(mine) > 0 && mine[0] == them:
		return "manager"
	case slices.Contains(mine, them):
		return "above"
	case them == c.rootID || (c.rootID != "" && c.byID[them].ManagerID == c.rootID):
		return "leadership"
	}
	theirs := c.byID[them].ManagerID
	if theirs == meID {
		return "report"
	}
	if theirs != "" && len(mine) > 0 && theirs == mine[0] {
		return "peer"
	}
	return ""
}

// --- reading Graph, with what is worth keeping between polls ----------------------

const (
	orgTTL      = 24 * time.Hour
	orgMaxPages = 100
)

// Graph reads Outlook, Teams and the org chart, keeping who you are and the
// org chart (a day) between polls.
type Graph struct {
	mu    sync.Mutex
	me    *Me
	org   *OrgChart
	orgAt time.Time
}

// Me is who is signed in.
func (g *Graph) Me(ctx context.Context, token string) (Me, error) {
	g.mu.Lock()
	known := g.me
	g.mu.Unlock()
	if known != nil {
		return *known, nil
	}
	var raw json.RawMessage
	if err := graphJSON(ctx, token, "/v1.0/me?$select=id,mail,userPrincipalName,displayName", &raw); err != nil {
		return Me{}, err
	}
	me := MeFrom(raw)
	g.mu.Lock()
	g.me = &me
	g.mu.Unlock()
	return me, nil
}

// Forget drops who was signed in: someone else may sign in next.
func (g *Graph) Forget() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.me, g.org = nil, nil
}

// Org is the org chart, read once a day. A failed read keeps the last one.
func (g *Graph) Org(ctx context.Context, token string) (*OrgChart, error) {
	g.mu.Lock()
	if g.org != nil && time.Since(g.orgAt) < orgTTL {
		defer g.mu.Unlock()
		return g.org, nil
	}
	g.mu.Unlock()
	var people []Person
	next := orgPath
	for page := 0; next != "" && page < orgMaxPages; page++ {
		var raw json.RawMessage
		if err := graphJSON(ctx, token, next, &raw); err != nil {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.org != nil {
				return g.org, nil
			}
			return nil, err
		}
		var found []Person
		found, next = OrgPage(raw)
		people = append(people, found...)
	}
	chart := NewOrgChart(people)
	g.mu.Lock()
	g.org, g.orgAt = chart, time.Now()
	g.mu.Unlock()
	return chart, nil
}

// GraphRead is one poll of Outlook and Teams.
type GraphRead struct {
	Outlook, Teams       []Item
	MailError, ChatError error
}

// Read polls the unread mail (when mail is on) and the chats (when chats are).
func (g *Graph) Read(ctx context.Context, token string, mail, chats bool) (GraphRead, error) {
	me, err := g.Me(ctx, token)
	if err != nil {
		return GraphRead{}, err
	}
	var read GraphRead
	var wg sync.WaitGroup
	if mail {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var raw json.RawMessage
			if read.MailError = graphJSON(ctx, token, mailPath(25), &raw); read.MailError == nil {
				read.Outlook = MailItems(raw, me)
			}
		}()
	}
	if chats {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var raw json.RawMessage
			if read.ChatError = graphJSON(ctx, token, chatsPath(25), &raw); read.ChatError == nil {
				read.Teams = ChatItems(raw, me, time.Now())
			}
		}()
	}
	wg.Wait()
	return read, nil
}
