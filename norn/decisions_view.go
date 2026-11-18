package norn

// The decisions page's view of each dispatch decision, ported from my-agent
// (tasks/decisionsView.ts): the live pick, the model's own before the guards
// changed it, the shadow Laya's pick, your correction, and whether they
// agree. Corrections are written the way the correction route takes them:
// answer, delegate, code:<project>, reply_to_agent:<agent id>, status:<agent id>.

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// Pick is one side's choice: the kind, and its agent (reply, status) or project (code).
type Pick struct {
	Action string  `json:"action"`
	Target *string `json:"target"`
}

// ShadowPick is the shadow's pick, with its time and confidence.
type ShadowPick struct {
	Pick
	MS         int64   `json:"ms"`
	Confidence float64 `json:"confidence"`
}

// CorrectionView is where you said a message belonged.
type CorrectionView struct {
	Action string  `json:"action"`
	Target *string `json:"target"`
	At     int64   `json:"at"`
}

// DecisionView is one decision as the decisions page shows it.
type DecisionView struct {
	ID           int64           `json:"id"`
	At           int64           `json:"at"`
	Message      string          `json:"message"`
	Source       string          `json:"source"`
	Live         Pick            `json:"live"`
	BeforeGuards *Pick           `json:"beforeGuards,omitempty"`
	Guards       []string        `json:"guards"`
	MS           *int64          `json:"ms"`
	Shadow       *ShadowPick     `json:"shadow"`
	Correction   *CorrectionView `json:"correction"`
	Agrees       *bool           `json:"agrees"`
	ShadowRight  *bool           `json:"shadowRight"`
	LiveRef      *string         `json:"liveRef"`
	ShadowRef    *string         `json:"shadowRef"`
}

// DecisionsSummary counts the decisions, and how the shadow did.
type DecisionsSummary struct {
	Decisions              int `json:"decisions"`
	Shadowed               int `json:"shadowed"`
	Agree                  int `json:"agree"`
	Corrected              int `json:"corrected"`
	ShadowRightOnCorrected int `json:"shadowRightOnCorrected"`
	ShadowedCorrected      int `json:"shadowedCorrected"`
	MedianMS               struct {
		Live   *int64 `json:"live"`
		Shadow *int64 `json:"shadow"`
	} `json:"medianMs"`
}

// CorrectionPattern is the correction route's spelling of a pick.
var CorrectionPattern = regexp.MustCompile(`^(answer|delegate|code|reply_to_agent|status)(?::([\w.-]{1,64}))?$`)

