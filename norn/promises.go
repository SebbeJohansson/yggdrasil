package norn

// The promise ledger: what you said you would do for someone ("I'll send it
// Friday", "jag kollar med juristen"), found in what you send (promises_sent.go)
// and the meeting notes you save, so none is forgotten.
//
// The local model (CHAT_URL) reads each message: your own words never go to
// Claude or Sindri. It returns the promises as JSON, with the words for when;
// promises_due.go turns those into a day in TIME_ZONE. A promise is kept once
// per message (or meeting) and text. On the working day before its day (or on
// the day, when made too late for that) Norn writes a follow-up draft with
// the reply drafter (drafts.go) when the promise was made in a thread, and
// publishes promise.due once, which Huginn turns into a message. When a later
// message of yours to the same person plausibly keeps a promise, the local
// model is asked, conservatively, and the promise is marked done with why;
// you can open it again. Huginn marks a promise that is development work
// (PUT /v1/promises/{id}/dev), and the dashboard then offers to make it a goal.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/llm"
	"yggdrasil/internal/part"
)

const (
	// SentHorizon is how far back sent messages are read: one older than this is never looked at.
	SentHorizon = 3 * 24 * time.Hour
	// PromisePassMessages is how many messages one pass gives the local model, so it is not tied up.
	PromisePassMessages = 4
	promisesEvery       = 2 * time.Minute
	remindEvery         = 10 * time.Minute
	// promisesKept is how long a done or dropped promise stays listed.
	promisesKept = 60 * 24 * time.Hour
	// maxPromisesPerMessage is the most promises taken from one message.
	maxPromisesPerMessage = 5
)

const promiseSchema = `CREATE TABLE IF NOT EXISTS promises (
	id INTEGER PRIMARY KEY,
	source_key TEXT NOT NULL,
	norm TEXT NOT NULL,
	source TEXT NOT NULL,
	what TEXT NOT NULL,
	to_name TEXT NOT NULL DEFAULT '',
	to_address TEXT NOT NULL DEFAULT '',
	due TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT 'open',
	message TEXT NOT NULL DEFAULT '',
	link TEXT NOT NULL DEFAULT '',
	subject TEXT NOT NULL DEFAULT '',
	thread INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	changed_at INTEGER NOT NULL,
	done_why TEXT NOT NULL DEFAULT '',
	done_by TEXT NOT NULL DEFAULT '',
	done_link TEXT NOT NULL DEFAULT '',
	reminded_at INTEGER NOT NULL DEFAULT 0,
	dev INTEGER,
	UNIQUE (source_key, norm)
);
CREATE TABLE IF NOT EXISTS promise_seen (id TEXT PRIMARY KEY, at INTEGER NOT NULL);`

// Promise is one thing you said you would do.
type Promise struct {
	ID   int64  `json:"id"`
	What string `json:"what"`
	// To is who it was promised to, as Norn knows them; ToAddress their address or user id.
	To        string `json:"to"`
	ToAddress string `json:"toAddress,omitempty"`
	// Due is the day, YYYY-MM-DD in TIME_ZONE; "" when no day was said.
	Due   string `json:"due,omitempty"`
	State string `json:"state"`
	// Source is where it was said: outlook, gmail, teams, slack or meeting.
	Source string `json:"source"`
	// Message is the message's id (or the meeting's), Link where it opens.
	Message string `json:"message"`
	Link    string `json:"link,omitempty"`
	Subject string `json:"subject,omitempty"`
	// Thread is whether a reply can go back where it was said; only then is a follow-up drafted.
	Thread    bool  `json:"thread"`
	CreatedAt int64 `json:"createdAt"`
	ChangedAt int64 `json:"changedAt"`
	// DoneBy is "you", or "norn" when a later message of yours kept it; DoneWhy says how.
	DoneBy   string `json:"doneBy,omitempty"`
	DoneWhy  string `json:"doneWhy,omitempty"`
	DoneLink string `json:"doneLink,omitempty"`
	// RemindedAt is when promise.due went out for it (epoch ms); 0 not yet.
	RemindedAt int64 `json:"remindedAt,omitempty"`
	// Dev is Huginn's judgement that it is development work; unset until judged.
	Dev *bool `json:"dev,omitempty"`
	// Key is what it is deduplicated by with its text; Norm that text.
	Key  string `json:"-"`
	Norm string `json:"-"`
}

