package norn

// Your own Claude Code sessions, started outside Norn, as Sindri reads them
// from their transcripts (GET /v1/sessions/recent). Read-only: for "how is
// the one-line comment going?", overviews, the local answer's background,
// and the hand-over, so a question agent needs no digging.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OwnSession is one of your own Claude Code sessions from the last day.
type OwnSession struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	Title        string `json:"title"`
	Cwd          string `json:"cwd"`
	Project      string `json:"project"`
	LastActivity int64  `json:"lastActivity"`
	LastPrompt   string `json:"lastPrompt"`
	LastText     string `json:"lastText"`
	Working      bool   `json:"working"`
}

// Sessions is your own sessions; empty when Sindri cannot say.
func (c *SindriClient) Sessions(ctx context.Context) []OwnSession {
	var sessions []OwnSession
	if c.call(ctx, http.MethodGet, "/v1/sessions/recent", nil, &sessions) != nil {
		return nil
	}
	return sessions
}

// LikelySession is the session a status question is about, when it names one
// of your sessions (by title or your last prompt) better than any agent.
func LikelySession(question string, sessions []OwnSession, agents []Agent, now time.Time) *OwnSession {
	asked := wordsOf(question)
	score := func(text string) int {
		n := 0
		for word := range wordsOf(text) {
			if asked[word] {
				n++
			}
		}
		return n
	}
	best, bestScore := (*OwnSession)(nil), 0
	for _, a := range RecentAgents(agents, now) {
		bestScore = max(bestScore, score(a.Label))
	}
	for i, s := range sessions {
		if value := score(s.Title + " " + s.LastPrompt); value >= 1 && value > bestScore {
			best, bestScore = &sessions[i], value
		}
	}
	return best
}

// SessionStatus answers "how is it going?" about one of your sessions, from its transcript.
func SessionStatus(s OwnSession, now time.Time) string {
	where := ""
	if s.Project != "" {
		where = " in " + s.Project
	}
	ago := max(1, int((now.UnixMilli()-s.LastActivity+30_000)/60_000))
	when := fmt.Sprintf("%d min ago", ago)
	if ago >= 60 {
		when = fmt.Sprintf("%d h ago", (ago+30)/60)
	}
	state := "last answered " + when
	if s.Working {
		state = "is still working"
	}
	said := ""
	if s.LastText != "" {
		said = " Its last message: " + s.LastText
	}
	return fmt.Sprintf("Your Claude Code session %q%s %s.%s", s.Title, where, state, said)
}

// SessionLines is one line per session, in the shape of OverviewLines.
func SessionLines(sessions []OwnSession) []string {
	var lines []string
	for _, s := range sessions[:min(8, len(sessions))] {
		project, state := s.Project, "idle"
		if project == "" {
			project = "your session"
		}
		if s.Working {
			state = "working"
		}
		line := fmt.Sprintf("%q (%s, your own Claude Code session): %s", s.Title, project, state)
		if s.LastText != "" {
			line += fmt.Sprintf("; last said %q", questionTail(s.LastText, 300))
		}
		lines = append(lines, line)
	}
	return lines
}

// SessionsSection is the hand-over's part about your sessions.
func SessionsSection(sessions []OwnSession) []string {
	if len(sessions) == 0 {
		return nil
	}
	lines := []string{"",
		"Their own Claude Code sessions from the last day, newest first. Usually the line below is enough.",
		"If not, open the transcript (JSONL) with the Read or Grep tool, which needs no approval -- not with",
		"Bash, which asks the person every time. `ai-title`, `last-prompt` and assistant `text` lines say what happened:"}
	for _, s := range sessions[:min(12, len(sessions))] {
		project, state := s.Project, "idle"
		if project == "" {
			project = "no project"
		}
		if s.Working {
			state = "working"
		}
		line := fmt.Sprintf("- %q (%s, %s)", s.Title, project, state)
		if s.Path != "" {
			line += " " + s.Path
		}
		if s.LastText != "" {
			line += fmt.Sprintf(" -- last said: %q", questionTail(s.LastText, 200))
		}
		lines = append(lines, line)
	}
	return lines
}

// OwnSessionsOverview is the overview's lines about your sessions.
func OwnSessionsOverview(sessions []OwnSession) []string {
	own := sessions[:min(8, len(sessions))]
	if len(own) == 0 {
		return nil
	}
	working := 0
	for _, s := range own {
		if s.Working {
			working++
		}
	}
	head := fmt.Sprintf("Your own Claude Code sessions: %d", len(own))
	if working > 0 {
		head += fmt.Sprintf(", %d working", working)
	}
	lines := []string{head + "."}
	for _, s := range own {
		project, state := s.Project, "idle"
		if project == "" {
			project = "no project"
		}
		if s.Working {
			state = "working"
		}
		lines = append(lines, fmt.Sprintf("• %q (%s): %s", s.Title, project, state))
	}
	return lines
}

// ReadLink is a line linking to read a session on the dashboard without
// taking it over; empty without a dashboard or a folder.
func ReadLink(dashboardURL, id, cwd, name, label string) string {
	if dashboardURL == "" || dashboardURL == "off" || cwd == "" {
		return ""
	}
	if label == "" {
		label = "Read the session"
	}
	query := strings.NewReplacer("(", "%28", ")", "%29").Replace(url.Values{"dir": {cwd}, "name": {name}}.Encode())
	return fmt.Sprintf("\n[%s](%s/dashboard/sessions/%s?%s)", label, strings.TrimRight(dashboardURL, "/"), url.PathEscape(id), query)
}

const (
	namedTitleMin = 6
	namedLinksMax = 3
)

// NamedReadLinks are read links for the sessions and agents a model-written
// answer names by their exact title; when any title is quoted, only quoted ones count.
func NamedReadLinks(answer string, sessions []OwnSession, agents []Agent, dashboardURL string) string {
	text := strings.NewReplacer("“", `"`, "”", `"`).Replace(strings.ToLower(answer))
	type named struct{ id, title, cwd string }
	var candidates []named
	seen := map[string]bool{}
	add := func(id, title, cwd string) {
		if len(title) >= namedTitleMin && strings.Contains(text, strings.ToLower(title)) && !seen[id] {
			seen[id] = true
			candidates = append(candidates, named{id, title, cwd})
		}
	}
	for _, s := range sessions {
		add(s.ID, s.Title, s.Cwd)
	}
	for _, a := range agents {
		add(a.ID, TaskOf(a.Label), a.Cwd)
	}
	var quoted []named
	for _, c := range candidates {
		if strings.Contains(text, `"`+strings.ToLower(c.title)+`"`) {
			quoted = append(quoted, c)
		}
	}
	if len(quoted) > 0 {
		candidates = quoted
	}
	var links []string
	for _, c := range candidates[:min(namedLinksMax, len(candidates))] {
		label := "Read the session"
		if len(candidates) > 1 {
			label = fmt.Sprintf("Read %q", c.title)
		}
		links = append(links, ReadLink(dashboardURL, c.id, c.cwd, c.title, label))
	}
	return strings.Join(links, "")
}
