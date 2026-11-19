package norn

// One list of people. Norn sees the same human in many shapes: "Anna Berg
// <anna@x.se>" in mail, a Teams user id and "Anna B." in a chat, "Anna" in a
// meeting note or a promise. The list keeps each person once with every
// alias they are known by (names, email addresses, Teams user ids, Slack
// ids), so the recap, the promise ledger and the name swap before anything
// goes to Claude all treat the aliases of one person as that person.
//
// The list fills itself from what Norn already sees: the calendars' organizers
// and attendees, the inbox's senders and recipients, the members of your Teams
// chats, and the promises' people. Each alias starts as a person of its own,
// unless it is the same account as one already listed: an address and a
// Teams user id Graph gives together, or a name written beside an address.
//
// Norn never merges two people itself. It suggests pairs that look like one
// person, with a reason in words and a confidence, from fixed rules only (no
// model reads a name). You combine them, or say "not the same", which is
// remembered so the pair is not suggested again; a combine can be undone by
// splitting an alias back out. Everything stays on this machine.
//
//	GET  /v1/people                   everyone, with their aliases
//	GET  /v1/people/suggestions       the open merge suggestions
//	POST /v1/people/combine           { keep, merge }: one person (owner)
//	POST /v1/people/apart             { a, b }: not the same (owner)
//	POST /v1/people/{id}/split        { kind, value }: an alias out again (owner)

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"yggdrasil/internal/part"
)

const peopleSchema = `CREATE TABLE IF NOT EXISTS people (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS person_aliases (
	person INTEGER NOT NULL,
	-- name, email, teams or slack.
	kind TEXT NOT NULL,
	-- The alias as compared: lower case, spaces collapsed.
	key TEXT NOT NULL,
	-- The alias as first seen.
	value TEXT NOT NULL,
	-- Where it was seen first: calendar, mail, teams, slack or promise.
	source TEXT NOT NULL,
	at INTEGER NOT NULL,
	PRIMARY KEY (person, kind, key)
);
-- An address or an account id is one person's; a name may be several people's.
CREATE UNIQUE INDEX IF NOT EXISTS person_aliases_one_owner ON person_aliases (kind, key) WHERE kind != 'name';
-- Two aliases Graph gave as one account (kind:key each, a < b).
CREATE TABLE IF NOT EXISTS people_together (a TEXT NOT NULL, b TEXT NOT NULL, PRIMARY KEY (a, b));
-- Two aliases you said are not the same person (kind:key each, a < b).
CREATE TABLE IF NOT EXISTS people_apart (a TEXT NOT NULL, b TEXT NOT NULL, at INTEGER NOT NULL, PRIMARY KEY (a, b));`

const (
	// peopleEvery is how often the list is filled from what Norn sees.
	peopleEvery = 10 * time.Minute
	// MergeConfidence is the least confidence a suggestion is shown with: better few than noisy.
	MergeConfidence = 0.7
	// maxSuggestions is the most suggestions listed at a time.
	maxSuggestions = 50
)

// Alias kinds.
const (
	AliasName  = "name"
	AliasEmail = "email"
	AliasTeams = "teams"
	AliasSlack = "slack"
)

// Alias is one way a person is known.
type Alias struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
	// Source is where it was first seen: calendar, mail, teams, slack or promise.
	Source string `json:"source"`
	At     int64  `json:"at"`
	key    string
}

// ref is the alias as remembered in pairs: kind:key.
func (a Alias) ref() string { return a.Kind + ":" + a.key }

// Listed is one human on the people list, with every alias they are known by.
type Listed struct {
	ID      int64   `json:"id"`
	Name    string  `json:"name"`
	Aliases []Alias `json:"aliases"`
}

// has is whether the person has the alias ref.
func (p Listed) has(ref string) bool {
	return slices.ContainsFunc(p.Aliases, func(a Alias) bool { return a.ref() == ref })
}

// MergeSide is one of a suggestion's two people, and the alias the reason is about.
type MergeSide struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Alias  string `json:"alias"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
}

// MergeSuggestion is two people who look like one.
type MergeSuggestion struct {
	A MergeSide `json:"a"`
	B MergeSide `json:"b"`
	// Rule is which rule found it: same-account, same-name, address-name, short-name or first-name.
	Rule       string  `json:"rule"`
	Reason     string  `json:"reason"`
	Confidence float64 `json:"confidence"`
}

// Observation is one sighting of a person: whatever a source gives at once.
type Observation struct {
	Name, Email, Teams, Slack string
	Source                    string
}

// --- what an alias is -------------------------------------------------------------

var slackLike = regexp.MustCompile(`^[UW][A-Z0-9]{6,}$`)

// IDKind is what kind of id a sender or recipient key is: email, teams or slack; "" for none.
func IDKind(id string) string {
	id = strings.TrimSpace(id)
	switch {
	case strings.Contains(id, "@") && !strings.ContainsAny(id, " <>"):
		return AliasEmail
	case guidLike.MatchString(id):
		return AliasTeams
	case slackLike.MatchString(id):
		return AliasSlack
	}
	return ""
}

func cleanName(name string) string {
	return strings.Join(strings.Fields(strings.Trim(strings.TrimSpace(name), `"'“”‘’<>()`)), " ")
}