// PromiseDue is promise.due: a promise whose day is tomorrow (or the next
// working day, or today). What and to are the model's short words, never the message.
type PromiseDue struct {
	ID     int64  `json:"id"`
	What   string `json:"what"`
	To     string `json:"to"`
	Due    string `json:"due"`
	By     string `json:"by"`
	Source string `json:"source"`
	// Draft is whether a follow-up draft waits on the dashboard.
	Draft bool   `json:"draft"`
	Dev   bool   `json:"dev"`
	Link  string `json:"link,omitempty"`
	At    int64  `json:"at"`
}

// Chatter is a model call: the local model, or a fake in tests.
type Chatter func(ctx context.Context, messages []llm.Message, options llm.Options) (string, error)

// Promises is the ledger's running state.
type Promises struct {
	n *Norn
	// Model is the local model the messages are read by.
	Model Chatter
	now   func() time.Time

	mu      sync.Mutex
	offered map[string]SentMessage
	ready   bool
}

// NewPromises makes the ledger over Norn's local model.
func NewPromises(n *Norn) *Promises {
	return &Promises{n: n, now: time.Now, offered: map[string]SentMessage{},
		Model: func(ctx context.Context, messages []llm.Message, options llm.Options) (string, error) {
			return n.LLM.Chat(ctx, messages, options)
		}}
}

// Offer hands over messages the inbox read (Teams, Slack) or notes you saved;
// the next pass reads them. Safe on a nil ledger.
func (p *Promises) Offer(messages ...SentMessage) {
	if p == nil || len(messages) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range messages {
		if len(p.offered) < 500 {
			p.offered[m.ID] = m
		}
	}
}

// OfferNotes hands over meeting notes you saved on the dashboard.
func (n *Norn) OfferNotes(meeting Meeting, text, link string) {
	if strings.TrimSpace(text) == "" || n.Promises == nil {
		return
	}
	n.Promises.Offer(MeetingNotesSent(meeting, text, link, n.Promises.now()))
}

func shortHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:6])
}

func normWhat(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.Trim(strings.TrimSpace(text), ".!")), " "))
}

func (p *Promises) db() *sql.DB { return p.n.Store.db }

func (p *Promises) init(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ready {
		return nil
	}
	if _, err := p.db().ExecContext(ctx, promiseSchema); err != nil {
		return err
	}
	p.ready = true
	return nil
}

// --- extraction --------------------------------------------------------------------

// Extracted is one promise as the model gives it.
type Extracted struct {
	What string `json:"what"`
	To   string `json:"to"`
	When string `json:"when"`
}

var promisesFormat = map[string]any{
	"type":     "object",
	"required": []string{"promises"},
	"properties": map[string]any{
		"promises": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":     "object",
				"required": []string{"what", "to", "when"},
				"properties": map[string]any{
					"what": map[string]any{"type": "string"},
					"to":   map[string]any{"type": "string"},
					"when": map[string]any{"type": "string"},
				},
			},
		},
	},
}

func recipientLine(to []Recipient) string {
	var names []string
	for _, r := range to {
		if r.Address != "" && strings.Contains(r.Address, "@") && r.Address != strings.ToLower(r.Name) {
			names = append(names, r.Name+" <"+r.Address+">")
		} else {
			names = append(names, r.Name)
		}
	}
	return strings.Join(names, ", ")
}

