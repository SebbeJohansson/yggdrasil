// Package sindri is Yggdrasil's coding orchestrator, named for the dwarf
// smith who forged Mjölnir. It starts and tracks coding agents (Claude Code
// or GitHub Copilot CLI) in your repos, each in its own worktree, and
// reviews, checks, merges and publishes what they make.
//
// It runs on the host (a systemd user unit): an agent needs your repos, git
// and ssh setup and toolchain at their real paths. Ported from my-agent's
// runner; goals, approvals and Linear move here next.
package sindri

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"yggdrasil/internal/part"
)

// Name is this part's name.
const Name = "sindri"

// Sindri is the running orchestrator.
type Sindri struct {
	Agents *Manager
	Root   string
	// ClaudeProjects is where Claude Code keeps transcripts (~/.claude/projects).
	ClaudeProjects string
	// Sessions reads every coding-agent session on the machine: Claude Code, OpenCode, herdr.
	Sessions *AgentSessions
	// Work hands work to agents and follows them; nil serves the agents alone.
	Work *Work
	// PromptsKeep is how long your indexed prompts are kept (sessionprompts.go); 0 is forever.
	PromptsKeep time.Duration
	// Cloud is the cloud sessions started here (cloud.go); nil is off.
	Cloud *Cloud
	// Usage is the Claude usage ledger (usage.go); nil is off.
	Usage *Usage
	// Upgrader upgrades this machine to a release (upgrade.go); nil is off.
	Upgrader *Upgrader
}