// aliasKey is an alias as compared.
func aliasKey(kind, value string) string {
	switch kind {
	case AliasName:
		return strings.ToLower(cleanName(value))
	case AliasSlack:
		return strings.ToUpper(strings.TrimSpace(value))
	}
	return strings.ToLower(strings.TrimSpace(value))
}

// notNames are what a promise's "to" or a recipient list holds that is no one in particular.
var notNames = map[string]bool{
	"team": true, "the team": true, "everyone": true, "all": true, "alla": true, "teamet": true, "gruppen": true,
	"you": true, "me": true, "jag": true, "mig": true, "us": true, "oss": true, "them": true, "dem": true, "myself": true,
	"customer": true, "the customer": true, "kunden": true, "unknown": true,
}

// looksLikeName is whether text can be a person's name: a few words of
// letters, the first a capital, nothing like an address or an id.
func looksLikeName(text string) bool {
	text = cleanName(text)
	if text == "" || utf8.RuneCountInString(text) > 80 || strings.ContainsAny(text, "@/:\\|#") || IDKind(text) != "" || notNames[strings.ToLower(text)] {
		return false
	}
	words := strings.Fields(text)
	if len(words) > 5 {
		return false
	}
	first, _ := utf8.DecodeRuneInString(text)
	if !unicode.IsUpper(first) {
		return false
	}
	letters := 0
	for _, r := range text {
		switch {
		case unicode.IsLetter(r):
			letters++
		case unicode.IsDigit(r):
			return false
		}
	}
	return letters >= 2
}

// nameParts is a name as lower-case words in the order written, initials
// kept: "Anna B." is anna, b.
func nameParts(name string) []string {
	return strings.Fields(nonName.ReplaceAllString(strings.ToLower(name), " "))
}

// fullName is the name's words sorted, when it is a full name (two words or
// more, none an initial): "Anna Berg" and "Berg, Anna" are both "anna berg".
func fullName(name string) string {
	words := nameParts(name)
	if len(words) < 2 {
		return ""
	}
	for _, w := range words {
		if utf8.RuneCountInString(w) < 2 {
			return ""
		}
	}
	sorted := slices.Clone(words)
	slices.Sort(sorted)
	return strings.Join(sorted, " ")
}

// firstName is a full name's first word: "Anna" for "Anna Berg", and for "Berg, Anna".
func firstName(name string) string {
	if before, after, ok := strings.Cut(name, ","); ok && strings.TrimSpace(after) != "" {
		name = after + " " + before
	}
	words := nameParts(name)
	if len(words) == 0 {
		return ""
	}
	return words[0]
}

// shortened is "Anna B." as its first name and the initial: anna, b.
func shortened(name string) (string, string, bool) {
	words := nameParts(name)
	if len(words) != 2 || utf8.RuneCountInString(words[0]) < 2 || utf8.RuneCountInString(words[1]) != 1 {
		return "", "", false
	}
	return words[0], words[1], true
}

// addressName is an address's local part as a full name, when it is one: "anna.berg@" is "anna berg".
func addressName(address string) string {
	local, _, ok := strings.Cut(strings.ToLower(address), "@")
	if !ok {
		return ""
	}
	words := strings.FieldsFunc(local, func(r rune) bool { return r == '.' || r == '_' || r == '-' })
	if len(words) < 2 {
		return ""
	}
	for _, w := range words {
		if utf8.RuneCountInString(w) < 2 || strings.ContainsFunc(w, unicode.IsDigit) {
			return ""
		}
	}
	return fullName(strings.Join(words, " "))
}

// ordinaryFirst are first names that are also ordinary words: alone they say too little.
var ordinaryFirst = map[string]bool{
	"per": true, "max": true, "may": true, "will": true, "mark": true, "bill": true, "rose": true, "june": true,
	"april": true, "august": true, "sol": true, "dag": true,
}

