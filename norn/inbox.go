package norn

// The inbox: collected and ranked here, all the time. Outlook, Teams and
// Gmail are polled every two minutes (every 20 s while failing); Slack is
// swept on its own loop, since a sweep can take minutes. Nothing about your
// inbox is written to disk but the mutes: the ranked list is in memory and
// goes stale after 15 minutes. Ported from my-agent's
// tasks/inbox/collector.ts, collect.ts and tasks/inboxContext.ts.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/part"
)

const (
	pollEvery  = 2 * time.Minute
	retryEvery = 20 * time.Second
	// InboxStale is when the ranked list no longer counts as your inbox.
	InboxStale = 15 * time.Minute
	// PromptItems is how many items a question about the inbox gets.
	PromptItems = 10
)

type slackSweep struct {
	state string // off, pending, done, failed
	items []Item
	err   error
	at    int64
}

// Inbox collects and ranks.
type Inbox struct {
	n         *Norn
	Graph     *Graph
	Slack     *Slack
	SlackAuth *SlackAuth
	Gmail     *Gmail
	Judge     *Judge

	mu             sync.Mutex
	items          []Item
	sources        []SourceStatus
	feedAt         int64
	shown          []Ranked
	justHidden     []Ranked
	reportedHidden map[string]bool
	summaries      map[string]Summary
	graphStep      *string
	graphAt        *int64
	slack          slackSweep
	slackProgress  *SweepProgress
	slackAt        *int64
	err            string
	judgeLabel     *string
	quiet          map[string]bool
	lastPoll       time.Time
	lastOK         bool

	polling   sync.Mutex
	pollAgain chan struct{}
}

// NewInbox makes the inbox, reading the Slack sign-in kept in dir.
func NewInbox(n *Norn, dir string) *Inbox {
	return &Inbox{
		n: n, Graph: &Graph{}, Slack: NewSlack(), SlackAuth: NewSlackAuth(dir + "/slack.json"), Gmail: NewGmail(), Judge: NewJudge(),
		summaries: map[string]Summary{"work": {}, "personal": {}},
		slack:     slackSweep{state: "pending"},
		quiet:     map[string]bool{},
		pollAgain: make(chan struct{}, 1),
	}
}

func (in *Inbox) setStep(step string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if step == "" {
		in.graphStep = nil
		return
	}
	in.graphStep = &step
}

func (in *Inbox) muted(ctx context.Context) map[string]bool {
	muted := map[string]bool{}
	mutes, _ := in.n.Store.InboxMutes(ctx)
	for _, m := range mutes {
		muted[m.ID] = true
	}
	return muted
}

// rerank ranks what was collected again (a mute or quiet changed), and
// works out what a mute or quiet newly hid, so the panel can say so once.
func (in *Inbox) rerank(ctx context.Context) []Ranked {
	muted := in.muted(ctx)
	in.mu.Lock()
	defer in.mu.Unlock()
	entries := RankAll(Silence(in.items, muted, in.quiet), time.Now(), in.Judge.Judged())
	seen := map[string]bool{}
	fresh := []Ranked{}
	for _, e := range entries {
		if e.Item.Hidden == MuteReason || e.Item.Hidden == QuietReason {
			seen[e.Item.ID] = true
			if in.reportedHidden != nil && !in.reportedHidden[e.Item.ID] {
				fresh = append(fresh, e)
			}
		}
	}
	in.reportedHidden = seen
	in.shown, in.justHidden = visible(entries), fresh
	return in.shown
}

// SetQuiet is the chats Teams has gone quiet about, from the window's toasts.
func (in *Inbox) SetQuiet(ctx context.Context, chats []string) {
	in.mu.Lock()
	in.quiet = map[string]bool{}
	for _, chat := range chats {
		in.quiet[chat] = true
	}
	in.mu.Unlock()
	in.rerank(ctx)
}

// Poll collects once now. A poll asked for while one runs runs once after it.
func (in *Inbox) Poll(ctx context.Context) error {
	if !in.polling.TryLock() {
		select {
		case in.pollAgain <- struct{}{}:
		default:
		}
		return nil
	}
	err := in.pollOnce(ctx)
	in.polling.Unlock()
	select {
	case <-in.pollAgain:
		return in.Poll(ctx)
	default:
	}
	return err
}

// pollDue polls when two minutes have passed, or 20 s after a failure.
func (in *Inbox) pollDue(ctx context.Context) error {
	in.mu.Lock()
	wait := pollEvery
	if !in.lastOK {
		wait = retryEvery
	}
	due := time.Since(in.lastPoll) >= wait
	in.mu.Unlock()
	if !due {
		return nil
	}
	return in.Poll(ctx)
}

