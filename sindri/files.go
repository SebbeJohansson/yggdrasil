package sindri

// Checks run in a worktree, screenshots of a branch's changes, files sent
// along with a message, and project docs. Ported from my-agent's
// runner/src/checks.ts, screenshots.ts, uploads.ts and projects.ts.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- checks --------------------------------------------------------------------

// CheckResult is one check run in a worktree.
type CheckResult struct {
	Kind       string `json:"kind"` // script or workflows
	Script     string `json:"script,omitempty"`
	Status     string `json:"status"` // running, passed, failed, unavailable
	Head       string `json:"head"`
	Command    string `json:"command"`
	StartedAt  int64  `json:"startedAt"`
	DurationMS int64  `json:"durationMs,omitempty"`
	ExitCode   *int   `json:"exitCode,omitempty"`
	Output     string `json:"output"`
	Reason     string `json:"reason,omitempty"`
}

// Limits on checks.
const (
	checkOutputTail = 12_000
	checkTimeout    = 10 * time.Minute
)

func managerOf(dir string) string {
	switch {
	case exists(filepath.Join(dir, "yarn.lock")):
		return "yarn"
	case exists(filepath.Join(dir, "pnpm-lock.yaml")):
		return "pnpm"
	}
	return "npm"
}

func packageScripts(dir string) map[string]string {
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil || json.Unmarshal(data, &pkg) != nil {
		return map[string]string{}
	}
	return pkg.Scripts
}

// PlanCheck is the command a check runs, or why it can't.
func PlanCheck(dir, kind, script string) (command string, args []string, reason string) {
	if kind == "script" {
		if packageScripts(dir)[script] == "" {
			return "", nil, fmt.Sprintf(`package.json has no "%s" script`, script)
		}
		manager := managerOf(dir)
		if manager == "npm" {
			return manager, []string{"run", script}, ""
		}
		return manager, []string{script}, ""
	}
	entries, _ := os.ReadDir(filepath.Join(dir, ".github", "workflows"))
	workflows := slices.ContainsFunc(entries, func(e os.DirEntry) bool {
		return strings.HasSuffix(e.Name(), ".yml") || strings.HasSuffix(e.Name(), ".yaml")
	})
	if !workflows {
		return "", nil, "the repo has no .github/workflows to run"
	}
	if _, err := exec.LookPath("act"); err != nil {
		return "", nil, "`act` is not installed (https://github.com/nektos/act)"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, "docker", "info").Run() != nil {
		return "", nil, "Docker is not running, which `act` needs"
	}
	return "act", []string{"pull_request", "--pull=false"}, ""
}

type tail struct {
	mu   sync.Mutex
	text []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.text = append(t.text, p...)
	if len(t.text) > checkOutputTail {
		t.text = t.text[len(t.text)-checkOutputTail:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.text)
}

// RunCheck starts a check and returns it running; done is called with the
// result when it ends.
func RunCheck(w *Worktree, kind, script string, done func(CheckResult)) CheckResult {
	if kind == "script" && script == "" {
		script = "test"
	}
	if kind != "script" {
		script = ""
	}
	result := CheckResult{Kind: kind, Script: script, Head: w.Head(), StartedAt: time.Now().UnixMilli()}
	command, args, reason := PlanCheck(w.Path, kind, script)
	if reason != "" {
		result.Status, result.Reason = "unavailable", reason
		return result
	}
	result.Status, result.Command = "running", strings.Join(append([]string{command}, args...), " ")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
		defer cancel()
		run := exec.CommandContext(ctx, command, args...)
		run.Dir = w.Path
		run.Env = append(os.Environ(), "CI=1", "FORCE_COLOR=0")
		output := &tail{}
		run.Stdout, run.Stderr = output, output
		err := run.Run()
		finished := result
		finished.DurationMS = time.Now().UnixMilli() - result.StartedAt
		finished.Output = output.String()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			code = -1
			finished.Output += "\n" + err.Error()
		}
		finished.ExitCode = &code
		finished.Status = "failed"
		if code == 0 {
			finished.Status = "passed"
		}
		done(finished)
	}()
	return result
}

