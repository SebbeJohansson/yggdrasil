package norn

// Slack: the conversations you are in, swept on a loop of their own (a sweep
// can take minutes), and the sign-in. Ported from my-agent's
// tasks/inbox/sources/slack.ts, slackFetch.ts and slackAuth.ts.
//
// The sign-in is a user token from your own Slack app, with PKCE, so the
// sweep reads what you can read. Slack sends the browser back to
// http://localhost:42813/slack, which Norn listens on for that one redirect.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SlackMe is who the token belongs to.
type SlackMe struct {
	UserID string
	Handle string
	URL    string
	// TeamID is the workspace's id, which the desktop app's slack: links need.
	TeamID string
}

// SlackChannel is one conversation.
type SlackChannel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	IsIM        bool   `json:"is_im"`
	IsMPIM      bool   `json:"is_mpim"`
	IsPrivate   bool   `json:"is_private"`
	IsArchived  bool   `json:"is_archived"`
	IsMuted     bool   `json:"is_muted"`
	User        string `json:"user"`
	UnreadCount int    `json:"unread_count_display"`
	LastRead    string `json:"last_read"`
}

// SlackMessage is one message.
type SlackMessage struct {
	Subtype  string `json:"subtype"`
	User     string `json:"user"`
	BotID    string `json:"bot_id"`
	Username string `json:"username"`
	Text     string `json:"text"`
	TS       string `json:"ts"`
	Files    []struct {
		Name, Title, Filetype string
	} `json:"files"`
	Attachments []struct {
		Fallback, Title, Text string
	} `json:"attachments"`
	ThreadTS    string `json:"thread_ts"`
	ReplyCount  int    `json:"reply_count"`
	LatestReply string `json:"latest_reply"`
	Subscribed  bool   `json:"subscribed"`
	LastRead    string `json:"last_read"`
}

func tsToMillis(ts string) (int64, bool) {
	seconds, err := strconv.ParseFloat(ts, 64)
	if err != nil {
		return 0, false
	}
	return int64(math.Round(seconds * 1000)), true
}

// SlackPermalink is the message's link in the workspace.
func SlackPermalink(workspace, channel, ts, threadTS string) string {
	if workspace == "" || channel == "" || ts == "" {
		return ""
	}
	link := fmt.Sprintf("%s/archives/%s/p%s", workspace, channel, strings.Replace(ts, ".", "", 1))
	if threadTS != "" && threadTS != ts {
		link += "?thread_ts=" + url.QueryEscape(threadTS) + "&cid=" + url.QueryEscape(channel)
	}
	return link
}

// SlackAppLink opens the message in the Slack desktop app, without the
// browser's redirect page in between. thread_ts makes a reply open in its
// thread rather than in the channel behind it.
func SlackAppLink(team, channel, ts, threadTS string) string {
	if team == "" || channel == "" || ts == "" {
		return ""
	}
	query := url.Values{"team": {team}, "id": {channel}, "message": {ts}}
	if threadTS != "" && threadTS != ts {
		query.Set("thread_ts", threadTS)
	}
	return "slack://channel?" + query.Encode()
}

var (
	slackSpecial = regexp.MustCompile(`<!(here|channel|everyone)(\|[^>]*)?>`)
	slackUser    = regexp.MustCompile(`<@([UBW][A-Z0-9]+)(\|([^>]*))?>`)
	slackChan    = regexp.MustCompile(`<#(C[A-Z0-9]+)(\|([^>]*))?>`)
	slackLink    = regexp.MustCompile(`<((?:https?|mailto):[^|>]+)(\|([^>]*))?>`)
	slackAnyone  = regexp.MustCompile(`<!(here|channel|everyone)(\||>)`)
	slackUserRef = regexp.MustCompile(`<@([UBW][A-Z0-9]+)`)
)

func labelled(re *regexp.Regexp, text, prefix string, names map[string]string) string {
	return re.ReplaceAllStringFunc(text, func(whole string) string {
		match := re.FindStringSubmatch(whole)
		label := match[3]
		if label == "" {
			label = names[match[1]]
		}
		if label == "" {
			label = match[1]
		}
		return prefix + label
	})
}

