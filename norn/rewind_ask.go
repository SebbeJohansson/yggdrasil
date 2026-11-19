package norn

// Rewind's questions in plain words: "Tuesday afternoon", "yesterday at 3",
// "when did I last work on races?". The local model (CHAT_URL, never Claude
// or Sindri) only splits the question into the words for when and the words
// to look for; Go turns the when into a range in TIME_ZONE, as the promise
// ledger does with a promise's day. Without a model, the question itself is
// read for date words, and its other words are looked for.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"yggdrasil/internal/llm"
	"yggdrasil/internal/part"
)

// RewindWhen is a question's time as a range.
type RewindWhen struct {
	From, To time.Time
	// Said is the range in words: "Tuesday 18 November, 14:00-16:00".
	Said string
	// Narrow is whether a time of day was named, not just a day.
	Narrow bool
}

var (
	clockTime   = regexp.MustCompile(`\b(?:at|around|about|kl\.?|klockan|vid|runt)?\s*(\d{1,2})(?:[:.](\d{2}))?\s*(am|pm)?\b`)
	daysAgo     = regexp.MustCompile(`\b(\d{1,2})\s+(?:days?\s+ago|dagar\s+sedan)\b`)
	dayPartsSet = []struct {
		words    []string
		from, to int
		said     string
	}{
		{[]string{"morning", "förmiddag", "förmiddagen", "morgon", "morgonen", "imorse", "i morse"}, 6, 12, "morning"},
		{[]string{"lunch", "lunchen"}, 11, 14, "lunch"},
		{[]string{"afternoon", "eftermiddag", "eftermiddagen", "em"}, 12, 18, "afternoon"},
		{[]string{"evening", "tonight", "kväll", "kvällen", "ikväll"}, 18, 24, "evening"},
		{[]string{"night", "natt", "natten"}, 0, 6, "night"},
	}
)

// pastWeekdays are Swedish "i tisdags": the last one, never today.
var pastWeekdays = map[string]time.Weekday{"måndags": time.Monday, "tisdags": time.Tuesday, "onsdags": time.Wednesday,
	"torsdags": time.Thursday, "fredags": time.Friday, "lördags": time.Saturday, "söndags": time.Sunday}

// ResolveWhen is the range a question names, read back from now in zone:
// a weekday is the latest one (today when it is today), "last Tuesday" the
// one before today. False when it names no day, week or time.
func ResolveWhen(text string, now time.Time, zone *time.Location) (RewindWhen, bool) {
	lower := strings.ToLower(strings.TrimSpace(text))
	words := " " + strings.TrimSpace(notLetter.ReplaceAllString(lower, " ")) + " "
	local := now.In(zone)
	today := dayOf(local)
	var day time.Time
	found, week := false, false
	switch {
	case phraseIn(words, "day before yesterday", "i förrgår", "förrgår"):
		day, found = today.AddDate(0, 0, -2), true
	case phraseIn(words, "yesterday", "igår", "i går"):
		day, found = today.AddDate(0, 0, -1), true
	case phraseIn(words, "today", "idag", "i dag", "this morning", "this afternoon", "tonight", "imorse", "i morse", "ikväll", "i kväll"):
		day, found = today, true
	case phraseIn(words, "last week", "förra veckan"):
		day, found, week = mondayOf(today).AddDate(0, 0, -7), true, true
	case phraseIn(words, "this week", "i veckan", "den här veckan", "denna vecka"):
		day, found, week = mondayOf(today), true, true
	}
	if !found {
		if match := daysAgo.FindStringSubmatch(lower); match != nil {
			n, _ := strconv.Atoi(match[1])
			day, found = today.AddDate(0, 0, -n), true
		}
	}
	if !found {
		tokens := strings.Fields(words)
		for i, token := range tokens {
			weekday, ok := weekdayWords[token]
			past, wasPast := pastWeekdays[token]
			if wasPast {
				weekday = past
			}
			if (!ok && !wasPast) || len(token) < 4 { // "sun" and "sat" are words too
				continue
			}
			back := (int(today.Weekday()) - int(weekday) + 7) % 7
			if (wasPast || (i > 0 && (tokens[i-1] == "last" || tokens[i-1] == "förra"))) && back == 0 {
				back = 7
			}
			day, found = today.AddDate(0, 0, -back), true
			break
		}
	}
	if !found && (isoDay.MatchString(lower) || slashDay.MatchString(lower) || hasMonth(words)) {
		if due := ResolveDue(lower, now, zone); due != "" {
			if parsed, err := time.ParseInLocation(dayLayout, due, zone); err == nil {
				if parsed.After(today) {
					parsed = parsed.AddDate(-1, 0, 0)
				}
				day, found = parsed, true
			}
		}
	}
	// A time of day: a part of it, or a clock time with an hour either side.
	fromHour, toHour, partSaid := -1, -1, ""
	for _, part := range dayPartsSet {
		if phraseIn(words, part.words...) {
			fromHour, toHour, partSaid = part.from, part.to, part.said
			break
		}
	}
	clockFrom, clockTo := time.Duration(-1), time.Duration(-1)
	if fromHour < 0 {
		if hour, minute, ok := clockOf(lower); ok {
			at := time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute
			clockFrom, clockTo = at-time.Hour, at+time.Hour
		}
	}
	if !found && fromHour < 0 && clockFrom < 0 {
		return RewindWhen{}, false
	}
	if !found {
		day = today
	}
	if week {
		end := day.AddDate(0, 0, 7)
		return RewindWhen{From: day, To: end, Said: "the week of " + day.Format("Monday 2 January")}, true
	}
	when := RewindWhen{From: day, To: day.AddDate(0, 0, 1), Said: day.Format("Monday 2 January")}
	switch {
	case fromHour >= 0:
		when.From, when.To = day.Add(time.Duration(fromHour)*time.Hour), day.Add(time.Duration(toHour)*time.Hour)
		when.Said += " " + partSaid
		when.Narrow = true
	case clockFrom >= 0:
		when.From, when.To = day.Add(max(0, clockFrom)), day.Add(min(24*time.Hour, clockTo))
		when.Said += fmt.Sprintf(", %s-%s", when.From.Format("15:04"), when.To.Format("15:04"))
		when.Narrow = true
	}
	return when, true
}