// --- screenshots -----------------------------------------------------------------

// Screenshot is one picture of a branch's changes.
type Screenshot struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	View   string `json:"view"`
	Before bool   `json:"before,omitempty"`
}

var (
	// ShotName is a screenshot file an agent may save.
	ShotName  = regexp.MustCompile(`(?i)^[A-Za-z0-9][A-Za-z0-9._-]{0,99}\.(png|jpe?g|webp|gif)$`)
	before    = regexp.MustCompile(`(?i)\.before\.[a-z]+$`)
	extension = regexp.MustCompile(`(?i)\.[a-z]+$`)
	shotTypes = map[string]string{"png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "webp": "image/webp", "gif": "image/gif"}
)

// ListScreenshots is the pictures in an agent's folder.
func ListScreenshots(dir, agentID string) []Screenshot {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []Screenshot{}
	}
	shots := []Screenshot{}
	for _, entry := range entries {
		name := entry.Name()
		if !ShotName.MatchString(name) {
			continue
		}
		shots = append(shots, Screenshot{Name: name, URL: "/v1/agents/" + agentID + "/screenshots/" + name,
			View: extension.ReplaceAllString(before.ReplaceAllString(name, ""), ""), Before: before.MatchString(name)})
	}
	return shots
}

// ReadScreenshot is a picture's bytes and type.
func ReadScreenshot(dir, name string) ([]byte, string, bool) {
	if !ShotName.MatchString(name) {
		return nil, "", false
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, "", false
	}
	kind := shotTypes[strings.ToLower(name[strings.LastIndex(name, ".")+1:])]
	if kind == "" {
		kind = "application/octet-stream"
	}
	return data, kind, true
}

var visibleFile = regexp.MustCompile(`(?i)\.(vue|tsx|jsx|svelte|astro|html?|css|scss|sass|less|svg|png|jpe?g|webp|gif|ico|templ|tmpl|gohtml|erb|hbs|ejs|pug|md|mdx)$`)

// ShowsOnScreen is whether any changed file is of a kind that is displayed
// (components, pages, styles, templates, images).
func ShowsOnScreen(paths []string) bool {
	for _, p := range paths {
		if visibleFile.MatchString(p) && !strings.HasSuffix(strings.ToLower(p), ".md") || strings.Contains(p, "/components/") || strings.Contains(p, "/pages/") {
			return true
		}
	}
	return false
}

// InsistShotsPrompt is sent to a screenshot agent that ended with no pictures.
func InsistShotsPrompt(dir string) string {
	return "No picture is saved in " + dir + " yet, but this branch changes files that show on screen. Pictures are required here: " +
		"do not stop without them. Start the app (a dev server, or a small static page or test harness that renders the changed component if the real app needs " +
		"services you cannot reach), open the changed view in Playwright and save `<view>-desktop.png` (1440x900) and `<view>-phone.png` (390x844) there. " +
		"If something fails, fix your approach and try another way. Only if it is truly impossible, say in one line exactly what blocked you."
}