// SlackToText turns Slack's markup into what a person reads.
func SlackToText(text string, names map[string]string) string {
	text = slackSpecial.ReplaceAllString(text, "@$1")
	text = labelled(slackUser, text, "@", names)
	text = labelled(slackChan, text, "#", names)
	text = slackLink.ReplaceAllStringFunc(text, func(whole string) string {
		match := slackLink.FindStringSubmatch(whole)
		if match[3] != "" {
			return match[3]
		}
		return match[1]
	})
	return strings.NewReplacer("&gt;", ">", "&lt;", "<", "&amp;", "&").Replace(text)
}

func slackMentionsMe(raw string, me SlackMe) bool {
	return (me.UserID != "" && strings.Contains(raw, "<@"+me.UserID)) || slackAnyone.MatchString(raw)
}

var mpimName = regexp.MustCompile(`-\d+$`)

func groupMembers(channel SlackChannel) []string {
	if !channel.IsMPIM {
		return nil
	}
	var members []string
	for _, member := range strings.Split(mpimName.ReplaceAllString(strings.TrimPrefix(channel.Name, "mpdm-"), ""), "--") {
		if member != "" {
			members = append(members, member)
		}
	}
	return members
}

func channelLabel(channel SlackChannel, names map[string]string) string {
	switch {
	case channel.IsIM:
		if who := names[channel.User]; who != "" {
			return who
		}
		return "direct message"
	case channel.IsMPIM:
		people := groupMembers(channel)
		if len(people) == 0 {
			return "group message"
		}
		return "group: " + strings.Join(people[:min(3, len(people))], ", ")
	case channel.Name != "":
		return "#" + channel.Name
	}
	return "channel"
}

func tierOf(c SlackChannel) int {
	switch {
	case c.IsIM:
		return 0
	case c.IsMPIM:
		return 1
	case c.IsPrivate:
		return 2
	}
	return 3
}

func pollOrder(conversations []SlackChannel) []SlackChannel {
	tiers := make([][]SlackChannel, 4)
	for _, c := range conversations {
		tiers[tierOf(c)] = append(tiers[tierOf(c)], c)
	}
	var ordered []SlackChannel
	rounds := 0
	for _, tier := range tiers {
		rounds = max(rounds, len(tier))
	}
	for round := 0; round < rounds; round++ {
		for _, tier := range tiers {
			if round < len(tier) {
				ordered = append(ordered, tier[round])
			}
		}
	}
	return ordered
}

// ChannelFloor is the share of the sweep's budget kept for channels, so DMs
// can't crowd them out entirely.
const ChannelFloor = 0.25

// SweepOrder picks which conversations a sweep asks about: DMs and groups
// first, a quarter of the budget kept for channels.
func SweepOrder(conversations []SlackChannel, budget int) []SlackChannel {
	var personal, channels []SlackChannel
	for _, c := range conversations {
		if c.IsIM || c.IsMPIM {
			personal = append(personal, c)
		} else {
			channels = append(channels, c)
		}
	}
	personal, channels = pollOrder(personal), pollOrder(channels)
	reserved := min(len(channels), int(math.Ceil(float64(max(0, budget))*ChannelFloor)))
	taken := personal[:min(len(personal), max(0, budget-reserved))]
	return append(append([]SlackChannel{}, taken...), channels[:min(len(channels), max(0, budget-len(taken)))]...)
}

func attachmentContext(message SlackMessage) string {
	var parts []string
	for _, file := range message.Files {
		for _, label := range []string{file.Title, file.Name, file.Filetype} {
			if label != "" {
				parts = append(parts, label)
				break
			}
		}
	}
	for _, attachment := range message.Attachments {
		for _, label := range []string{attachment.Title, attachment.Fallback, attachment.Text} {
			if label != "" {
				parts = append(parts, label)
				break
			}
		}
	}
	return OneLine(strings.Join(parts, ", "), PreviewChars)
}

func newerThan(messages []SlackMessage, lastRead string) []SlackMessage {
	read, ok := tsToMillis(lastRead)
	if !ok {
		return messages
	}
	var newer []SlackMessage
	for _, m := range messages {
		if at, ok := tsToMillis(m.TS); ok && at > read {
			newer = append(newer, m)
		}
	}
	return newer
}

func unreadThreads(messages []SlackMessage) []SlackMessage {
	var threads []SlackMessage
	for _, m := range messages {
		if m.TS == "" || m.ThreadTS != m.TS || !m.Subscribed || m.ReplyCount == 0 {
			continue
		}
		latest, ok := tsToMillis(m.LatestReply)
		if !ok {
			continue
		}
		if read, ok := tsToMillis(m.LastRead); !ok || latest > read {
			threads = append(threads, m)
		}
	}
	return threads
}

