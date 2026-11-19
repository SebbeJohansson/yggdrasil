package norn

// Names stay on this machine. Before a question, a reply or a rewrite goes
// to Claude (through Sindri), people's names and email addresses in it are
// swapped for placeholders (Person 1, Person 2…), and the agent's answer has
// them swapped back when Sindri posts it into the conversation. The
// placeholders are kept per conversation, so a follow-up names the same
// person the same way.
//
// Names are found three ways: the people Norn already knows (you, the
// calendars, the inbox), the names the local model finds in the text, and
// email addresses by their shape. Anything known that is still there after
// the swap stops the handover: nothing is sent.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"yggdrasil/internal/llm"
	"yggdrasil/internal/pseudo"
)

const pseudoSchema = `CREATE TABLE IF NOT EXISTS pseudonyms (
	session TEXT PRIMARY KEY,
	at INTEGER NOT NULL,
	map TEXT NOT NULL
);`

// Pseudonyms is a conversation's placeholders; an empty map when it has none.
func (s *Store) Pseudonyms(ctx context.Context, session string) (*pseudo.Map, error) {
	m := &pseudo.Map{}
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT map FROM pseudonyms WHERE session = ?`, session).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal([]byte(raw), m)
}

// PutPseudonyms keeps a conversation's placeholders.
func (s *Store) PutPseudonyms(ctx context.Context, session string, m *pseudo.Map) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO pseudonyms (session, at, map) VALUES (?, ?, ?)
		ON CONFLICT(session) DO UPDATE SET at = excluded.at, map = excluded.map`, session, s.now().UnixMilli(), string(raw))
	return err
}

// ErrNamesLeft is a handover stopped because names were still in it.
var ErrNamesLeft = errors.New("names were still in the text after swapping them out, so nothing was sent to Claude")

// person is someone written as a name, or as an address when there is no name.
func person(written, role, source string) pseudo.Person {
	written = strings.TrimSpace(written)
	if strings.Contains(written, "@") && !strings.Contains(written, " ") {
		return pseudo.Person{Emails: []string{written}, Role: role, Source: source}
	}
	return pseudo.Person{Name: written, Role: role, Source: source}
}

// knownPeople is everyone Norn knows by name right now: you, who is in the
// calendars' meetings, and who wrote or was sent what is unread.
func (n *Norn) knownPeople(ctx context.Context) []pseudo.Person {
	var people []pseudo.Person
	if me := n.selfNames(ctx); len(me) == 2 {
		people = append(people, pseudo.Person{Name: me[0], Emails: []string{me[1]}, Role: "the person you work for, who is asking", Source: "you"})
	}
	now := time.Now()
	for _, c := range []*Calendar{n.Accounts.Work, n.Accounts.Personal} {
		if c == nil {
			continue
		}
		snapshot := c.Current(now)
		if snapshot == nil {
			continue
		}
		for _, event := range snapshot.Events {
			if event.Organizer != "" {
				people = append(people, person(event.Organizer, "", "calendar"))
			}
			for _, attendee := range event.Attendees {
				people = append(people, person(attendee, "", "calendar"))
			}
		}
	}
	if n.Inbox != nil {
		items, _, _ := n.Inbox.Current(now)
		for _, ranked := range items {
			item := ranked.Item
			from := person(item.From, "", "inbox")
			if strings.Contains(item.Sender, "@") {
				from.Emails = append(from.Emails, item.Sender)
			}
			people = append(people, from)
			for _, to := range item.Recipients {
				people = append(people, person(to, "", "inbox"))
			}
		}
	}
	return slices.DeleteFunc(people, func(p pseudo.Person) bool { return p.Name == "" && len(p.Emails) == 0 })
}