// PromiseMessages asks for the promises in one message you sent, or in your meeting notes.
func PromiseMessages(m SentMessage, owner []string, zone *time.Location) []llm.Message {
	sent := time.UnixMilli(m.At).In(zone)
	var system strings.Builder
	if m.Source == "meeting" {
		system.WriteString("You read the notes the owner took in a meeting and list the promises the owner made in it: things the owner said they themselves will do for someone (\"I'll send the deck\", \"jag kollar med juristen\").\n")
		system.WriteString("Only what the notes say the owner will do: written in the first person (I, me, jag), or naming the owner. Actions for others or for the team are not the owner's promises.\n")
	} else {
		system.WriteString("You read a message the owner sent and list the promises in it: things the owner says they will do for someone (\"I'll send it Friday\", \"I'll check with legal\", \"jag återkommer i morgon\").\n")
		system.WriteString("Only the owner's own words in this message. Quoted earlier messages, what others will do, and requests to others are not promises.\n")
	}
	system.WriteString("Not a promise: thanks, questions, opinions, something already done, or a vague pleasantry (\"let's talk sometime\").\n")
	system.WriteString("what: what the owner will do, as a short imperative in the message's language, at most 100 characters (\"Send the Q3 deck\", \"Kolla med juristen\").\n")
	system.WriteString("to: who it is promised to, as written in the To list when one fits, else as the text names them; empty when unclear.\n")
	system.WriteString("when: the exact words the text uses for when it will be done (\"Friday\", \"tomorrow\", \"by end of week\", \"på fredag\", \"24/11\"); empty when no time is given.\n")
	system.WriteString("No promises: an empty list. Answer with JSON only: {\"promises\":[{\"what\":\"…\",\"to\":\"…\",\"when\":\"…\"}]}")
	var user strings.Builder
	if len(owner) > 0 {
		user.WriteString("The owner: " + strings.Join(owner, ", ") + "\n")
	}
	user.WriteString("Sent: " + sent.Format("Monday 2006-01-02") + "\n")
	if m.Source == "meeting" {
		user.WriteString("Meeting: " + m.Subject + "\n")
	} else {
		user.WriteString("Via: " + m.Source)
		if m.Subject != "" {
			user.WriteString(" — " + m.Subject)
		}
		user.WriteString("\n")
	}
	if line := recipientLine(m.To); line != "" {
		user.WriteString("To: " + line + "\n")
	}
	user.WriteString("\nText:\n" + m.Text)
	return []llm.Message{{Role: "system", Content: system.String()}, {Role: "user", Content: user.String()}}
}

// jsonObject is the outermost {...} of a model's answer.
func jsonObject(raw string) string {
	raw = strings.TrimSpace(raw)
	if from, to := strings.Index(raw, "{"), strings.LastIndex(raw, "}"); from >= 0 && to > from {
		return raw[from : to+1]
	}
	return raw
}

// ParsePromises is the model's answer as promises: blanks and repeats
// dropped, what cut to 160 characters, at most five.
func ParsePromises(raw string) ([]Extracted, error) {
	var answer struct {
		Promises []Extracted `json:"promises"`
	}
	if err := json.Unmarshal([]byte(jsonObject(raw)), &answer); err != nil {
		return nil, err
	}
	out := []Extracted{}
	seen := map[string]bool{}
	for _, e := range answer.Promises {
		e.What = cutText(strings.TrimRight(strings.TrimSpace(e.What), "."), 160)
		e.To, e.When = cutText(e.To, 120), cutText(e.When, 60)
		key := normWhat(e.What)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
		if len(out) == maxPromisesPerMessage {
			break
		}
	}
	return out, nil
}