func threadReplies(messages []SlackMessage, parent SlackMessage) []SlackMessage {
	read, hasRead := tsToMillis(parent.LastRead)
	var replies []SlackMessage
	for _, m := range messages {
		if m.TS == "" || m.TS == parent.TS {
			continue
		}
		if at, ok := tsToMillis(m.TS); ok && (!hasRead || at > read) {
			replies = append(replies, m)
		}
	}
	return replies
}

var notAMessage = map[string]bool{"bot_add": true, "bot_remove": true, "message_changed": true, "message_deleted": true,
	"pinned_item": true, "unpinned_item": true, "reminder_add": true, "tombstone": true}

func isPost(m SlackMessage) bool {
	return m.Subtype == "" || (!strings.HasPrefix(m.Subtype, "channel_") && !strings.HasPrefix(m.Subtype, "group_") && !notAMessage[m.Subtype])
}

// SlackItems turns a conversation's unread messages into items.
func SlackItems(channel SlackChannel, messages []SlackMessage, me SlackMe, names, emails map[string]string, thread bool) []Item {
	if channel.ID == "" {
		return nil
	}
	subject := channelLabel(channel, names)
	if thread {
		subject += " thread"
	}
	members := groupMembers(channel)
	var items []Item
	for _, m := range messages {
		at, ok := tsToMillis(m.TS)
		if !ok || !isPost(m) || (m.User != "" && m.User == me.UserID) {
			continue
		}
		text := strings.TrimSpace(m.Text)
		context := ""
		if text == "" {
			context = attachmentContext(m)
		}
		if text == "" && context == "" {
			continue
		}
		from := m.Username
		switch {
		case m.User != "":
			from = names[m.User]
			if from == "" {
				from = m.User
			}
		case from == "" && m.BotID != "":
			from = "a bot"
		case from == "":
			from = "someone"
		}
		preview, full := "", ""
		if text != "" {
			full = OneLine(SlackToText(text, names), DraftTextChars)
			preview = OneLine(full, PreviewChars)
		}
		items = append(items, Item{
			ID: "slack:" + channel.ID + ":" + m.TS, Source: "slack", From: from, Sender: emails[m.User],
			Recipients: members, Subject: subject, Preview: preview, Text: full, Context: context, At: at,
			Direct: channel.IsIM || channel.IsMPIM, MentionsMe: slackMentionsMe(m.Text, me),
			Link:    SlackPermalink(me.URL, channel.ID, m.TS, m.ThreadTS),
			AppLink: SlackAppLink(me.TeamID, channel.ID, m.TS, m.ThreadTS),
		})
	}
	return items
}

func referencedUsers(messages []SlackMessage) []string {
	var ids []string
	for _, m := range messages {
		if m.User != "" && !slices.Contains(ids, m.User) {
			ids = append(ids, m.User)
		}
		for _, match := range slackUserRef.FindAllStringSubmatch(m.Text, -1) {
			if !slices.Contains(ids, match[1]) {
				ids = append(ids, match[1])
			}
		}
	}
	return ids
}

// --- the sweep -------------------------------------------------------------------

const (
	slackConcurrency      = 4
	historyLimit          = 15
	maxThreads            = 20
	maxConversationPages  = 6
	historyRefresh        = 10 * time.Minute
	rateLimitRetries      = 2
	maxSlackConversations = 120
)

// SweepProgress is how far a sweep has got, for the dashboard.
type SweepProgress struct {
	Stage string `json:"stage"` // listing, details, history, threads, names
	Done  int    `json:"done"`
	Total int    `json:"total"`
}

type rememberedHistory struct {
	lastRead string
	unread   int
	messages []SlackMessage
	at       time.Time
}

// Slack sweeps your conversations, keeping names and recent histories
// between sweeps so an unchanged channel costs no call.
type Slack struct {
	mu      sync.Mutex
	names   map[string]string
	emails  map[string]string
	history map[string]rememberedHistory
	HTTP    *http.Client
}