func pickKind(action string) string {
	if action == "reply_to_agent" {
		return "reply"
	}
	return action
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type rawDecision struct {
	Action  string `json:"action"`
	Agent   string `json:"agent"`
	Project string `json:"project"`
	AgentID string `json:"agentId"`
}

func livePick(d rawDecision) Pick {
	action := pickKind(d.Action)
	switch action {
	case "code":
		return Pick{action, strPtrOrNil(d.Project)}
	case "reply", "status":
		return Pick{action, strPtrOrNil(d.Agent)}
	}
	return Pick{action, nil}
}

func samePick(a, b Pick) bool {
	if a.Action != b.Action {
		return false
	}
	if a.Action != "reply" && a.Action != "status" {
		return true
	}
	return (a.Target == nil && b.Target == nil) || (a.Target != nil && b.Target != nil && *a.Target == *b.Target)
}

func pickRef(action, agentID, project string) *string {
	switch action {
	case "answer", "delegate":
		return &action
	case "code":
		if project != "" {
			return strPtrOrNil("code:" + project)
		}
	case "reply":
		if agentID != "" {
			return strPtrOrNil("reply_to_agent:" + agentID)
		}
	case "status":
		if agentID != "" {
			return strPtrOrNil("status:" + agentID)
		}
	}
	return nil
}

func boolPtr(b bool) *bool { return &b }

// ViewDecision is a decision row as the page shows it.
func ViewDecision(row DecisionRow) DecisionView {
	view := DecisionView{ID: row.ID, At: row.At, Message: row.Message, Source: row.Source, MS: row.MS, Guards: []string{}}
	var decision rawDecision
	hasDecision := json.Unmarshal(row.Decision, &decision) == nil && decision.Action != ""
	view.Live = Pick{Action: "answer"}
	if hasDecision {
		view.Live = livePick(decision)
	}
	var model rawDecision
	if json.Unmarshal(row.Model, &model) == nil && model.Action != "" {
		if before := livePick(model); !samePick(before, view.Live) {
			view.BeforeGuards = &before
		}
	}
	_ = json.Unmarshal(row.Guards, &view.Guards)
	if view.Guards == nil {
		view.Guards = []string{}
	}

	var shadow struct {
		Action        string  `json:"action"`
		AgentID       *string `json:"agentId"`
		Probabilities struct {
			Action map[string]float64 `json:"action"`
			Agent  map[string]float64 `json:"agent"`
		} `json:"probabilities"`
		MS int64 `json:"ms"`
	}
	hasShadow := json.Unmarshal(row.Shadow, &shadow) == nil && shadow.Action != ""
	if hasShadow {
		action := pickKind(shadow.Action)
		names := make([]string, 0, len(shadow.Probabilities.Agent))
		for name := range shadow.Probabilities.Agent {
			names = append(names, name)
		}
		sort.SliceStable(names, func(i, j int) bool {
			return shadow.Probabilities.Agent[names[i]] > shadow.Probabilities.Agent[names[j]]
		})
		var target *string
		// "none" is the shadow saying no agent; agentId is null then too.
		if (action == "reply" || action == "status") && shadow.AgentID != nil && len(names) > 0 && names[0] != "none" {
			target = &names[0]
		}
		view.Shadow = &ShadowPick{Pick: Pick{action, target}, MS: shadow.MS, Confidence: shadow.Probabilities.Action[shadow.Action]}
	}

	var said struct {
		Decision json.RawMessage `json:"decision"`
		At       int64           `json:"at"`
	}
	if json.Unmarshal(row.Correction, &said) == nil && len(said.Decision) > 0 {
		var text string
		if json.Unmarshal(said.Decision, &text) != nil {
			// Written before corrections were strings: { action, agent, project }.
			var old rawDecision
			_ = json.Unmarshal(said.Decision, &old)
			text = old.Action
			if t := firstNonEmpty(old.Project, old.AgentID, old.Agent); t != "" {
				text += ":" + t
			}
		}
		if text != "" {
			action, target, _ := strings.Cut(text, ":")
			view.Correction = &CorrectionView{Action: pickKind(action), Target: strPtrOrNil(target), At: said.At}
		}
	}

	if view.Shadow != nil {
		view.Agrees = boolPtr(samePick(view.Shadow.Pick, view.Live))
	}
	if view.Correction != nil && view.Shadow != nil {
		// Only the action is judged: a correction names an agent by id, the shadow by name.
		view.ShadowRight = boolPtr(view.Shadow.Action == view.Correction.Action)
	}
	if hasDecision {
		view.LiveRef = pickRef(view.Live.Action, decision.AgentID, decision.Project)
	}
	if view.Shadow != nil {
		agentID := ""
		if shadow.AgentID != nil {
			agentID = *shadow.AgentID
		}
		view.ShadowRef = pickRef(view.Shadow.Action, agentID, "")
	}
	return view
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func median(values []int64) *int64 {
	if len(values) == 0 {
		return nil
	}
	sorted := append([]int64{}, values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return &sorted[len(sorted)/2]
}

// SummariseDecisions counts the views.
func SummariseDecisions(views []DecisionView) DecisionsSummary {
	var s DecisionsSummary
	s.Decisions = len(views)
	var live, shadow []int64
	for _, v := range views {
		if v.Shadow != nil {
			s.Shadowed++
			shadow = append(shadow, v.Shadow.MS)
			if v.Agrees != nil && *v.Agrees {
				s.Agree++
			}
		}
		if v.Correction != nil {
			s.Corrected++
			if v.ShadowRight != nil {
				s.ShadowedCorrected++
				if *v.ShadowRight {
					s.ShadowRightOnCorrected++
				}
			}
		}
		if v.Source == "model" && v.MS != nil {
			live = append(live, *v.MS)
		}
	}
	s.MedianMS.Live, s.MedianMS.Shadow = median(live), median(shadow)
	return s
}