// ScreenshotPrompt asks an agent to photograph a branch's visible changes.
func ScreenshotPrompt(branch, base, dir string) string {
	return strings.Join([]string{
		"You are in the git worktree of branch " + branch + ", which another agent has just worked on. Take screenshots of what its changes look like.",
		"1. Look at `git diff " + base + "...HEAD` to see what changed. Treat the change as visible by default: a change to styles, a component, a page, a template, or text shown on screen is visible, and then you must save pictures. Save nothing only when every changed file is plainly not shown on screen (backend code, tests, config, docs), and say so.",
		"2. Start the project the way its README or package.json says, and open the changed pages or views with Playwright or any headless browser that is installed (Playwright's browsers may already be in ~/.cache/ms-playwright: reuse them before downloading). Show them as a person meets them: with the project's real data and services when you can reach them (its .env, dev servers already running), logged in if that is how it is used, with a realistic amount of content. Use seeded or mock data, or a small page that renders the changed component alone, when real data is out of reach, and say so. Never finish without at least one picture of a visible change.",
		"3. Shoot each changed view at real screen sizes: desktop 1440x900 as `<view>-desktop.png` and phone 390x844 as `<view>-phone.png`. If the view is made for one screen (a kiosk, an always-on display, a widget or panel of a fixed size), shoot it at that size instead, as `<view>-<size>.png`. Capture the viewport, scrolled so the change is in view; never crop to a single element. Save them in " + dir + " (at most 10; the folder exists). They show the branch as it is now.",
		"4. Look at every picture you saved. If one shows a broken layout, overflow, cut-off text, or an error page, say so in your final message in one line per problem.",
		"5. Only for a view that already existed on " + base + " and whose change is hard to see without a comparison (a restyle, a moved or removed element, a fixed layout bug), also save how it looked before as `<same name>.before.png`, like `dashboard-review-desktop.before.png`, with the same size and state as the after shot. Take it from a temporary worktree (`git worktree add --detach <tmp dir> " + base + "`, then `git worktree remove --force <tmp dir>`), never by switching this one. Skip before shots for new pages, new views, and added elements: most changes need none.",
		"6. Stop every process you started.",
		"Do not edit, stage, or commit anything in the worktree, and do not push. If you cannot run the app, say why in one line.",
	}, "\n")
}

// --- uploads -----------------------------------------------------------------------

// Upload limits.
const (
	MaxUploadBytes = 10 << 20
	MaxUploadFiles = 5
)

var imageTypes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}

// UploadedFile is one saved upload.
type UploadedFile struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Bytes     int    `json:"bytes"`
	MediaType string `json:"mediaType,omitempty"`
}

func scratchRoot() string {
	return filepath.Join(os.TempDir(), "claude-"+strconv.Itoa(os.Getuid()))
}

var notPathChar = regexp.MustCompile(`[^a-zA-Z0-9]`)

// ScratchpadOf is where Claude Code lets a session read without asking.
func ScratchpadOf(cwd, sessionID string) string {
	return filepath.Join(scratchRoot(), notPathChar.ReplaceAllString(cwd, "-"), sessionID, "scratchpad")
}

// UploadsRule lets an agent read what was uploaded for it.
func UploadsRule() string {
	return "Read(/" + scratchRoot() + "/**/scratchpad/uploads/**)"
}

var (
	unsafeName = regexp.MustCompile(`[^\w.-]+`)
	leadingDot = regexp.MustCompile(`^[.-]+`)
	sessionIDs = regexp.MustCompile(`^[\w-]{1,64}$`)
)

func safeName(name string) string {
	cleaned := leadingDot.ReplaceAllString(unsafeName.ReplaceAllString(filepath.Base(name), "-"), "")
	if len(cleaned) > 80 {
		cleaned = cleaned[len(cleaned)-80:]
	}
	if cleaned == "" {
		return "file"
	}
	return cleaned
}

// HTTPError is a refusal with the status to answer it with.
type HTTPError struct {
	Status  int
	Message string
	// Code is a machine-readable reason, such as herdr's agent_blocked; empty for most.
	Code string
}

func (e *HTTPError) Error() string { return e.Message }

func httpErr(status int, format string, args ...any) error {
	return &HTTPError{Status: status, Message: fmt.Sprintf(format, args...)}
}

// UploadRequest is POST /v1/uploads.
type UploadRequest struct {
	Cwd       string `json:"cwd"`
	SessionID string `json:"sessionId"`
	Files     []struct {
		Name string `json:"name"`
		Data string `json:"data"`
	} `json:"files"`
}