// bestName is what a person is shown as: the name kept while it is still
// theirs (an address or id only while they have no name), else their first
// full name, any name, an address, an id.
func bestName(current string, aliases []Alias) string {
	hasName := slices.ContainsFunc(aliases, func(a Alias) bool { return a.Kind == AliasName })
	for _, a := range aliases {
		if a.Value == current && (a.Kind == AliasName || !hasName) {
			return current
		}
	}
	rank := func(a Alias) int {
		switch {
		case a.Kind == AliasName && fullName(a.Value) != "":
			return 0
		case a.Kind == AliasName:
			return 1
		case a.Kind == AliasEmail:
			return 2
		}
		return 3
	}
	best := -1
	for i, a := range aliases {
		if best < 0 || rank(a) < rank(aliases[best]) {
			best = i
		}
	}
	if best < 0 {
		return current
	}
	return aliases[best].Value
}

// --- the store ---------------------------------------------------------------------

func pairOf(a, b string) [2]string {
	if b < a {
		a, b = b, a
	}
	return [2]string{a, b}
}

// People is everyone on the list, by id.
func (s *Store) People(ctx context.Context) ([]Listed, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id, p.name, a.kind, a.key, a.value, a.source, a.at
		FROM people p JOIN person_aliases a ON a.person = p.id ORDER BY p.id, a.at, a.kind, a.key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Listed{}
	for rows.Next() {
		var id int64
		var name string
		var a Alias
		if err := rows.Scan(&id, &name, &a.Kind, &a.key, &a.Value, &a.Source, &a.At); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != id {
			out = append(out, Listed{ID: id, Name: name, Aliases: []Alias{}})
		}
		out[len(out)-1].Aliases = append(out[len(out)-1].Aliases, a)
	}
	return out, rows.Err()
}

// PersonByID is one person; nil when there is no such one.
func (s *Store) PersonByID(ctx context.Context, id int64) (*Listed, error) {
	people, err := s.People(ctx)
	if err != nil {
		return nil, err
	}
	for i := range people {
		if people[i].ID == id {
			return &people[i], nil
		}
	}
	return nil, nil
}

func (s *Store) pairs(ctx context.Context, table string) (map[[2]string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a, b FROM `+table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]string]bool{}
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		out[pairOf(a, b)] = true
	}
	return out, rows.Err()
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func newPerson(ctx context.Context, tx execer, name string, at int64) (int64, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO people (name, at) VALUES (?, ?)`, name, at)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func addAlias(ctx context.Context, tx execer, person int64, a Alias) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO person_aliases (person, kind, key, value, source, at) VALUES (?, ?, ?, ?, ?, ?)`,
		person, a.Kind, a.key, a.Value, a.Source, a.At)
	return err
}

// aliasesOf is one person's aliases inside a transaction.
func aliasesOf(ctx context.Context, tx execer, person int64) ([]Alias, error) {
	rows, err := tx.QueryContext(ctx, `SELECT kind, key, value, source, at FROM person_aliases WHERE person = ? ORDER BY at, kind, key`, person)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alias
	for rows.Next() {
		var a Alias
		if err := rows.Scan(&a.Kind, &a.key, &a.Value, &a.Source, &a.At); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// rename shows a person by their best name after their aliases changed.
func rename(ctx context.Context, tx execer, person int64) error {
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM people WHERE id = ?`, person).Scan(&current); err != nil {
		return err
	}
	aliases, err := aliasesOf(ctx, tx, person)
	if err != nil {
		return err
	}
	if name := bestName(current, aliases); name != current {
		_, err = tx.ExecContext(ctx, `UPDATE people SET name = ? WHERE id = ?`, name, person)
	}
	return err
}