func firstWord(name string) string {
	fields := strings.Fields(strings.ToLower(name))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// matchRecipient is the recipient `named` means: the whole name or address,
// or a first name only one of them has. With nothing named and one
// recipient, that one.
func matchRecipient(named string, to []Recipient) (Recipient, bool) {
	wanted := strings.ToLower(strings.TrimSpace(named))
	if wanted == "" {
		if len(to) == 1 {
			return to[0], true
		}
		return Recipient{}, false
	}
	for _, r := range to {
		if strings.EqualFold(r.Name, wanted) || (r.Address != "" && strings.EqualFold(r.Address, wanted)) || strings.Contains(wanted, strings.ToLower(r.Name)) {
			return r, true
		}
	}
	var found []Recipient
	for _, r := range to {
		if first := firstWord(r.Name); first != "" && first == firstWord(wanted) {
			found = append(found, r)
		}
	}
	if len(found) == 1 {
		return found[0], true
	}
	return Recipient{}, false
}

// promiseFrom is an extracted promise as kept: the day resolved, the person matched.
func promiseFrom(e Extracted, m SentMessage, zone *time.Location, now time.Time) Promise {
	p := Promise{What: e.What, To: strings.TrimSpace(e.To), Due: ResolveDue(e.When, time.UnixMilli(m.At), zone), State: "open",
		Source: m.Source, Message: m.ID, Link: m.Link, Subject: m.Subject, Thread: m.Thread,
		CreatedAt: now.UnixMilli(), ChangedAt: now.UnixMilli(), Key: m.Key, Norm: normWhat(e.What)}
	if r, ok := matchRecipient(e.To, m.To); ok {
		p.To, p.ToAddress = r.Name, r.Address
	}
	return p
}

// promiseMarkers is what a message that promises something nearly always
// holds, so most messages never reach the model.
var promiseMarkers = []string{
	"i'll", "i’ll", "i will", "i'm going to", "i am going to", "i can ", "let me", "i promise", "will send", "will check", "will get back",
	"will follow", "will share", "will look", "will update", "will fix", "will do", "will have", "will write", "by tomorrow", "by friday", "by monday",
	"jag ska", "ska jag", "jag kan", "jag kollar", "kollar ", "fixar", "skickar", "återkommer", "återkopplar", "hör av mig", "jag tar", "tar jag",
	"jag lovar", "jag ordnar", "ordnar", "jag hör", "jag återkommer", "jag fixar", "jag skickar",
}

// MayPromise is whether a message is worth asking the model about.
func MayPromise(m SentMessage) bool {
	if m.Source == "meeting" {
		return true
	}
	text := strings.ToLower(m.Text)
	for _, marker := range promiseMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// --- done on its own ----------------------------------------------------------------------

var keptFormat = map[string]any{
	"type":     "object",
	"required": []string{"done"},
	"properties": map[string]any{
		"done": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":     "object",
				"required": []string{"id", "why"},
				"properties": map[string]any{
					"id":  map[string]any{"type": "integer"},
					"why": map[string]any{"type": "string"},
				},
			},
		},
	},
}

// KeptMessages asks which open promises a later message of yours keeps.
func KeptMessages(m SentMessage, open []Promise) []llm.Message {
	system := `The owner promised the things listed below. Then they sent the message below to the same person. Which promises does the message itself keep: it delivers the thing (attaches it, sends the answer, says it is done)?
Be strict: when in doubt, it is not kept. Mentioning the topic, promising it again, asking about it, or saying it is late does not keep it.
why: one short sentence on what in the message keeps it, in the message's language.
Answer with JSON only: {"done":[{"id":1,"why":"…"}]}; an empty list when none is kept.`
	var user strings.Builder
	user.WriteString("Promises:\n")
	for _, p := range open {
		user.WriteString("- id " + strconv.FormatInt(p.ID, 10) + ": " + p.What)
		if p.Due != "" {
			user.WriteString(" (by " + p.Due + ")")
		}
		user.WriteString("\n")
	}
	user.WriteString("\nThe later message")
	if m.Subject != "" {
		user.WriteString(" (" + m.Source + " — " + m.Subject + ")")
	}
	user.WriteString(":\n" + m.Text)
	return []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: user.String()}}
}

// Kept is a promise the model says a message keeps.
type Kept struct {
	ID  int64  `json:"id"`
	Why string `json:"why"`
}

// ParseKept is the model's answer, only ids that were asked about.
func ParseKept(raw string, asked []Promise) ([]Kept, error) {
	var answer struct {
		Done []Kept `json:"done"`
	}
	if err := json.Unmarshal([]byte(jsonObject(raw)), &answer); err != nil {
		return nil, err
	}
	out := []Kept{}
	for _, k := range answer.Done {
		known := slices.ContainsFunc(asked, func(p Promise) bool { return p.ID == k.ID })
		if known && !slices.ContainsFunc(out, func(o Kept) bool { return o.ID == k.ID }) {
			k.Why = cutText(k.Why, 300)
			out = append(out, k)
		}
	}
	return out, nil
}

