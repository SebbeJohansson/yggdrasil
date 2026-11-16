package dellingr

// Five-field cron (minute hour day-of-month month day-of-week), and turning
// words like "every weekday at 8" into it. Words are turned into cron once,
// when a job is saved, and never read again at run time.

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed cron line.
type Schedule struct {
	Minute, Hour, Day, Month, Weekday map[int]bool
	// AnyDay and AnyWeekday follow cron: when both day fields are
	// restricted, either may match.
	AnyDay, AnyWeekday bool
}

var fieldRanges = [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}

// ParseCron reads five fields: *, numbers, a-b ranges, lists and /steps.
// Sunday is 0 or 7.
func ParseCron(line string) (Schedule, error) {
	fields := strings.Fields(line)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("cron needs five fields, not %d", len(fields))
	}
	var sets [5]map[int]bool
	for i, field := range fields {
		low, high := fieldRanges[i][0], fieldRanges[i][1]
		if i == 4 {
			high = 7
		}
		set, err := parseField(field, low, high)
		if err != nil {
			return Schedule{}, fmt.Errorf("cron field %d (%q): %w", i+1, field, err)
		}
		sets[i] = set
	}
	if sets[4][7] {
		sets[4][0] = true
		delete(sets[4], 7)
	}
	return Schedule{Minute: sets[0], Hour: sets[1], Day: sets[2], Month: sets[3], Weekday: sets[4],
		AnyDay: fields[2] == "*", AnyWeekday: fields[4] == "*"}, nil
}

func parseField(field string, low, high int) (map[int]bool, error) {
	set := map[int]bool{}
	for _, part := range strings.Split(field, ",") {
		step := 1
		if base, stepText, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 {
				return nil, errors.New("a step must be a positive number")
			}
			part, step = base, n
		}
		from, to := low, high
		switch {
		case part == "*":
		case strings.Contains(part, "-"):
			a, b, _ := strings.Cut(part, "-")
			var errA, errB error
			from, errA = strconv.Atoi(a)
			to, errB = strconv.Atoi(b)
			if errA != nil || errB != nil {
				return nil, errors.New("not a range")
			}
		default:
			n, err := strconv.Atoi(part)
			if err != nil {
				return nil, errors.New("not a number")
			}
			from, to = n, n
			if step > 1 {
				to = high
			}
		}
		if from < low || to > high || from > to {
			return nil, fmt.Errorf("must be within %d to %d", low, high)
		}
		for n := from; n <= to; n += step {
			set[n] = true
		}
	}
	return set, nil
}

func (s Schedule) dayMatches(t time.Time) bool {
	day, weekday := s.Day[t.Day()], s.Weekday[int(t.Weekday())]
	switch {
	case s.AnyDay && s.AnyWeekday:
		return true
	case s.AnyDay:
		return weekday
	case s.AnyWeekday:
		return day
	}
	return day || weekday
}

