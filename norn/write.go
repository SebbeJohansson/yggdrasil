package norn

// The writing checks Pennvass calls, ported from its write-core.js: check
// (LanguageTool), tone (Laya), improve (a local model, one sentence at a
// time, behind a cache) and rewrite (a Claude agent with no tools, run by
// Sindri).
//
// Offsets are UTF-16 code units, as in JavaScript strings: Pennvass is a
// browser extension, and LanguageTool (Java) counts the same way.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"yggdrasil/internal/llm"
	"yggdrasil/internal/part"
)

// MaxText is a page's whole textbox, not a book. LanguageTool's own limit is 20k characters.
const MaxText = 20_000

// Issue is one underline in the writer's text.
type Issue struct {
	Offset       int      `json:"offset"`
	Length       int      `json:"length"`
	Message      string   `json:"message"`
	Replacements []string `json:"replacements"`
	Kind         string   `json:"kind"`
	Source       string   `json:"source"`
	Rule         string   `json:"rule,omitempty"`
}

// Checked is what a check found, and the language of most of the text.
type Checked struct {
	Language string  `json:"language"`
	Issues   []Issue `json:"issues"`
}

// WriteConfig is where the writing engines are.
type WriteConfig struct {
	LanguageToolURL string
	// Model improves sentences; RewriteModel is the Claude model that rewrites.
	Model, RewriteModel string
}

func u16len(s string) int { return len(utf16.Encode([]rune(s))) }

// --- check ---------------------------------------------------------------

type ltMatch struct {
	Offset       *int   `json:"offset"`
	Length       *int   `json:"length"`
	Message      string `json:"message"`
	Replacements []struct {
		Value string `json:"value"`
	} `json:"replacements"`
	Rule *struct {
		ID        string `json:"id"`
		IssueType string `json:"issueType"`
		Category  *struct {
			ID string `json:"id"`
		} `json:"category"`
	} `json:"rule"`
}

type ltBody struct {
	Matches  []ltMatch `json:"matches"`
	Language *struct {
		Code             string `json:"code"`
		DetectedLanguage *struct {
			Code string `json:"code"`
		} `json:"detectedLanguage"`
	} `json:"language"`
}

// kindOf folds LanguageTool's issue types into the four underline colours.
func kindOf(m ltMatch) string {
	if m.Rule == nil {
		return "grammar"
	}
	switch m.Rule.IssueType {
	case "misspelling":
		return "spelling"
	case "style", "locale-violation", "register":
		return "style"
	}
	if m.Rule.Category != nil && m.Rule.Category.ID == "STYLE" {
		return "style"
	}
	return "grammar"
}

// ReadLanguageTool turns LanguageTool's answer into issues.
func ReadLanguageTool(body ltBody) Checked {
	issues := []Issue{}
	for _, m := range body.Matches {
		if m.Offset == nil || m.Length == nil || m.Message == "" {
			continue
		}
		kind := kindOf(m)
		message := m.Message
		if kind == "spelling" {
			// LanguageTool writes its message in the text's language; the card is English.
			message = "Possible spelling mistake"
		}
		replacements := []string{}
		for _, r := range m.Replacements {
			if r.Value != "" && len(replacements) < 5 {
				replacements = append(replacements, r.Value)
			}
		}
		issue := Issue{Offset: *m.Offset, Length: *m.Length, Message: message, Replacements: replacements, Kind: kind, Source: "languagetool"}
		if m.Rule != nil {
			issue.Rule = m.Rule.ID
		}
		issues = append(issues, issue)
	}
	language := "auto"
	if body.Language != nil {
		if body.Language.Code != "" {
			language = body.Language.Code
		} else if body.Language.DetectedLanguage != nil && body.Language.DetectedLanguage.Code != "" {
			language = body.Language.DetectedLanguage.Code
		}
	}
	return Checked{Language: language, Issues: issues}
}

// minDetectLetters: below this many letters a line says too little for LanguageTool to tell its language.
const minDetectLetters = 25

// variants: LanguageTool checks spelling only in a variant; plain "de" has no dictionary.
var variants = map[string]string{"de": "de-DE", "pt": "pt-PT", "nl": "nl-NL", "ca": "ca-ES"}

func langBase(code string) string { return strings.ToLower(strings.SplitN(code, "-", 2)[0]) }