// apartFrom is whether you said alias is not the same as any of person's aliases.
func apartFrom(ctx context.Context, tx execer, ref string, person int64) (bool, error) {
	aliases, err := aliasesOf(ctx, tx, person)
	if err != nil {
		return false, err
	}
	for _, a := range aliases {
		if a.ref() == ref {
			continue
		}
		pair := pairOf(ref, a.ref())
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM people_apart WHERE a = ? AND b = ?`, pair[0], pair[1]).Scan(&one)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
	}
	return false, nil
}

// aliasesSeen is the aliases one observation gives, its ids first.
func (o Observation) aliases(at int64) (ids []Alias, name *Alias) {
	source := o.Source
	add := func(kind, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		a := Alias{Kind: kind, Value: value, Source: source, At: at, key: aliasKey(kind, value)}
		if !slices.ContainsFunc(ids, func(have Alias) bool { return have.ref() == a.ref() }) {
			ids = append(ids, a)
		}
	}
	add(AliasEmail, strings.ToLower(o.Email))
	add(AliasTeams, strings.ToLower(o.Teams))
	add(AliasSlack, o.Slack)
	written := cleanName(o.Name)
	if kind := IDKind(written); kind != "" {
		if kind == AliasEmail {
			written = strings.ToLower(written)
		}
		add(kind, written)
		written = ""
	}
	if looksLikeName(written) && !slices.ContainsFunc(ids, func(a Alias) bool { return strings.EqualFold(a.Value, written) }) {
		name = &Alias{Kind: AliasName, Value: written, Source: source, At: at, key: aliasKey(AliasName, written)}
	}
	return ids, name
}

// Observe adds what was seen to the list. An alias already listed stays
// where it is; ids seen together that belong to different people are
// remembered as one account and suggested, never merged here.
func (s *Store) Observe(ctx context.Context, seen []Observation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := s.now().UnixMilli()
	for _, o := range seen {
		if err := observe(ctx, tx, o, at); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func observe(ctx context.Context, tx *sql.Tx, o Observation, at int64) error {
	ids, name := o.aliases(at)
	if len(ids) == 0 && name == nil {
		return nil
	}
	for i := range ids {
		for _, b := range ids[i+1:] {
			pair := pairOf(ids[i].ref(), b.ref())
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO people_together (a, b) VALUES (?, ?)`, pair[0], pair[1]); err != nil {
				return err
			}
		}
	}
	if len(ids) == 0 {
		// A name alone: listed once, as whoever has it already or a person of its own.
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM person_aliases WHERE kind = 'name' AND key = ? LIMIT 1`, name.key).Scan(&one)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		id, err := newPerson(ctx, tx, name.Value, at)
		if err != nil {
			return err
		}
		return addAlias(ctx, tx, id, *name)
	}
	var owners []int64
	var unowned []Alias
	for _, a := range ids {
		var owner int64
		err := tx.QueryRowContext(ctx, `SELECT person FROM person_aliases WHERE kind = ? AND key = ?`, a.Kind, a.key).Scan(&owner)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			unowned = append(unowned, a)
		case err != nil:
			return err
		case !slices.Contains(owners, owner):
			owners = append(owners, owner)
		}
	}
	// The new aliases go to the one person the ids already are; with none,
	// or ids of several people (suggested, not merged), to a new person,
	// as does what you split out of the owner before.
	var leftover []Alias
	if len(owners) == 1 {
		owner := owners[0]
		for _, a := range append(unowned, nameAlias(name)...) {
			apart, err := apartFrom(ctx, tx, a.ref(), owner)
			if err != nil {
				return err
			}
			if apart {
				leftover = append(leftover, a)
				continue
			}
			if err := addAlias(ctx, tx, owner, a); err != nil {
				return err
			}
		}
		if err := rename(ctx, tx, owner); err != nil {
			return err
		}
	} else {
		leftover = unowned
		if len(owners) == 0 {
			leftover = append(leftover, nameAlias(name)...)
		}
	}
	if !slices.ContainsFunc(leftover, func(a Alias) bool { return a.Kind != AliasName }) {
		return nil // a name the owner was split from stays with whoever has it
	}
	id, err := newPerson(ctx, tx, leftover[0].Value, at)
	if err != nil {
		return err
	}
	for _, a := range leftover {
		if err := addAlias(ctx, tx, id, a); err != nil {
			return err
		}
	}
	return rename(ctx, tx, id)
}

func nameAlias(name *Alias) []Alias {
	if name == nil {
		return nil
	}
	return []Alias{*name}
}

// ErrNoPerson is a person who is not on the list.
var ErrNoPerson = errors.New("no such person")

// CombinePeople makes merge part of keep: keep has every alias of both, merge is gone.
func (s *Store) CombinePeople(ctx context.Context, keep, merge int64) (*Listed, error) {
	if keep == merge {
		return nil, errors.New("a person is already themselves")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, id := range []int64{keep, merge} {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM people WHERE id = ?`, id).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoPerson
		} else if err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE person_aliases SET person = ? WHERE person = ?`, keep, merge); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM person_aliases WHERE person = ?`, merge); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM people WHERE id = ?`, merge); err != nil {
		return nil, err
	}
	if err := rename(ctx, tx, keep); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.PersonByID(ctx, keep)
}

// remember keeps every pair of a's and b's aliases that are not both's as not the same.
func remember(ctx context.Context, tx execer, a, b []Alias, at int64) error {
	in := func(list []Alias, ref string) bool {
		return slices.ContainsFunc(list, func(x Alias) bool { return x.ref() == ref })
	}
	for _, x := range a {
		for _, y := range b {
			if in(b, x.ref()) || in(a, y.ref()) {
				continue
			}
			pair := pairOf(x.ref(), y.ref())
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO people_apart (a, b, at) VALUES (?, ?, ?)`, pair[0], pair[1], at); err != nil {
				return err
			}
		}
	}
	return nil
}