// NewSlack makes a sweeper.
func NewSlack() *Slack {
	return &Slack{names: map[string]string{}, emails: map[string]string{}, history: map[string]rememberedHistory{}, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func slackError(body map[string]any) string {
	if ok, _ := body["ok"].(bool); ok {
		return ""
	}
	if message := text(body, "error"); message != "" {
		return message
	}
	return "slack refused the request"
}

func (s *Slack) call(ctx context.Context, token, path string, into any) error {
	for attempt := 0; ; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://slack.com"+path, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := s.HTTP.Do(request)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(io.LimitReader(response.Body, 16<<20))
		response.Body.Close()
		if response.StatusCode == http.StatusTooManyRequests && attempt < rateLimitRetries {
			wait := 2 * time.Second
			if after, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && after > 0 {
				wait = time.Duration(after) * time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		if response.StatusCode >= 300 {
			detail := strings.TrimSpace(string(body))
			if len(detail) > 200 {
				detail = detail[:200]
			}
			return fmt.Errorf("Slack answered %d: %s", response.StatusCode, detail)
		}
		var generic map[string]any
		if err := json.Unmarshal(body, &generic); err != nil {
			return err
		}
		if message := slackError(generic); message != "" {
			return errors.New(message)
		}
		return json.Unmarshal(body, into)
	}
}

func pooled[T, R any](items []T, work func(T) R) []R {
	results := make([]R, len(items))
	var next int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range min(slackConcurrency, len(items)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				index := next
				next++
				mu.Unlock()
				if index >= len(items) {
					return
				}
				results[index] = work(items[index])
			}
		}()
	}
	wg.Wait()
	return results
}

// Sweep reads every unread message you can see, DMs first.
func (s *Slack) Sweep(ctx context.Context, token string, progress func(SweepProgress)) ([]Item, error) {
	report := func(stage string, done, total int) {
		if progress != nil {
			progress(SweepProgress{Stage: stage, Done: done, Total: total})
		}
	}
	report("listing", 0, 0)
	var who struct {
		UserID string `json:"user_id"`
		User   string `json:"user"`
		URL    string `json:"url"`
		TeamID string `json:"team_id"`
	}
	if err := s.call(ctx, token, "/api/auth.test", &who); err != nil {
		return nil, err
	}
	me := SlackMe{UserID: who.UserID, Handle: who.User, URL: strings.TrimRight(who.URL, "/"), TeamID: who.TeamID}

	var listed []SlackChannel
	cursor := ""
	for page := 0; page < maxConversationPages; page++ {
		path := "/api/users.conversations?types=public_channel,private_channel,im,mpim&exclude_archived=true&limit=200"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var body struct {
			Channels []SlackChannel `json:"channels"`
			Metadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := s.call(ctx, token, path, &body); err != nil {
			return nil, err
		}
		listed = append(listed, body.Channels...)
		if cursor = body.Metadata.NextCursor; cursor == "" {
			break
		}
	}
	var usable []SlackChannel
	for _, c := range listed {
		if c.ID != "" && !c.IsArchived {
			usable = append(usable, c)
		}
	}
	conversations := SweepOrder(usable, maxSlackConversations)

	var done int
	var doneMu sync.Mutex
	step := func(stage string, total int) func() {
		doneMu.Lock()
		done = 0
		doneMu.Unlock()
		report(stage, 0, total)
		return func() {
			doneMu.Lock()
			done++
			n := done
			doneMu.Unlock()
			report(stage, n, total)
		}
	}

	tick := step("details", len(conversations))
	detailed := pooled(conversations, func(c SlackChannel) *SlackChannel {
		defer tick()
		var body struct {
			Channel SlackChannel `json:"channel"`
		}
		if s.call(ctx, token, "/api/conversations.info?channel="+url.QueryEscape(c.ID), &body) != nil {
			return nil
		}
		return &body.Channel
	})
	var active []SlackChannel
	for _, c := range detailed {
		if c != nil && c.ID != "" && !c.IsMuted {
			active = append(active, *c)
		}
	}

	type read struct {
		channel  SlackChannel
		messages []SlackMessage
	}
	tick = step("history", len(active))
	histories := pooled(active, func(c SlackChannel) read {
		defer tick()
		now := time.Now()
		s.mu.Lock()
		memo, ok := s.history[c.ID]
		s.mu.Unlock()
		if ok && now.Sub(memo.at) < historyRefresh && memo.lastRead == c.LastRead && memo.unread == max(0, c.UnreadCount) {
			return read{c, memo.messages}
		}
		var body struct {
			Messages []SlackMessage `json:"messages"`
		}
		if s.call(ctx, token, fmt.Sprintf("/api/conversations.history?channel=%s&limit=%d", url.QueryEscape(c.ID), historyLimit), &body) != nil {
			return read{c, nil}
		}
		s.mu.Lock()
		s.history[c.ID] = rememberedHistory{lastRead: c.LastRead, unread: max(0, c.UnreadCount), messages: body.Messages, at: now}
		s.mu.Unlock()
		return read{c, body.Messages}
	})
	var channels []read
	type thread struct {
		channel SlackChannel
		parent  SlackMessage
	}
	var threads []thread
	for _, h := range histories {
		var unread []SlackMessage
		if h.channel.UnreadCount > 0 {
			unread = newerThan(h.messages, h.channel.LastRead)
		}
		channels = append(channels, read{h.channel, unread})
		for _, parent := range unreadThreads(h.messages) {
			threads = append(threads, thread{h.channel, parent})
		}
	}
	threads = threads[:min(len(threads), maxThreads)]
	tick = step("threads", len(threads))
	replies := pooled(threads, func(t thread) read {
		defer tick()
		path := fmt.Sprintf("/api/conversations.replies?channel=%s&ts=%s&limit=5", url.QueryEscape(t.channel.ID), url.QueryEscape(t.parent.TS))
		if t.parent.LastRead != "" {
			path += "&oldest=" + url.QueryEscape(t.parent.LastRead)
		}
		var body struct {
			Messages []SlackMessage `json:"messages"`
		}
		if s.call(ctx, token, path, &body) != nil {
			return read{t.channel, nil}
		}
		return read{t.channel, threadReplies(body.Messages, t.parent)}
	})

	var ids []string
	for _, r := range append(append([]read{}, channels...), replies...) {
		for _, id := range referencedUsers(r.messages) {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		if r.channel.User != "" && !slices.Contains(ids, r.channel.User) {
			ids = append(ids, r.channel.User)
		}
	}
	names, emails := s.resolveNames(ctx, token, ids, step)

	var items []Item
	for _, r := range channels {
		items = append(items, SlackItems(r.channel, r.messages, me, names, emails, false)...)
	}
	for _, r := range replies {
		items = append(items, SlackItems(r.channel, r.messages, me, names, emails, true)...)
	}
	return items, nil
}

func (s *Slack) resolveNames(ctx context.Context, token string, ids []string, step func(string, int) func()) (map[string]string, map[string]string) {
	s.mu.Lock()
	var unknown []string
	for _, id := range ids {
		if _, ok := s.names[id]; !ok {
			unknown = append(unknown, id)
		}
	}
	s.mu.Unlock()
	tick := step("names", len(unknown))
	pooled(unknown, func(id string) struct{} {
		defer tick()
		var body struct {
			User struct {
				Name    string `json:"name"`
				Profile struct {
					DisplayName string `json:"display_name"`
					RealName    string `json:"real_name"`
					Email       string `json:"email"`
				} `json:"profile"`
			} `json:"user"`
		}
		name := id
		email := ""
		if s.call(ctx, token, "/api/users.info?user="+url.QueryEscape(id), &body) == nil {
			for _, candidate := range []string{body.User.Profile.DisplayName, body.User.Profile.RealName, body.User.Name} {
				if candidate != "" {
					name = candidate
					break
				}
			}
			email = strings.ToLower(body.User.Profile.Email)
		}
		s.mu.Lock()
		s.names[id] = name
		if email != "" {
			s.emails[id] = email
		}
		s.mu.Unlock()
		return struct{}{}
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	names, emails := map[string]string{}, map[string]string{}
	for _, id := range ids {
		names[id] = s.names[id]
		if email, ok := s.emails[id]; ok {
			emails[id] = email
		}
	}
	return names, emails
}

// --- the sign-in -----------------------------------------------------------------

// SlackRedirectPort is where Slack sends the browser back to.
const SlackRedirectPort = 42813

// SlackUserScopes are what the user token may read.
var SlackUserScopes = []string{"channels:history", "groups:history", "im:history", "mpim:history",
	"channels:read", "groups:read", "im:read", "mpim:read", "users:read", "users:read.email"}

var userToken = regexp.MustCompile(`^(?:xoxe\.)?xoxp-`)

// SlackStatus is the sign-in as the dashboard shows it.
type SlackStatus struct {
	State        string `json:"state"` // signed-out, waiting, signed-in, error
	AuthorizeURL string `json:"authorizeUrl,omitempty"`
	Handle       string `json:"handle,omitempty"`
	Error        string `json:"error,omitempty"`
}

type slackStored struct {
	ClientID     string `json:"clientId"`
	Handle       string `json:"handle"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"`
}

// SlackAuth holds the Slack sign-in.
type SlackAuth struct {
	file string
	post formPost
	who  func(ctx context.Context, token string) (string, error)
	now  func() time.Time
	port int
	// OnChange is called when a sign-in completes (nil) or ends with a reason.
	OnChange func(err error)

	mu         sync.Mutex
	stored     *slackStored
	waiting    *googleWaiting
	clientID   string
	err        string
	refreshing chan struct{}
}

// NewSlackAuth reads the sign-in kept at file, if there is one.
func NewSlackAuth(file string) *SlackAuth {
	a := &SlackAuth{file: file, post: defaultFormPost, now: time.Now, port: SlackRedirectPort, who: slackWho}
	if data, err := os.ReadFile(file); err == nil {
		var stored slackStored
		if json.Unmarshal(data, &stored) == nil && stored.AccessToken != "" {
			a.stored = &stored
		}
	}
	return a
}

func slackWho(ctx context.Context, token string) (string, error) {
	var who struct {
		User string `json:"user"`
	}
	err := NewSlack().call(ctx, token, "/api/auth.test", &who)
	return who.User, err
}

// Status is the sign-in now.
func (a *SlackAuth) Status() SlackStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.statusLocked()
}

func (a *SlackAuth) statusLocked() SlackStatus {
	switch {
	case a.waiting != nil:
		return SlackStatus{State: "waiting", AuthorizeURL: a.waiting.url}
	case a.stored != nil:
		return SlackStatus{State: "signed-in", Handle: a.stored.Handle}
	case a.err != "":
		return SlackStatus{State: "error", Error: a.err}
	}
	return SlackStatus{State: "signed-out"}
}

func (a *SlackAuth) redirectURI() string {
	return fmt.Sprintf("http://localhost:%d/slack", a.port)
}

// SignIn holds the redirect port and returns Slack's consent page for your
// app. A second call while one waits hands back the same page.
func (a *SlackAuth) SignIn(clientID string) (SlackStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.waiting != nil {
		return a.statusLocked(), nil
	}
	verifier, challenge := PKCEPair()
	state := randomString()
	params := url.Values{"client_id": {clientID}, "user_scope": {strings.Join(SlackUserScopes, ",")}, "redirect_uri": {a.redirectURI()},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {state}}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", a.port))
	if err != nil {
		return a.statusLocked(), fmt.Errorf("could not listen for Slack's redirect on port %d: %w", a.port, err)
	}
	waiting := &googleWaiting{verifier: verifier, state: state, url: "https://slack.com/oauth/v2/authorize?" + params.Encode()}
	waiting.server = &http.Server{Handler: http.HandlerFunc(a.callback), ReadHeaderTimeout: 10 * time.Second}
	waiting.timer = time.AfterFunc(googleSignInWait, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.stopWaitingLocked("the Slack sign-in timed out; start it again")
	})
	go func() { _ = waiting.server.Serve(listener) }()
	a.err, a.waiting, a.clientID = "", waiting, clientID
	return a.statusLocked(), nil
}

func (a *SlackAuth) stopWaitingLocked(why string) {
	waiting := a.waiting
	if waiting == nil {
		return
	}
	a.waiting = nil
	waiting.timer.Stop()
	go func() { _ = waiting.server.Close() }()
	a.err = why
}

// slackTokens reads oauth.v2.access: the user token, when it is one.
func slackTokens(body map[string]any, now time.Time) *slackStored {
	user, ok := body["authed_user"].(map[string]any)
	if !ok {
		user = body
	}
	access := text(user, "access_token")
	if !userToken.MatchString(strings.TrimSpace(access)) {
		return nil
	}
	tokens := &slackStored{AccessToken: access, RefreshToken: text(user, "refresh_token")}
	if expires, ok := user["expires_in"].(float64); ok && expires > 0 {
		tokens.ExpiresAt = now.Add(time.Duration(expires) * time.Second).UnixMilli()
	}
	return tokens
}

func (a *SlackAuth) callback(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	waiting, clientID := a.waiting, a.clientID
	a.mu.Unlock()
	if waiting == nil || r.URL.Path != "/slack" {
		page(w, http.StatusNotFound, "Not here", "This only answers Slack's sign-in redirect.")
		return
	}
	query := r.URL.Query()
	if query.Get("state") != waiting.state {
		page(w, http.StatusBadRequest, "Wrong sign-in", "This is not the sign-in that was started. Start it again.")
		return
	}
	fail := func(status int, why string) {
		a.mu.Lock()
		a.stopWaitingLocked(why)
		a.mu.Unlock()
		page(w, status, "Not signed in", why)
	}
	code := query.Get("code")
	if code == "" {
		why := "Slack refused the sign-in: " + query.Get("error")
		if query.Get("error") == "access_denied" {
			why = "the Slack sign-in was cancelled"
		}
		fail(http.StatusBadRequest, why)
		return
	}
	body, err := a.post(r.Context(), "https://slack.com/api/oauth.v2.access", url.Values{
		"client_id": {clientID}, "code": {code}, "code_verifier": {waiting.verifier}, "redirect_uri": {a.redirectURI()}})
	tokens := slackTokens(body, a.now())
	if err != nil || tokens == nil {
		fail(http.StatusBadGateway, "Slack would not finish the sign-in: "+slackError(body))
		return
	}
	handle, _ := a.who(r.Context(), tokens.AccessToken)
	tokens.ClientID, tokens.Handle = clientID, handle
	a.mu.Lock()
	a.stopWaitingLocked("")
	a.saveLocked(tokens)
	onChange := a.OnChange
	a.mu.Unlock()
	as := ""
	if handle != "" {
		as = "As " + handle + ". "
	}
	page(w, http.StatusOK, "Signed in to Slack", as+"You can close this tab; the inbox sweeps Slack now.")
	if onChange != nil {
		onChange(nil)
	}
}

// Token is a usable user token, refreshed when it rotates and is close to
// expiring; callers at once share one refresh.
func (a *SlackAuth) Token(ctx context.Context) (string, error) {
	for {
		a.mu.Lock()
		stored := a.stored
		if stored == nil {
			a.mu.Unlock()
			return "", ErrSignedOut
		}
		if stored.ExpiresAt == 0 || time.UnixMilli(stored.ExpiresAt).Add(-refreshSlack).After(a.now()) {
			a.mu.Unlock()
			return stored.AccessToken, nil
		}
		if waiting := a.refreshing; waiting != nil {
			a.mu.Unlock()
			<-waiting
			continue
		}
		done := make(chan struct{})
		a.refreshing = done
		a.mu.Unlock()
		token, err := a.refresh(ctx, stored)
		a.mu.Lock()
		a.refreshing = nil
		a.mu.Unlock()
		close(done)
		return token, err
	}
}

func (a *SlackAuth) ended(why string) error {
	a.mu.Lock()
	a.forgetLocked(why)
	onChange := a.OnChange
	a.mu.Unlock()
	if onChange != nil {
		onChange(errors.New(why))
	}
	return ErrSignedOut
}

func (a *SlackAuth) refresh(ctx context.Context, stored *slackStored) (string, error) {
	if stored.RefreshToken == "" {
		return "", a.ended("the Slack sign-in expired")
	}
	body, err := a.post(ctx, "https://slack.com/api/oauth.v2.access", url.Values{
		"client_id": {stored.ClientID}, "grant_type": {"refresh_token"}, "refresh_token": {stored.RefreshToken}})
	if err != nil {
		return "", fmt.Errorf("could not refresh the Slack sign-in: %w", err)
	}
	next := slackTokens(body, a.now())
	if next == nil {
		switch why := slackError(body); why {
		case "invalid_refresh_token", "token_revoked", "invalid_grant_type":
			return "", a.ended("Slack ended the sign-in: " + why)
		default:
			return "", fmt.Errorf("could not refresh the Slack sign-in: %s", why)
		}
	}
	next.ClientID, next.Handle = stored.ClientID, stored.Handle
	if next.RefreshToken == "" {
		next.RefreshToken = stored.RefreshToken
	}
	a.mu.Lock()
	a.saveLocked(next)
	a.mu.Unlock()
	return next.AccessToken, nil
}

// SignOut forgets the sign-in.
func (a *SlackAuth) SignOut() SlackStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopWaitingLocked("")
	a.forgetLocked("")
	return a.statusLocked()
}

func (a *SlackAuth) saveLocked(stored *slackStored) {
	a.stored = stored
	a.err = ""
	_ = saveJSON(a.file, stored)
}

func (a *SlackAuth) forgetLocked(why string) {
	a.stored = nil
	a.err = why
	_ = os.Remove(a.file)
}