// sameRecipient is whether a message went to the person a promise was made to.
func sameRecipient(p Promise, to []Recipient) bool {
	for _, r := range to {
		if p.ToAddress != "" && strings.EqualFold(p.ToAddress, r.Address) {
			return true
		}
		if p.To != "" && strings.EqualFold(strings.TrimSpace(p.To), strings.TrimSpace(r.Name)) {
			return true
		}
	}
	return false
}

// --- the pass ----------------------------------------------------------------------------

func (p *Promises) seen(ctx context.Context, id string) bool {
	var one int
	return p.db().QueryRowContext(ctx, `SELECT 1 FROM promise_seen WHERE id = ?`, id).Scan(&one) == nil
}

func (p *Promises) markSeen(ctx context.Context, id string) error {
	_, err := p.db().ExecContext(ctx, `INSERT OR IGNORE INTO promise_seen (id, at) VALUES (?, ?)`, id, p.now().UnixMilli())
	return err
}

// gather is every message to read: what was offered, and your sent mail.
// A source that fails is skipped this pass; the others still count.
func (p *Promises) gather(ctx context.Context, since time.Time) ([]SentMessage, error) {
	n := p.n
	p.mu.Lock()
	var messages []SentMessage
	for _, m := range p.offered {
		messages = append(messages, m)
	}
	p.offered = map[string]SentMessage{}
	p.mu.Unlock()
	var failed []error
	if status := n.Accounts.Microsoft.Status(); status.State == "signed-in" && status.Scopes != nil && status.Scopes.Mail && !n.hidden(ctx, "outlook") {
		if token, err := n.Accounts.Microsoft.Token(ctx); err == nil {
			sent, err := readSentMail(ctx, defaultBearerGet, token.AccessToken, since)
			if err != nil {
				failed = append(failed, err)
			}
			messages = append(messages, sent...)
		}
	}
	if google := n.Accounts.Google; google.Configured() && google.Grants().Gmail && !n.hidden(ctx, "gmail") {
		if token, err := google.Token(ctx); err == nil {
			sent, err := readSentGmail(ctx, defaultBearerGet, token, since, func(id string) bool { return p.seen(ctx, id) })
			if err != nil {
				failed = append(failed, err)
			}
			messages = append(messages, sent...)
		}
	}
	return messages, errors.Join(failed...)
}

// pass reads what you sent since the horizon, oldest first, a few at a time.
func (p *Promises) pass(ctx context.Context) error {
	if err := p.init(ctx); err != nil {
		return err
	}
	now := p.now()
	since := now.Add(-SentHorizon)
	messages, gatherErr := p.gather(ctx, since)
	slices.SortStableFunc(messages, func(a, b SentMessage) int { return int(a.At - b.At) })
	read := 0
	stopped := false
	var left []SentMessage
	for _, m := range messages {
		if m.At < since.UnixMilli() || p.seen(ctx, m.ID) || (m.Source != "meeting" && p.n.hidden(ctx, m.Source)) {
			continue
		}
		if stopped || read >= PromisePassMessages {
			left = append(left, m)
			continue
		}
		asked, err := p.Read(ctx, m)
		if err != nil {
			// Not marked seen: the next pass asks again. A model that fails
			// once is not asked about the rest this pass.
			left = append(left, m)
			gatherErr = errors.Join(gatherErr, err)
			stopped = true
			continue
		}
		if asked {
			read++
		}
	}
	// What was offered but not read yet is offered again; mail is read again anyway.
	var again []SentMessage
	for _, m := range left {
		if m.Source == "teams" || m.Source == "slack" || m.Source == "meeting" {
			again = append(again, m)
		}
	}
	p.Offer(again...)
	if _, err := p.db().ExecContext(ctx, `DELETE FROM promise_seen WHERE at < ?`, now.Add(-2*SentHorizon).UnixMilli()); err != nil {
		return err
	}
	if _, err := p.db().ExecContext(ctx, `DELETE FROM promises WHERE state != 'open' AND changed_at < ?`, now.Add(-promisesKept).UnixMilli()); err != nil {
		return err
	}
	return gatherErr
}