// SaveUploads saves files where the session reads them unasked.
func SaveUploads(request UploadRequest) ([]UploadedFile, error) {
	switch {
	case !filepath.IsAbs(request.Cwd):
		return nil, httpErr(400, "`cwd` must be the session's absolute working directory")
	case !sessionIDs.MatchString(request.SessionID):
		return nil, httpErr(400, "`sessionId` must be the session's id")
	case len(request.Files) == 0 || len(request.Files) > MaxUploadFiles:
		return nil, httpErr(400, "`files` must be 1 to %d files, each { name, data } with data in base64", MaxUploadFiles)
	}
	type decoded struct {
		name string
		data []byte
	}
	var files []decoded
	for _, file := range request.Files {
		data, err := base64.StdEncoding.DecodeString(file.Data)
		if err != nil || file.Name == "" {
			return nil, httpErr(400, "each file must be { name, data } with data in base64")
		}
		if len(data) > MaxUploadBytes {
			return nil, httpErr(413, "%s is over %d MB", file.Name, MaxUploadBytes>>20)
		}
		files = append(files, decoded{safeName(file.Name), data})
	}
	folder := filepath.Join(ScratchpadOf(request.Cwd, request.SessionID), "uploads")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	saved := []UploadedFile{}
	stamp := strconv.FormatInt(time.Now().UnixMilli(), 36)
	for _, file := range files {
		name := stamp + "-" + file.name
		ext := filepath.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		for n := 2; taken[name]; n++ {
			name = fmt.Sprintf("%s-%d%s", stem, n, ext)
		}
		taken[name] = true
		path := filepath.Join(folder, name)
		if err := os.WriteFile(path, file.data, 0o600); err != nil {
			return nil, err
		}
		saved = append(saved, UploadedFile{Name: file.name, Path: path, Bytes: len(file.data), MediaType: imageTypes[strings.ToLower(filepath.Ext(file.name))]})
	}
	return saved, nil
}

// ImageBlocks turns uploaded image paths into image blocks for a message.
func ImageBlocks(paths []string, cwd, sessionID string) ([]any, error) {
	folder := filepath.Join(ScratchpadOf(cwd, sessionID), "uploads")
	var blocks []any
	for _, path := range paths {
		inside, err := filepath.Rel(folder, path)
		mediaType := imageTypes[strings.ToLower(filepath.Ext(path))]
		if err != nil || inside == "." || strings.HasPrefix(inside, "..") || mediaType == "" {
			return nil, httpErr(400, "%s is not an image uploaded for this agent", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, httpErr(400, "%s is not an image uploaded for this agent", path)
		}
		blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": base64.StdEncoding.EncodeToString(data)}})
	}
	return blocks, nil
}

// --- projects ------------------------------------------------------------------------

// ProjectInfo is a project as the dashboard lists it.
type ProjectInfo struct {
	Name   string  `json:"name"`
	Path   string  `json:"path"`
	Branch *string `json:"branch"`
	Docs   bool    `json:"docs"`
}

var (
	projectName = regexp.MustCompile(`(?i)^[a-z0-9][a-z0-9._-]*$`)
	rootDoc     = regexp.MustCompile(`(?i)^(readme|todo)\.md$`)
)

func rootDocs(path string) []string {
	entries, _ := os.ReadDir(path)
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && rootDoc.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	slices.SortFunc(names, func(a, b string) int {
		todoA, todoB := strings.HasPrefix(strings.ToLower(a), "todo"), strings.HasPrefix(strings.ToLower(b), "todo")
		if todoA != todoB {
			if todoA {
				return 1
			}
			return -1
		}
		return strings.Compare(a, b)
	})
	return names
}

func describeProject(name, path string) ProjectInfo {
	info := ProjectInfo{Name: name, Path: path, Docs: exists(filepath.Join(path, "docs", "docs.json")) || len(rootDocs(path)) > 0}
	if branch, ok := tryGit(path, "symbolic-ref", "--short", "HEAD"); ok && branch != "" {
		info.Branch = &branch
	}
	return info
}

// ProjectInfos is every project under root.
func ProjectInfos(root string) []ProjectInfo {
	infos := []ProjectInfo{}
	for _, project := range FindProjects(root) {
		infos = append(infos, describeProject(project.Name, project.Path))
	}
	return infos
}

// CreateProject makes a new git repository under root.
func CreateProject(root, name string) (ProjectInfo, error) {
	if !projectName.MatchString(name) || len(name) > 100 || strings.HasSuffix(name, ".") {
		return ProjectInfo{}, httpErr(400, `name must be letters, digits, ".", "_" or "-", starting with a letter or digit`)
	}
	path := filepath.Join(root, name)
	if exists(path) {
		return ProjectInfo{}, httpErr(409, "%s already exists", name)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		return ProjectInfo{}, err
	}
	if _, err := git(path, "init", "--quiet"); err != nil {
		return ProjectInfo{}, err
	}
	if err := ensureFirstCommit(path); err != nil {
		return ProjectInfo{}, err
	}
	return describeProject(name, path), nil
}

// DocFile is one document of a project.
type DocFile struct {
	File    string  `json:"file"`
	Title   *string `json:"title"`
	Content *string `json:"content"`
	Error   string  `json:"error,omitempty"`
}

// ProjectDocs is a project's documents: what docs/docs.json lists, else
// README.md and TODO.md in its root.
type ProjectDocs struct {
	Configured bool      `json:"configured"`
	Files      []DocFile `json:"files"`
	Error      string    `json:"error,omitempty"`
}

func readText(path string) *string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text := string(data)
	return &text
}

