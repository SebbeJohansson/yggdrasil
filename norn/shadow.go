package norn

// The shadow Laya dispatcher: beside every dispatch decision, Laya is asked
// the same thing, and its answer is kept with the decision, never acted on.
// The decisions view compares the two, and your corrections say which was
// right. No LAYA_SHADOW_KEY: no shadow.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

var shadowStop = map[string]bool{}

func init() {
	for _, w := range strings.Fields("the a an to of in on at for with and or my it this that is are do does what which how many there can you please add make fix change update") {
		shadowStop[w] = true
	}
}

// clipFlat flattens whitespace and clips to limit code points.
func clipFlat(text string, limit int) string {
	flat := strings.Join(strings.Fields(text), " ")
	if runes := []rune(flat); len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return flat
}

var shadowWord = regexp.MustCompile(`[a-z0-9]+`)

// LayaName is a short, unique name for an agent from its task: "dm-titles".
func LayaName(task string, used map[string]bool) string {
	var words []string
	for _, w := range shadowWord.FindAllString(strings.ToLower(task), -1) {
		if !shadowStop[w] {
			words = append(words, w)
		}
	}
	base := strings.Join(words[:min(2, len(words))], "-")
	if base == "" {
		base = "agent"
	}
	name := base
	for n := 2; used[name]; n++ {
		name = fmt.Sprintf("%s-%d", base, n)
	}
	used[name] = true
	return name
}

type layaAgent struct {
	name, task, state, question string
}

// ShadowInput is what the shadow reads: the message, the last turn before it
// (who, text; who is an agent id, "person" or "Norn"), the agents and projects.
type ShadowInput struct {
	Message  string
	Last     *[2]string
	Agents   []Agent
	Projects []string
}

// ShadowDecision is what Laya would have done.
type ShadowDecision struct {
	Action        string  `json:"action"`
	AgentID       *string `json:"agentId"`
	Probabilities struct {
		Action map[string]float64 `json:"action"`
		Agent  map[string]float64 `json:"agent"`
	} `json:"probabilities"`
	MS int64 `json:"ms"`
}

var shadowActions = map[string]string{
	"answer":   "answer it itself: general knowledge, how-to, the person's own team, profile or unread inbox",
	"delegate": "look it up with tools: files or code in a project, the web, current information, calendar, tickets",
	"code":     "change code or files in one of the person's projects",
	"reply":    "pass it to an agent: an answer to that agent's question, or more about that agent's task",
	"status":   "report how an agent's task is going",
}

// ShadowState is the state Laya reads.
func ShadowState(input ShadowInput) (string, map[string]string, []layaAgent) {
	used, idOf := map[string]bool{}, map[string]string{}
	nameOf := map[string]string{}
	agents := make([]layaAgent, 0, len(input.Agents))
	for _, a := range input.Agents {
		task := TaskOf(a.Label)
		name := LayaName(task, used)
		idOf[name], nameOf[a.ID] = a.ID, name
		state := "finished"
		switch {
		case a.AsksYou:
			state = "asks"
		case slices.Contains([]string{"starting", "working", "needs-approval"}, a.Status):
			state = "working"
		}
		question := ""
		if a.AsksYou {
			question = a.LastText
		}
		agents = append(agents, layaAgent{name, task, state, question})
	}
	lines := []string{"message: " + clipFlat(input.Message, 200)}
	if input.Last != nil {
		who := input.Last[0]
		if name, ok := nameOf[who]; ok {
			who = name
		}
		lines = append(lines, "last said by "+who+": "+clipFlat(input.Last[1], 160))
	}
	listed := "none"
	if len(agents) > 0 {
		parts := make([]string, len(agents))
		for i, a := range agents {
			state := a.state
			if state == "asks" {
				state = "asks: " + clipFlat(a.question, 90)
			}
			parts[i] = a.name + " (" + clipFlat(a.task, 60) + "; " + state + ")"
		}
		listed = strings.Join(parts, "; ")
	}
	lines = append(lines, "agents: "+listed, "projects: "+strings.Join(input.Projects, ", "))
	return strings.Join(lines, "\n"), idOf, agents
}

// AskShadow asks Laya what it would have done.
func AskShadow(ctx context.Context, client *http.Client, url, key string, input ShadowInput, timeout time.Duration) (ShadowDecision, error) {
	state, idOf, agents := ShadowState(input)
	criteria := map[string]string{"none": "the message is not about any of these agents"}
	for _, a := range agents {
		criteria[a.name] = clipFlat(a.task, 70)
	}
	payload, _ := json.Marshal(map[string]any{
		"model": "typed-decisions",
		"state": state,
		"questions": map[string]any{
			"action": map[string]any{"type": "choice", "instructions": "What should the assistant do with the person's newest message?", "criteria": shadowActions},
			"agent":  map[string]any{"type": "choice", "instructions": "Which agent is the person's message for or about?", "criteria": criteria},
		},
	})
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(url, "/")+"/v1/systemone", strings.NewReader(string(payload)))
	if err != nil {
		return ShadowDecision{}, err
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return ShadowDecision{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ShadowDecision{}, fmt.Errorf("laya shadow answered %d", response.StatusCode)
	}
	var body struct {
		Answers map[string]struct {
			Choice        string             `json:"choice"`
			Probabilities map[string]float64 `json:"probabilities"`
		} `json:"answers"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return ShadowDecision{}, err
	}
	action, agent := body.Answers["action"], body.Answers["agent"]
	if action.Choice == "" || agent.Choice == "" {
		return ShadowDecision{}, fmt.Errorf("laya shadow answered without a choice")
	}
	var d ShadowDecision
	d.Action, d.MS = action.Choice, time.Since(start).Milliseconds()
	if id, ok := idOf[agent.Choice]; ok {
		d.AgentID = &id
	}
	d.Probabilities.Action, d.Probabilities.Agent = action.Probabilities, agent.Probabilities
	return d, nil
}

// shadow asks Laya in the background and keeps its answer with the decision.
func (n *Norn) shadow(decision int64, input ShadowInput) {
	if n.Config.LayaShadowKey == "" {
		return
	}
	go func() {
		d, err := AskShadow(context.Background(), n.HTTP, n.Config.LayaShadowURL, n.Config.LayaShadowKey, input, 30*time.Second)
		if err != nil {
			n.logf("the shadow dispatcher did not answer: %v", err)
			return
		}
		if err := n.Store.SetShadow(context.Background(), decision, d); err != nil {
			n.logf("could not keep the shadow's answer: %v", err)
		}
	}()
}

// WakeShadow loads the shadow model, so the first comparison is not lost to a cold start.
func (n *Norn) WakeShadow() {
	if n.Config.LayaShadowKey == "" {
		return
	}
	go func() {
		if _, err := AskShadow(context.Background(), n.HTTP, n.Config.LayaShadowURL, n.Config.LayaShadowKey, ShadowInput{Message: "hi"}, 2*time.Minute); err != nil {
			n.logf("could not wake the laya shadow: %v", err)
		}
	}()
}

// SetShadow keeps what the shadow would have done about a decision.
func (s *Store) SetShadow(ctx context.Context, id int64, shadow ShadowDecision) error {
	encoded, _ := json.Marshal(shadow)
	_, err := s.db.ExecContext(ctx, `UPDATE decisions SET shadow = ? WHERE id = ?`, string(encoded), id)
	return err
}