// Read reads one message: first whether it keeps an open promise to the same
// person, then the promises it makes. asked is whether the model was called.
func (p *Promises) Read(ctx context.Context, m SentMessage) (bool, error) {
	asked := false
	if m.Source != "meeting" {
		open, err := p.List(ctx, "open")
		if err != nil {
			return false, err
		}
		var candidates []Promise
		for _, promise := range open {
			if promise.Message != m.ID && promise.CreatedAt <= m.At+time.Minute.Milliseconds() && sameRecipient(promise, m.To) {
				candidates = append(candidates, promise)
			}
		}
		if len(candidates) > 0 {
			asked = true
			raw, err := p.Model(ctx, KeptMessages(m, candidates), llm.Options{Purpose: "promise kept", Model: p.n.Config.AskModel,
				Temperature: 0, MaxTokens: 400, Format: keptFormat, Timeout: 90 * time.Second})
			if err != nil {
				return asked, err
			}
			// An answer that is not JSON is set aside, not asked again forever.
			kept, err := ParseKept(raw, candidates)
			if err != nil {
				p.n.logf("promises: the model's answer on %s was not JSON: %v", m.ID, err)
			}
			for _, k := range kept {
				if err := p.markKept(ctx, k, m); err != nil {
					return asked, err
				}
			}
		}
	}
	if MayPromise(m) {
		asked = true
		raw, err := p.Model(ctx, PromiseMessages(m, p.owner(ctx, m), p.n.Zone), llm.Options{Purpose: "promises", Model: p.n.Config.AskModel,
			Temperature: 0, MaxTokens: 600, Format: promisesFormat, Timeout: 90 * time.Second})
		if err != nil {
			return asked, err
		}
		found, err := ParsePromises(raw)
		if err != nil {
			p.n.logf("promises: the model's answer on %s was not JSON: %v", m.ID, err)
		}
		for _, e := range found {
			if _, err := p.keep(ctx, promiseFrom(e, m, p.n.Zone, p.now())); err != nil {
				return asked, err
			}
		}
	}
	return asked, p.markSeen(ctx, m.ID)
}

// owner is your names, for meeting notes, where "I" is not always written.
func (p *Promises) owner(ctx context.Context, m SentMessage) []string {
	if m.Source != "meeting" {
		return nil
	}
	return p.n.selfNames(ctx)
}

// keep stores a promise unless the same message (or meeting) already gave one with the same text.
func (p *Promises) keep(ctx context.Context, promise Promise) (bool, error) {
	result, err := p.db().ExecContext(ctx, `INSERT OR IGNORE INTO promises (source_key, norm, source, what, to_name, to_address, due, state, message, link, subject, thread, created_at, changed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'open', ?, ?, ?, ?, ?, ?)`,
		promise.Key, promise.Norm, promise.Source, promise.What, promise.To, promise.ToAddress, promise.Due, promise.Message, promise.Link,
		promise.Subject, promise.Thread, promise.CreatedAt, promise.ChangedAt)
	if err != nil {
		return false, err
	}
	added, _ := result.RowsAffected()
	return added > 0, nil
}

func (p *Promises) markKept(ctx context.Context, k Kept, m SentMessage) error {
	why := k.Why
	if why == "" {
		why = "A later message of yours seems to keep it."
	}
	_, err := p.db().ExecContext(ctx, `UPDATE promises SET state = 'done', done_by = 'norn', done_why = ?, done_link = ?, changed_at = ? WHERE id = ? AND state = 'open'`,
		why, m.Link, p.now().UnixMilli(), k.ID)
	return err
}

const promiseColumns = `id, source_key, norm, source, what, to_name, to_address, due, state, message, link, subject, thread, created_at, changed_at, done_why, done_by, done_link, reminded_at, dev`

