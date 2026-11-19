// Package pseudo swaps people's names and email addresses for placeholders
// (Person 1, Person 2…) before text leaves the machine, and swaps them back
// in what comes back. The placeholders stay the same for a person across
// calls that share a Map, so a conversation can go on over several turns.
//
// Names are found three ways: people the caller already knows (from the
// calendar, the inbox, the directory), names the caller found some other way
// (a local model), and email addresses by their shape. Leaks is the last
// check before sending: anything known that is still in the text.
package pseudo

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Person is someone whose name must not leave the machine.
type Person struct {
	Name string
	// Aliases are the other names they go by ("Anna B."), from Norn's
	// people list: one placeholder covers them all.
	Aliases []string
	Emails  []string
	// Role is what they are to you, told to the model beside their
	// placeholder: "invited to the meeting", "your manager".
	Role string
	// Source is where they are known from, for the debug view: "calendar", "inbox".
	Source string
}

// Entry is one person's placeholder.
type Entry struct {
	Token string `json:"token"`
	// Name is what the placeholder is turned back into.
	Name string `json:"name"`
	// Variants are the names and addresses that become the placeholder.
	Variants []string `json:"variants"`
	Role     string   `json:"role,omitempty"`
	// Found is how the name was found: where the person is known from, the
	// local model, or the shape of an email address.
	Found string `json:"found,omitempty"`
}

// Map is the placeholders given out so far.
type Map struct {
	Entries []Entry `json:"entries"`
	Next    int     `json:"next"`
}

// tokenPrefix starts every placeholder.
const tokenPrefix = "Person "