// ReadProjectDocs reads a project's documents, read-only.
func ReadProjectDocs(root, name string) (ProjectDocs, error) {
	index := slices.IndexFunc(FindProjects(root), func(p Project) bool { return p.Name == name })
	if index < 0 {
		return ProjectDocs{}, httpErr(404, "no project %s", name)
	}
	project := FindProjects(root)[index]
	docsDir := filepath.Join(project.Path, "docs")
	data, err := os.ReadFile(filepath.Join(docsDir, "docs.json"))
	if err != nil {
		docs := ProjectDocs{Files: []DocFile{}}
		for _, file := range rootDocs(project.Path) {
			entry := DocFile{File: file, Content: readText(filepath.Join(project.Path, file))}
			if entry.Content == nil {
				entry.Error = "missing"
			}
			docs.Files = append(docs.Files, entry)
		}
		docs.Configured = len(docs.Files) > 0
		return docs, nil
	}
	var config struct {
		Files []json.RawMessage `json:"files"`
	}
	if json.Unmarshal(data, &config) != nil || config.Files == nil {
		return ProjectDocs{Configured: true, Files: []DocFile{}, Error: `docs.json: docs.json needs a "files" array`}, nil
	}
	docs := ProjectDocs{Configured: true, Files: []DocFile{}}
	for _, raw := range config.Files {
		var file string
		var entry struct {
			File  string `json:"file"`
			Title string `json:"title"`
		}
		if json.Unmarshal(raw, &file) != nil {
			if json.Unmarshal(raw, &entry) != nil || entry.File == "" {
				docs.Files = append(docs.Files, DocFile{Error: "invalid entry"})
				continue
			}
			file = entry.File
		}
		doc := DocFile{File: file}
		if strings.TrimSpace(entry.Title) != "" {
			title := entry.Title
			doc.Title = &title
		}
		clean := filepath.Clean(file)
		if file == "" || filepath.IsAbs(file) || strings.Contains(file, "\x00") || !strings.HasSuffix(strings.ToLower(file), ".md") || strings.HasPrefix(clean, "..") || slices.Contains(strings.Split(file, "/"), "..") {
			doc.Error = "invalid path"
			docs.Files = append(docs.Files, doc)
			continue
		}
		full := filepath.Join(docsDir, clean)
		if info, err := os.Stat(full); err != nil || info.IsDir() || !strings.HasPrefix(full, docsDir+string(filepath.Separator)) {
			doc.Error = "missing"
		} else {
			doc.Content = readText(full)
		}
		docs.Files = append(docs.Files, doc)
	}
	return docs, nil
}

