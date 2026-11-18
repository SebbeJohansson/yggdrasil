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
func person(written, role string) pseudo.Person {
	written = strings.TrimSpace(written)
	if strings.Contains(written, "@") && !strings.Contains(written, " ") {
		return pseudo.Person{Emails: []string{written}, Role: role}
	}
	return pseudo.Person{Name: written, Role: role}
}

// knownPeople is everyone Norn knows by name right now: you, who is in the
// calendars' meetings, and who wrote or was sent what is unread.
func (n *Norn) knownPeople(ctx context.Context) []pseudo.Person {
	var people []pseudo.Person
	if me := n.selfNames(ctx); len(me) == 2 {
		people = append(people, pseudo.Person{Name: me[0], Emails: []string{me[1]}, Role: "the person you work for, who is asking"})
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
				people = append(people, person(event.Organizer, ""))
			}
			for _, attendee := range event.Attendees {
				people = append(people, person(attendee, ""))
			}
		}
	}
	if n.Inbox != nil {
		items, _, _ := n.Inbox.Current(now)
		for _, ranked := range items {
			item := ranked.Item
			from := person(item.From, "")
			if strings.Contains(item.Sender, "@") {
				from.Emails = append(from.Emails, item.Sender)
			}
			people = append(people, from)
			for _, to := range item.Recipients {
				people = append(people, person(to, ""))
			}
		}
	}
	return slices.DeleteFunc(people, func(p pseudo.Person) bool { return p.Name == "" && len(p.Emails) == 0 })
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

// findNames is the people's names the local model finds in text. It fails
// when the model does not answer: then the text is not sent.
func (n *Norn) findNames(ctx context.Context, text string) ([]string, error) {
	var names []string
	think := false
	for _, chunk := range chunks(text, namesChunk) {
		raw, err := n.LLM.Chat(ctx, []llm.Message{{Role: "system", Content: namesSystem}, {Role: "user", Content: chunk}}, llm.Options{
			Purpose: "find names", Model: n.Config.NamesModel, Temperature: 0, TopP: 1, MaxTokens: 600,
			Format: namesFormat, Think: &think, KeepAlive: n.Config.DispatchKeepAlive, Timeout: 90 * time.Second,
		})
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
	if session != "" {
		var err error
		if m, err = n.Store.Pseudonyms(ctx, session); err != nil {
			return nil, nil, err
		}
	}
	known := slices.Concat(extra, n.knownPeople(ctx))
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
