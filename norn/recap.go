package norn

// Last time with this person: a small recap of one person, for the card the
// dashboard shows beside a message from them and on the overview just before
// a meeting with them. It holds the last exchange (when, a one-line summary,
// where it opens), the open promises you made them (the promise ledger) and
// what is still open between you: their messages in the inbox, and a mail
// thread or Teams chat where they wrote last.
//
// Read on this machine only: your mail and Teams chats through Graph, the
// work calendar, the inbox and the ledger. The one-line summary is written by
// the local model (CHAT_URL), never Claude or Sindri, so nothing here needs
// names swapped for placeholders: no text leaves for a cloud model. A recap is
// kept for a few minutes per person, so the dashboard's polling does not read
// Graph each time.

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/llm"
	"yggdrasil/internal/part"
)

const (
	// recapKept is how long one person's recap is reused.
	recapKept = 3 * time.Minute
	// recapDays is how far back the last exchange and open threads are looked for.
	recapDays = 30
	// recapOpenDays is how old a thread where they wrote last may be and still count as open.
	recapOpenDays = 14
	// recapMeetingDays is how far back the work calendar is read for the last meeting together.
	recapMeetingDays = 14
	// recapPromises and recapOpen are the most of each a recap holds.
	recapPromises = 5
	recapOpen     = 3
	// recapLineChars is the longest one-line summary kept.
	recapLineChars = 140
)

// RecapPerson is who a recap is about, as Norn identifies people: an email
// address, a Teams (Entra) user id, a name, or several of them.
type RecapPerson struct {
	Name    string
	Address string
	UserID  string
}