func scanPromise(row interface{ Scan(...any) error }) (Promise, error) {
	var p Promise
	var dev sql.NullBool
	err := row.Scan(&p.ID, &p.Key, &p.Norm, &p.Source, &p.What, &p.To, &p.ToAddress, &p.Due, &p.State, &p.Message, &p.Link, &p.Subject,
		&p.Thread, &p.CreatedAt, &p.ChangedAt, &p.DoneWhy, &p.DoneBy, &p.DoneLink, &p.RemindedAt, &dev)
	if dev.Valid {
		p.Dev = &dev.Bool
	}
	return p, err
}

// List is the promises in one state: open ones by day (none last), then newest; the others newest changed first.
func (p *Promises) List(ctx context.Context, state string) ([]Promise, error) {
	if err := p.init(ctx); err != nil {
		return nil, err
	}
	order := `CASE WHEN due = '' THEN 1 ELSE 0 END, due, created_at DESC, id DESC`
	if state != "open" {
		order = `changed_at DESC, id DESC`
	}
	rows, err := p.db().QueryContext(ctx, `SELECT `+promiseColumns+` FROM promises WHERE state = ? ORDER BY `+order, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Promise{}
	for rows.Next() {
		promise, err := scanPromise(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, promise)
	}
	return out, rows.Err()
}

// Get is one promise.
func (p *Promises) Get(ctx context.Context, id int64) (Promise, error) {
	if err := p.init(ctx); err != nil {
		return Promise{}, err
	}
	return scanPromise(p.db().QueryRowContext(ctx, `SELECT `+promiseColumns+` FROM promises WHERE id = ?`, id))
}

// --- reminders -------------------------------------------------------------------------

func (p *Promises) dashboardLink() string {
	base := p.n.Config.DashboardURL
	if base == "" || base == "off" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/dashboard/inbox#promises"
}

// FollowUpItem is a promise as the reply drafter reads it: no new message,
// a note to the drafter on what was promised, so the draft follows it up.
func FollowUpItem(promise Promise) Item {
	text := "(No new message: a reminder to the owner.) The owner promised " + promise.To + " to: " + promise.What
	if promise.Due != "" {
		text += ", by " + promise.Due
	}
	text += ". Write the owner's message to " + promise.To + " that follows this up: delivering it, or saying where it stands. Put what only the owner knows in [square brackets], like [link] or [status]."
	return Item{ID: "promise:" + strconv.FormatInt(promise.ID, 10), Source: promise.Source, From: promise.To, Subject: promise.Subject, Text: text,
		Link: promise.Link, At: promise.CreatedAt}
}

// FollowUp is the item a follow-up on promise:<id> is drafted from, for
// "Draft a reply" on the dashboard; only for an open promise made in a thread.
func (p *Promises) FollowUp(ctx context.Context, item string) (Item, bool) {
	raw, ok := strings.CutPrefix(item, "promise:")
	if p == nil || !ok {
		return Item{}, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return Item{}, false
	}
	promise, err := p.Get(ctx, id)
	if err != nil || promise.State != "open" || !promise.Thread || promise.To == "" || draftWords[promise.Source] == 0 {
		return Item{}, false
	}
	return FollowUpItem(promise), true
}

// remind publishes promise.due once for each promise whose time has come,
// with a follow-up draft first when it was made in a thread.
func (p *Promises) remind(ctx context.Context) error {
	open, err := p.List(ctx, "open")
	if err != nil {
		return err
	}
	now := p.now()
	for _, promise := range open {
		by, due := ReminderDue(promise, now, p.n.Zone)
		if !due {
			continue
		}
		drafted := false
		if promise.Thread && promise.To != "" && draftWords[promise.Source] > 0 {
			if _, err := p.n.WriteDraft(ctx, FollowUpItem(promise), true); err == nil {
				drafted = true
			} else {
				p.n.logf("promises: no follow-up draft for %d: %v", promise.ID, err)
			}
		}
		// Kept as reminded before it is published, so a crash in between never sends it twice.
		if _, err := p.db().ExecContext(ctx, `UPDATE promises SET reminded_at = ? WHERE id = ?`, now.UnixMilli(), promise.ID); err != nil {
			return err
		}
		p.n.Events.Publish("promise.due", PromiseDue{ID: promise.ID, What: promise.What, To: promise.To, Due: promise.Due, By: by,
			Source: promise.Source, Draft: drafted, Dev: promise.Dev != nil && *promise.Dev, Link: p.dashboardLink(), At: now.UnixMilli()})
	}
	return nil
}

// --- routes ------------------------------------------------------------------------------

func (n *Norn) mountPromises(p *part.Part) {
	p.Handle("GET /v1/promises", n.listPromises)
	p.Handle("PATCH /v1/promises/{id}", n.changePromise)
	p.Handle("PUT /v1/promises/{id}/dev", n.markPromiseDev)
	p.Loop("promises", promisesEvery, n.Promises.pass)
	p.Loop("promise-reminders", remindEvery, n.Promises.remind)
}

var promiseStates = []string{"open", "done", "dropped"}

func (n *Norn) listPromises(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	if !slices.Contains(promiseStates, state) {
		part.Error(w, http.StatusBadRequest, "`state` is open, done or dropped")
		return
	}
	promises, err := n.Promises.List(r.Context(), state)
	if err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"promises": promises})
}