// line is a non-empty line, with its offsets in bytes and in UTF-16 units.
type line struct {
	text              string
	start, end        int // bytes
	offset, offsetEnd int // UTF-16
	letters           int
	language          string
	result            *Checked
	long              bool
}

var lineRE = regexp.MustCompile(`\S[^\n]*`)

// lines are the text's non-empty lines. A Swedish email with an English
// disclaimer is one text in two languages, and one detected language for all
// of it marks every word of the other.
func lines(text string) []*line {
	found := []*line{}
	for _, span := range lineRE.FindAllStringIndex(text, -1) {
		l := &line{text: text[span[0]:span[1]], start: span[0], end: span[1]}
		l.offset = u16len(text[:span[0]])
		l.offsetEnd = l.offset + u16len(l.text)
		for _, r := range l.text {
			if unicode.IsLetter(r) {
				l.letters++
			}
		}
		found = append(found, l)
	}
	return found
}

type checker struct {
	url, englishVariant string
	languages           []string
	http                *http.Client
}

func (c *checker) one(ctx context.Context, text, language string) (Checked, error) {
	form := url.Values{"text": {text}, "language": {language}}
	if language == "auto" {
		form.Set("preferredVariants", c.englishVariant)
		if len(c.languages) > 0 {
			seen, bases := map[string]bool{}, []string{}
			for _, code := range c.languages {
				if b := langBase(code); !seen[b] {
					seen[b] = true
					bases = append(bases, b)
				}
			}
			form.Set("preferredLanguages", strings.Join(bases, ","))
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.url, "/")+"/v2/check", strings.NewReader(form.Encode()))
	if err != nil {
		return Checked{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.http.Do(request)
	if err != nil {
		return Checked{}, err
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if response.StatusCode != http.StatusOK {
		return Checked{}, fmt.Errorf("languagetool answered %d: %.200s", response.StatusCode, data)
	}
	var body ltBody
	if err := json.Unmarshal(data, &body); err != nil {
		return Checked{}, err
	}
	return ReadLanguageTool(body), nil
}

// eachLimited runs work on every item, a few at a time: an email of 80 lines
// should not be 80 requests at once. The first error wins.
func eachLimited[T any](items []T, work func(T) error) error {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	slots := make(chan struct{}, 6)
	for _, item := range items {
		wg.Add(1)
		slots <- struct{}{}
		go func(item T) {
			defer func() { <-slots; wg.Done() }()
			if err := work(item); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}(item)
	}
	wg.Wait()
	return first
}

// others are the writer's languages other than language, one variant each.
func (c *checker) others(language string, detected []string) []string {
	seen, out := map[string]bool{}, []string{}
	for _, code := range append(append([]string{}, detected...), c.languages...) {
		b := langBase(code)
		if b == langBase(language) || seen[b] {
			continue
		}
		seen[b] = true
		switch {
		case b == "en":
			out = append(out, c.englishVariant)
		case strings.Contains(code, "-"):
			out = append(out, code)
		case variants[b] != "":
			out = append(out, variants[b])
		default:
			out = append(out, code)
		}
	}
	return out
}

// acceptOtherLanguages drops each misspelling that one of the writer's other
// languages spells right: "deployment" in a Swedish sentence.
func (c *checker) acceptOtherLanguages(ctx context.Context, text string, result Checked, others []string) (Checked, error) {
	misspelled := false
	for _, issue := range result.Issues {
		misspelled = misspelled || issue.Kind == "spelling"
	}
	if !misspelled || len(others) == 0 {
		return result, nil
	}
	readings := make([]Checked, len(others))
	if err := eachLimited(indexes(len(others)), func(i int) error {
		var err error
		readings[i], err = c.one(ctx, text, others[i])
		return err
	}); err != nil {
		return result, err
	}
	fineIn := func(issue Issue, reading Checked) bool {
		for _, o := range reading.Issues {
			if o.Kind == "spelling" && o.Offset < issue.Offset+issue.Length && issue.Offset < o.Offset+o.Length {
				return false
			}
		}
		return true
	}
	kept := []Issue{}
	for _, issue := range result.Issues {
		accepted := false
		if issue.Kind == "spelling" {
			for _, reading := range readings {
				accepted = accepted || fineIn(issue, reading)
			}
		}
		if !accepted {
			kept = append(kept, issue)
		}
	}
	result.Issues = kept
	return result, nil
}

func indexes(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// Check finds spelling and grammar issues. Each line long enough to tell
// gets its own detected language; short ones take the language of the line
// before (or after, when they open the text). Consecutive lines in one
// language are checked together. languages are the writer's, primary first:
// a line is only ever checked in one of them.
func Check(ctx context.Context, client *http.Client, languageToolURL, text string, languages []string) (Checked, error) {
	c := &checker{url: languageToolURL, languages: languages, http: client, englishVariant: "en-US"}
	for _, code := range languages {
		if langBase(code) == "en" {
			c.englishVariant = code
			break
		}
	}
	mine, order := map[string]string{}, []string{}
	for _, code := range languages {
		b := langBase(code)
		if _, ok := mine[b]; ok {
			continue
		}
		if b == "en" {
			mine[b] = c.englishVariant
		} else {
			mine[b] = code
		}
		order = append(order, b)
	}
	primary := "auto"
	if len(order) > 0 {
		primary = mine[order[0]]
	}

	parts := lines(text)
	long := []*line{}
	for _, l := range parts {
		if l.letters >= minDetectLetters {
			l.long = true
			long = append(long, l)
		}
	}
	if err := eachLimited(long, func(l *line) error {
		result, err := c.one(ctx, l.text, "auto")
		if err != nil {
			return err
		}
		l.result, l.language = &result, result.Language
		if len(mine) > 0 {
			l.language = mine[langBase(result.Language)]
		}
		if l.language == "" {
			l.language, l.result = primary, nil
		}
		return nil
	}); err != nil {
		return Checked{}, err
	}
	if len(long) == 0 {
		result, err := c.one(ctx, text, primary)
		if err != nil {
			return Checked{}, err
		}
		return c.acceptOtherLanguages(ctx, text, result, c.others(result.Language, nil))
	}
	for _, l := range parts {
		if l.language != "" {
			continue
		}
		l.language = long[0].language
		for _, q := range long {
			if q.offset < l.offset {
				l.language = q.language
			}
		}
	}

	type run struct {
		language   string
		start, end int // bytes
		offset     int // UTF-16
		lines      []*line
		result     Checked
	}
	runs := []*run{}
	for _, l := range parts {
		if n := len(runs); n > 0 && runs[n-1].language == l.language {
			runs[n-1].end = l.end
			runs[n-1].lines = append(runs[n-1].lines, l)
			continue
		}
		runs = append(runs, &run{language: l.language, start: l.start, end: l.end, offset: l.offset, lines: []*line{l}})
	}
	detected := make([]string, len(long))
	for i, l := range long {
		detected[i] = l.language
	}
	if err := eachLimited(runs, func(r *run) error {
		runText := text[r.start:r.end]
		var result Checked
		if len(r.lines) == 1 && r.lines[0].result != nil {
			result = *r.lines[0].result
		} else {
			var err error
			if result, err = c.one(ctx, runText, r.language); err != nil {
				return err
			}
		}
		var err error
		r.result, err = c.acceptOtherLanguages(ctx, runText, result, c.others(r.language, detected))
		return err
	}); err != nil {
		return Checked{}, err
	}

	issues := []Issue{}
	for _, r := range runs {
		for _, issue := range r.result.Issues {
			issue.Offset += r.offset
			issues = append(issues, issue)
		}
	}
	// The language of most of the text.
	letters := map[string]int{}
	for _, l := range long {
		letters[l.language] += l.letters
	}
	language, most := "", -1
	for _, l := range long {
		if letters[l.language] > most {
			language, most = l.language, letters[l.language]
		}
	}
	return Checked{Language: language, Issues: issues}, nil
}

// --- tone ----------------------------------------------------------------

// Tones are the tone badge's choices.
var Tones = map[string]string{
	"formal":     "polite and professional",
	"friendly":   "warm and kind",
	"casual":     "relaxed and informal",
	"confident":  "direct and sure of itself",
	"frustrated": "annoyed or impatient",
}

// toneWindow: tone is about the message as a whole; its last 1500 characters are what the reader is left with.
const toneWindow = 1500

// Toned is the text's tone, with Laya's probabilities.
type Toned struct {
	Tone          string             `json:"tone"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// Tone asks Laya for the text's tone.
func Tone(ctx context.Context, client *http.Client, layaURL, layaKey, text string) (Toned, error) {
	runes := []rune(text)
	if len(runes) > toneWindow {
		runes = runes[len(runes)-toneWindow:]
	}
	payload, _ := json.Marshal(map[string]any{
		"state": "Text: " + string(runes),
		"questions": map[string]any{"tone": map[string]any{
			"type": "choice", "instructions": "What is the tone of the text?", "criteria": Tones,
		}},
	})
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(layaURL, "/")+"/v1/systemone", strings.NewReader(string(payload)))
	if err != nil {
		return Toned{}, err
	}
	request.Header.Set("Authorization", "Bearer "+layaKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return Toned{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Toned{}, fmt.Errorf("laya answered %d", response.StatusCode)
	}
	var body struct {
		Answers struct {
			Tone *struct {
				Choice        string             `json:"choice"`
				Probabilities map[string]float64 `json:"probabilities"`
			} `json:"tone"`
		} `json:"answers"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err != nil {
		return Toned{}, err
	}
	answer := body.Answers.Tone
	if answer == nil || Tones[answer.Choice] == "" {
		return Toned{}, errors.New("laya answered without a tone")
	}
	if answer.Probabilities == nil {
		answer.Probabilities = map[string]float64{}
	}
	return Toned{Tone: answer.Choice, Probabilities: answer.Probabilities}, nil
}

// --- improve -------------------------------------------------------------

// Sentence is one sentence, its offset in UTF-16 units.
type Sentence struct {
	Offset int
	Text   string
}

// Sentences splits text the way a browser's sentence segmenter does, near
// enough: after . ! ? (and closing quotes or brackets) followed by space, and
// at every line break. Surrounding whitespace is trimmed off.
func Sentences(text string) []Sentence {
	out := []Sentence{}
	emit := func(start, end int) {
		segment := text[start:end]
		trimmed := strings.TrimSpace(segment)
		if trimmed == "" {
			return
		}
		lead := len(segment) - len(strings.TrimLeftFunc(segment, unicode.IsSpace))
		out = append(out, Sentence{Offset: u16len(text[:start+lead]), Text: trimmed})
	}
	start := 0
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		next := i + size
		switch {
		case r == '\n':
			emit(start, next)
			start = next
		case r == '.' || r == '!' || r == '?' || r == '…':
			j := next
			for j < len(text) {
				c, s := utf8.DecodeRuneInString(text[j:])
				if !strings.ContainsRune(".!?…\"'”’)]", c) {
					break
				}
				j += s
			}
			if c, _ := utf8.DecodeRuneInString(text[j:]); j == len(text) || unicode.IsSpace(c) {
				emit(start, j)
				start, next = j, j
			}
		}
		i = next
	}
	emit(start, len(text))
	return out
}

// WorthImproving: short sentences are LanguageTool's job; very long ones are usually pasted, not written.
func WorthImproving(sentence string) bool {
	return len(strings.Fields(sentence)) >= 6 && utf8.RuneCountInString(sentence) <= 400
}

// ImproveSystem makes the model name a sentence's problems before fixing it:
// asked "is this clear?", qwen2.5:7b called wordy sentences fine.
const ImproveSystem = `You are a strict copy editor. Read one sentence and list its concrete problems: grammar or spelling errors, wrong word forms, filler words (just, basically, actually, really), wordy phrases ("in order to", "at this point in time", "due to the fact that"), needless passive voice, or confusing word order.
If there are no problems, answer {"problems":[],"better":""}.
Otherwise answer problems = short names of each problem, and better = the sentence fixed, in the same language and tone, keeping every name, number and fact. Do not add new content. Do not change a sentence only for taste.
Not problems: commas versus semicolons, contractions, British versus American spelling, informal but correct wording. A short, correct sentence has no problems.
Use only real words of the sentence's language.`

// ImproveFormat is the improve reply's JSON schema.
var ImproveFormat = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"problems": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"better":   map[string]any{"type": "string"},
	},
	"required": []string{"problems", "better"},
}

var (
	contractions = []struct {
		re *regexp.Regexp
		to string
	}{
		{regexp.MustCompile(`n['’]t\b`), " not"},
		{regexp.MustCompile(`['’]ll\b`), " will"},
		{regexp.MustCompile(`['’]re\b`), " are"},
		{regexp.MustCompile(`['’]ve\b`), " have"},
		{regexp.MustCompile(`\bi['’]m\b`), "i am"},
		{regexp.MustCompile(`\bcan not\b`), "cannot"},
		{regexp.MustCompile(`\bca not\b`), "cannot"},
		{regexp.MustCompile(`\bwo not\b`), "will not"},
	}
	nonWord  = regexp.MustCompile(`[^\p{L}\p{N}]+`)
	ourEnd   = regexp.MustCompile(`our$`)
	iseEnd   = regexp.MustCompile(`is(e|ed|ing|ation)$`)
	treEnd   = regexp.MustCompile(`tre$`)
	hasUnder = regexp.MustCompile(`_`)
)

// collapseDoubles turns "ll" into "l": travelled and traveled read the same.
func collapseDoubles(word string) string {
	var b strings.Builder
	var last rune = -1
	for _, r := range word {
		if r == last && unicode.IsLetter(r) {
			continue
		}
		b.WriteRune(r)
		last = r
	}
	return b.String()
}

// comparable is a sentence's words without case, punctuation, contractions
// or British/American spelling.
func comparable(sentence string) string {
	s := strings.ToLower(sentence)
	for _, c := range contractions {
		s = c.re.ReplaceAllString(s, c.to)
	}
	words := []string{}
	for _, word := range nonWord.Split(s, -1) {
		if word == "" {
			continue
		}
		word = collapseDoubles(word)
		word = ourEnd.ReplaceAllString(word, "or")
		word = iseEnd.ReplaceAllString(word, "iz$1")
		word = treEnd.ReplaceAllString(word, "ter")
		words = append(words, word)
	}
	return strings.Join(words, " ")
}

// OnlySpellingVariants is whether better differs from sentence only in ways
// the prompt says are not problems.
func OnlySpellingVariants(sentence, better string) bool {
	return comparable(sentence) == comparable(better)
}

// DescribeProblems turns ["needless_passive_voice"] into "Needless passive voice".
func DescribeProblems(problems []string) string {
	seen, names := map[string]bool{}, []string{}
	for _, p := range problems {
		name := strings.ToLower(strings.TrimSpace(regexp.MustCompile(`[_-]+`).ReplaceAllString(p, " ")))
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	line := strings.Join(names, ", ")
	if line == "" {
		return "Clearer wording"
	}
	r, size := utf8.DecodeRuneInString(line)
	return string(unicode.ToUpper(r)) + line[size:]
}

// Suggestion is a clearer sentence and why.
type Suggestion struct {
	Better string `json:"better"`
	Reason string `json:"reason"`
}

// ReadSuggestion is the model's reply as a suggestion, or nil when it has none worth showing.
func ReadSuggestion(sentence, reply string) *Suggestion {
	var parsed struct {
		Problems json.RawMessage `json:"problems"`
		Better   any             `json:"better"`
	}
	if json.Unmarshal([]byte(reply), &parsed) != nil {
		return nil
	}
	better, _ := parsed.Better.(string)
	better = strings.TrimSpace(better)
	var problems []any
	isList := json.Unmarshal(parsed.Problems, &problems) == nil && problems != nil
	if better == "" || (isList && len(problems) == 0) || OnlySpellingVariants(sentence, better) {
		return nil
	}
	// Seen once, in Swedish: a made-up "att_meet_up". Real sentences have no underscores.
	if hasUnder.MatchString(better) && !hasUnder.MatchString(sentence) {
		return nil
	}
	names := []string{}
	for _, p := range problems {
		if s, ok := p.(string); ok {
			names = append(names, s)
		}
	}
	return &Suggestion{Better: better, Reason: DescribeProblems(names)}
}

// ImprovePerCall is how many sentences the model reads per request; the rest come back as pending.
const ImprovePerCall = 3

// SuggestionCache remembers each sentence's answer (nil: it was fine), the oldest dropped first.
type SuggestionCache struct {
	mu      sync.Mutex
	limit   int
	order   []string
	entries map[string]*Suggestion
}

// NewSuggestionCache keeps up to limit sentences.
func NewSuggestionCache(limit int) *SuggestionCache {
	return &SuggestionCache{limit: limit, entries: map[string]*Suggestion{}}
}

func (c *SuggestionCache) get(sentence string) (*Suggestion, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.entries[sentence]
	return s, ok
}

func (c *SuggestionCache) set(sentence string, s *Suggestion) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[sentence]; !ok {
		c.order = append(c.order, sentence)
	}
	c.entries[sentence] = s
	for len(c.order) > c.limit {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
}

// Improved is the clearer versions found, and how many sentences wait for the next call.
type Improved struct {
	Issues  []Issue `json:"issues"`
	Pending int     `json:"pending"`
}

// Improve reads the text's sentences, a few per call, and suggests clearer
// versions. complete sends one sentence to the model and returns its JSON reply.
func Improve(text string, cache *SuggestionCache, complete func(system, sentence string) (string, error)) (Improved, error) {
	out := Improved{Issues: []Issue{}}
	read := 0
	for _, sentence := range Sentences(text) {
		if !WorthImproving(sentence.Text) {
			continue
		}
		suggestion, known := cache.get(sentence.Text)
		if !known {
			if read >= ImprovePerCall {
				out.Pending++
				continue
			}
			read++
			reply, err := complete(ImproveSystem, sentence.Text)
			if err != nil {
				return out, err
			}
			suggestion = ReadSuggestion(sentence.Text, reply)
			cache.set(sentence.Text, suggestion)
		}
		if suggestion != nil {
			out.Issues = append(out.Issues, Issue{
				Offset: sentence.Offset, Length: u16len(sentence.Text), Message: suggestion.Reason,
				Replacements: []string{suggestion.Better}, Kind: "clarity", Source: "ollama",
			})
		}
	}
	return out, nil
}

// --- rewrite -------------------------------------------------------------

// Styles are what each rewrite button asks for.
var Styles = map[string]string{
	"clearer":   "clearer and easier to read",
	"shorter":   "noticeably shorter, keeping every point",
	"formal":    "more formal and professional",
	"friendly":  "warmer and friendlier",
	"confident": "more direct and confident",
	"english":   "in English",
	"swedish":   "in Swedish",
}

// RewriteSystem is the rewrite agent's instructions.
const RewriteSystem = "You rewrite text for the person. Reply with the rewritten text only: no preamble, no quotes, no explanation, no markdown fences. " +
	"Keep the meaning, every fact, name, number and link, and the paragraph breaks. Do not use any tools."

// RewriteDisallowed are the tools the rewrite agent is refused, beside starting with none.
var RewriteDisallowed = []string{"Bash", "Edit", "Write", "NotebookEdit", "Read", "Glob", "Grep", "WebFetch", "WebSearch", "Task", "Agent", "TodoWrite"}

// RewritePrompt asks for the text in a style.
func RewritePrompt(text, style string) string {
	return "Rewrite this to be " + Styles[style] + ":\n\n" + text
}

// Rewrite has Sindri run a Claude agent with no tools over the text, and waits for it.
func (c *SindriClient) Rewrite(ctx context.Context, text, style, model string, poll, timeout time.Duration) (string, error) {
	request := map[string]any{
		"label": "pennvass: " + style, "prompt": RewritePrompt(text, style), "permissionMode": "default",
		"noTools": true, "disallowedTools": RewriteDisallowed, "appendSystemPrompt": RewriteSystem,
	}
	if model != "" {
		request["model"] = model
	}
	var agent Agent
	if err := c.call(ctx, http.MethodPost, "/v1/agents", request, &agent); err != nil {
		return "", err
	}
	deadline := time.Now().Add(timeout)
	id := agent.ID
	for !settledStatus(agent.Status) {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("claude did not finish the rewrite in time (agent %s)", id)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}
		next, err := c.Agent(ctx, id)
		if err != nil {
			return "", err
		}
		agent = *next
	}
	result := strings.TrimSpace(agent.Result)
	if agent.Status == "failed" || agent.Status == "aborted" || result == "" {
		return "", fmt.Errorf("claude's rewrite %s: %s", agent.Status, agent.LastText)
	}
	return result, nil
}

func settledStatus(status string) bool {
	return status == "waiting" || status == "done" || status == "failed" || status == "aborted"
}

// --- routes --------------------------------------------------------------

type writeRequest struct {
	Text      string   `json:"text"`
	Languages []any    `json:"languages"`
	Style     string   `json:"style"`
}

func readWrite(w http.ResponseWriter, r *http.Request) (writeRequest, bool) {
	var body writeRequest
	if err := part.Decode(r, &body); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return body, false
	}
	if strings.TrimSpace(body.Text) == "" {
		part.Error(w, http.StatusBadRequest, "`text` must be a non-empty string")
		return body, false
	}
	if u16len(body.Text) > MaxText {
		part.Error(w, http.StatusBadRequest, fmt.Sprintf("`text` is longer than %d characters", MaxText))
		return body, false
	}
	return body, true
}