// Next is the first time after `after` the schedule fires, in loc; zero when
// none comes within five years (the 31st of February).
func (s Schedule) Next(after time.Time, loc *time.Location) time.Time {
	t := after.In(loc).Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		switch {
		case !s.Month[int(t.Month())]:
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
		case !s.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
		case !s.Hour[t.Hour()]:
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
		case !s.Minute[t.Minute()]:
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

var weekdays = map[string]int{"sunday": 0, "monday": 1, "tuesday": 2, "wednesday": 3, "thursday": 4, "friday": 5, "saturday": 6,
	"sun": 0, "mon": 1, "tue": 2, "tues": 2, "wed": 3, "thu": 4, "thur": 4, "thurs": 4, "fri": 5, "sat": 6}

var weekdayNames = []string{"Sundays", "Mondays", "Tuesdays", "Wednesdays", "Thursdays", "Fridays", "Saturdays"}

var (
	atTime     = regexp.MustCompile(`\bat (\d{1,2})(?:[:.](\d{2}))?\s*(am|pm)?\b`)
	everyN     = regexp.MustCompile(`^every (\d+) (minute|hour)s?$`)
	monthDay   = regexp.MustCompile(`\b(\d{1,2})(?:st|nd|rd|th)\b`)
	cronLike   = regexp.MustCompile(`^[\d*/,\-]+(\s+[\d*/,\-]+){4}$`)
	dayWordsRe = regexp.MustCompile(`[a-z]+`)
)

// Words turns a schedule in words into cron. It understands: every N
// minutes or hours, every hour, every day / weekday / weekend at a time,
// named days at a time ("every monday and friday at 9"), a day of the month
// ("the 1st of each month at 10"), and cron itself.
func Words(text string) (string, error) {
	text = strings.ToLower(strings.TrimSpace(text))
	text = strings.NewReplacer(" o'clock", "", "  ", " ").Replace(text)
	if cronLike.MatchString(text) {
		if _, err := ParseCron(text); err != nil {
			return "", err
		}
		return strings.Join(strings.Fields(text), " "), nil
	}
	if match := everyN.FindStringSubmatch(text); match != nil {
		n, _ := strconv.Atoi(match[1])
		if match[2] == "minute" {
			if n < 1 || n > 59 {
				return "", errors.New("every 1 to 59 minutes")
			}
			return fmt.Sprintf("*/%d * * * *", n), nil
		}
		if n < 1 || n > 23 {
			return "", errors.New("every 1 to 23 hours")
		}
		return fmt.Sprintf("0 */%d * * *", n), nil
	}
	switch text {
	case "every minute":
		return "* * * * *", nil
	case "every hour", "hourly":
		return "0 * * * *", nil
	}
	match := atTime.FindStringSubmatch(text)
	if match == nil {
		return "", errors.New(`say when, e.g. "every weekday at 8", "every monday at 9:30", "the 1st of each month at 10"`)
	}
	hour, _ := strconv.Atoi(match[1])
	minute := 0
	if match[2] != "" {
		minute, _ = strconv.Atoi(match[2])
	}
	switch match[3] {
	case "pm":
		if hour < 12 {
			hour += 12
		}
	case "am":
		if hour == 12 {
			hour = 0
		}
	}
	if hour > 23 || minute > 59 {
		return "", errors.New("that time doesn't exist")
	}
	rest := strings.TrimSpace(atTime.ReplaceAllString(text, ""))
	at := fmt.Sprintf("%d %d", minute, hour)
	switch {
	case strings.Contains(rest, "weekday"):
		return at + " * * 1-5", nil
	case strings.Contains(rest, "weekend"):
		return at + " * * 0,6", nil
	case strings.Contains(rest, "month"):
		day := 1
		if m := monthDay.FindStringSubmatch(rest); m != nil {
			day, _ = strconv.Atoi(m[1])
		}
		if day < 1 || day > 28 {
			return "", errors.New("a day of the month from the 1st to the 28th, so every month has it")
		}
		return fmt.Sprintf("%s %d * *", at, day), nil
	}
	var days []int
	for _, word := range dayWordsRe.FindAllString(rest, -1) {
		if n, ok := weekdays[strings.TrimSuffix(word, "s")]; ok {
			days = append(days, n)
		}
	}
	if len(days) > 0 {
		sort.Ints(days)
		list := make([]string, len(days))
		for i, d := range days {
			list[i] = strconv.Itoa(d)
		}
		return at + " * * " + strings.Join(list, ","), nil
	}
	if rest == "" || strings.Contains(rest, "day") || strings.Contains(rest, "daily") || strings.Contains(rest, "night") || strings.Contains(rest, "morning") {
		return at + " * * *", nil
	}
	return "", fmt.Errorf("could not tell which days from %q", rest)
}

// Said is a cron line in plain words, for showing back.
func Said(line string) string {
	fields := strings.Fields(line)
	if len(fields) != 5 {
		return "cron " + line
	}
	minute, hour, day, month, weekday := fields[0], fields[1], fields[2], fields[3], fields[4]
	if month != "*" {
		return "cron " + line
	}
	if step, ok := strings.CutPrefix(minute, "*/"); ok && hour == "*" && day == "*" && weekday == "*" {
		return "every " + step + " minutes"
	}
	m, errM := strconv.Atoi(minute)
	if minute == "*" && hour == "*" && day == "*" && weekday == "*" {
		return "every minute"
	}
	if errM == nil && hour == "*" && day == "*" && weekday == "*" {
		return fmt.Sprintf("every hour at :%02d", m)
	}
	if step, ok := strings.CutPrefix(hour, "*/"); ok && errM == nil && day == "*" && weekday == "*" {
		return fmt.Sprintf("every %s hours at :%02d", step, m)
	}
	h, errH := strconv.Atoi(hour)
	if errM != nil || errH != nil {
		return "cron " + line
	}
	at := fmt.Sprintf("at %02d:%02d", h, m)
	switch {
	case day == "*" && weekday == "*":
		return "every day " + at
	case day == "*" && weekday == "1-5":
		return "Monday to Friday " + at
	case day == "*" && (weekday == "0,6" || weekday == "6,0"):
		return "Saturdays and Sundays " + at
	case day == "*":
		var names []string
		for _, d := range strings.Split(weekday, ",") {
			n, err := strconv.Atoi(d)
			if err != nil || n < 0 || n > 7 {
				return "cron " + line
			}
			names = append(names, weekdayNames[n%7])
		}
		if len(names) > 1 {
			return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1] + " " + at
		}
		return names[0] + " " + at
	case weekday == "*":
		if n, err := strconv.Atoi(day); err == nil {
			return fmt.Sprintf("the %s of each month %s", ordinal(n), at)
		}
	}
	return "cron " + line
}

func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return strconv.Itoa(n) + suffix
}