func (in *Inbox) pollOnce(ctx context.Context) (err error) {
	n := in.n
	defer func() {
		in.mu.Lock()
		in.lastPoll, in.lastOK = time.Now(), err == nil
		if err != nil {
			in.err = "could not read the inbox: " + err.Error()
		} else {
			in.err = ""
		}
		in.graphStep = nil
		in.mu.Unlock()
	}()
	off := func(source string) bool { return n.hidden(ctx, source) }

	in.setStep("signing in")
	graphToken := ""
	if token, err := n.Accounts.Microsoft.Token(ctx); err == nil {
		graphToken = token.AccessToken
	}
	in.setStep("fetching")
	var items []Item
	var sources []SourceStatus
	notConfigured := func(source string) SourceStatus {
		return SourceStatus{Source: source, Error: "not configured"}
	}
	var gmailItems []Item
	var gmailStatus *SourceStatus
	var wg sync.WaitGroup
	if !off("gmail") && n.Accounts.Google.Configured() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items, status := in.readGmail(ctx)
			gmailItems, gmailStatus = items, &status
		}()
	}
	if graphToken == "" {
		sources = append(sources, SourceStatus{Source: "outlook", Error: "not signed in to Microsoft"}, SourceStatus{Source: "teams", Error: "not signed in to Microsoft"})
	} else {
		status := n.Accounts.Microsoft.Status()
		mail := status.Scopes != nil && status.Scopes.Mail && !off("outlook")
		read, err := in.Graph.Read(ctx, graphToken, mail, !off("teams"))
		switch {
		case err != nil:
			sources = append(sources, failedStatus("outlook", err), failedStatus("teams", err))
		default:
			items = append(append(items, read.Outlook...), read.Teams...)
			switch {
			case !mail:
				sources = append(sources, notConfigured("outlook"))
			case read.MailError != nil:
				sources = append(sources, failedStatus("outlook", read.MailError))
			default:
				sources = append(sources, SourceStatus{Source: "outlook", OK: true, Count: len(read.Outlook)})
			}
			switch {
			case off("teams"):
				sources = append(sources, notConfigured("teams"))
			case read.ChatError != nil:
				sources = append(sources, failedStatus("teams", read.ChatError))
			default:
				sources = append(sources, SourceStatus{Source: "teams", OK: true, Count: len(read.Teams)})
			}
		}
	}
	in.mu.Lock()
	sweep := in.slack
	in.mu.Unlock()
	switch {
	case off("slack") || sweep.state == "off":
		sources = append(sources, notConfigured("slack"))
	case sweep.state == "pending":
		sources = append(sources, SourceStatus{Source: "slack", OK: true})
	case sweep.state == "failed":
		sources = append(sources, failedStatus("slack", sweep.err))
	default:
		items = append(items, sweep.items...)
		sources = append(sources, SourceStatus{Source: "slack", OK: true, Count: len(sweep.items)})
	}
	wg.Wait()
	if gmailStatus != nil {
		items = append(items, gmailItems...)
		sources = append(sources, *gmailStatus)
	}
	var kept []Item
	for _, item := range items {
		if !off(item.Source) {
			kept = append(kept, item)
		}
	}
	var keptSources []SourceStatus
	for _, s := range sources {
		if !off(s.Source) {
			keptSources = append(keptSources, s)
		}
	}
	items, sources = kept, keptSources

	in.setStep("org chart")
	reader := "the reader"
	if graphToken != "" {
		if me, err := in.Graph.Me(ctx, graphToken); err == nil {
			reader = me.DisplayName
			if chart, err := in.Graph.Org(ctx, graphToken); err == nil {
				for i := range items {
					if items[i].Sender != "" {
						items[i].Relation = chart.RelationOf(me.ID, items[i].Sender)
					}
				}
			}
		}
	}

	in.setStep("picking a judge")
	now := time.Now()
	judge := PickJudge(ctx, n.Config.Judge, n.HTTP)
	in.mu.Lock()
	if judge != nil {
		in.judgeLabel = &judge.Label
	} else {
		in.judgeLabel = nil
	}
	in.mu.Unlock()
	if judge != nil {
		var unhidden []Item
		for _, entry := range RankAll(Silence(items, in.muted(ctx), in.quiet), now, in.Judge.Judged()) {
			unhidden = append(unhidden, entry.Item)
		}
		in.Judge.JudgeAll(ctx, *judge, unhidden, reader, now, func(done, total int) {
			in.setStep(fmtProgress("judging", done, total))
		})
	}

	in.mu.Lock()
	in.items, in.sources, in.feedAt = items, sources, now.UnixMilli()
	in.mu.Unlock()
	shown := in.rerank(ctx)
	at := now.UnixMilli()
	in.mu.Lock()
	in.graphAt = &at
	in.mu.Unlock()
	in.setStep("summarising")

	present := map[string]bool{}
	for _, s := range sources {
		if s.OK {
			present[SourceScope[s.Source]] = true
		}
	}
	summaries := map[string]Summary{}
	for _, scope := range Scopes {
		if !present[scope] {
			summaries[scope] = Summary{}
			continue
		}
		var section []Ranked
		for _, entry := range shown {
			if SourceScope[entry.Item.Source] == scope {
				section = append(section, entry)
			}
		}
		pressing := Pressing(section)
		text, fromModel := n.Summarise(ctx, pressing[:min(len(pressing), SummariseTop)], len(section), now, scope)
		summaries[scope] = Summary{Text: &text, FromModel: fromModel}
	}
	in.mu.Lock()
	in.summaries = summaries
	in.mu.Unlock()
	in.setStep("telling what's new")
	n.inboxEvents(ctx, shown, now)
	return nil
}

