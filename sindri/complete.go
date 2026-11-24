package sindri

// Complete: one answer from Claude Code or Copilot CLI, for Norn's cloud
// mode. A machine too slow for a local model sends every call that would go
// to XeStrata or Ollama here instead. Each call is one `-p` run with no
// tools, no session kept and no settings or hooks loaded, in an empty
// folder: a plain model answer, like the local model's, never an agent.
// Claude Code is used when it is installed and on, else Copilot CLI.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"yggdrasil/internal/part"
)

// CompleteTimeout is the longest one answer may take.
const CompleteTimeout = 3 * time.Minute

// CompleteRequest is what Norn asks.
type CompleteRequest struct {
	System string `json:"system"`
	Prompt string `json:"prompt"`
	// Model is the CLI's model (claude: haiku unless set; copilot: its default).
	Model string `json:"model"`
	// Feature is what asked, e.g. the Norn feature, for the usage ledger.
	Feature string `json:"feature"`
}

// CompleteAnswer is the answer, and who gave it.
type CompleteAnswer struct {
	Text   string `json:"text"`
	Engine string `json:"engine"`
	Model  string `json:"model"`
	// used is what the call cost, for the usage ledger.
	used UsageEntry
}

// CompleteEngine is which engine answers now, "" when neither can.
type CompleteEngine struct {
	Engine string `json:"engine"`
	Label  string `json:"label"`
}

// ErrNoCloud is a machine with neither Claude Code nor Copilot CLI.
var ErrNoCloud = errors.New("neither Claude Code nor Copilot CLI is installed and on")

// CompleteEngine is the engine a one-shot answer runs on now.
func (m *Manager) CompleteEngine() CompleteEngine {
	if m.ClaudeOn() && binUp(m.options.ClaudeBin) {
		return CompleteEngine{Engine: "claude", Label: "Claude Code"}
	}
	if m.CopilotOn() && binUp(m.options.CopilotBin) {
		return CompleteEngine{Engine: "copilot", Label: "Copilot CLI"}
	}
	return CompleteEngine{}
}

// Complete runs one answer.
func (m *Manager) Complete(ctx context.Context, req CompleteRequest) (CompleteAnswer, error) {
	engine := m.CompleteEngine().Engine
	if engine == "" {
		return CompleteAnswer{}, ErrNoCloud
	}
	dir, err := os.MkdirTemp("", "sindri-complete-")
	if err != nil {
		return CompleteAnswer{}, err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, CompleteTimeout)
	defer cancel()
	model := req.Model
	var command *exec.Cmd
	if engine == "claude" {
		if model == "" {
			model = "haiku"
		}
		args := []string{"-p", "--output-format", "json", "--tools", "", "--no-session-persistence",
			"--setting-sources", "", "--model", model}
		if req.System != "" {
			args = append(args, "--system-prompt", req.System)
		}
		// After "--", so a prompt starting with "-" is never read as an option.
		args = append(args, "--", req.Prompt)
		command = exec.CommandContext(ctx, m.options.ClaudeBin, args...)
	} else {
		prompt := req.Prompt
		if req.System != "" {
			prompt = req.System + "\n\n---\n\n" + req.Prompt
		}
		args := []string{"-p", prompt, "--output-format", "json", "--no-ask-user", "--no-color",
			"--deny-tool", "shell", "--deny-tool", "write", "--secret-env-vars", "SINDRI_TOKENS,NORN_TOKEN"}
		if model != "" {
			args = append(args, "--model", model)
		}
		command = exec.CommandContext(ctx, m.options.CopilotBin, args...)
	}
	command.Dir = dir
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	runErr := command.Run()
	var text string
	var used UsageEntry
	if engine == "claude" {
		used.Model, used.TokensIn, used.TokensOut, used.CostUSD = claudeTokens(stdout.Bytes())
		text, err = claudeResult(stdout.Bytes())
		if expired := (*loginExpiredError)(nil); errors.As(err, &expired) {
			m.login.MarkExpired(expired.text)
		}
	} else {
		text, err = copilotResult(stdout.Bytes())
	}
	if err != nil || runErr != nil {
		detail := strings.TrimSpace(stderr.String())
		if err != nil && (detail == "" || engine == "claude") {
			detail = err.Error()
		}
		if detail == "" {
			detail = runErr.Error()
		}
		if len(detail) > 300 {
			detail = detail[:300]
		}
		return CompleteAnswer{}, fmt.Errorf("%s did not answer: %s", engine, detail)
	}
	if used.Model == "" {
		used.Model = model
	}
	return CompleteAnswer{Text: text, Engine: engine, Model: model, used: used}, nil
}

// loginExpiredError is Claude failing because its sign-in ran out.
type loginExpiredError struct{ text string }

func (e *loginExpiredError) Error() string { return "claude: " + e.text + " " + LoginHint }

// claudeResult reads `claude -p --output-format json`: one object.
func claudeResult(out []byte) (string, error) {
	var body struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &body); err != nil {
		return "", fmt.Errorf("unreadable answer: %w", err)
	}
	if body.IsError {
		if loginExpired.MatchString(body.Result) {
			return "", &loginExpiredError{text: strings.TrimSpace(body.Result)}
		}
		return "", fmt.Errorf("claude: %s", body.Result)
	}
	return body.Result, nil
}

// copilotResult reads `copilot -p --output-format json`: JSON lines, the
// answer being the last assistant.message.
func copilotResult(out []byte) (string, error) {
	text, found := "", false
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for scanner.Scan() {
		var event struct {
			Type string `json:"type"`
			Data struct {
				Content string `json:"content"`
			} `json:"data"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if event.Type == "assistant.message" && strings.TrimSpace(event.Data.Content) != "" {
			text, found = event.Data.Content, true
		}
	}
	if !found {
		return "", errors.New("copilot gave no answer")
	}
	return text, nil
}

func (s *Sindri) complete(w http.ResponseWriter, r *http.Request) {
	var req CompleteRequest
	if err := part.Decode(r, &req); err != nil || strings.TrimSpace(req.Prompt) == "" {
		part.Error(w, http.StatusBadRequest, "`prompt` is required")
		return
	}
	answer, err := s.Agents.Complete(r.Context(), req)
	if errors.Is(err, ErrNoCloud) {
		part.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if err != nil {
		part.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	if answer.Engine == "claude" {
		e := answer.used
		e.Source, e.Caller, e.Feature = "complete", part.Caller(r), req.Feature
		s.Usage.Record(e)
	}
	part.JSON(w, http.StatusOK, answer)
}

func (s *Sindri) completeEngine(w http.ResponseWriter, _ *http.Request) {
	part.JSON(w, http.StatusOK, s.Agents.CompleteEngine())
}
