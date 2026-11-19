package norn

// When a promise is due, and when to remind you of it. The local model copies
// the words a message uses for when ("Friday", "i morgon", "by end of
// week"); this turns them into a day in TIME_ZONE, counted from the day the
// message was sent. Words too vague to name a day ("soon", "next week") give
// none: such a promise is listed without a day and never reminded.

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

const dayLayout = "2006-01-02"

var weekdayWords = map[string]time.Weekday{
	"monday": time.Monday, "mon": time.Monday, "måndag": time.Monday, "måndagen": time.Monday,
	"tuesday": time.Tuesday, "tue": time.Tuesday, "tues": time.Tuesday, "tisdag": time.Tuesday, "tisdagen": time.Tuesday,
	"wednesday": time.Wednesday, "wed": time.Wednesday, "onsdag": time.Wednesday, "onsdagen": time.Wednesday,
	"thursday": time.Thursday, "thu": time.Thursday, "thurs": time.Thursday, "torsdag": time.Thursday, "torsdagen": time.Thursday,
	"friday": time.Friday, "fri": time.Friday, "fredag": time.Friday, "fredagen": time.Friday,
	"saturday": time.Saturday, "sat": time.Saturday, "lördag": time.Saturday, "lördagen": time.Saturday,
	"sunday": time.Sunday, "sun": time.Sunday, "söndag": time.Sunday, "söndagen": time.Sunday,
}

var monthWords = map[string]time.Month{
	"jan": 1, "january": 1, "januari": 1, "feb": 2, "february": 2, "februari": 2, "mar": 3, "march": 3, "mars": 3,
	"apr": 4, "april": 4, "may": 5, "maj": 5, "jun": 6, "june": 6, "juni": 6, "jul": 7, "july": 7, "juli": 7,
	"aug": 8, "august": 8, "augusti": 8, "sep": 9, "sept": 9, "september": 9, "oct": 10, "october": 10, "okt": 10, "oktober": 10,
	"nov": 11, "november": 11, "dec": 12, "december": 12,
}

