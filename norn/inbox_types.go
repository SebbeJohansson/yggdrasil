package norn

// What one unread thing looks like, whichever place it came from: Outlook,
// Teams, Slack or Gmail. Ported from my-agent's tasks/inbox/types.ts and
// sources/text.ts.
//
// None of them has a notifications API. What there is: Outlook messages with
// isRead false, Teams chats whose last message is newer than the last one you
// read, Slack conversations the server says have unread messages, and Gmail's
// unread inbox. "Unread" means that source considers it unseen, nothing
// stronger.

import (
	"regexp"
	"strconv"
	"strings"
)

// Sources, and which half of your life each belongs to.
var (
	SourceNames = []string{"outlook", "teams", "slack", "gmail"}
	Scopes      = []string{"work", "personal"}
	SourceScope = map[string]string{"outlook": "work", "teams": "work", "slack": "work", "gmail": "personal"}
	// sourceMark is how a source is written in the line the summary model reads.
	sourceMark = map[string]string{"outlook": "ol", "teams": "tm", "slack": "sl", "gmail": "gm"}
)

// Item is one unread message, chat or conversation.
type Item struct {
	// ID is stable and unique across sources: the source is part of it.
	ID     string `json:"id"`
	Source string `json:"source"`
	// From is who sent it, as a person reads it.
	From string `json:"from"`
	// Sender is who sent it as the directory knows them (an address, an
	// Entra user id): never shown, the key the org chart is read by.
	Sender string `json:"sender,omitempty"`
	// Relation is what the sender is to you in the org chart.
	Relation string `json:"relation,omitempty"`
	// Recipients is everyone else it went to, where the source says cheaply.
	Recipients []string `json:"recipients,omitempty"`
	Subject    string   `json:"subject"`
	Preview    string   `json:"preview"`
	// Text is the message as far as it was read, longer than the preview where
	// the source gives it (Teams, Slack): what a reply draft is written from.
	// Not sent to the widget, and not kept across a restart.
	Text string `json:"-"`
	// Context is what a wordless message carries: a file title, GIF.
	Context string `json:"context,omitempty"`
	// At is when it arrived, epoch ms.
	At         int64 `json:"at"`
	Direct     bool  `json:"direct"`
	MentionsMe bool  `json:"mentionsMe"`
	Important  bool  `json:"important"`
	// Conversation is the Teams chat it arrived in, which can be muted.
	Conversation string `json:"conversation,omitempty"`
	// Hidden is why it never reaches the panel, in words.
	Hidden string `json:"hidden,omitempty"`
	Link   string `json:"link,omitempty"`
}

// SourceStatus is whether a source answered. Carried, not thrown: two of
// four working is normal, and worth showing.
type SourceStatus struct {
	Source string `json:"source"`
	OK     bool   `json:"ok"`
	Count  int    `json:"count"`
	Error  string `json:"error,omitempty"`
}

func failedStatus(source string, err error) SourceStatus {
	message := strings.SplitN(err.Error(), "\n", 2)[0]
	if len(message) > 200 {
		message = message[:200]
	}
	return SourceStatus{Source: source, Count: 0, Error: message}
}

// --- text ------------------------------------------------------------------

// PreviewChars is how much of a body a preview keeps.
const PreviewChars = 220

var (
	entity   = regexp.MustCompile(`(?i)&(#x?[0-9a-f]+|[a-z]+);`)
	entities = map[string]string{"amp": "&", "lt": "<", "gt": ">", "quot": `"`, "#39": "'", "apos": "'", "nbsp": " "}
)

// DecodeEntities turns HTML entities into the characters they stand for.
func DecodeEntities(text string) string {
	return entity.ReplaceAllStringFunc(text, func(whole string) string {
		name := whole[1 : len(whole)-1]
		if known, ok := entities[strings.ToLower(name)]; ok {
			return known
		}
		if strings.HasPrefix(name, "#") {
			var code int64
			var err error
			if len(name) > 1 && (name[1] == 'x' || name[1] == 'X') {
				code, err = strconv.ParseInt(name[2:], 16, 32)
			} else {
				code, err = strconv.ParseInt(name[1:], 10, 32)
			}
			if err == nil && code > 0 && code <= 0x10FFFF {
				return string(rune(code))
			}
		}
		return whole
	})
}

var (
	scriptBlock = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`)
	styleBlock  = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`)
	lineBreak   = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|li|tr|h[1-6])>`)
	tag         = regexp.MustCompile(`<[^>]*>`)
)

// StripHTML is the text of an HTML body.
func StripHTML(html string) string {
	html = scriptBlock.ReplaceAllString(html, " ")
	html = styleBlock.ReplaceAllString(html, " ")
	html = lineBreak.ReplaceAllString(html, " ")
	return DecodeEntities(tag.ReplaceAllString(html, ""))
}

// OneLine collapses whitespace and cuts at limit characters, at a word
// when one is near.
func OneLine(text string, limit int) string {
	flat := []rune(strings.TrimSpace(whitespace.ReplaceAllString(text, " ")))
	if len(flat) <= limit {
		return string(flat)
	}
	cut := flat[:limit]
	if space := strings.LastIndex(string(cut), " "); space >= 0 && len([]rune(string(cut)[:space])) > limit*6/10 {
		return string(cut)[:space] + "…"
	}
	return string(cut) + "…"
}

var quoteMarkers = []*regexp.Regexp{
	regexp.MustCompile(`(?im)^\s*-{3,}\s*original message\s*-{3,}`),
	regexp.MustCompile(`(?im)^\s*-{3,}\s*ursprungligt meddelande\s*-{3,}`),
	regexp.MustCompile(`(?m)^\s*_{5,}\s*$`),
	regexp.MustCompile(`(?im)^\s*from:\s[^\n]+\n(?:[^\n]*\n){0,3}?\s*sent:\s`),
	regexp.MustCompile(`(?im)^\s*från:\s[^\n]+\n(?:[^\n]*\n){0,3}?\s*skickat:\s`),
	regexp.MustCompile(`(?im)^\s*on .{4,120}\bwrote:\s*$`),
	regexp.MustCompile(`(?im)^\s*den .{4,120}\bskrev\b[^\n]{0,80}:\s*$`),
}

// Unquote drops the quoted thread under a reply, in English and Swedish.
func Unquote(body string) string {
	earliest := len(body)
	for _, marker := range quoteMarkers {
		if found := marker.FindStringIndex(body); found != nil && found[0] < earliest {
			earliest = found[0]
		}
	}
	if head := strings.TrimSpace(body[:earliest]); head != "" {
		return head
	}
	return body
}