func (n *Norn) promiseFor(w http.ResponseWriter, r *http.Request) (Promise, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		part.Error(w, http.StatusNotFound, "no such promise")
		return Promise{}, false
	}
	promise, err := n.Promises.Get(r.Context(), id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		part.Error(w, http.StatusNotFound, "no such promise")
		return Promise{}, false
	case err != nil:
		n.fail(w, err)
		return Promise{}, false
	}
	return promise, true
}

// changePromise is Done, Drop, open again and Change day, from the dashboard.
func (n *Norn) changePromise(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only the owner changes a promise")
		return
	}
	var body struct {
		State *string `json:"state"`
		Due   *string `json:"due"`
	}
	if err := part.Decode(r, &body); err != nil || (body.State == nil && body.Due == nil) {
		part.Error(w, http.StatusBadRequest, "send `state` (open, done, dropped), `due` (YYYY-MM-DD, or \"\" for none), or both")
		return
	}
	if body.State != nil && !slices.Contains(promiseStates, *body.State) {
		part.Error(w, http.StatusBadRequest, "`state` is open, done or dropped")
		return
	}
	if body.Due != nil && *body.Due != "" {
		if _, err := time.Parse(dayLayout, *body.Due); err != nil {
			part.Error(w, http.StatusBadRequest, "`due` is a day, YYYY-MM-DD, or \"\" for none")
			return
		}
	}
	promise, ok := n.promiseFor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	now := n.Promises.now().UnixMilli()
	if body.State != nil && *body.State != promise.State {
		doneBy := ""
		why, link := "", ""
		if *body.State == "done" {
			doneBy = "you"
		}
		if _, err := n.Store.db.ExecContext(ctx, `UPDATE promises SET state = ?, done_by = ?, done_why = ?, done_link = ?, changed_at = ? WHERE id = ?`,
			*body.State, doneBy, why, link, now, promise.ID); err != nil {
			n.fail(w, err)
			return
		}
	}
	if body.Due != nil && *body.Due != promise.Due {
		// A new day is reminded again.
		if _, err := n.Store.db.ExecContext(ctx, `UPDATE promises SET due = ?, reminded_at = 0, changed_at = ? WHERE id = ?`, *body.Due, now, promise.ID); err != nil {
			n.fail(w, err)
			return
		}
	}
	changed, err := n.Promises.Get(ctx, promise.ID)
	if err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, changed)
}

// markPromiseDev keeps Huginn's judgement that a promise is development work.
func (n *Norn) markPromiseDev(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Dev *bool `json:"dev"`
	}
	if err := part.Decode(r, &body); err != nil || body.Dev == nil {
		part.Error(w, http.StatusBadRequest, "send `dev`, true or false")
		return
	}
	promise, ok := n.promiseFor(w, r)
	if !ok {
		return
	}
	if _, err := n.Store.db.ExecContext(r.Context(), `UPDATE promises SET dev = ? WHERE id = ?`, *body.Dev, promise.ID); err != nil {
		n.fail(w, err)
		return
	}
	changed, err := n.Promises.Get(r.Context(), promise.ID)
	if err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, changed)
}