// Group is known people with everyone the people list says is one person
// made one: their names and addresses together, and their other full names
// and addresses from the list added, so one person gets one placeholder
// however they are written ("Anna Berg", "Anna B.", anna@x). A first name
// alone is not added: the person's full name already covers it, and a word
// like "Will" would be swapped wherever it is written.
func (b *PeopleBook) Group(known []pseudo.Person) []pseudo.Person {
	var out []pseudo.Person
	at := map[int64]int{}
	for _, k := range known {
		var listed *Listed
		for _, email := range k.Emails {
			if listed = b.Find(email, ""); listed != nil {
				break
			}
		}
		if listed == nil && k.Name != "" {
			listed = b.Find("", k.Name)
		}
		if listed == nil {
			out = append(out, k)
			continue
		}
		i, ok := at[listed.ID]
		if !ok {
			at[listed.ID] = len(out)
			out = append(out, pseudo.Person{Name: k.Name, Role: k.Role, Source: k.Source})
			i = len(out) - 1
			for _, a := range listed.Aliases {
				switch {
				case a.Kind == AliasEmail:
					out[i].Emails = append(out[i].Emails, a.Value)
				case a.Kind == AliasName && len(strings.Fields(a.Value)) > 1:
					out[i].Aliases = append(out[i].Aliases, a.Value)
				}
			}
		}
		g := &out[i]
		if g.Name == "" {
			g.Name = k.Name
		} else if k.Name != "" && !strings.EqualFold(k.Name, g.Name) && !slices.ContainsFunc(g.Aliases, func(a string) bool { return strings.EqualFold(a, k.Name) }) {
			g.Aliases = append(g.Aliases, k.Name)
		}
		g.Aliases = slices.DeleteFunc(g.Aliases, func(a string) bool { return strings.EqualFold(a, g.Name) })
		for _, email := range k.Emails {
			if !slices.ContainsFunc(g.Emails, func(e string) bool { return strings.EqualFold(e, email) }) {
				g.Emails = append(g.Emails, email)
			}
		}
		if g.Role == "" {
			g.Role = k.Role
		}
		if g.Source == "" {
			g.Source = k.Source
		}
	}
	return out
}

// namesChunk is how much text the local model reads for names at a time.
const namesChunk = 4000

var namesFormat = map[string]any{
	"type":     "object",
	"required": []string{"names"},
	"properties": map[string]any{
		"names": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	},
}

const namesSystem = "List every person's name in the text, exactly as it is written there: full names, first names alone, " +
	"surnames alone, nicknames, in any language. Leave out companies, products, apps, places, teams and placeholders like \"Person 3\". " +
	"Answer with JSON: {\"names\": [...]}, an empty list when there are none."

// notPeople are names the local model tends to take for people's.
var notPeople = map[string]bool{
	"claude": true, "norn": true, "sindri": true, "huginn": true, "munin": true, "gjallar": true, "dellingr": true,
	"hermóðr": true, "ratatoskr": true, "copilot": true, "teams": true, "outlook": true, "slack": true, "gmail": true,
	"linear": true, "github": true, "obsidian": true, "pennvass": true, "xestrata": true, "laya": true, "ollama": true,
}

