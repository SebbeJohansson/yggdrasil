package norn

// Reply drafts for the inbox: Norn writes a reply you can copy into Teams,
// Slack or your mail, and never sends one. A pass every two minutes drafts
// the messages that ask you something and are ranked urgent or notable; the
// widget's "Draft a reply" asks for one on any message. Written by the local
// model (CHAT_URL), from the message, who sent it, what Norn remembers and
// your calendar when the message is about meeting; inbox text never leaves
// for Claude from here (a rewrite of the draft is the widget's, on request).
// The tone check (Laya) says how the draft reads, so a blunt one to your
// manager is flagged before you paste it.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"yggdrasil/internal/llm"
	"yggdrasil/internal/part"
)

const (
	// DraftTextChars is how much of a message a draft is written from.
	DraftTextChars = 2000
	// DraftsPerPass is how many drafts a pass writes at most, so the local model is not tied up.
	DraftsPerPass = 2
	// DraftKeep is how long a draft is kept.
	DraftKeep = 7 * 24 * time.Hour
	// draftMaxAge is the oldest message the pass drafts for by itself.
	draftMaxAge = 48 * time.Hour
	draftEvery  = 2 * time.Minute
)

const draftSchema = `CREATE TABLE IF NOT EXISTS reply_drafts (item TEXT PRIMARY KEY, at INTEGER NOT NULL, body TEXT NOT NULL);`

// Draft is a reply written for one inbox item.
type Draft struct {
	Item   string `json:"item"`
	Source string `json:"source"`
	Text   string `json:"text"`
	// Tone is how the draft reads, by Laya; empty when Laya did not answer.
	Tone string `json:"tone,omitempty"`
	// Auto is true when the pass wrote it, false when you asked for it.
	Auto bool  `json:"auto"`
	At   int64 `json:"at"`
	// Checked is true when a second pass checked every sentence against the
	// message, the calendar and your notes; Dropped is what it took out.
	Checked bool     `json:"checked"`
	Dropped []string `json:"dropped,omitempty"`
}

// asksSomething: a question, or a request, in English or Swedish.
var asksSomething = regexp.MustCompile(`(?i)\?|\b(can|could|would|will) you\b|\bplease\b|\blet me know\b|\bkan du\b|\bskulle du\b|\bvill du\b|\bsnälla\b|\bhör av dig\b|\båterkom\b`)

// WantsDraft is whether the pass drafts for an entry by itself: urgent or
// notable, to you, asking something, and not old.
func WantsDraft(entry Ranked, now time.Time) bool {
	item := entry.Item
	return (entry.Band == "urgent" || entry.Band == "notable") &&
		(item.Direct || item.MentionsMe) &&
		now.UnixMilli()-item.At <= draftMaxAge.Milliseconds() &&
		asksSomething.MatchString(item.Subject+" "+messageText(item))
}

func messageText(item Item) string {
	if item.Text != "" {
		return item.Text
	}
	return item.Preview
}

// draftWords is the most words a draft may have, by where it goes.
var draftWords = map[string]int{"teams": 40, "slack": 40, "outlook": 80, "gmail": 80}

var channelStyle = map[string]string{
	"teams":   "a Teams chat: short and plain, no greeting line or sign-off",
	"slack":   "a Slack message: short and plain, no greeting line or sign-off",
	"outlook": "an email: a short greeting with their first name, then the reply",
	"gmail":   "an email: a short greeting with their first name, then the reply",
}