var (
	emailShape = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}`)
	tokenShape = regexp.MustCompile(`(?i)\bperson (\d+)`)
	// ordinary are words that are also first names: alone they are left
	// alone, since swapping every "May" or "Per" would garble the text.
	ordinary = map[string]bool{
		"per": true, "max": true, "may": true, "will": true, "mark": true, "bill": true, "rose": true,
		"june": true, "april": true, "august": true, "sol": true, "dag": true, "von": true,
		"van": true, "de": true, "del": true, "la": true, "le": true, "the": true, "and": true,
	}
)

// Variants are the ways a name is written: the whole name in any case, and
// each part of it alone, as written, when it is long enough and not a
// common word.
func Variants(name string) (whole string, parts []string) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || strings.Contains(name, "@") {
		return name, nil
	}
	for _, part := range strings.Fields(name) {
		part = strings.Trim(part, ".,;:()\"'")
		if utf8.RuneCountInString(part) < 3 || ordinary[strings.ToLower(part)] || !startsUpper(part) {
			continue
		}
		if !slices.Contains(parts, part) {
			parts = append(parts, part)
		}
	}
	if len(parts) == 1 && strings.EqualFold(parts[0], name) {
		return name, nil
	}
	return name, parts
}

func startsUpper(word string) bool {
	r, _ := utf8.DecodeRuneInString(word)
	return unicode.IsUpper(r)
}

// entryFor is the entry holding variant, or -1.
func (m *Map) entryFor(variant string) int {
	for i, e := range m.Entries {
		for _, v := range e.Variants {
			if strings.EqualFold(v, variant) {
				return i
			}
		}
	}
	return -1
}

// add gives a person a placeholder, or the one they have, and records every
// way they are written.
func (m *Map) add(p Person) int {
	variants := []string{}
	var wholes []string
	for _, written := range p.names() {
		whole, parts := Variants(written)
		if whole != "" {
			variants = append(variants, whole)
			wholes = append(wholes, whole)
		}
		variants = append(variants, parts...)
	}
	for _, email := range p.Emails {
		if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
			variants = append(variants, email)
		}
	}
	at := -1
	for _, v := range variants {
		// A first name alone does not tie two people together.
		if at = m.entryFor(v); at >= 0 && (slices.Contains(wholes, v) || strings.Contains(v, "@")) {
			break
		}
		at = -1
	}
	if at < 0 {
		m.Next++
		name := ""
		if len(wholes) > 0 {
			name = wholes[0]
		}
		if name == "" && len(p.Emails) > 0 {
			name = p.Emails[0]
		}
		m.Entries = append(m.Entries, Entry{Token: tokenPrefix + strconv.Itoa(m.Next), Name: name, Role: p.Role, Found: p.Source})
		at = len(m.Entries) - 1
	}
	e := &m.Entries[at]
	if e.Role == "" {
		e.Role = p.Role
	}
	if e.Found == "" {
		e.Found = p.Source
	}
	for _, v := range variants {
		if !slices.ContainsFunc(e.Variants, func(have string) bool { return strings.EqualFold(have, v) }) {
			e.Variants = append(e.Variants, v)
		}
	}
	return at
}

// names is every name the person goes by, their own first.
func (p Person) names() []string {
	var out []string
	for _, name := range append([]string{p.Name}, p.Aliases...) {
		if name = strings.TrimSpace(name); name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// isWord is a letter or digit: what a name may not run into.
func isWord(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// span is one name found in the text.
type span struct {
	start, end int
	token      string
}

// find is every place variant is written in text, standing alone (a
// possessive s after it is fine: "Annas", "Anna's").
func find(text, variant string, foldCase bool) [][2]int {
	pattern := regexp.QuoteMeta(variant)
	if foldCase {
		pattern = "(?i)" + pattern
	}
	re := regexp.MustCompile(pattern)
	var found [][2]int
	for offset := 0; offset < len(text); {
		at := re.FindStringIndex(text[offset:])
		if at == nil {
			break
		}
		start, end := offset+at[0], offset+at[1]
		before, _ := utf8.DecodeLastRuneInString(text[:start])
		rest := text[end:]
		for _, suffix := range []string{"'s", "’s", "s"} {
			if strings.HasPrefix(rest, suffix) {
				if after, _ := utf8.DecodeRuneInString(rest[len(suffix):]); !isWord(after) {
					rest = rest[len(suffix):]
					break
				}
			}
		}
		after, _ := utf8.DecodeRuneInString(rest)
		if (start == 0 || !isWord(before)) && (rest == "" || !isWord(after)) && before != '@' && after != '@' {
			found = append(found, [2]int{start, end})
			offset = end
			continue
		}
		_, size := utf8.DecodeRuneInString(text[start:])
		offset = start + size
	}
	return found
}

// Pseudonymize swaps every known person, every name in found, and every
// email address in text for a placeholder, adding to m as it goes.
func (m *Map) Pseudonymize(text string, known []Person, found []string) string {
	for _, email := range emailShape.FindAllString(text, -1) {
		email = strings.ToLower(email)
		if m.entryFor(email) >= 0 {
			continue
		}
		person := Person{Emails: []string{email}, Source: "email address"}
		for _, k := range known {
			if slices.ContainsFunc(k.Emails, func(e string) bool { return strings.EqualFold(e, email) }) {
				person = k
				break
			}
		}
		m.add(person)
	}
	for _, k := range known {
		for _, name := range k.names() {
			whole, parts := Variants(name)
			if len(find(text, whole, true)) > 0 || slices.ContainsFunc(parts, func(p string) bool { return len(find(text, p, false)) > 0 }) {
				m.add(k)
				break
			}
		}
	}
	for _, name := range found {
		if name = strings.TrimSpace(name); name != "" && !tokenShape.MatchString(name) && len(find(text, name, true)) > 0 {
			m.add(Person{Name: name, Source: "local model"})
		}
	}
	return m.swap(text)
}

// swap puts the placeholders in, longest variant first so a full name wins
// over the first name inside it.
func (m *Map) swap(text string) string {
	type variant struct {
		text, token string
		fold        bool
	}
	var all []variant
	for _, e := range m.Entries {
		whole, _ := Variants(e.Name)
		for _, v := range e.Variants {
			fold := strings.EqualFold(v, whole) || strings.Contains(v, "@") || strings.Contains(v, " ")
			all = append(all, variant{v, e.Token, fold})
		}
	}
	slices.SortFunc(all, func(a, b variant) int { return len(b.text) - len(a.text) })
	var spans []span
	taken := func(start, end int) bool {
		return slices.ContainsFunc(spans, func(s span) bool { return start < s.end && s.start < end })
	}
	for _, v := range all {
		for _, at := range find(text, v.text, v.fold) {
			if !taken(at[0], at[1]) {
				spans = append(spans, span{at[0], at[1], v.token})
			}
		}
	}
	slices.SortFunc(spans, func(a, b span) int { return a.start - b.start })
	var out strings.Builder
	last := 0
	for _, s := range spans {
		out.WriteString(text[last:s.start])
		out.WriteString(s.token)
		last = s.end
	}
	out.WriteString(text[last:])
	return out.String()
}

// Restore turns the placeholders in text back into the names they stand for.
// One it never gave out is left as it is.
func (m *Map) Restore(text string) string {
	if m == nil || len(m.Entries) == 0 {
		return text
	}
	byToken := map[string]string{}
	for _, e := range m.Entries {
		byToken[strings.ToLower(e.Token)] = e.Name
	}
	return tokenShape.ReplaceAllStringFunc(text, func(token string) string {
		if name, ok := byToken[strings.ToLower(token)]; ok {
			return name
		}
		return token
	})
}

// Leaks is what in text should not be there: an email address, or the whole
// name or an address of anyone known or given a placeholder, in any case.
func (m *Map) Leaks(text string, known []Person) []string {
	var leaks []string
	note := func(what string) {
		if !slices.Contains(leaks, what) {
			leaks = append(leaks, what)
		}
	}
	for _, email := range emailShape.FindAllString(text, -1) {
		note(email)
	}
	lower := strings.ToLower(text)
	check := func(name string, emails []string) {
		if whole, _ := Variants(name); strings.Contains(whole, " ") && len(find(text, whole, true)) > 0 {
			note(whole)
		}
		for _, email := range emails {
			if email != "" && strings.Contains(lower, strings.ToLower(email)) {
				note(email)
			}
		}
	}
	for _, k := range known {
		check(k.Name, k.Emails)
		for _, alias := range k.Aliases {
			check(alias, nil)
		}
	}
	for _, e := range m.Entries {
		check(e.Name, nil)
	}
	return leaks
}

// Used is the entries whose placeholder is in text.
func (m *Map) Used(text string) []Entry {
	var used []Entry
	for _, e := range m.Entries {
		if tokenIn(text, e.Token) {
			used = append(used, e)
		}
	}
	return used
}

// tokenIn is whether token is in text, not as the start of a longer one
// (Person 1 in Person 12).
func tokenIn(text, token string) bool {
	for offset := 0; ; {
		at := strings.Index(strings.ToLower(text[offset:]), strings.ToLower(token))
		if at < 0 {
			return false
		}
		end := offset + at + len(token)
		if end == len(text) || text[end] < '0' || text[end] > '9' {
			return true
		}
		offset = end
	}
}

// Legend tells the model what the placeholders in text are, and how to treat
// them; empty when there are none.
func (m *Map) Legend(text string) string {
	var lines []string
	for _, e := range m.Entries {
		if !tokenIn(text, e.Token) {
			continue
		}
		line := "- " + e.Token
		if e.Role != "" {
			line += ": " + e.Role
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	return "Names and email addresses in this message are replaced with placeholders (Person 1, Person 2…) to keep them private. " +
		"Write the placeholders exactly as they are when you mean those people; do not guess who they are or ask.\n" + strings.Join(lines, "\n")
}

// String is a short account of the map, for logs: how many people, never who.
func (m *Map) String() string {
	return fmt.Sprintf("%d placeholders", len(m.Entries))
}
