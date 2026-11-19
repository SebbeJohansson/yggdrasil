package loki

// Tales: short, calm, playful stories, written from templates (no model).
// The variant is picked from the alert or strike id, so a tale reads the same
// every time the plan is rebuilt.

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"strings"
	"text/template"
	"time"
)

// TaleData is what a tale template reads.
type TaleData struct {
	Part    string // Norn
	Title   string // the alert's title
	Day     string // Tuesday
	Clock   string // 14:05
	Noticed string // "4 minutes", empty when nobody acknowledged it
	Healed  string // "12 minutes"
	// For strikes.
	Trick    string // Norn stops answering /health
	Length   string // 3 minutes
	Watchers string // Ratatoskr after 1 minute and Gjallar after 1 minute
	Missed   string // Gjallar
}

func parse(texts ...string) []*template.Template {
	var list []*template.Template
	for i, text := range texts {
		list = append(list, template.Must(template.New(fmt.Sprint(i)).Parse(text)))
	}
	return list
}

var downTales = parse(
	`{{.Day}} at {{.Clock}}, {{.Part}} went quiet. {{if .Noticed}}You spotted it after {{.Noticed}}, and it{{else}}Nobody had to step in: it{{end}} was answering again {{.Healed}} after it fell silent.`,
	`{{.Part}} took an unplanned nap on {{.Day}} at {{.Clock}}. Ratatoskr shook it; {{if .Noticed}}you looked after {{.Noticed}}, and {{end}}it was up again within {{.Healed}}.`,
	`A small outage: {{.Part}} stopped answering on {{.Day}} at {{.Clock}}. It came back after {{.Healed}}{{if .Noticed}}, and you had seen it after {{.Noticed}}{{end}}. Nothing else went with it.`,
	`On {{.Day}} at {{.Clock}} {{.Part}} slipped out of the room for {{.Healed}}. {{if .Noticed}}You noticed after {{.Noticed}}.{{else}}It came back before anyone asked.{{end}}`,
)

var loopTales = parse(
	`{{.Day}} at {{.Clock}}: {{.Title}}. {{if .Noticed}}You saw it after {{.Noticed}}; {{end}}the loop found its feet again {{.Healed}} later.`,
	`One of {{.Part}}'s chores stumbled on {{.Day}} at {{.Clock}} ({{.Title}}). It picked itself up after {{.Healed}}{{if .Noticed}}, {{.Noticed}} after you had looked{{end}}.`,
	`A hiccup in {{.Part}} on {{.Day}} at {{.Clock}}: {{.Title}}. It sorted itself out within {{.Healed}}.{{if .Noticed}} You had it in view after {{.Noticed}}.{{end}}`,
)

var otherTales = parse(
	`{{.Day}} at {{.Clock}}, {{.Part}} raised a hand: {{.Title}}. {{if .Noticed}}You saw it after {{.Noticed}}, and it{{else}}It{{end}} was settled {{.Healed}} later.`,
	`{{.Part}} said "{{.Title}}" on {{.Day}} at {{.Clock}}. {{.Healed}} later all was calm again{{if .Noticed}}, and you had seen it after {{.Noticed}}{{end}}.`,
	`A note from {{.Part}} on {{.Day}} at {{.Clock}}: {{.Title}}. It lasted {{.Healed}}.{{if .Noticed}} You were on it after {{.Noticed}}.{{end}}`,
)

var caughtTales = parse(
	`Loki tried a trick on {{.Day}} at {{.Clock}}: {{.Trick}}, for {{.Length}}. It didn't get far. {{.Watchers}}{{if .Healed}}, and all was well again {{.Healed}} after the trick ended{{end}}.`,
	`{{.Day}}, {{.Clock}}. Loki whispered to {{.Part}} and it played along: {{.Trick}}. The watchers were not fooled: {{.Watchers}}.{{if .Healed}} Healed {{.Healed}} after.{{end}}`,
	`A quiet test on {{.Day}} at {{.Clock}}: {{.Trick}} for {{.Length}}. Caught. {{.Watchers}}.{{if .Healed}} Back to normal {{.Healed}} after the trick.{{end}}`,
)

var missedTales = parse(
	`Loki tried a trick on {{.Day}} at {{.Clock}}: {{.Trick}}, for {{.Length}}. {{.Missed}} never noticed.{{if .Watchers}} {{.Watchers}}.{{end}} That's a blind spot worth closing; it is on Gjallar now.`,
	`{{.Day}}, {{.Clock}}. {{.Trick}}, for {{.Length}}, and {{.Missed}} looked the other way the whole time.{{if .Watchers}} {{.Watchers}}.{{end}} Self-repair has it now.`,
	`A test on {{.Day}} at {{.Clock}} slipped through: {{.Trick}} for {{.Length}}, unseen by {{.Missed}}.{{if .Watchers}} {{.Watchers}}.{{end}} Better found by Loki than by a real failure.`,
)