func hasMonth(words string) bool {
	for _, token := range strings.Fields(words) {
		if _, ok := monthWords[token]; ok && len(token) >= 3 && token != "may" && token != "mar" {
			return true
		}
	}
	return false
}

// clockOf is a clock time in the question: "at 3", "15:30", "kl 15", "3pm".
// A bare hour from 1 to 7 is the afternoon: "at 3" at work is 15:00.
func clockOf(text string) (int, int, bool) {
	for _, match := range clockTime.FindAllStringSubmatch(text, -1) {
		whole := strings.TrimSpace(match[0])
		marked := match[2] != "" || match[3] != "" || whole != match[1]
		if !marked {
			continue // a bare number ("3 days", "24 nov") is not a time
		}
		hour, _ := strconv.Atoi(match[1])
		minute, _ := strconv.Atoi(match[2])
		if hour > 23 || minute > 59 {
			continue
		}
		switch {
		case match[3] == "pm" && hour < 12:
			hour += 12
		case match[3] == "am" && hour == 12:
			hour = 0
		case match[3] == "" && match[2] == "" && hour >= 1 && hour <= 7:
			hour += 12
		}
		return hour, minute, true
	}
	return 0, 0, false
}

// rewindStopWords are left out of what is looked for.
var rewindStopWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`what was were did does doing have done when where which who how last work worked working
		on at in the a an i my me we to of for and or with about around this that week day today yesterday morning afternoon
		evening night lunch ago days time vad gjorde jag var när senast med på om för och eller den det denna förra igår idag
		kväll morgon eftermiddag förmiddag dag dagar sedan klockan kl jobbade arbetade`) {
		rewindStopWords[w] = true
	}
	for w := range weekdayWords {
		rewindStopWords[w] = true
	}
	for w := range monthWords {
		rewindStopWords[w] = true
	}
}

// SearchWords is what a question looks for: its words that are not about when.
func SearchWords(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, word := range regexp.MustCompile(`[\p{L}\p{N}][\p{L}\p{N}_-]+`).FindAllString(strings.ToLower(text), -1) {
		if rewindStopWords[word] || seen[word] || len([]rune(word)) < 3 || regexp.MustCompile(`^\d+$`).MatchString(word) {
			continue
		}
		seen[word] = true
		out = append(out, word)
	}
	return out
}

// stem drops a plural s, so "races" finds "race".
func stem(word string) string {
	if len([]rune(word)) > 4 && strings.HasSuffix(word, "s") && !strings.HasSuffix(word, "ss") {
		return strings.TrimSuffix(word, "s")
	}
	return word
}

// MatchesWords is whether an entry holds any of the words.
func MatchesWords(entry RewindEntry, words []string) bool {
	text := strings.ToLower(entry.Title + " " + entry.Detail)
	for _, word := range words {
		if word = stem(strings.ToLower(strings.TrimSpace(word))); word != "" && strings.Contains(text, word) {
			return true
		}
	}
	return false
}

// --- the model's part ------------------------------------------------------------------------

var rewindFormat = map[string]any{
	"type":     "object",
	"required": []string{"when", "words"},
	"properties": map[string]any{
		"when":  map[string]any{"type": "string"},
		"words": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	},
}

// RewindMessages asks the local model to split a question into when and what.
func RewindMessages(question string, now time.Time) []llm.Message {
	system := `You split a question about the past into two parts. Answer with JSON only: {"when":"…","words":["…"]}.
- when: the words in the question that say when, copied as written ("Tuesday afternoon", "yesterday at 3", "igår"); "" when it says no time.
- words: the topic to look for, as 1 to 4 short search words with close variants ("races", "race"); [] when it asks only about a time.
Never answer the question.`
	return []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: "Today is " + now.Format("Monday 2006-01-02") + ".\nQuestion: " + question}}
}

