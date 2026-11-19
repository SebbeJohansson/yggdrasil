package norn

// Actions from a meeting: when you press Done, the dashboard sends what you
// wrote during the meeting, the local model suggests the actions in it, and
// each one you approve becomes a Linear issue assigned to you. The notes go to
// the local model only (they are as private as the vault), and nothing reaches
// Linear without your click.

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"yggdrasil/internal/linear"
	"yggdrasil/internal/llm"
	"yggdrasil/internal/part"
)

const (
	// maxActions is the most actions suggested for one meeting.
	maxActions = 8
	// defaultActionTeam is the Linear team a suggested action goes to unless you pick another.
	defaultActionTeam = "AI"
)

// MeetingAction is one thing to do that came out of a meeting.
type MeetingAction struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// actionsFormat is the JSON the model answers with.
var actionsFormat = map[string]any{
	"type":     "object",
	"required": []string{"actions"},
	"properties": map[string]any{
		"actions": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":     "object",
				"required": []string{"title", "description"},
				"properties": map[string]any{
					"title":       map[string]any{"type": "string"},
					"description": map[string]any{"type": "string"},
				},
			},
		},
	},
}

// ActionMessages asks for the actions in a meeting's notes.
func ActionMessages(subject string, attendees []string, notes string) []llm.Message {
	system := `You read the notes someone took during a meeting and list the actions in them: things the note taker or the team has to do afterwards, such as checking something, deciding, asking someone, building or changing something.
Rules:
- Only actions the notes say or clearly imply. A fact, an opinion or an open question is not an action unless the notes say it must be found out or settled; then the action is to find out or settle it.
- One action per thing to do. Merge lines about the same thing.
- title: a short imperative, at most 80 characters, in the notes' language, naming who to ask when the notes name someone.
- description: one to three sentences in the notes' language with the context from the notes the action needs. Nothing the notes do not say.
- No actions in the notes: an empty list.
Answer with JSON only: {"actions":[{"title":"…","description":"…"}]}`
	var user strings.Builder
	user.WriteString("Meeting: " + subject + "\n")
	if len(attendees) > 0 {
		user.WriteString("Attendees: " + strings.Join(attendees, ", ") + "\n")
	}
	user.WriteString("\nNotes:\n" + notes)
	return []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: user.String()}}
}

// ParseActions is the model's answer as actions: blank and repeated titles
// dropped, titles cut to 120 characters, at most maxActions.
func ParseActions(raw string) ([]MeetingAction, error) {
	raw = strings.TrimSpace(raw)
	if from, to := strings.Index(raw, "{"), strings.LastIndex(raw, "}"); from >= 0 && to > from {
		raw = raw[from : to+1]
	}
	var answer struct {
		Actions []MeetingAction `json:"actions"`
	}
	if err := json.Unmarshal([]byte(raw), &answer); err != nil {
		return nil, err
	}
	actions := []MeetingAction{}
	seen := map[string]bool{}
	for _, action := range answer.Actions {
		title := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(action.Title), "."))
		if runes := []rune(title); len(runes) > 120 {
			title = string(runes[:120])
		}
		if title == "" || seen[strings.ToLower(title)] {
			continue
		}
		seen[strings.ToLower(title)] = true
		actions = append(actions, MeetingAction{Title: title, Description: strings.TrimSpace(action.Description)})
		if len(actions) == maxActions {
			break
		}
	}
	return actions, nil
}

func (n *Norn) mountMeetingActions(p *part.Part) {
	p.Handle("POST /v1/notes/meeting/actions", n.suggestMeetingActions)
	p.Handle("GET /v1/linear/teams", n.linearTeams)
	p.Handle("POST /v1/linear/issues", n.createLinearIssue)
}