// DraftMessages is the prompt for one reply.
func DraftMessages(item Item, relation, profile string, memories []Memory, calendar []string) []llm.Message {
	var system strings.Builder
	system.WriteString("You write a reply draft for the owner of this inbox, to paste and send themselves.\n")
	system.WriteString("Write only the reply text: no subject line, no quotes, no notes about the reply.\n")
	system.WriteString("Write in the language of the message (Swedish to Swedish, English to English).\n")
	system.WriteString("Reply as " + channelStyle[item.Source] + ". At most " + itoa(draftWords[item.Source]) + " words: shorter is better.\n")
	system.WriteString("Never add a signature or sign-off: no closing line (Mvh, Vänliga hälsningar, Best regards, Thanks), and not the owner's name at the end. The reply ends with its last sentence.\n")
	system.WriteString("Answer only what the message asks. Add nothing of your own: no new topics, no questions of your own, no requests (\"please confirm\", \"invite me\"), no offers to prepare anything, no \"looking forward to\".\n")
	system.WriteString("State only what the message, the calendar below or the owner's notes below say. Whether the owner can, wants to, has time or agrees is not known unless written below: put it in [square brackets] for them to fill in, like [ja/nej], [tid] or [yes/no].\n")
	system.WriteString("Say nothing about the owner's calendar unless it is listed below.\n")
	if profile != "" {
		system.WriteString("\nAbout the owner:\n" + profile + "\n")
	}
	if len(memories) > 0 {
		system.WriteString("\nWhat the owner has told you before:\n")
		for _, m := range memories {
			system.WriteString("- " + m.Text + "\n")
		}
	}
	if len(calendar) > 0 {
		system.WriteString("\nThe owner's calendar, for proposing times:\n" + strings.Join(calendar, "\n") + "\n")
	}
	return []llm.Message{{Role: "system", Content: system.String()}, {Role: "user", Content: draftFacts(item, relation) + "\n\nWrite the reply."}}
}

// draftFacts is the message as both passes see it: who, where, and what it says.
func draftFacts(item Item, relation string) string {
	var user strings.Builder
	user.WriteString("From: " + item.From)
	if relation != "" {
		user.WriteString(" (the owner's " + relation + ")")
	}
	user.WriteString("\nWhere: " + item.Source)
	if item.Subject != "" {
		user.WriteString(" — " + item.Subject)
	}
	if item.Context != "" {
		user.WriteString("\nEarlier in the thread: " + item.Context)
	}
	text := messageText(item)
	user.WriteString("\nMessage: " + text)
	if item.Text == "" && len([]rune(item.Preview)) >= PreviewChars-1 {
		user.WriteString("\n(Only the start of the message was read.)")
	}
	return user.String()
}

// CheckMessages asks for the draft back with every sentence nothing below
// backs up taken out: the second pass, against what the first one invents.
func CheckMessages(item Item, relation string, memories []Memory, calendar []string, draft string) []llm.Message {
	var system strings.Builder
	system.WriteString("You check a reply draft before its owner sends it. Go through it sentence by sentence.\n")
	system.WriteString("Take out every sentence that states, promises, asks for or offers something the message, the calendar or the owner's notes below do not say. Also take out \"looking forward to\" and \"please confirm\" lines.\n")
	system.WriteString("A sentence with a [bracketed] blank for the owner to fill in stays. A greeting stays.\n")
	system.WriteString("Change nothing else: keep the words of every sentence you keep, in the same order. Return only the checked draft, nothing about what you did.\n")
	if len(memories) > 0 {
		system.WriteString("\nThe owner's notes:\n")
		for _, m := range memories {
			system.WriteString("- " + m.Text + "\n")
		}
	}
	if len(calendar) > 0 {
		system.WriteString("\nThe owner's calendar:\n" + strings.Join(calendar, "\n") + "\n")
	}
	return []llm.Message{{Role: "system", Content: system.String()},
		{Role: "user", Content: draftFacts(item, relation) + "\n\nThe draft:\n" + draft}}
}