// chunks cuts text into pieces of at most size bytes, at line ends where it can.
func chunks(text string, size int) []string {
	var out []string
	for len(text) > size {
		cut := strings.LastIndex(text[:size], "\n")
		if cut < size/2 {
			cut = size
			for cut > 0 && !utf8RuneStart(text[cut]) {
				cut--
			}
		}
		out = append(out, text[:cut])
		text = text[cut:]
	}
	if strings.TrimSpace(text) != "" {
		out = append(out, text)
	}
	return out
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// NamesTimeout is how long the local model may take over one piece of text.
var NamesTimeout = 90 * time.Second

// askNames is the local model's raw answer for one piece of text. A timeout
// is tried once more: when Windows has paged XeStrata's model out, the first
// try is what brings it back, and reads at a few tokens a second meanwhile.
func (n *Norn) askNames(ctx context.Context, chunk string) (string, error) {
	think := false
	ask := func() (string, error) {
		return n.LLM.Chat(ctx, []llm.Message{{Role: "system", Content: namesSystem}, {Role: "user", Content: chunk}}, llm.Options{
			Purpose: "find names", Model: n.Config.NamesModel, Temperature: 0, TopP: 1, MaxTokens: 600,
			Format: namesFormat, Think: &think, KeepAlive: n.Config.DispatchKeepAlive, Timeout: NamesTimeout,
		})
	}
	raw, err := ask()
	if err != nil && ctx.Err() == nil && FailureKind(err.Error()) == failTimeout {
		n.logf("finding names timed out, trying once more: %v", err)
		raw, err = ask()
	}
	return raw, err
}

// findNames is the people's names the local model finds in text. It fails
// when the model does not answer: then the text is not sent.
func (n *Norn) findNames(ctx context.Context, text string) ([]string, error) {
	var names []string
	for _, chunk := range chunks(text, namesChunk) {
		raw, err := n.askNames(ctx, chunk)
		if err != nil {
			return nil, fmt.Errorf("the local model could not look for names: %w", err)
		}
		var answer struct {
			Names []string `json:"names"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &answer); err != nil {
			return nil, fmt.Errorf("the local model's list of names was not JSON: %w", err)
		}
		for _, name := range answer.Names {
			name = strings.TrimSpace(name)
			if len([]rune(name)) >= 2 && !notPeople[strings.ToLower(name)] && strings.Contains(chunk, name) && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names, nil
}

// pseudonymize is texts as they may go to Claude: names and addresses
// swapped for placeholders that stay the same across the conversation's
// handovers. extra is people the caller knows about beside knownPeople (a
// meeting's invitees, with their addresses). With session empty the
// placeholders are not kept. It fails, and nothing may be sent, when the
// local model cannot look for names or names are still there afterwards.
func (n *Norn) pseudonymize(ctx context.Context, session string, extra []pseudo.Person, texts ...string) ([]string, *pseudo.Map, error) {
	m := &pseudo.Map{}
	if n.NamesOff(ctx) {
		// Switched off for a while (PUT /v1/settings/send-names): sent as they are.
		return texts, m, nil
	}
	if session != "" {
		var err error
		if m, err = n.Store.Pseudonyms(ctx, session); err != nil {
			return nil, nil, err
		}
	}
	known := n.Book(ctx).Group(slices.Concat(extra, n.knownPeople(ctx)))
	out := make([]string, len(texts))
	for i, text := range texts {
		if strings.TrimSpace(text) == "" {
			out[i] = text
			continue
		}
		// A text inside one already read (the question inside its prompt)
		// has its names in the map already.
		var found []string
		if !slices.ContainsFunc(texts[:i], func(earlier string) bool { return strings.Contains(earlier, text) }) {
			var err error
			if found, err = n.findNames(ctx, text); err != nil {
				return nil, nil, err
			}
		}
		out[i] = m.Pseudonymize(text, known, found)
		if leaks := m.Leaks(out[i], known); len(leaks) > 0 {
			n.logf("pseudonymize: %d names left after the swap; not sent", len(leaks))
			return nil, nil, ErrNamesLeft
		}
	}
	if session != "" {
		if err := n.Store.PutPseudonyms(ctx, session, m); err != nil {
			return nil, nil, err
		}
	}
	return out, m, nil
}

// withLegend is a prompt with what its placeholders are added at the end.
func withLegend(prompt string, m *pseudo.Map) string {
	if legend := m.Legend(prompt); legend != "" {
		return prompt + "\n\n" + legend
	}
	return prompt
}

// restoreNames is an agent's words with a conversation's placeholders turned
// back into the names they stand for.
func (n *Norn) restoreNames(ctx context.Context, session, text string) string {
	m, err := n.Store.Pseudonyms(ctx, session)
	if err != nil {
		n.logf("reading a conversation's placeholders: %v", err)
		return text
	}
	return m.Restore(text)
}

// --- the debug view ---------------------------------------------------------------

// placeholders is the entries in text, as the debug view lists them.
func placeholders(m *pseudo.Map, text string) []llm.Placeholder {
	var out []llm.Placeholder
	for _, e := range m.Used(text) {
		out = append(out, llm.Placeholder{Token: e.Token, Name: e.Name, Found: e.Found})
	}
	return out
}

// noteSent puts what was handed to Claude in the chat's debug view: the
// text exactly as sent, and the names swapped out of it. A handover that was
// stopped is noted with why.
func (n *Norn) noteSent(ctx context.Context, purpose, how, sent string, m *pseudo.Map, stopped error) {
	call := llm.Call{At: time.Now().UnixMilli(), Purpose: "to Claude: " + purpose, Backend: "claude", Model: how}
	if stopped != nil {
		call.Error = "not sent: " + stopped.Error()
	} else {
		call.Messages = []llm.Message{{Role: "sent to Claude", Content: sent}}
		call.Placeholders = placeholders(m, sent)
	}
	n.LLM.Note(ctx, call)
}

// noteRestored puts an agent's answer in the debug view of the message it
// answers: as Claude wrote it, and with the names put back.
func (n *Norn) noteRestored(ctx context.Context, session, raw, restored string) {
	if raw == restored {
		return
	}
	m, err := n.Store.Pseudonyms(ctx, session)
	if err != nil {
		return
	}
	call := llm.Call{At: time.Now().UnixMilli(), Session: session, Purpose: "from Claude: names put back", Backend: "claude", Model: "on this machine",
		Messages: []llm.Message{{Role: "Claude wrote", Content: raw}}, Response: restored, Placeholders: placeholders(m, raw)}
	// The debug view files a call under the first message of yours at or
	// after its exchange: the newest one is what the agent answers.
	if chat, err := n.Store.Chat(ctx, session, 0, 10_000); err == nil {
		for i := len(chat) - 1; i >= 0; i-- {
			if chat[i].Role == "user" && chat[i].Author == nil {
				at := chat[i].At
				call.Exchange = at
				break
			}
		}
	}
	n.LLM.Note(ctx, call)
}