var (
	isoDay      = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`)
	slashDay    = regexp.MustCompile(`\b(\d{1,2})/(\d{1,2})\b`)
	ordinalTail = regexp.MustCompile(`^(\d{1,2})(st|nd|rd|th|e|:e|:a)?$`)
	notLetter   = regexp.MustCompile(`[^\p{L}\p{N}/:-]+`)
)

// phraseIn reports whether `words` (space-padded) holds one of the phrases.
func phraseIn(words string, phrases ...string) bool {
	for _, phrase := range phrases {
		if strings.Contains(words, " "+phrase+" ") {
			return true
		}
	}
	return false
}

func dayOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// fridayOf is the Friday of t's working week; on a weekend, the next one.
func fridayOf(t time.Time) time.Time {
	switch t.Weekday() {
	case time.Saturday:
		return t.AddDate(0, 0, 6)
	case time.Sunday:
		return t.AddDate(0, 0, 5)
	}
	return t.AddDate(0, 0, int(time.Friday-t.Weekday()))
}

// mondayOf is the Monday of t's ISO week.
func mondayOf(t time.Time) time.Time {
	offset := (int(t.Weekday()) + 6) % 7
	return t.AddDate(0, 0, -offset)
}

// ResolveDue is the day `when` names, said on `sent`, as YYYY-MM-DD in
// `zone`; "" when it names none. A weekday is the next one after the day it
// was said ("Friday" said on a Friday is a week later); "next Friday" is the
// Friday of the next week.
func ResolveDue(when string, sent time.Time, zone *time.Location) string {
	text := strings.ToLower(strings.TrimSpace(when))
	if text == "" {
		return ""
	}
	day := dayOf(sent.In(zone))
	format := func(t time.Time) string { return t.Format(dayLayout) }
	if match := isoDay.FindStringSubmatch(text); match != nil {
		if t, err := time.ParseInLocation(dayLayout, match[0], zone); err == nil {
			return format(t)
		}
	}
	words := " " + strings.TrimSpace(notLetter.ReplaceAllString(text, " ")) + " "
	switch {
	case phraseIn(words, "day after tomorrow", "övermorgon", "i övermorgon"):
		return format(day.AddDate(0, 0, 2))
	case phraseIn(words, "tomorrow", "tmrw", "imorgon", "i morgon", "imorn", "i morn"):
		return format(day.AddDate(0, 0, 1))
	case phraseIn(words, "today", "tonight", "eod", "end of day", "end of the day", "idag", "i dag", "ikväll", "i kväll", "innan dagens slut", "senare idag", "later today"):
		return format(day)
	case phraseIn(words, "end of next week", "slutet av nästa vecka"):
		return format(fridayOf(day).AddDate(0, 0, 7))
	case phraseIn(words, "end of week", "end of the week", "this week", "before the weekend", "i veckan", "slutet av veckan", "innan helgen", "under veckan"):
		return format(fridayOf(day))
	}
	tokens := strings.Fields(words)
	for i, token := range tokens {
		weekday, ok := weekdayWords[token]
		if !ok {
			continue
		}
		if i > 0 && (tokens[i-1] == "next" || tokens[i-1] == "nästa") {
			next := mondayOf(day).AddDate(0, 0, 7)
			return format(next.AddDate(0, 0, (int(weekday)+6)%7))
		}
		ahead := (int(weekday) - int(day.Weekday()) + 7) % 7
		if ahead == 0 {
			ahead = 7
		}
		return format(day.AddDate(0, 0, ahead))
	}
	// "24 nov", "24th of November", "Nov 24", "den 24 november".
	for i, token := range tokens {
		month, ok := monthWords[token]
		if !ok {
			continue
		}
		number := ""
		switch {
		case i > 0 && ordinalTail.MatchString(tokens[i-1]):
			number = tokens[i-1]
		case i > 1 && tokens[i-1] == "of" && ordinalTail.MatchString(tokens[i-2]):
			number = tokens[i-2]
		case i+1 < len(tokens) && ordinalTail.MatchString(tokens[i+1]):
			number = tokens[i+1]
		}
		if number == "" {
			continue
		}
		if date := dateFrom(day, month, ordinalTail.FindStringSubmatch(number)[1]); date != "" {
			return date
		}
	}
	// "24/11", day first as written here; "11/24" when that is the only reading.
	if match := slashDay.FindStringSubmatch(text); match != nil {
		first, _ := strconv.Atoi(match[1])
		second, _ := strconv.Atoi(match[2])
		if second >= 1 && second <= 12 {
			if date := dateFrom(day, time.Month(second), match[1]); date != "" {
				return date
			}
		}
		if first >= 1 && first <= 12 {
			return dateFrom(day, time.Month(first), match[2])
		}
	}
	return ""
}

// dateFrom is day `number` of `month` on or after `from`: this year, or next
// year when that day has passed.
func dateFrom(from time.Time, month time.Month, number string) string {
	n, err := strconv.Atoi(number)
	if err != nil || n < 1 || n > 31 {
		return ""
	}
	date := time.Date(from.Year(), month, n, 0, 0, 0, 0, from.Location())
	if date.Month() != month {
		return ""
	}
	if date.Before(from) {
		date = date.AddDate(1, 0, 0)
	}
	return date.Format(dayLayout)
}

// WorkingDayBefore is the last Monday-to-Friday day before `day`.
func WorkingDayBefore(day time.Time) time.Time {
	before := day.AddDate(0, 0, -1)
	for before.Weekday() == time.Saturday || before.Weekday() == time.Sunday {
		before = before.AddDate(0, 0, -1)
	}
	return before
}

const (
	// remindFrom is the hour of the day reminders start going out.
	remindFrom = 8
	// sameDayGrace is how long a promise made on its own due day waits before it is reminded.
	sameDayGrace = 2 * time.Hour
)

// ReminderDue says whether a promise is reminded now, and how its day reads
// in the reminder ("tomorrow", "today", "Monday"). Once per promise: on the
// working day before its day, or on the day itself when it was made too late
// for that. Never before 08:00, never after its day, never for one without a day.
func ReminderDue(p Promise, now time.Time, zone *time.Location) (string, bool) {
	if p.State != "open" || p.Due == "" || p.RemindedAt != 0 {
		return "", false
	}
	due, err := time.ParseInLocation(dayLayout, p.Due, zone)
	if err != nil {
		return "", false
	}
	local := now.In(zone)
	if local.Hour() < remindFrom {
		return "", false
	}
	today := dayOf(local)
	before := WorkingDayBefore(due)
	made := dayOf(time.UnixMilli(p.CreatedAt).In(zone))
	switch {
	case today.After(due) || today.Before(before):
		return "", false
	case today.Equal(due):
		if now.Sub(time.UnixMilli(p.CreatedAt)) < sameDayGrace {
			return "", false
		}
		return "today", true
	case !made.Before(before):
		// Made on the day before or later: the day itself reminds, not right away.
		return "", false
	case today.AddDate(0, 0, 1).Equal(due):
		return "tomorrow", true
	}
	return due.Weekday().String(), true
}
