package norn

// Cloud mode: a switch in the dashboard's settings that sends every call
// that would go to the local model (XeStrata or Ollama) to Claude Code or
// Copilot CLI instead, through Sindri (llm/cloud.go, sindri/complete.go),
// for a machine that cannot run a model. Embeddings stay where they are.
//
// What leaves the machine is set by the privacy level beside it:
//
//	as-is            sent unchanged (the default, and the most exposed)
//	placeholders     the people Norn knows and email addresses swapped for
//	                 Person 1, Person 2… and swapped back in the answer
//	local-sensitive  as placeholders, and what reads your mail and chats
//	                 (cloudKeptLocal) is not sent at all: it fails instead

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"yggdrasil/internal/llm"
	"yggdrasil/internal/part"
	"yggdrasil/internal/pseudo"
)

const modelModeKey = "model-mode"

// Model modes and privacy levels.
const (
	ModeLocal = "local"
	ModeCloud = "cloud"

	PrivacyAsIs           = "as-is"
	PrivacyPlaceholders   = "placeholders"
	PrivacyLocalSensitive = "local-sensitive"
)

// cloudKeptLocal is the purposes that read your mail and chats: with
// local-sensitive, they are not sent to the cloud.
var cloudKeptLocal = []string{"summary", "reply draft", "reply draft check", "judge", "person recap",
	"promises", "promise kept", "meeting actions", "rewind", "weave"}

// ModelMode is where model calls go, and what may leave the machine.
type ModelMode struct {
	Mode    string `json:"mode"`
	Privacy string `json:"privacy"`
}

// ModelModeView is ModelMode with the engine cloud mode answers on.
type ModelModeView struct {
	ModelMode
	// Engine is claude, copilot, or "" when neither is installed (or Sindri is off).
	Engine      string `json:"engine"`
	EngineLabel string `json:"engineLabel"`
}

// ModelMode is the stored setting: local, as-is, until it is changed.
func (n *Norn) ModelMode(ctx context.Context) ModelMode {
	mode := ModelMode{Mode: ModeLocal, Privacy: PrivacyAsIs}
	if _, err := n.Store.Setting(ctx, modelModeKey, &mode); err != nil {
		n.logf("reading the model mode: %v", err)
	}
	return mode
}

// StartModelMode hands the model client to the cloud through Sindri, and
// turns cloud mode on when it was left on.
func (n *Norn) StartModelMode(ctx context.Context) {
	n.LLM.Cloud = n.askCloud
	n.LLM.Guard = n.guardCloud
	n.Config.Judge.CloudChat = n.LLM.Chat
	n.Config.Judge.Cloud = func() bool {
		return n.LLM.CloudOn() && n.ModelMode(context.Background()).Privacy != PrivacyLocalSensitive
	}
	n.LLM.SetCloud(n.ModelMode(ctx).Mode == ModeCloud)
}

// askCloud is one answer from Sindri's POST /v1/complete.
func (n *Norn) askCloud(ctx context.Context, system, prompt string) (llm.CloudAnswer, error) {
	if !n.Sindri.On() {
		return llm.CloudAnswer{}, errors.New("cloud mode needs Sindri, and SINDRI_TOKEN is unset")
	}
	var answer struct {
		Text   string `json:"text"`
		Engine string `json:"engine"`
		Model  string `json:"model"`
	}
	limit := 4 * time.Minute
	if deadline, ok := ctx.Deadline(); ok {
		limit = time.Until(deadline)
	}
	err := n.Sindri.callWithin(ctx, limit, http.MethodPost, "/v1/complete", map[string]string{"system": system, "prompt": prompt}, &answer)
	return llm.CloudAnswer{Text: answer.Text, Engine: answer.Engine, Model: answer.Model}, err
}

// guardCloud applies the privacy level to a call about to leave.
func (n *Norn) guardCloud(ctx context.Context, purpose string, messages []llm.Message) ([]llm.Message, func(string) string, error) {
	same := func(s string) string { return s }
	privacy := n.ModelMode(ctx).Privacy
	if privacy == PrivacyAsIs || n.NamesOff(ctx) {
		return messages, same, nil
	}
	if privacy == PrivacyLocalSensitive && slices.Contains(cloudKeptLocal, purpose) {
		return nil, nil, llm.ErrKeptLocal
	}
	m := &pseudo.Map{}
	known := n.Book(ctx).Group(n.knownPeople(ctx))
	out := make([]llm.Message, len(messages))
	all := ""
	for i, message := range messages {
		out[i] = llm.Message{Role: message.Role, Content: m.Pseudonymize(message.Content, known, nil)}
		if leaks := m.Leaks(out[i].Content, known); len(leaks) > 0 {
			return nil, nil, ErrNamesLeft
		}
		all += out[i].Content + "\n"
	}
	if legend := m.Legend(all); legend != "" {
		if len(out) > 0 && out[0].Role == "system" {
			out[0].Content += "\n\n" + legend
		} else {
			out = append([]llm.Message{{Role: "system", Content: legend}}, out...)
		}
	}
	return out, m.Restore, nil
}

// cloudEngine is the engine Sindri would answer on now.
func (n *Norn) cloudEngine(ctx context.Context) (string, string) {
	if !n.Sindri.On() {
		return "", ""
	}
	var engine struct {
		Engine string `json:"engine"`
		Label  string `json:"label"`
	}
	if err := n.Sindri.callWithin(ctx, 5*time.Second, http.MethodGet, "/v1/complete", nil, &engine); err != nil {
		return "", ""
	}
	return engine.Engine, engine.Label
}

func (n *Norn) modelModeView(ctx context.Context, mode ModelMode) ModelModeView {
	engine, label := n.cloudEngine(ctx)
	return ModelModeView{ModelMode: mode, Engine: engine, EngineLabel: label}
}

func (n *Norn) modelModeSettings(w http.ResponseWriter, r *http.Request) {
	part.JSON(w, http.StatusOK, n.modelModeView(r.Context(), n.ModelMode(r.Context())))
}

func (n *Norn) setModelMode(w http.ResponseWriter, r *http.Request) {
	var body ModelMode
	err := part.Decode(r, &body)
	if err != nil || !slices.Contains([]string{ModeLocal, ModeCloud}, body.Mode) ||
		!slices.Contains([]string{PrivacyAsIs, PrivacyPlaceholders, PrivacyLocalSensitive}, body.Privacy) {
		part.Error(w, http.StatusBadRequest, "`mode` must be local or cloud, and `privacy` as-is, placeholders or local-sensitive")
		return
	}
	if err := n.Store.SetSetting(r.Context(), modelModeKey, body); err != nil {
		n.fail(w, err)
		return
	}
	n.LLM.SetCloud(body.Mode == ModeCloud)
	n.logf("model mode: %s, privacy %s", body.Mode, body.Privacy)
	part.JSON(w, http.StatusOK, n.modelModeView(r.Context(), body))
}