// engineFailed says which engine did not answer, rather than a bare 500.
func (n *Norn) engineFailed(w http.ResponseWriter, engine string, err error) {
	n.logf("%s did not answer: %v", engine, err)
	part.Error(w, http.StatusBadGateway, engine+" did not answer")
}

func (n *Norn) writeCheck(w http.ResponseWriter, r *http.Request) {
	body, ok := readWrite(w, r)
	if !ok {
		return
	}
	// Pennvass sends the writer's languages, primary first.
	languages := []string{}
	for _, l := range body.Languages {
		if s, ok := l.(string); ok && len(languages) < 10 {
			languages = append(languages, s)
		}
	}
	result, err := Check(r.Context(), n.HTTP, n.Config.Write.LanguageToolURL, body.Text, languages)
	if err != nil {
		n.engineFailed(w, "languagetool", err)
		return
	}
	part.JSON(w, http.StatusOK, result)
}

func (n *Norn) writeTone(w http.ResponseWriter, r *http.Request) {
	body, ok := readWrite(w, r)
	if !ok {
		return
	}
	if n.Config.LayaKey == "" {
		part.Error(w, http.StatusServiceUnavailable, "no LAYA_KEY in norn.env")
		return
	}
	result, err := Tone(r.Context(), n.HTTP, n.Config.LayaURL, n.Config.LayaKey, body.Text)
	if err != nil {
		n.engineFailed(w, "laya", err)
		return
	}
	part.JSON(w, http.StatusOK, result)
}