// sentencesOf splits a draft into its lines and sentences.
func sentencesOf(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		start := 0
		runes := []rune(line)
		for i, r := range runes {
			if (r == '.' || r == '!' || r == '?') && (i+1 == len(runes) || runes[i+1] == ' ') {
				if s := strings.TrimSpace(string(runes[start : i+1])); s != "" {
					out = append(out, s)
				}
				start = i + 1
			}
		}
		if s := strings.TrimSpace(string(runes[start:])); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func normalised(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

// droppedSentences is what the check took out: the sentences of `before` that are not in `after`.
func droppedSentences(before, after string) []string {
	kept := normalised(after)
	var dropped []string
	for _, sentence := range sentencesOf(before) {
		if !strings.Contains(kept, normalised(sentence)) {
			dropped = append(dropped, sentence)
		}
	}
	return dropped
}

// checkedDraft is the check's answer, if it is one: shorter or the same, not
// empty, and every sentence it kept one of the draft's own. Anything else is
// the check rewriting, not checking, and the draft stays as it was.
func checkedDraft(before, raw string) (string, []string, bool) {
	after := cleanDraft(raw)
	if after == "" || len(strings.Fields(after)) > len(strings.Fields(before)) {
		return before, nil, false
	}
	own := normalised(before)
	for _, sentence := range sentencesOf(after) {
		if !strings.Contains(own, normalised(sentence)) {
			return before, nil, false
		}
	}
	return after, droppedSentences(before, after), true
}

var (
	// signOff is a closing line: what a model ends a reply with although told not to.
	signOff = regexp.MustCompile(`(?i)^(mvh|med vänlig(a)? hälsning(ar)?|vänlig(a)? hälsning(ar)?|hälsningar|hälsning|tack( på förhand)?|tack igen|thanks( again| in advance)?|thank you|best( regards| wishes)?|kind regards|regards|cheers|all the best)[!.,]*$`)
	// nameLine is a line of one to three capitalised words and nothing else: a name signed under a reply.
	nameLine = regexp.MustCompile(`^\p{Lu}[\p{L}'-]*( \p{Lu}[\p{L}'-]*){0,2}$`)
)

// dropSignature takes a sign-off and a signed name off the end of a reply.
func dropSignature(text string) string {
	lines := strings.Split(strings.TrimRight(text, " \n"), "\n")
	for len(lines) > 1 {
		last := strings.TrimSpace(lines[len(lines)-1])
		if last != "" && !signOff.MatchString(last) && !nameLine.MatchString(last) {
			break
		}
		lines = lines[:len(lines)-1]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), " \n,")
}

// cleanDraft takes off what a model puts around a reply despite being told not to.
func cleanDraft(raw string) string {
	text := strings.TrimSpace(raw)
	for _, lead := range []string{"Here is a draft:", "Here's a draft:", "Reply:", "Svar:", "Draft:"} {
		if strings.HasPrefix(strings.ToLower(text), strings.ToLower(lead)) {
			text = strings.TrimSpace(text[len(lead):])
		}
	}
	return dropSignature(strings.Trim(text, "\"“”"))
}

// WriteDraft writes, checks and keeps a reply for `item`.
func (n *Norn) WriteDraft(ctx context.Context, item Item, auto bool) (Draft, error) {
	memories, _ := n.Brain.RecallFor(ctx, item.Subject+" "+messageText(item))
	now := time.Now()
	calendar := n.calendarLines(messageText(item), now)
	raw, err := n.LLM.Chat(ctx, DraftMessages(item, item.Relation, LoadProfile(n.Config.ProfilePath), memories, calendar),
		llm.Options{Purpose: "reply draft", Temperature: 0.2, TopP: 0.9, MaxTokens: 300})
	if err != nil {
		return Draft{}, err
	}
	text := cleanDraft(raw)
	if text == "" {
		return Draft{}, errors.New("the model wrote nothing")
	}
	draft := Draft{Item: item.ID, Source: item.Source, Text: text, Auto: auto, At: now.UnixMilli()}
	// The second pass: what nothing backs up comes out. A failed check leaves the draft unchecked, not lost.
	if checked, err := n.LLM.Chat(ctx, CheckMessages(item, item.Relation, memories, calendar, text),
		llm.Options{Purpose: "reply draft check", Temperature: 0, MaxTokens: 300}); err == nil {
		draft.Text, draft.Dropped, draft.Checked = checkedDraft(text, checked)
		text = draft.Text
	}
	if n.Config.LayaURL != "" {
		if toned, err := Tone(ctx, n.HTTP, n.Config.LayaURL, n.Config.LayaKey, text); err == nil {
			draft.Tone = toned.Tone
		}
	}
	return draft, n.keepDraft(ctx, draft)
}

func (n *Norn) keepDraft(ctx context.Context, draft Draft) error {
	body, err := json.Marshal(draft)
	if err != nil {
		return err
	}
	_, err = n.Store.db.ExecContext(ctx, `INSERT INTO reply_drafts (item, at, body) VALUES (?, ?, ?)
		ON CONFLICT (item) DO UPDATE SET at = excluded.at, body = excluded.body`, draft.Item, draft.At, string(body))
	return err
}

// Drafts is every draft kept, newest first; older than DraftKeep are dropped.
func (n *Norn) Drafts(ctx context.Context) ([]Draft, error) {
	if _, err := n.Store.db.ExecContext(ctx, `DELETE FROM reply_drafts WHERE at < ?`, time.Now().Add(-DraftKeep).UnixMilli()); err != nil {
		return nil, err
	}
	rows, err := n.Store.db.QueryContext(ctx, `SELECT body FROM reply_drafts ORDER BY at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	drafts := []Draft{}
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var d Draft
		if json.Unmarshal([]byte(body), &d) == nil {
			drafts = append(drafts, d)
		}
	}
	return drafts, rows.Err()
}

func (n *Norn) hasDraft(ctx context.Context, item string) bool {
	var one int
	err := n.Store.db.QueryRowContext(ctx, `SELECT 1 FROM reply_drafts WHERE item = ?`, item).Scan(&one)
	return err == nil
}

// draftPass drafts for what the inbox ranks as asking you something, a few at a time.
func (n *Norn) draftPass(ctx context.Context) error {
	if !n.Config.Drafts {
		return nil
	}
	now := time.Now()
	shown, _, ok := n.Inbox.Current(now)
	if !ok {
		return nil
	}
	written := 0
	for _, entry := range shown {
		if written >= DraftsPerPass {
			break
		}
		if !WantsDraft(entry, now) || n.hasDraft(ctx, entry.Item.ID) {
			continue
		}
		draft, err := n.WriteDraft(ctx, n.Inbox.fuller(entry.Item), true)
		if err != nil {
			return err
		}
		n.Events.Publish("draft.ready", DraftReady{
			ID: entry.Item.ID, Source: entry.Item.Source, From: entry.Item.From, Relation: entry.Item.Relation,
			Band: entry.Band, Direct: entry.Item.Direct, MentionsMe: entry.Item.MentionsMe, Tone: draft.Tone,
			Link: entry.Item.Link, At: draft.At,
		})
		written++
	}
	return nil
}

// fuller is the item as the inbox holds it, with the text the ranked copy may lack.
func (in *Inbox) fuller(item Item) Item {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, held := range in.items {
		if held.ID == item.ID {
			if item.Relation != "" && held.Relation == "" {
				held.Relation = item.Relation
			}
			return held
		}
	}
	return item
}

func (n *Norn) listDrafts(w http.ResponseWriter, r *http.Request) {
	drafts, err := n.Drafts(r.Context())
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"drafts": drafts})
}

func (n *Norn) makeDraft(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Item string `json:"item"`
	}
	if err := part.Decode(r, &body); err != nil || strings.TrimSpace(body.Item) == "" {
		part.Error(w, http.StatusBadRequest, "`item` must be an inbox item's id")
		return
	}
	shown, _, _ := n.Inbox.Current(time.Now())
	var found *Item
	for _, entry := range shown {
		if entry.Item.ID == body.Item {
			item := n.Inbox.fuller(entry.Item)
			found = &item
			break
		}
	}
	if found == nil {
		part.Error(w, http.StatusNotFound, "no such item in the inbox now")
		return
	}
	draft, err := n.WriteDraft(r.Context(), *found, false)
	switch {
	case errors.Is(err, llm.ErrNoModel):
		part.Error(w, http.StatusServiceUnavailable, "no model to write with (CHAT_URL)")
	case err != nil:
		part.Error(w, http.StatusBadGateway, err.Error())
	default:
		part.JSON(w, http.StatusOK, draft)
	}
}

func (n *Norn) dropDraft(w http.ResponseWriter, r *http.Request) {
	result, err := n.Store.db.ExecContext(r.Context(), `DELETE FROM reply_drafts WHERE item = ?`, r.PathValue("item"))
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if count, _ := result.RowsAffected(); count == 0 {
		part.Error(w, http.StatusNotFound, "no draft for that item")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (n *Norn) mountDrafts(p *part.Part) {
	p.Handle("GET /v1/drafts", n.listDrafts)
	p.Handle("POST /v1/drafts", n.makeDraft)
	p.Handle("DELETE /v1/drafts/{item}", n.dropDraft)
	p.Loop("drafts", draftEvery, n.draftPass)
}