var guidLike = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ParseRecapPerson reads `who` (an address, a Teams user id, or a name when
// neither is known) and an optional name.
func ParseRecapPerson(who, name string) RecapPerson {
	who, name = strings.TrimSpace(who), strings.TrimSpace(name)
	person := RecapPerson{Name: name}
	switch {
	case strings.Contains(who, "@"):
		person.Address = strings.ToLower(who)
	case guidLike.MatchString(who):
		person.UserID = who
	case person.Name == "":
		person.Name = who
	}
	return person
}

// key is what the recap is kept under.
func (p RecapPerson) key() string {
	return strings.ToLower(p.Address + "|" + p.UserID + "|" + p.Name)
}

// Is is whether an address, user id or name is this person.
func (p RecapPerson) Is(id, name string) bool {
	id = strings.TrimSpace(id)
	if id != "" && ((p.Address != "" && strings.EqualFold(id, p.Address)) || (p.UserID != "" && strings.EqualFold(id, p.UserID))) {
		return true
	}
	return p.Name != "" && SamePersonName(p.Name, name)
}

var nonName = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// personNameWords is a name as words: "Berg, Anna" and "anna.berg@x.se" are "anna berg".
func personNameWords(name string) []string {
	name = strings.ToLower(strings.TrimSpace(name))
	if local, _, ok := strings.Cut(name, "@"); ok {
		name = local
	}
	var words []string
	for _, w := range strings.Fields(nonName.ReplaceAllString(name, " ")) {
		if len([]rune(w)) > 1 {
			words = append(words, w)
		}
	}
	return words
}

// SamePersonName is whether two names are the same person: every word of the
// shorter in the longer, and two words at least unless both are one word.
func SamePersonName(a, b string) bool {
	short, long := personNameWords(a), personNameWords(b)
	if len(short) > len(long) {
		short, long = long, short
	}
	if len(short) == 0 || (len(short) == 1 && len(long) > 1) {
		return false
	}
	for _, w := range short {
		if !slices.Contains(long, w) {
			return false
		}
	}
	return true
}

// Exchange is one time you and the person met or wrote.
type Exchange struct {
	// ID is the inbox's id for it where there is one (outlook:<id>, teams:<chat>:<message>).
	ID string
	// Kind is mail, chat or meeting; Source outlook, teams or calendar.
	Kind, Source string
	At           int64
	// Title is the mail's subject, the chat's name or the meeting's subject.
	Title string
	// Text is what was written, for the one-line summary; never sent anywhere but the local model.
	Text string
	Link string
	// Thread is the mail conversation or the chat, so only its newest counts as open.
	Thread string
	// FromThem is whether they wrote it (a meeting is neither's).
	FromThem bool
}

// RecapLast is the last exchange, as the card shows it.
type RecapLast struct {
	Kind   string `json:"kind"`
	Source string `json:"source"`
	At     int64  `json:"at"`
	Title  string `json:"title"`
	// Words is the local model's one line on it; the subject when the model said nothing usable.
	Words    string `json:"words"`
	Link     string `json:"link,omitempty"`
	FromThem bool   `json:"fromThem"`
}

// RecapPromise is an open promise you made them.
type RecapPromise struct {
	ID        int64  `json:"id"`
	What      string `json:"what"`
	Due       string `json:"due,omitempty"`
	Source    string `json:"source"`
	Link      string `json:"link,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

// RecapThread is something still open between you: they wrote last.
type RecapThread struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Source  string `json:"source"`
	Subject string `json:"subject"`
	At      int64  `json:"at"`
	Link    string `json:"link,omitempty"`
	// InInbox is whether it is in the inbox now, unread.
	InInbox bool `json:"inInbox"`
}

// PersonRecap is the card's content for one person. Empty lists, never null.
type PersonRecap struct {
	On       bool           `json:"on"`
	Who      string         `json:"who"`
	Name     string         `json:"name"`
	Last     *RecapLast     `json:"last,omitempty"`
	Promises []RecapPromise `json:"promises"`
	Open     []RecapThread  `json:"open"`
}

// BuildRecap puts a recap together from what was read: the newest exchange,
// the open promises to them, and what is open between you (their inbox
// messages first, then threads where they wrote last).
func BuildRecap(person RecapPerson, exchanges []Exchange, promises []Promise, inbox []Ranked, now time.Time) PersonRecap {
	recap := PersonRecap{On: true, Who: firstOf(person.Address, person.UserID, person.Name), Name: person.Name, Promises: []RecapPromise{}, Open: []RecapThread{}}
	nowMs := now.UnixMilli()
	sorted := slices.Clone(exchanges)
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].At > sorted[b].At })
	for _, e := range sorted {
		if e.At <= nowMs {
			recap.Last = &RecapLast{Kind: e.Kind, Source: e.Source, At: e.At, Title: e.Title, Words: e.Title, Link: e.Link, FromThem: e.FromThem}
			break
		}
	}
	for _, p := range promises {
		if p.State != "open" || len(recap.Promises) == recapPromises {
			continue
		}
		if person.Is(p.ToAddress, p.To) {
			recap.Promises = append(recap.Promises, RecapPromise{ID: p.ID, What: p.What, Due: p.Due, Source: p.Source, Link: p.Link, CreatedAt: p.CreatedAt})
		}
	}
	listed := map[string]bool{}
	for _, r := range inbox {
		if len(recap.Open) == recapOpen {
			break
		}
		if r.Band == "hidden" || r.Item.Hidden != "" || !person.Is(r.Item.Sender, r.Item.From) {
			continue
		}
		listed[r.Item.ID] = true
		kind := "chat"
		if r.Item.Source == "outlook" || r.Item.Source == "gmail" {
			kind = "mail"
		}
		recap.Open = append(recap.Open, RecapThread{ID: r.Item.ID, Kind: kind, Source: r.Item.Source, Subject: firstOf(r.Item.Subject, r.Item.From),
			At: r.Item.At, Link: r.Item.Link, InInbox: true})
	}
	threads := map[string]bool{}
	for _, e := range sorted {
		if e.Kind == "meeting" || e.At > nowMs {
			continue
		}
		thread := firstOf(e.Thread, e.ID)
		if threads[thread] {
			continue
		}
		threads[thread] = true // only the newest in a thread says who wrote last
		if !e.FromThem || listed[e.ID] || nowMs-e.At > recapOpenDays*24*time.Hour.Milliseconds() || len(recap.Open) == recapOpen {
			continue
		}
		listed[e.ID] = true
		recap.Open = append(recap.Open, RecapThread{ID: firstOf(e.ID, thread), Kind: e.Kind, Source: e.Source, Subject: e.Title, At: e.At, Link: e.Link})
	}
	return recap
}

func firstOf(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// recapLineSystem asks for one line on one exchange.
const recapLineSystem = "You write one line for a small card that reminds me what my last exchange with someone was about. " +
	"Reply with ONE short line, at most 12 words, plain text, no quotes, no preamble, no names, no dates. " +
	"Say what it was about, using only what is in the input. Write in the language of the input."

// RecapLineMessages is what the local model reads for the one-line summary.
func RecapLineMessages(e Exchange) []llm.Message {
	kind := map[string]string{"mail": "An email", "chat": "A Teams chat message"}[e.Kind]
	who := "I wrote it."
	if e.FromThem {
		who = "They wrote it to me."
	}
	text := kind + ". " + who + "\nSubject: " + e.Title + "\n\n" + clipRunes(strings.TrimSpace(e.Text), 1500)
	return []llm.Message{{Role: "system", Content: recapLineSystem}, {Role: "user", Content: text}}
}

// RecapLine is the model's answer as one line, or "" when it is not one.
func RecapLine(raw string) string {
	line := strings.TrimSpace(raw)
	if strings.Contains(line, "\n") {
		return ""
	}
	line = strings.Trim(line, `"'“”‘’ `)
	if line == "" || len([]rune(line)) > recapLineChars {
		return ""
	}
	return line
}

// Recaps is the recap's running state.
type Recaps struct {
	n *Norn
	// Model is the local model the one-line summary is written by.
	Model Chatter
	// Exchanges reads the person's mail, chats and meetings with you, and
	// fills in their address or user id where it learns them.
	Exchanges func(ctx context.Context, person RecapPerson, now time.Time) ([]Exchange, RecapPerson)
	// Invited is who is in a meeting, with addresses where Graph has them (recap_next.go).
	Invited func(ctx context.Context, event CalendarEvent) []invitee
	now     func() time.Time

	mu    sync.Mutex
	kept  map[string]keptRecap
	lines map[string]string
}

type keptRecap struct {
	at    time.Time
	recap PersonRecap
}

// NewRecaps makes the recap over Norn's local model, Graph and calendar.
func NewRecaps(n *Norn) *Recaps {
	r := &Recaps{n: n, now: time.Now, kept: map[string]keptRecap{}, lines: map[string]string{},
		Model: func(ctx context.Context, messages []llm.Message, options llm.Options) (string, error) {
			return n.LLM.Chat(ctx, messages, options)
		}}
	r.Exchanges = n.readExchanges
	r.Invited = n.meetingInvitees
	return r
}

// For is one person's recap: kept for recapKept, else read now.
func (r *Recaps) For(ctx context.Context, person RecapPerson) (PersonRecap, error) {
	now := r.now()
	r.mu.Lock()
	if kept, ok := r.kept[person.key()]; ok && now.Sub(kept.at) < recapKept {
		r.mu.Unlock()
		return kept.recap, nil
	}
	r.mu.Unlock()
	exchanges, known := r.Exchanges(ctx, person, now)
	if known.Name == "" {
		known.Name = person.Name
	}
	promises, err := r.n.Promises.List(ctx, "open")
	if err != nil {
		return PersonRecap{}, err
	}
	var inbox []Ranked
	if r.n.Inbox != nil {
		inbox, _, _ = r.n.Inbox.Current(now)
	}
	recap := BuildRecap(known, exchanges, promises, inbox, now)
	recap.Who = firstOf(person.Address, person.UserID, person.Name, recap.Who)
	if recap.Last != nil && recap.Last.Kind != "meeting" {
		for _, e := range exchanges {
			if e.At == recap.Last.At && e.Title == recap.Last.Title {
				if line := r.line(ctx, e); line != "" {
					recap.Last.Words = line
				}
				break
			}
		}
	}
	r.mu.Lock()
	if len(r.kept) > 200 {
		clear(r.kept)
	}
	r.kept[person.key()] = keptRecap{at: now, recap: recap}
	r.mu.Unlock()
	return recap, nil
}

// line is the local model's one line on an exchange, kept by what it read.
func (r *Recaps) line(ctx context.Context, e Exchange) string {
	if strings.TrimSpace(e.Text) == "" {
		return ""
	}
	key := shortHash(e.Kind + "\x00" + e.Title + "\x00" + e.Text)
	r.mu.Lock()
	known, ok := r.lines[key]
	r.mu.Unlock()
	if ok {
		return known
	}
	raw, err := r.Model(ctx, RecapLineMessages(e), llm.Options{Purpose: "person recap", Model: r.n.Config.SummaryModel,
		Temperature: 0.2, MaxTokens: 60, Timeout: 30 * time.Second})
	if err != nil {
		r.n.logf("person recap: no line from the local model: %v", err)
		return ""
	}
	line := RecapLine(raw)
	r.mu.Lock()
	if len(r.lines) > 500 {
		clear(r.lines)
	}
	r.lines[key] = line
	r.mu.Unlock()
	return line
}

// recapChat is a Teams chat as the recap reads it: members with their addresses.
type recapChat struct {
	ID       string  `json:"id"`
	Topic    *string `json:"topic"`
	ChatType string  `json:"chatType"`
	WebURL   string  `json:"webUrl"`
	Members  []struct {
		DisplayName string `json:"displayName"`
		UserID      string `json:"userId"`
		Email       string `json:"email"`
	} `json:"members"`
	LastMessagePreview *struct {
		ID              string `json:"id"`
		CreatedDateTime string `json:"createdDateTime"`
		Body            struct {
			Content string `json:"content"`
		} `json:"body"`
		From *struct {
			User *struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"from"`
	} `json:"lastMessagePreview"`
}

// ChatExchanges is the last message of each one-to-one Teams chat with the
// person, and who they are there (their user id and address).
func ChatExchanges(chats []recapChat, person RecapPerson, me Me) ([]Exchange, RecapPerson) {
	var out []Exchange
	for _, chat := range chats {
		if chat.ChatType != "oneOnOne" || chat.LastMessagePreview == nil {
			continue
		}
		with := false
		for _, m := range chat.Members {
			if m.UserID == me.ID || !(person.Is(m.UserID, m.DisplayName) || person.Is(m.Email, "")) {
				continue
			}
			with = true
			person.UserID = firstOf(person.UserID, m.UserID)
			person.Address = firstOf(person.Address, strings.ToLower(m.Email))
			person.Name = firstOf(person.Name, m.DisplayName)
		}
		preview := chat.LastMessagePreview
		at, ok := when(preview.CreatedDateTime)
		if !with || !ok {
			continue
		}
		fromThem := preview.From != nil && preview.From.User != nil && preview.From.User.ID != me.ID
		id := "teams:" + chat.ID
		if preview.ID != "" {
			id += ":" + preview.ID
		}
		out = append(out, Exchange{ID: id, Kind: "chat", Source: "teams", At: at, Title: firstOf(person.Name, "Teams chat"),
			Text: OneLine(StripHTML(preview.Body.Content), 1500), Link: chat.WebURL, Thread: "teams:" + chat.ID, FromThem: fromThem})
	}
	return out, person
}

// recapMail is a message as the recap reads it.
type recapMail struct {
	ID               string        `json:"id"`
	Subject          string        `json:"subject"`
	ConversationID   string        `json:"conversationId"`
	From             *graphAddress `json:"from"`
	ReceivedDateTime string        `json:"receivedDateTime"`
	BodyPreview      string        `json:"bodyPreview"`
	WebLink          string        `json:"webLink"`
}

// MailExchanges is your mail with the person since since.
func MailExchanges(mails []recapMail, person RecapPerson, since time.Time) []Exchange {
	var out []Exchange
	for _, m := range mails {
		at, ok := when(m.ReceivedDateTime)
		if !ok || time.UnixMilli(at).Before(since) {
			continue
		}
		fromThem := m.From != nil && person.Is(m.From.EmailAddress.Address, m.From.EmailAddress.Name)
		out = append(out, Exchange{ID: "outlook:" + m.ID, Kind: "mail", Source: "outlook", At: at, Title: firstOf(m.Subject, "(no subject)"),
			Text: Unquote(m.BodyPreview), Link: m.WebLink, Thread: "outlook:" + firstOf(m.ConversationID, m.ID), FromThem: fromThem})
	}
	return out
}

// MeetingExchanges is the meetings you had with the person that have ended by now.
func MeetingExchanges(events []CalendarEvent, person RecapPerson, now time.Time) []Exchange {
	var out []Exchange
	for _, e := range events {
		if e.Break || e.Focus || declined(e) || e.AllDay || e.ShowAs == "free" || e.End > now.UnixMilli() {
			continue
		}
		with := person.Is("", e.Organizer)
		for _, a := range e.Attendees {
			with = with || person.Is(a, a)
		}
		if with {
			out = append(out, Exchange{ID: "meeting:" + firstOf(e.ID, e.Subject), Kind: "meeting", Source: "calendar", At: e.Start, Title: e.Subject})
		}
	}
	return out
}

// readExchanges reads the person's Teams chats, mail and meetings with you
// through Graph and the work calendar. A source that is hidden where you are,
// or not signed in, is left out; one that fails is logged and left out.
func (n *Norn) readExchanges(ctx context.Context, person RecapPerson, now time.Time) ([]Exchange, RecapPerson) {
	var out []Exchange
	if events, err := n.calendarRange(ctx, "work", dayOf(now.In(n.Zone)).AddDate(0, 0, -recapMeetingDays), now.Truncate(time.Hour).Add(time.Hour)); err == nil {
		out = append(out, MeetingExchanges(events, person, now)...)
	} else {
		n.logf("person recap: no calendar: %v", err)
	}
	token, err := n.Accounts.Microsoft.Token(ctx)
	if err != nil || n.Inbox == nil {
		return out, person
	}
	me, err := n.Inbox.Graph.Me(ctx, token.AccessToken)
	if err != nil {
		n.logf("person recap: %v", err)
		return out, person
	}
	if !n.hidden(ctx, "teams") {
		var page struct {
			Value []recapChat `json:"value"`
		}
		if err := graphJSON(ctx, token.AccessToken, chatsPath(50), &page); err == nil {
			var chats []Exchange
			chats, person = ChatExchanges(page.Value, person, me)
			out = append(out, chats...)
		} else {
			n.logf("person recap: no Teams chats: %v", err)
		}
	}
	if person.Address != "" && !n.hidden(ctx, "outlook") {
		var page struct {
			Value []recapMail `json:"value"`
		}
		search := url.QueryEscape(`"participants:` + person.Address + `"`)
		err := graphJSON(ctx, token.AccessToken, "/v1.0/me/messages?$search="+search+"&$top=15&$select=id,subject,conversationId,from,receivedDateTime,bodyPreview,webLink", &page)
		if err == nil {
			out = append(out, MailExchanges(page.Value, person, now.AddDate(0, 0, -recapDays))...)
		} else {
			n.logf("person recap: no mail: %v", err)
		}
	}
	return out, person
}

// --- routes ------------------------------------------------------------------------

func (n *Norn) mountRecap(p *part.Part) {
	p.Handle("GET /v1/people/recap", n.personRecap)
}

func (n *Norn) personRecap(w http.ResponseWriter, r *http.Request) {
	who := strings.TrimSpace(r.URL.Query().Get("who"))
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if who == "" || len(who) > 320 || len(name) > 200 {
		part.Error(w, http.StatusBadRequest, "`who` is an email address, a Teams user id or a name; `name` is optional")
		return
	}
	person := ParseRecapPerson(who, name)
	if !n.Recaps.On(r.Context()) {
		part.JSON(w, http.StatusOK, PersonRecap{Who: who, Name: person.Name, Promises: []RecapPromise{}, Open: []RecapThread{}})
		return
	}
	recap, err := n.Recaps.For(r.Context(), person)
	if err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, recap)
}