// KeepApart remembers that two people are not the same, so they are never suggested again.
func (s *Store) KeepApart(ctx context.Context, a, b int64) error {
	if a == b {
		return errors.New("a person is the same as themselves")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	first, err := aliasesOf(ctx, tx, a)
	if err != nil {
		return err
	}
	second, err := aliasesOf(ctx, tx, b)
	if err != nil {
		return err
	}
	if len(first) == 0 || len(second) == 0 {
		return ErrNoPerson
	}
	if err := remember(ctx, tx, first, second, s.now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrLastAlias is a split that would leave a person with no alias.
var ErrLastAlias = errors.New("that is the only alias they have, so there is nothing to split it from")

// SplitAlias moves one alias out of a person into a person of its own, and
// remembers it is not the same as the rest. It returns both.
func (s *Store) SplitAlias(ctx context.Context, person int64, kind, value string) (*Listed, *Listed, error) {
	key := aliasKey(kind, value)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	aliases, err := aliasesOf(ctx, tx, person)
	if err != nil {
		return nil, nil, err
	}
	at := slices.IndexFunc(aliases, func(a Alias) bool { return a.Kind == kind && a.key == key })
	if at < 0 {
		return nil, nil, ErrNoPerson
	}
	if len(aliases) == 1 {
		return nil, nil, ErrLastAlias
	}
	moved := aliases[at]
	id, err := newPerson(ctx, tx, moved.Value, s.now().UnixMilli())
	if err != nil {
		return nil, nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_aliases SET person = ? WHERE person = ? AND kind = ? AND key = ?`, id, person, kind, key); err != nil {
		return nil, nil, err
	}
	rest := slices.Delete(slices.Clone(aliases), at, at+1)
	if err := remember(ctx, tx, []Alias{moved}, rest, s.now().UnixMilli()); err != nil {
		return nil, nil, err
	}
	if err := rename(ctx, tx, person); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	left, err := s.PersonByID(ctx, person)
	if err != nil {
		return nil, nil, err
	}
	split, err := s.PersonByID(ctx, id)
	return left, split, err
}

// --- suggestions -------------------------------------------------------------------

// sourceWords is where an alias was seen, for the card: "mail", "Teams".
var sourceWords = map[string]string{"mail": "mail", "teams": "Teams", "slack": "Slack", "calendar": "the calendar", "promise": "a promise"}

// SuggestMerges is the pairs of people who look like one, best first: each
// pair once, by its strongest reason, none under MergeConfidence and none you
// said are not the same. together is ids Graph gave as one account, apart
// the alias pairs you kept apart (both kind:key).
func SuggestMerges(people []Listed, together, apart map[[2]string]bool) []MergeSuggestion {
	owner := map[string]int{}
	for i, p := range people {
		for _, a := range p.Aliases {
			if a.Kind != AliasName {
				owner[a.ref()] = i
			}
		}
	}
	kept := func(x, y Listed) bool {
		for _, a := range x.Aliases {
			if y.has(a.ref()) {
				continue
			}
			for _, b := range y.Aliases {
				if !x.has(b.ref()) && apart[pairOf(a.ref(), b.ref())] {
					return true
				}
			}
		}
		return false
	}
	best := map[[2]int64]MergeSuggestion{}
	consider := func(x int, xa Alias, y int, ya Alias, rule string, confidence float64, reason string) {
		px, py := people[x], people[y]
		if px.ID == py.ID || confidence < MergeConfidence {
			return
		}
		if py.ID < px.ID {
			px, py, xa, ya = py, px, ya, xa
		}
		key := [2]int64{px.ID, py.ID}
		if have, ok := best[key]; ok && have.Confidence >= confidence {
			return
		}
		if kept(px, py) {
			return
		}
		side := func(p Listed, a Alias) MergeSide {
			return MergeSide{ID: p.ID, Name: p.Name, Alias: a.Value, Kind: a.Kind, Source: a.Source}
		}
		best[key] = MergeSuggestion{A: side(px, xa), B: side(py, ya), Rule: rule, Reason: reason, Confidence: confidence}
	}
	aliasOf := func(i int, ref string) Alias {
		for _, a := range people[i].Aliases {
			if a.ref() == ref {
				return a
			}
		}
		return Alias{}
	}

	// One account: Graph gave the address and the Teams id together.
	for pair := range together {
		x, okX := owner[pair[0]]
		y, okY := owner[pair[1]]
		if okX && okY {
			consider(x, aliasOf(x, pair[0]), y, aliasOf(y, pair[1]), "same-account", 0.99, "Microsoft gives them as one account")
		}
	}

	type named struct {
		person int
		alias  Alias
	}
	byFull := map[string][]named{}
	byFirst := map[string][]named{}
	for i, p := range people {
		for _, a := range p.Aliases {
			if a.Kind != AliasName {
				continue
			}
			if full := fullName(a.Value); full != "" {
				byFull[full] = append(byFull[full], named{i, a})
				first := firstName(a.Value)
				byFirst[first] = append(byFirst[first], named{i, a})
			}
		}
	}
	onePerson := func(list []named) bool {
		return len(list) > 0 && !slices.ContainsFunc(list, func(n named) bool { return n.person != list[0].person })
	}

	// The same full name in two places.
	for _, list := range byFull {
		for i, x := range list {
			for _, y := range list[i+1:] {
				consider(x.person, x.alias, y.person, y.alias, "same-name", 0.9, "the same name in both")
			}
		}
	}
	for i, p := range people {
		for _, a := range p.Aliases {
			switch a.Kind {
			case AliasEmail:
				// An address that spells the name: anna.berg@ and Anna Berg.
				if full := addressName(a.Value); full != "" {
					local, _, _ := strings.Cut(a.Value, "@")
					for _, y := range byFull[full] {
						consider(i, a, y.person, y.alias, "address-name", 0.85, "the address "+local+"@ spells the name "+y.alias.Value)
					}
				}
			case AliasName:
				// A shortened name only one person's full name fits: Anna B. and Anna Berg.
				if first, initial, ok := shortened(a.Value); ok {
					var fits []named
					for _, y := range byFirst[first] {
						words := nameParts(y.alias.Value)
						last := words[len(words)-1]
						if strings.Contains(y.alias.Value, ",") {
							last = words[0]
						}
						if strings.HasPrefix(last, initial) {
							fits = append(fits, y)
						}
					}
					if onePerson(fits) {
						consider(i, a, fits[0].person, fits[0].alias, "short-name", 0.8, a.Value+" looks like "+fits[0].alias.Value+" shortened")
					}
					continue
				}
				// A first name alone that only ever goes with one full name.
				words := nameParts(a.Value)
				if len(words) != 1 || slices.ContainsFunc(p.Aliases, func(b Alias) bool { return b.Kind == AliasName && fullName(b.Value) != "" }) {
					continue
				}
				fits := byFirst[words[0]]
				if !onePerson(fits) {
					continue
				}
				confidence := 0.75
				if utf8.RuneCountInString(words[0]) < 3 || ordinaryFirst[words[0]] {
					confidence = 0.5 // "Max" or "Per" alone could be anything
				}
				consider(i, a, fits[0].person, fits[0].alias, "first-name", confidence, "the only "+a.Value+" you know by full name is "+fits[0].alias.Value)
			}
		}
	}

	out := make([]MergeSuggestion, 0, len(best))
	for _, s := range best {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		if out[i].A.ID != out[j].A.ID {
			return out[i].A.ID < out[j].A.ID
		}
		return out[i].B.ID < out[j].B.ID
	})
	if len(out) > maxSuggestions {
		out = out[:maxSuggestions]
	}
	return out
}

// MergeSuggestions is the open suggestions over the list as it is now.
func (s *Store) MergeSuggestions(ctx context.Context) ([]MergeSuggestion, error) {
	people, err := s.People(ctx)
	if err != nil {
		return nil, err
	}
	together, err := s.pairs(ctx, "people_together")
	if err != nil {
		return nil, err
	}
	apart, err := s.pairs(ctx, "people_apart")
	if err != nil {
		return nil, err
	}
	return SuggestMerges(people, together, apart), nil
}

// SuggestionWords is a suggestion as one sentence, for logs and tests:
// "Anna Berg (mail) and Anna B. (Teams) look like the same person: the same name in both".
func SuggestionWords(s MergeSuggestion) string {
	where := func(side MergeSide) string {
		if words := sourceWords[side.Source]; words != "" {
			return side.Alias + " (" + words + ")"
		}
		return side.Alias
	}
	return where(s.A) + " and " + where(s.B) + " look like the same person: " + s.Reason
}

// --- the book: who is who, for the rest of Norn --------------------------------

// PeopleBook is the list as read once, for looking people up.
type PeopleBook struct {
	people []Listed
	byID   map[string]int
	byName map[string][]int
}

// NewPeopleBook indexes people by their ids and names.
func NewPeopleBook(people []Listed) *PeopleBook {
	b := &PeopleBook{people: people, byID: map[string]int{}, byName: map[string][]int{}}
	for i, p := range people {
		for _, a := range p.Aliases {
			if a.Kind == AliasName {
				if !slices.Contains(b.byName[a.key], i) {
					b.byName[a.key] = append(b.byName[a.key], i)
				}
			} else {
				b.byID[a.key] = i
			}
		}
	}
	return b
}

// Book is the list as it is now; an empty book when it cannot be read.
func (n *Norn) Book(ctx context.Context) *PeopleBook {
	people, err := n.Store.People(ctx)
	if err != nil {
		n.logf("people: %v", err)
	}
	return NewPeopleBook(people)
}

// Find is the person an id is, else the one person a name is; nil when
// unknown or when the name is several people's.
func (b *PeopleBook) Find(id, name string) *Listed {
	if b == nil {
		return nil
	}
	if id = strings.TrimSpace(id); id != "" {
		kind := IDKind(id)
		if kind == "" {
			kind = AliasTeams
		}
		if i, ok := b.byID[aliasKey(kind, id)]; ok {
			return &b.people[i]
		}
	}
	if name = cleanName(name); name != "" {
		if kind := IDKind(name); kind != "" {
			if i, ok := b.byID[aliasKey(kind, name)]; ok {
				return &b.people[i]
			}
			return nil
		}
		if list := b.byName[aliasKey(AliasName, name)]; len(list) == 1 {
			return &b.people[list[0]]
		}
	}
	return nil
}

// Same is whether two sightings are one person on the list.
func (b *PeopleBook) Same(idA, nameA, idB, nameB string) bool {
	a, other := b.Find(idA, nameA), b.Find(idB, nameB)
	return a != nil && other != nil && a.ID == other.ID
}

// --- filling the list --------------------------------------------------------------

// inboxSource is where an inbox item's people are seen, as aliases say it.
func inboxSource(source string) string {
	switch source {
	case "outlook", "gmail":
		return "mail"
	}
	return source
}

// written is a person written as a name, or an address or id when there is no name.
func written(text, source string) Observation {
	text = strings.TrimSpace(text)
	switch IDKind(text) {
	case AliasEmail:
		return Observation{Email: text, Source: source}
	case AliasTeams:
		return Observation{Teams: text, Source: source}
	case AliasSlack:
		return Observation{Slack: text, Source: source}
	}
	return Observation{Name: text, Source: source}
}

// idObservation is a name and an id as one sighting.
func idObservation(name, id, source string) Observation {
	o := Observation{Name: name, Source: source}
	switch IDKind(id) {
	case AliasEmail:
		o.Email = id
	case AliasTeams:
		o.Teams = id
	case AliasSlack:
		o.Slack = id
	}
	return o
}

// PeopleSeen is everyone in what Norn holds now: the calendars' meetings, the
// inbox, Teams chat members and the promises. me is your own names, addresses
// and ids, left out.
func PeopleSeen(events []CalendarEvent, inbox []Ranked, chats []recapChat, promises []Promise, me []string) []Observation {
	var out []Observation
	mine := func(values ...string) bool {
		for _, v := range values {
			if v = strings.TrimSpace(v); v != "" && slices.ContainsFunc(me, func(m string) bool { return strings.EqualFold(m, v) }) {
				return true
			}
		}
		return false
	}
	add := func(o Observation) {
		if !mine(o.Name, o.Email, o.Teams, o.Slack) {
			out = append(out, o)
		}
	}
	for _, e := range events {
		if e.Organizer != "" {
			add(written(e.Organizer, "calendar"))
		}
		for _, a := range e.Attendees {
			add(written(a, "calendar"))
		}
	}
	for _, r := range inbox {
		source := inboxSource(r.Item.Source)
		add(idObservation(r.Item.From, r.Item.Sender, source))
		for _, to := range r.Item.Recipients {
			add(written(to, source))
		}
	}
	for _, chat := range chats {
		for _, m := range chat.Members {
			add(Observation{Name: m.DisplayName, Teams: m.UserID, Email: m.Email, Source: "teams"})
		}
	}
	for _, p := range promises {
		add(idObservation(p.To, p.ToAddress, "promise"))
	}
	return out
}

// fillPeople adds who Norn sees now to the list.
func (n *Norn) fillPeople(ctx context.Context) error {
	now := time.Now()
	var events []CalendarEvent
	for _, c := range []*Calendar{n.Accounts.Work, n.Accounts.Personal} {
		if c == nil {
			continue
		}
		if snapshot := c.Current(now); snapshot != nil {
			events = append(events, snapshot.Events...)
		}
	}
	var inbox []Ranked
	if n.Inbox != nil {
		inbox, _, _ = n.Inbox.Current(now)
	}
	var promises []Promise
	if n.Promises != nil {
		var err error
		if promises, err = n.Promises.List(ctx, "open"); err != nil {
			return err
		}
	}
	var chats []recapChat
	var me []string
	if n.Inbox != nil && n.Accounts.Microsoft.Status().State == "signed-in" {
		if token, err := n.Accounts.Microsoft.Token(ctx); err == nil {
			if self, err := n.Inbox.Graph.Me(ctx, token.AccessToken); err == nil {
				me = append(me, self.ID, self.Address, self.DisplayName)
				if !n.hidden(ctx, "teams") {
					var page struct {
						Value []recapChat `json:"value"`
					}
					if err := graphJSON(ctx, token.AccessToken, chatsPath(50), &page); err == nil {
						chats = page.Value
					} else {
						n.logf("people: no Teams chats: %v", err)
					}
				}
			}
		}
	}
	return n.Store.Observe(ctx, PeopleSeen(events, inbox, chats, promises, me))
}

// --- routes ------------------------------------------------------------------------

func (n *Norn) mountPeople(p *part.Part) {
	p.Handle("GET /v1/people", n.listPeople)
	p.Handle("GET /v1/people/suggestions", n.listMergeSuggestions)
	p.Handle("POST /v1/people/combine", n.combinePeople)
	p.Handle("POST /v1/people/apart", n.keepPeopleApart)
	p.Handle("POST /v1/people/{id}/split", n.splitPerson)
	p.Loop("people", peopleEvery, n.fillPeople)
}

func (n *Norn) listPeople(w http.ResponseWriter, r *http.Request) {
	people, err := n.Store.People(r.Context())
	if err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"people": people})
}

func (n *Norn) listMergeSuggestions(w http.ResponseWriter, r *http.Request) {
	suggestions, err := n.Store.MergeSuggestions(r.Context())
	if err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"suggestions": suggestions})
}

// peopleChanged forgets the recaps kept, so the next one reads the list as it is.
func (n *Norn) peopleChanged() {
	if n.Recaps == nil {
		return
	}
	n.Recaps.mu.Lock()
	clear(n.Recaps.kept)
	n.Recaps.mu.Unlock()
}

func (n *Norn) combinePeople(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only you combine people")
		return
	}
	var body struct {
		Keep  int64 `json:"keep"`
		Merge int64 `json:"merge"`
	}
	if err := part.Decode(r, &body); err != nil || body.Keep == 0 || body.Merge == 0 || body.Keep == body.Merge {
		part.Error(w, http.StatusBadRequest, "send `keep` and `merge`, two different people's ids")
		return
	}
	person, err := n.Store.CombinePeople(r.Context(), body.Keep, body.Merge)
	switch {
	case errors.Is(err, ErrNoPerson):
		part.Error(w, http.StatusNotFound, "no such person")
	case err != nil:
		n.fail(w, err)
	default:
		n.peopleChanged()
		part.JSON(w, http.StatusOK, person)
	}
}

func (n *Norn) keepPeopleApart(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only you say who is not the same")
		return
	}
	var body struct {
		A int64 `json:"a"`
		B int64 `json:"b"`
	}
	if err := part.Decode(r, &body); err != nil || body.A == 0 || body.B == 0 || body.A == body.B {
		part.Error(w, http.StatusBadRequest, "send `a` and `b`, two different people's ids")
		return
	}
	err := n.Store.KeepApart(r.Context(), body.A, body.B)
	switch {
	case errors.Is(err, ErrNoPerson):
		part.Error(w, http.StatusNotFound, "no such person")
	case err != nil:
		n.fail(w, err)
	default:
		part.JSON(w, http.StatusOK, map[string]any{"a": body.A, "b": body.B, "apart": true})
	}
}

func (n *Norn) splitPerson(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only you split a person")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		part.Error(w, http.StatusNotFound, "no such person")
		return
	}
	var body struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	kinds := []string{AliasName, AliasEmail, AliasTeams, AliasSlack}
	if err := part.Decode(r, &body); err != nil || !slices.Contains(kinds, body.Kind) || strings.TrimSpace(body.Value) == "" {
		part.Error(w, http.StatusBadRequest, "send the alias: `kind` (name, email, teams or slack) and `value`")
		return
	}
	left, split, err := n.Store.SplitAlias(r.Context(), id, body.Kind, body.Value)
	switch {
	case errors.Is(err, ErrNoPerson):
		part.Error(w, http.StatusNotFound, "no such person with that alias")
	case errors.Is(err, ErrLastAlias):
		part.Error(w, http.StatusBadRequest, err.Error())
	case err != nil:
		n.fail(w, err)
	default:
		n.peopleChanged()
		part.JSON(w, http.StatusOK, map[string]any{"person": left, "split": split})
	}
}

// String is a short account of a person for logs: never who, only how many aliases.
func (p Listed) String() string { return fmt.Sprintf("person %d (%d aliases)", p.ID, len(p.Aliases)) }