func fmtProgress(stage string, done, total int) string {
	return fmt.Sprintf("%s %d/%d", stage, done, total)
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

func (in *Inbox) readGmail(ctx context.Context) ([]Item, SourceStatus) {
	google := in.n.Accounts.Google
	if !google.Grants().Gmail {
		if google.Status().State == "signed-in" {
			return nil, SourceStatus{Source: "gmail", Error: "the Google sign-in does not cover Gmail"}
		}
		return nil, SourceStatus{Source: "gmail", Error: "not signed in to Google"}
	}
	token, err := google.Token(ctx)
	if err != nil {
		return nil, SourceStatus{Source: "gmail", Error: "not signed in to Google"}
	}
	items, err := in.Gmail.Items(ctx, token, 25)
	if err != nil {
		return nil, failedStatus("gmail", err)
	}
	return items, SourceStatus{Source: "gmail", OK: true, Count: len(items)}
}

// SweepSlack sweeps Slack once, then polls so the result shows.
func (in *Inbox) SweepSlack(ctx context.Context) error {
	var result slackSweep
	if in.n.hidden(ctx, "slack") {
		in.mu.Lock()
		result = in.slack
		in.mu.Unlock()
	} else if token, err := in.SlackAuth.Token(ctx); err != nil {
		result = slackSweep{state: "off"}
	} else {
		items, err := in.Slack.Sweep(ctx, token, func(progress SweepProgress) {
			in.mu.Lock()
			in.slackProgress = &progress
			in.mu.Unlock()
		})
		if err != nil {
			result = slackSweep{state: "failed", err: err, at: time.Now().UnixMilli()}
		} else {
			result = slackSweep{state: "done", items: items, at: time.Now().UnixMilli()}
		}
	}
	in.mu.Lock()
	in.slack, in.slackProgress = result, nil
	if result.state == "done" {
		at := result.at
		in.slackAt = &at
	}
	in.mu.Unlock()
	return errors.Join(result.err, in.Poll(ctx))
}

// View is GET /v1/inbox.
type View struct {
	Items      []Ranked           `json:"items"`
	JustHidden []Ranked           `json:"justHidden"`
	Summary    *string            `json:"summary"`
	FromModel  bool               `json:"fromModel"`
	Summaries  map[string]Summary `json:"summaries"`
	Sources    []SourceStatus     `json:"sources"`
	At         *int64             `json:"at"`
	Graph      struct {
		Step *string `json:"step"`
		At   *int64  `json:"at"`
	} `json:"graph"`
	Slack struct {
		State    string         `json:"state"`
		Progress *SweepProgress `json:"progress"`
		At       *int64         `json:"at"`
	} `json:"slack"`
	Error  string  `json:"error,omitempty"`
	Judge  *string `json:"judge"`
	Teams  []Item  `json:"teams"`
	Mutes  []Mute  `json:"mutes"`
	SignIn struct {
		Slack  SlackStatus  `json:"slack"`
		Google GoogleStatus `json:"google"`
	} `json:"signIn"`
}

// View is the inbox as the widget shows it.
func (in *Inbox) View(ctx context.Context) View {
	mutes, _ := in.n.Store.InboxMutes(ctx)
	in.mu.Lock()
	defer in.mu.Unlock()
	var view View
	view.Items, view.JustHidden = orEmpty(in.shown), orEmpty(in.justHidden)
	view.Summary, view.FromModel, view.Summaries = in.summaries["work"].Text, in.summaries["work"].FromModel, in.summaries
	view.Sources = append([]SourceStatus{}, in.sources...)
	if in.feedAt != 0 {
		at := in.feedAt
		view.At = &at
	}
	view.Graph.Step, view.Graph.At = in.graphStep, in.graphAt
	view.Slack.State, view.Slack.Progress, view.Slack.At = in.slack.state, in.slackProgress, in.slackAt
	view.Error, view.Judge, view.Mutes = in.err, in.judgeLabel, mutes
	view.Teams = []Item{}
	for _, item := range in.items {
		if item.Source == "teams" {
			view.Teams = append(view.Teams, item)
		}
	}
	view.SignIn.Slack, view.SignIn.Google = in.SlackAuth.Status(), in.n.Accounts.Google.Status()
	return view
}

func orEmpty(list []Ranked) []Ranked {
	if list == nil {
		return []Ranked{}
	}
	return append([]Ranked{}, list...)
}

// Current is the ranked list while fresh; nil when stale.
func (in *Inbox) Current(now time.Time) ([]Ranked, int64, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.feedAt == 0 || now.UnixMilli()-in.feedAt > InboxStale.Milliseconds() {
		return nil, 0, false
	}
	return append([]Ranked{}, in.shown...), in.feedAt, true
}

var inboxWords = regexp.MustCompile(`(?i)\b(inbox|unread|mails?|e-?mails?|messages?|slack|teams|chats?|dms?|waiting on me|wait(s|ing)? on me|wrote|ping(ed)?|mention(ed|s)?|urgent|inkorg(en)?|oläst(a)?|mejl|meddelanden?|väntar på mig|skrev)\b`)

var letters = regexp.MustCompile(`\p{L}+`)

// AsksAboutInbox is whether a question is about the inbox: it uses an inbox
// word, or names a sender.
func AsksAboutInbox(question string, items []Ranked) bool {
	if inboxWords.MatchString(question) {
		return true
	}
	words := map[string]bool{}
	for _, word := range letters.FindAllString(strings.ToLower(question), -1) {
		words[word] = true
	}
	for _, entry := range items {
		for _, part := range letters.FindAllString(strings.ToLower(entry.Item.From), -1) {
			if len([]rune(part)) >= 3 && words[part] {
				return true
			}
		}
	}
	return false
}

// InboxForQuestion is the lines a question about the inbox gets; nil when
// it is not one or there is no fresh inbox.
func (in *Inbox) InboxForQuestion(question string, now time.Time) []string {
	items, _, ok := in.Current(now)
	if !ok || !AsksAboutInbox(question, items) {
		return nil
	}
	var lines []string
	for _, entry := range items[:min(len(items), PromptItems)] {
		lines = append(lines, "["+entry.Band+"] "+strings.TrimPrefix(ItemLine(entry, now), "- "))
	}
	if rest := len(items) - len(lines); rest > 0 {
		lines = append(lines, "(and "+itoa(rest)+" more, less urgent)")
	}
	return lines
}

// --- routes ----------------------------------------------------------------------

func (n *Norn) mountInbox(p *part.Part) {
	in := n.Inbox
	in.SlackAuth.OnChange = func(err error) {
		n.signInAlert("slack", "Slack")(err)
		if err == nil {
			go func() { _ = in.SweepSlack(context.Background()) }()
		}
	}
	n.Accounts.Microsoft.OnChange = chain(n.Accounts.Microsoft.OnChange, func(err error) {
		in.Graph.Forget()
		if err == nil {
			go func() { _ = in.Poll(context.Background()) }()
		}
	})
	n.Accounts.Google.OnChange = chain(n.Accounts.Google.OnChange, func(error) {
		in.Gmail.Reset()
		go func() { _ = in.Poll(context.Background()) }()
	})
	p.Handle("GET /v1/inbox", func(w http.ResponseWriter, r *http.Request) {
		part.JSON(w, http.StatusOK, in.View(r.Context()))
	})
	p.Handle("POST /v1/inbox/refresh", func(w http.ResponseWriter, _ *http.Request) {
		go func() { _ = in.Poll(context.Background()) }()
		part.JSON(w, http.StatusAccepted, map[string]bool{"polling": true})
	})
	p.Handle("POST /v1/inbox/mutes", n.setMute)
	p.Handle("POST /v1/inbox/quiet", n.setQuiet)
	p.Handle("GET /v1/inbox/feed", func(w http.ResponseWriter, _ *http.Request) {
		items, at, ok := in.Current(time.Now())
		if !ok {
			part.JSON(w, http.StatusOK, map[string]any{"items": []any{}, "total": 0, "at": nil})
			return
		}
		part.JSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items), "at": at})
	})
	p.Handle("POST /v1/inbox/summary", n.inboxSummary)
	p.Handle("POST /v1/slack/sign-in", n.slackSignIn)
	p.Handle("POST /v1/slack/sign-out", func(w http.ResponseWriter, _ *http.Request) {
		status := in.SlackAuth.SignOut()
		in.mu.Lock()
		in.slack = slackSweep{state: "off"}
		in.mu.Unlock()
		part.JSON(w, http.StatusOK, status)
	})
	p.Loop("inbox", retryEvery, in.pollDue)
	p.Loop("slack", pollEvery, in.SweepSlack)
}