// ParseRewind reads the model's answer.
func ParseRewind(raw string) (string, []string, error) {
	var answer struct {
		When  string   `json:"when"`
		Words []string `json:"words"`
	}
	if err := json.Unmarshal([]byte(jsonObject(raw)), &answer); err != nil {
		return "", nil, err
	}
	var words []string
	for _, w := range answer.Words {
		if w = strings.TrimSpace(strings.ToLower(w)); w != "" && len(words) < 8 {
			words = append(words, w)
		}
	}
	return strings.TrimSpace(answer.When), words, nil
}

// RewindAnswer is POST /v1/rewind/ask's answer.
type RewindAnswer struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
	// Focus is where to look first: the range's start, or the latest match.
	Focus     int64         `json:"focus"`
	Entries   []RewindEntry `json:"entries"`
	Said      string        `json:"said"`
	Words     []string      `json:"words"`
	FromModel bool          `json:"fromModel"`
	// Unavailable is the sources that could not be read.
	Unavailable []string `json:"unavailable"`
}

// AnswerRewind marks what matches in a timeline: entries in a narrow time,
// and entries holding the words. With words and no day, only the matches are
// kept, so a month of entries is not sent back.
func AnswerRewind(entries []RewindEntry, when RewindWhen, dated bool, words []string) ([]RewindEntry, int64) {
	from, to := when.From.UnixMilli(), when.To.UnixMilli()
	out := []RewindEntry{}
	var focus int64
	for _, e := range entries {
		end := max(e.End, e.At)
		inWindow := e.At < to && (end >= from)
		e.Match = (when.Narrow && inWindow && len(words) == 0) || (len(words) > 0 && MatchesWords(e, words))
		if !dated && !e.Match {
			continue
		}
		if e.Match && e.At > focus && !dated {
			focus = e.At
		}
		out = append(out, e)
	}
	if dated {
		focus = from
		for _, e := range out {
			if e.Match {
				focus = e.At
				break
			}
		}
	}
	return out, focus
}

func (n *Norn) rewindModel() Chatter {
	if n.Rewind.Model != nil {
		return n.Rewind.Model
	}
	return func(ctx context.Context, messages []llm.Message, options llm.Options) (string, error) {
		return n.LLM.Chat(ctx, messages, options)
	}
}

// RewindAsk answers a question in plain words.
func (n *Norn) RewindAsk(ctx context.Context, question string, now time.Time) RewindAnswer {
	whenWords, words, fromModel := question, SearchWords(question), false
	raw, err := n.rewindModel()(ctx, RewindMessages(question, now.In(n.Zone)), llm.Options{Purpose: "rewind", Model: n.Config.AskModel,
		Temperature: 0, MaxTokens: 120, Format: rewindFormat, Timeout: 30 * time.Second})
	if err == nil {
		if when, found, parseErr := ParseRewind(raw); parseErr == nil {
			fromModel = true
			if when != "" {
				whenWords = when
			}
			if len(found) > 0 {
				words = found
			}
		}
	}
	when, dated := ResolveWhen(whenWords, now, n.Zone)
	if !dated && fromModel {
		when, dated = ResolveWhen(question, now, n.Zone) // the model may miss what Go reads
	}
	shownFrom, shownTo := when.From, when.To
	switch {
	case !dated:
		// No day: look back as far as one timeline reaches, for the words.
		shownTo = now
		shownFrom = now.Add(-RewindRangeMax)
		when = RewindWhen{From: shownFrom, To: shownTo, Said: "the last 31 days"}
	case when.Narrow:
		shownFrom, shownTo = dayRange(when.From, n.Zone)
	}
	entries, unavailable := n.Timeline(ctx, shownFrom, shownTo)
	marked, focus := AnswerRewind(entries, when, dated, words)
	said := when.Said
	if len(words) > 0 {
		said += ", looking for " + strings.Join(words, ", ")
		if !dated && focus > 0 {
			said += "; last on " + time.UnixMilli(focus).In(n.Zone).Format("Monday 2 January")
		} else if !dated {
			said += "; nothing found"
		}
	}
	if words == nil {
		words = []string{}
	}
	return RewindAnswer{From: when.From.UnixMilli(), To: when.To.UnixMilli(), Focus: focus, Entries: marked, Said: said, Words: words,
		FromModel: fromModel, Unavailable: unavailable}
}

func (n *Norn) rewindAsk(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Q string `json:"q"`
	}
	if err := part.Decode(r, &body); err != nil || strings.TrimSpace(body.Q) == "" || len([]rune(body.Q)) > 300 {
		part.Error(w, http.StatusBadRequest, "`q` is the question, at most 300 characters")
		return
	}
	part.JSON(w, http.StatusOK, n.RewindAsk(r.Context(), strings.TrimSpace(body.Q), time.Now()))
}