// Mount adds Sindri's routes and loops to the part.
func (s *Sindri) Mount(p *part.Part) {
	p.Handle("GET /v1/agents", s.list)
	p.Handle("GET /v1/complete", s.completeEngine)
	p.Handle("POST /v1/complete", s.complete)
	p.Handle("GET /v1/usage", s.usage)
	p.Handle("GET /v1/claude-login", s.claudeLoginState)
	p.Handle("POST /v1/claude-login", s.claudeLoginStart)
	p.Handle("POST /v1/claude-login/code", s.claudeLoginCode)
	p.Handle("GET /v1/sessions/recent", s.sessions)
	s.mountSessionPrompts(p)
	p.Handle("GET /v1/activity", s.activity)
	p.Handle("POST /v1/agents", s.start)
	p.Handle("POST /v1/agents/close-idle", func(w http.ResponseWriter, _ *http.Request) {
		a, err := s.Agents.CloseOldestIdle()
		if err != nil {
			fail(w, err)
			return
		}
		s.Agents.mu.Lock()
		defer s.Agents.mu.Unlock()
		part.JSON(w, http.StatusOK, Summarise(a))
	})
	p.Handle("GET /v1/agents/{id}", s.get)
	p.Handle("POST /v1/agents/{id}/prompt", only(s.prompt))
	p.Handle("POST /v1/agents/{id}/handoff", only(s.handoff))
	p.Handle("POST /v1/agents/{id}/resume-scheduled", s.resumeScheduled)
	p.Handle("POST /v1/agents/{id}/label", s.label)
	p.Handle("POST /v1/agents/{id}/permissions/{requestId}", only(s.answer, "norn"))
	p.Handle("POST /v1/agents/{id}/abort", s.agent(func(id string) (*Agent, error) { return s.Agents.Abort(id) }))
	p.Handle("POST /v1/agents/{id}/remote", s.agent(func(id string) (*Agent, error) { return s.Agents.Remote(id) }))
	p.Handle("POST /v1/agents/{id}/fix-conflicts", s.agent(func(id string) (*Agent, error) { return s.Agents.FixConflicts(id) }))
	p.Handle("GET /v1/upgrade", s.upgradeStatus)
	p.Handle("POST /v1/upgrade", only(s.startUpgrade))
	p.Handle("POST /v1/agents/{id}/move-onto", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Onto string `json:"onto"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		answer(w)(s.Agents.MoveOnto(r.PathValue("id"), body.Onto))
	})
	p.Handle("GET /v1/agents/{id}/review", func(w http.ResponseWriter, r *http.Request) {
		answer(w)(s.Agents.Review(r.PathValue("id"), r.URL.Query().Get("commit")))
	})
	p.Handle("GET /v1/agents/{id}/screenshots/{name}", s.screenshot)
	p.Handle("GET /v1/agents/{id}/checks", func(w http.ResponseWriter, r *http.Request) {
		answer(w)(s.Agents.Checks(r.PathValue("id")))
	})
	p.Handle("POST /v1/agents/{id}/checks", s.startChecks)
	p.Handle("POST /v1/agents/{id}/merge", func(w http.ResponseWriter, r *http.Request) {
		done(w)(s.Agents.Merge(r.PathValue("id")))
	})
	p.Handle("POST /v1/agents/{id}/pull-request", func(w http.ResponseWriter, r *http.Request) {
		answer(w)(s.Agents.PullRequest(r.PathValue("id"), s.closes(r.PathValue("id"))))
	})
	p.Handle("POST /v1/agents/{id}/push-base", func(w http.ResponseWriter, r *http.Request) {
		done(w)(s.Agents.PushBase(r.PathValue("id")))
	})
	p.Handle("POST /v1/agents/{id}/dismiss", func(w http.ResponseWriter, r *http.Request) {
		label, err := s.Agents.Dismiss(r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		part.JSON(w, http.StatusOK, map[string]string{"done": "dismissed " + label})
	})
	p.Handle("POST /v1/agents/{id}/remove-worktree", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Force bool `json:"force"`
		}
		_ = part.Decode(r, &body)
		done(w)(s.Agents.RemoveWorktree(r.PathValue("id"), body.Force))
	})
	p.Handle("POST /v1/uploads", s.uploads)
	p.Handle("GET /v1/worktrees", func(w http.ResponseWriter, _ *http.Request) {
		part.JSON(w, http.StatusOK, s.Agents.Worktrees())
	})
	p.Handle("GET /v1/worktrees/review", func(w http.ResponseWriter, r *http.Request) {
		answer(w)(s.Agents.OnUnowned(r.URL.Query().Get("path"), "review", r.URL.Query().Get("commit"), false))
	})
	for _, step := range []string{"merge", "remove", "move", "push-base", "pull-request"} {
		p.Handle("POST /v1/worktrees/"+step, s.unowned(step))
	}
	p.Handle("GET /v1/projects", func(w http.ResponseWriter, _ *http.Request) {
		part.JSON(w, http.StatusOK, ProjectInfos(s.Root))
	})
	p.Handle("POST /v1/projects", s.createProject)
	p.Handle("GET /v1/projects/{name}/docs", func(w http.ResponseWriter, r *http.Request) {
		answer(w)(ReadProjectDocs(s.Root, r.PathValue("name")))
	})
	p.Handle("GET /v1/projects/{name}/release", s.projectRelease)
	p.Handle("POST /v1/projects/{name}/release", only(s.startProjectRelease))
	if s.Work != nil {
		s.Work.MountWork(p)
		p.Loop("feed", 3*time.Second, s.Work.Feed)
		s.Work.mountNight(p)
		s.Work.mountSelfRepair(p)
		s.Work.mountRaces(p)
		s.Work.mountWhy(p)
		p.Loop("night", time.Minute, s.Work.RunNight)
		if s.Work.Linear.On() {
			s.Work.linearFailed = map[int64]time.Time{}
			p.Loop("linear", 30*time.Second, s.Work.SyncLinear)
		}
	}
	if s.Sessions == nil {
		s.Sessions = NewAgentSessions(ClaudeRoot{}, nil, nil)
	}
	{
		s.mountSessions(p)
		s.mountTranscripts(p)
		s.mountCloud(p)
		if s.Sessions.Opencode != nil {
			p.Loop("opencode", 3*time.Second, s.Sessions.ReadOpencode)
		}
		if s.Sessions.Herdr != nil {
			p.Loop("herdr", 2*time.Second, func(ctx context.Context) error {
				_ = s.Sessions.ReadHerdr(ctx)
				return s.Agents.RefreshHerdr(ctx)
			})
		}
	}
	p.Loop("reap", time.Minute, func(context.Context) error {
		s.Agents.ReapIdle()
		return nil
	})
}

// fail answers an error with its status: an HTTPError's own, 500 otherwise.
func fail(w http.ResponseWriter, err error) {
	var refused *HTTPError
	if errors.As(err, &refused) {
		if refused.Code != "" {
			part.JSON(w, refused.Status, map[string]string{"error": refused.Message, "code": refused.Code})
			return
		}
		part.Error(w, refused.Status, refused.Message)
		return
	}
	part.Error(w, http.StatusInternalServerError, err.Error())
}

func answer(w http.ResponseWriter) func(any, error) {
	return func(value any, err error) {
		if err != nil {
			fail(w, err)
			return
		}
		part.JSON(w, http.StatusOK, value)
	}
}

func done(w http.ResponseWriter) func(string, error) {
	return func(text string, err error) {
		if err != nil {
			fail(w, err)
			return
		}
		part.JSON(w, http.StatusOK, map[string]string{"done": text})
	}
}

func (s *Sindri) agent(step func(id string) (*Agent, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, err := step(r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		part.JSON(w, http.StatusOK, Summarise(a))
	}
}

func (s *Sindri) list(w http.ResponseWriter, _ *http.Request) {
	agents := s.Agents.List()
	summaries := make([]map[string]any, len(agents))
	s.Agents.mu.Lock()
	for i, a := range agents {
		summaries[i] = Summarise(a)
	}
	s.Agents.mu.Unlock()
	part.JSON(w, http.StatusOK, summaries)
}

func (s *Sindri) start(w http.ResponseWriter, r *http.Request) {
	var body StartRequest
	if err := part.Decode(r, &body); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if part.Caller(r) != part.Owner && !HelperOnly(body) {
		part.Error(w, http.StatusForbidden, "only you start an agent with tools, a folder or a mode; another part asks for one with `noTools`")
		return
	}
	// No folder: a plain agent (a rewrite, a question) runs where question agents do.
	if body.Cwd == "" && s.Work != nil {
		body.Cwd = s.Work.AskCwd
	}
	a, err := s.Agents.Start(body)
	if err != nil {
		fail(w, err)
		return
	}
	s.Agents.mu.Lock()
	summary := Summarise(a)
	s.Agents.mu.Unlock()
	part.JSON(w, http.StatusCreated, summary)
}

// HelperOnly is an agent another part may start: no tools, no folder of its
// own, no worktree and the default mode, like Norn's rewrite. A part's
// token, leaked or misused, then never gets a shell on this machine.
func HelperOnly(body StartRequest) bool {
	return body.NoTools && len(body.AllowedTools) == 0 && body.Cwd == "" && !body.AddRoots && !body.Worktree &&
		(body.PermissionMode == "" || body.PermissionMode == "default")
}

// only lets you call a route, and the parts named beside you.
func only(handler http.HandlerFunc, parts ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if caller := part.Caller(r); caller != part.Owner && !slices.Contains(parts, caller) {
			part.Error(w, http.StatusForbidden, "only you can do that")
			return
		}
		handler(w, r)
	}
}

func (s *Sindri) get(w http.ResponseWriter, r *http.Request) {
	a, err := s.Agents.Get(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	s.Agents.mu.Lock()
	defer s.Agents.mu.Unlock()
	full := Summarise(a)
	full["result"], full["log"] = a.Result, a.Log
	if a.Check != nil {
		full["check"] = a.Check
	}
	pending := []Pending{}
	for _, p := range a.Pending {
		pending = append(pending, p)
	}
	full["pending"] = pending
	part.JSON(w, http.StatusOK, full)
}

func (s *Sindri) prompt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text   string   `json:"text"`
		Images []string `json:"images"`
		Queue  bool     `json:"queue"`
	}
	if err := part.Decode(r, &body); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	s.agent(func(id string) (*Agent, error) { return s.Agents.Prompt(id, body.Text, body.Images, body.Queue) })(w, r)
}

func (s *Sindri) handoff(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
		To   string `json:"to"`
	}
	if err := part.Decode(r, &body); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	s.agent(func(id string) (*Agent, error) { return s.Agents.Handoff(id, body.Text, body.To) })(w, r)
}

func (s *Sindri) resumeScheduled(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := part.Decode(r, &body); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	s.agent(func(id string) (*Agent, error) { return s.Agents.ResumeScheduled(id, body.Text) })(w, r)
}

func (s *Sindri) label(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label string `json:"label"`
	}
	if err := part.Decode(r, &body); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	s.agent(func(id string) (*Agent, error) { return s.Agents.Rename(id, body.Label) })(w, r)
}

func (s *Sindri) answer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Allow   *bool             `json:"allow"`
		Message string            `json:"message"`
		Answers map[string]string `json:"answers"`
		Reply   string            `json:"reply"`
	}
	err := part.Decode(r, &body)
	if err == nil && (len(body.Answers) > 0 || body.Reply != "") {
		s.agent(func(id string) (*Agent, error) {
			return s.Agents.AnswerQuestion(id, r.PathValue("requestId"), body.Answers, body.Reply)
		})(w, r)
		return
	}
	if err != nil || body.Allow == nil {
		part.Error(w, http.StatusBadRequest, "`allow` must be true or false, or send `answers` or `reply` to a question")
		return
	}
	s.agent(func(id string) (*Agent, error) {
		return s.Agents.Answer(id, r.PathValue("requestId"), *body.Allow, body.Message)
	})(w, r)
}

func (s *Sindri) screenshot(w http.ResponseWriter, r *http.Request) {
	data, kind, err := s.Agents.Screenshot(r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", kind)
	w.Header().Set("Cache-Control", "private, max-age=300")
	_, _ = w.Write(data)
}

func (s *Sindri) startChecks(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind   string `json:"kind"`
		Script string `json:"script"`
		Force  bool   `json:"force"`
	}
	if err := part.Decode(r, &body); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Kind == "" {
		body.Kind = "script"
	}
	if body.Script == "" {
		body.Script = "test"
	}
	if body.Kind != "script" && body.Kind != "workflows" {
		part.Error(w, http.StatusBadRequest, "`kind` must be \"script\" or \"workflows\"")
		return
	}
	if body.Script != "test" && body.Script != "check" {
		part.Error(w, http.StatusBadRequest, "`script` must be \"test\" or \"check\"")
		return
	}
	answer(w)(s.Agents.StartChecks(r.PathValue("id"), body.Kind, body.Script, body.Force))
}

func (s *Sindri) uploads(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, (MaxUploadBytes*4/3+1024)*MaxUploadFiles)
	var body UploadRequest
	if err := decodeLarge(r, &body); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	files, err := SaveUploads(body)
	if err != nil {
		fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"files": files})
}

func (s *Sindri) unowned(step string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path  string `json:"path"`
			Force bool   `json:"force"`
		}
		if err := part.Decode(r, &body); err != nil {
			part.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		result, err := s.Agents.OnUnowned(body.Path, step, "", step == "remove" && body.Force)
		if err != nil {
			fail(w, err)
			return
		}
		if text, ok := result.(string); ok {
			part.JSON(w, http.StatusOK, map[string]string{"done": text})
			return
		}
		part.JSON(w, http.StatusOK, result)
	}
}

func (s *Sindri) createProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		// Repo, owner/name or its github.com link, clones it instead of starting an empty repo.
		Repo string `json:"repo"`
	}
	if err := part.Decode(r, &body); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	var project ProjectInfo
	var err error
	if body.Repo != "" {
		project, err = CloneProject(r.Context(), s.Root, body.Repo, body.Name)
	} else {
		project, err = CreateProject(s.Root, body.Name)
	}
	if err != nil {
		fail(w, err)
		return
	}
	part.JSON(w, http.StatusCreated, project)
}

// decodeLarge reads a body past part.MaxBody: uploads carry files.
func decodeLarge(r *http.Request, v any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(v)
}