var stoppedTales = parse(
	`Loki began a trick on {{.Day}} at {{.Clock}} ({{.Trick}}) and stopped it after {{.Length}}.{{if .Watchers}} By then: {{.Watchers}}.{{end}}`,
	`A trick on {{.Part}} on {{.Day}} at {{.Clock}} was called off after {{.Length}}.{{if .Watchers}} {{.Watchers}}.{{end}} No verdict this time.`,
)

func pick(list []*template.Template, id string, data TaleData) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	var out bytes.Buffer
	if err := list[int(h.Sum32()%uint32(len(list)))].Execute(&out, data); err != nil {
		return data.Title
	}
	return strings.Join(strings.Fields(out.String()), " ")
}

// words is a duration as a person says it: "40 seconds", "3 minutes",
// "1 hour 5 minutes".
func words(d time.Duration) string {
	plural := func(n int, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %ss", n, one)
	}
	switch {
	case d < time.Second:
		return "a moment"
	case d < time.Minute:
		return plural(int(d.Round(time.Second)/time.Second), "second")
	case d < time.Hour:
		return plural(int(d.Round(time.Minute)/time.Minute), "minute")
	case d < 24*time.Hour:
		d = d.Round(time.Minute)
		hours, minutes := int(d/time.Hour), int(d%time.Hour/time.Minute)
		if minutes == 0 {
			return plural(hours, "hour")
		}
		return plural(hours, "hour") + " " + plural(minutes, "minute")
	}
	return plural(int(d.Round(time.Hour)/(24*time.Hour)), "day")
}

// AlertTale is one real alert's story.
func AlertTale(a Alert, opened time.Time, noticed *time.Time, resolved time.Time, zone *time.Location) Tale {
	name := AlertPart(a)
	local := opened.In(zone)
	data := TaleData{Part: Title(name), Title: strings.TrimSuffix(a.Title, "."), Day: local.Weekday().String(), Clock: local.Format("15:04"), Healed: words(resolved.Sub(opened))}
	tale := Tale{ID: fmt.Sprintf("alert-%d-%d", a.ID, opened.UnixMilli()), AlertID: a.ID, Part: name, Title: a.Title, OpenedAt: opened}
	healed := resolved.Sub(opened).Milliseconds()
	tale.HealedAfterMs = &healed
	if noticed != nil {
		ms := noticed.Sub(opened).Milliseconds()
		tale.NoticedAfterMs = &ms
		data.Noticed = words(noticed.Sub(opened))
	}
	list := otherTales
	switch {
	case a.Source == Ratatoskr && strings.HasPrefix(a.Key, "down:"):
		list = downTales
	case a.Source == Ratatoskr:
		list = loopTales
	}
	tale.Text = pick(list, tale.ID, data)
	return tale
}

// StrikeTale is a finished strike's story.
func StrikeTale(s Strike, zone *time.Location) string {
	local := s.StartedAt.In(zone)
	data := TaleData{Part: Title(s.Part), Day: local.Weekday().String(), Clock: local.Format("15:04"),
		Trick: Label(s.Part, s.Trick), Length: words(s.EndsAt.Sub(s.StartedAt))}
	if s.HealedAt != nil {
		data.Healed = words(s.HealedAt.Sub(s.EndsAt))
	}
	var noticed, missed []string
	for _, c := range s.Checks {
		who := Title(c.Watcher)
		if c.NoticedAt != nil {
			noticed = append(noticed, fmt.Sprintf("%s noticed after %s", who, words(c.NoticedAt.Sub(s.StartedAt))))
		} else if !contains(missed, who) {
			missed = append(missed, who)
		}
	}
	data.Watchers = joinAnd(noticed)
	data.Missed = joinAnd(missed)
	switch s.State {
	case Caught:
		return pick(caughtTales, s.ID, data)
	case Missed:
		return pick(missedTales, s.ID, data)
	}
	return pick(stoppedTales, s.ID, data)
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func joinAnd(list []string) string {
	switch len(list) {
	case 0:
		return ""
	case 1:
		return list[0]
	}
	return strings.Join(list[:len(list)-1], ", ") + " and " + list[len(list)-1]
}