func (n *Norn) writeImprove(w http.ResponseWriter, r *http.Request) {
	body, ok := readWrite(w, r)
	if !ok {
		return
	}
	model := n.Config.Write.Model
	// Same model as the dispatcher by default: a different keep-alive here
	// would cut the dispatcher's short after each check.
	keepAlive := "60m"
	if model == n.Config.DispatchModel {
		keepAlive = n.Config.DispatchKeepAlive
	}
	think := false
	result, err := Improve(body.Text, n.suggestions, func(system, sentence string) (string, error) {
		return n.LLM.Chat(r.Context(), []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: sentence}}, llm.Options{
			Purpose: "improve", Model: model, Temperature: 0, TopP: 1, MaxTokens: 200, Format: ImproveFormat,
			Think: &think, KeepAlive: keepAlive, Timeout: 60 * time.Second,
		})
	})
	if err != nil {
		n.engineFailed(w, model, err)
		return
	}
	part.JSON(w, http.StatusOK, result)
}

func (n *Norn) writeRewrite(w http.ResponseWriter, r *http.Request) {
	body, ok := readWrite(w, r)
	if !ok {
		return
	}
	if Styles[body.Style] == "" {
		names := make([]string, 0, len(Styles))
		for name := range Styles {
			names = append(names, name)
		}
		sort.Strings(names)
		part.Error(w, http.StatusBadRequest, "`style` must be one of "+strings.Join(names, ", "))
		return
	}
	if !n.Sindri.On() {
		part.Error(w, http.StatusServiceUnavailable, "rewrites need Sindri (no SINDRI_TOKEN in norn.env)")
		return
	}
	text, err := n.Sindri.Rewrite(r.Context(), body.Text, body.Style, n.Config.Write.RewriteModel, time.Second, 2*time.Minute)
	if err != nil {
		n.engineFailed(w, "claude", err)
		return
	}
	part.JSON(w, http.StatusOK, map[string]string{"text": text})
}

func (n *Norn) mountWrite(p *part.Part) {
	p.Handle("POST /v1/write/check", n.writeCheck)
	p.Handle("POST /v1/write/tone", n.writeTone)
	p.Handle("POST /v1/write/improve", n.writeImprove)
	p.Handle("POST /v1/write/rewrite", n.writeRewrite)
}