func chain(first, then func(error)) func(error) {
	return func(err error) {
		if first != nil {
			first(err)
		}
		then(err)
	}
}

func (n *Norn) setMute(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Muted *bool  `json:"muted"`
	}
	if err := part.Decode(r, &body); err != nil || body.ID == "" || len(body.ID) > 300 || body.Muted == nil {
		part.Error(w, http.StatusBadRequest, "`id` (a chat id) and `muted` (true or false) are required")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = body.ID
	}
	if runes := []rune(name); len(runes) > 200 {
		name = string(runes[:200])
	}
	if err := n.Store.SetInboxMute(r.Context(), body.ID, name, *body.Muted); err != nil {
		n.fail(w, err)
		return
	}
	n.Inbox.rerank(r.Context())
	mutes, err := n.Store.InboxMutes(r.Context())
	if err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, mutes)
}

func (n *Norn) setQuiet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Chats []string `json:"chats"`
	}
	if err := part.Decode(r, &body); err != nil || body.Chats == nil || len(body.Chats) > 1000 {
		part.Error(w, http.StatusBadRequest, "`chats` must be a list of chat ids")
		return
	}
	for _, chat := range body.Chats {
		if len(chat) > 300 {
			part.Error(w, http.StatusBadRequest, "`chats` must be a list of chat ids")
			return
		}
	}
	n.Inbox.SetQuiet(r.Context(), body.Chats)
	w.WriteHeader(http.StatusNoContent)
}