// planDoc is a root markdown file named like a plan or a task list: plan.md,
// PLAN.md, todo-auth.md, roadmap.md, backlog.md, tasks.md. Documents (listed in
// docs.json or under docs/) are candidates whatever their name, except READMEs.
var planDoc = regexp.MustCompile(`(?i)(plan|todo|roadmap|backlog|tasks)[^/]*\.md$`)

// ideaDoc is a file whose "## heading" sections are goals when it has no
// checkboxes: plan-like names plus ideas, wishlist and similar.
var ideaDoc = regexp.MustCompile(`(?i)(plan|todo|roadmap|backlog|tasks|ideas?|wish|suggestion|proposal)[^/]*\.md$`)

var readmeDoc = regexp.MustCompile(`(?i)(^|/)readme[^/]*\.md$`)

// PlanFiles is the project's markdown files that can hold goals, as paths
// relative to the project: the documents ReadProjectDocs lists (docs/docs.json,
// else the root TODO.md), every markdown file under docs/ (at most three
// folders deep) whether or not docs.json lists it, and plan-like files in the
// root. READMEs are left out.
func PlanFiles(projectPath string, docs ProjectDocs) []string {
	seen := map[string]bool{}
	var found []string
	add := func(path string) {
		if !seen[path] {
			seen[path] = true
			found = append(found, path)
		}
	}
	for _, doc := range docs.Files {
		if doc.Content == nil || readmeDoc.MatchString(doc.File) {
			continue
		}
		if exists(filepath.Join(projectPath, doc.File)) && !exists(filepath.Join(projectPath, "docs", "docs.json")) {
			add(doc.File)
		} else {
			add("docs/" + doc.File)
		}
	}
	entries, _ := os.ReadDir(projectPath)
	for _, entry := range entries {
		if !entry.IsDir() && planDoc.MatchString(entry.Name()) {
			add(entry.Name())
		}
	}
	docsDir := filepath.Join(projectPath, "docs")
	_ = filepath.WalkDir(docsDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(docsDir, path)
		if entry.IsDir() {
			if strings.Count(rel, string(filepath.Separator)) >= 3 {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".md") && !readmeDoc.MatchString(entry.Name()) {
			add(filepath.ToSlash(filepath.Join("docs", rel)))
		}
		return nil
	})
	slices.Sort(found)
	return found
}

// PlanCandidates is the open "- [ ]" items of a project's plan files (see
// PlanFiles), read-only. Root TODO.md keeps its plain source; for any other
// file the path is in each candidate's source.
func PlanCandidates(root, name string, known map[string]int64) ([]Candidate, error) {
	docs, err := ReadProjectDocs(root, name)
	if err != nil {
		return nil, err
	}
	project := FindProjects(root)[slices.IndexFunc(FindProjects(root), func(p Project) bool { return p.Name == name })]
	found := []Candidate{}
	for _, file := range PlanFiles(project.Path, docs) {
		text := readText(filepath.Join(project.Path, file))
		if text == nil {
			continue
		}
		label := file
		if file == "TODO.md" {
			label = ""
		}
		if label != "" && !anyCheckbox.MatchString(*text) {
			if ideaDoc.MatchString(file) {
				found = append(found, sectionCandidates(name, label, *text, known)...)
			}
			continue
		}
		found = append(found, todoCandidatesIn(name, label, *text, known)...)
	}
	return found, nil
}