// suggestMeetingActions asks the local model for the actions in a meeting's notes.
func (n *Norn) suggestMeetingActions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Subject   string   `json:"subject"`
		Start     int64    `json:"start"`
		Attendees []string `json:"attendees"`
		Text      string   `json:"text"`
	}
	if err := part.Decode(r, &body); err != nil || strings.TrimSpace(body.Subject) == "" {
		part.Error(w, http.StatusBadRequest, "`subject` is the meeting's")
		return
	}
	text := strings.TrimSpace(body.Text)
	if len([]rune(text)) > maxLiveNote {
		part.Error(w, http.StatusBadRequest, "`text` is at most 20000 characters")
		return
	}
	answer := map[string]any{"actions": []MeetingAction{}, "team": defaultActionTeam}
	if text == "" {
		part.JSON(w, http.StatusOK, answer)
		return
	}
	// The same notes, for the promises you made in the meeting (promises.go).
	if body.Start <= 0 {
		body.Start = time.Now().UnixMilli()
	}
	n.OfferNotes(Meeting{Subject: body.Subject, Start: body.Start, Attendees: body.Attendees}, text, "")
	raw, err := n.LLM.Chat(r.Context(), ActionMessages(body.Subject, body.Attendees, text), llm.Options{
		Purpose: "meeting actions", Model: n.Config.AskModel, Temperature: 0, MaxTokens: 1200,
		Format: actionsFormat, Timeout: 90 * time.Second,
	})
	if err != nil {
		part.Error(w, http.StatusBadGateway, "the local model did not answer: "+err.Error())
		return
	}
	actions, err := ParseActions(raw)
	if err != nil {
		part.Error(w, http.StatusBadGateway, "the local model's answer was not the actions")
		return
	}
	answer["actions"] = actions
	part.JSON(w, http.StatusOK, answer)
}

const errNoLinear = "no LINEAR_KEY in norn.env"

// linearTeams is the teams an action can go to, and the one it goes to by
// default; `on` is false and the list empty without a Linear key.
func (n *Norn) linearTeams(w http.ResponseWriter, r *http.Request) {
	if !n.Linear.On() {
		part.JSON(w, http.StatusOK, map[string]any{"on": false, "teams": []linear.Team{}, "team": defaultActionTeam})
		return
	}
	teams, err := n.Linear.Teams(r.Context())
	if err != nil {
		part.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"on": true, "teams": teams, "team": defaultActionTeam})
}

// createLinearIssue makes an issue assigned to you, from an action you
// approved, and links it under "Åtgärder" in the meeting's Obsidian note when
// `meeting` names one.
func (n *Norn) createLinearIssue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Team        string   `json:"team"`
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Meeting     *Meeting `json:"meeting"`
	}
	if err := part.Decode(r, &body); err != nil {
		part.Error(w, http.StatusBadRequest, "send `team`, `title` and `description`")
		return
	}
	body.Team, body.Title = strings.TrimSpace(body.Team), strings.TrimSpace(body.Title)
	if body.Team == "" || body.Title == "" || len([]rune(body.Title)) > 250 || len([]rune(body.Description)) > 10000 {
		part.Error(w, http.StatusBadRequest, "`team` is a team key, `title` at most 250 characters, `description` at most 10000")
		return
	}
	if !n.Linear.On() {
		part.Error(w, http.StatusServiceUnavailable, errNoLinear)
		return
	}
	issue, err := n.Linear.Create(r.Context(), linear.NewIssue{Team: body.Team, Title: body.Title, Description: strings.TrimSpace(body.Description)})
	if err != nil {
		part.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	answer := map[string]any{"identifier": issue.Identifier, "title": issue.Title, "url": issue.URL}
	// The issue is made either way; a note that could not be written is only said.
	if meeting := body.Meeting; meeting != nil && strings.TrimSpace(meeting.Subject) != "" && meeting.Start > 0 && n.Vault.On() {
		now := time.Now()
		n.vaultMu.Lock()
		if len(meeting.Attendees) == 0 {
			meeting.Attendees = n.findMeeting(meeting.Subject, now).Attendees
		}
		note, _, err := n.Vault.AddMeetingIssue(*meeting, issue.Identifier, issue.Title, issue.URL, n.Zone, now)
		n.vaultMu.Unlock()
		if err != nil {
			answer["noteError"] = err.Error()
		} else {
			answer["note"] = note
		}
	}
	part.JSON(w, http.StatusCreated, answer)
}