func (n *Norn) inboxSummary(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Top   []Ranked `json:"top"`
		Total *int     `json:"total"`
		Now   *int64   `json:"now"`
		Scope string   `json:"scope"`
	}
	if err := part.Decode(r, &body); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	switch {
	case body.Top == nil:
		part.Error(w, http.StatusBadRequest, "`top` must be an array of ranked items")
		return
	case body.Total == nil || *body.Total < 0:
		part.Error(w, http.StatusBadRequest, "`total` must be a non-negative number")
		return
	case body.Scope != "" && body.Scope != "work" && body.Scope != "personal":
		part.Error(w, http.StatusBadRequest, "`scope` must be work or personal when given")
		return
	}
	for i, entry := range body.Top {
		if !strings.Contains(" urgent notable quiet hidden ", " "+entry.Band+" ") || entry.Band == "" {
			part.Error(w, http.StatusBadRequest, "top["+itoa(i)+"].band must be one of urgent, notable, quiet, hidden")
			return
		}
		if entry.Reasons == nil || entry.Item.Source == "" || entry.Item.At == 0 {
			part.Error(w, http.StatusBadRequest, "top["+itoa(i)+"] must have reasons and an item with source, from, subject, preview and at")
			return
		}
	}
	now := time.Now()
	if body.Now != nil {
		now = time.UnixMilli(*body.Now)
	}
	scope := body.Scope
	if scope == "" {
		scope = "work"
	}
	text, fromModel := n.Summarise(r.Context(), body.Top, *body.Total, now, scope)
	part.JSON(w, http.StatusOK, map[string]any{"text": text, "fromModel": fromModel})
}

var slackClientID = regexp.MustCompile(`^[\d.]{5,40}$`)

func (n *Norn) slackSignIn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientID string `json:"clientId"`
	}
	if err := part.Decode(r, &body); err != nil || !slackClientID.MatchString(body.ClientID) {
		part.Error(w, http.StatusBadRequest, "`clientId` must be the Slack app's client id")
		return
	}
	status, err := n.Inbox.SlackAuth.SignIn(body.ClientID)
	if err != nil {
		part.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, status)
}
