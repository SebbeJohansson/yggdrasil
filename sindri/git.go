package sindri

// Worktrees, and reviewing, merging and publishing what an agent made in
// one. Ported from my-agent's runner/src/worktree.ts and review.ts.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ReviewError is a step that can't be done, in words: answered with a 409.
type ReviewError struct{ Message string }

func (e *ReviewError) Error() string { return e.Message }

func reviewErr(format string, args ...any) error {
	return &ReviewError{Message: fmt.Sprintf(format, args...)}
}

func git(dir string, args ...string) (string, error) {
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr strings.Builder
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return strings.TrimRight(string(out), "\n"), fmt.Errorf("%w: %s", err, message)
		}
		return strings.TrimRight(string(out), "\n"), err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func tryGit(dir string, args ...string) (string, bool) {
	out, err := git(dir, args...)
	return strings.TrimSpace(out), err == nil
}

func gitOK(dir string, args ...string) bool {
	_, err := git(dir, args...)
	return err == nil
}

func firstLine(err error) string {
	return strings.SplitN(err.Error(), "\n", 2)[0]
}

// RepoRoot is the git repository a folder is in; empty when none.
func RepoRoot(dir string) string {
	root, _ := tryGit(dir, "rev-parse", "--show-toplevel")
	return root
}

func ensureFirstCommit(repo string) error {
	if _, ok := tryGit(repo, "rev-parse", "--verify", "--quiet", "HEAD"); ok {
		return nil
	}
	if _, err := git(repo, "add", "-A"); err != nil {
		return err
	}
	_, err := git(repo, "commit", "--allow-empty", "--no-verify", "-m", "Initial commit")
	return err
}

var (
	notSlug = regexp.MustCompile(`[^a-z0-9]+`)
	// Slug is what a worktree's branch may be called after agent/.
	Slug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,59}$`)
)

// SlugFor is a branch name from a label, made unique.
func SlugFor(label string) string {
	words := strings.Trim(notSlug.ReplaceAllString(strings.ToLower(label), "-"), "-")
	if len(words) > 40 {
		words = strings.TrimRight(words[:40], "-")
	}
	if words == "" {
		words = "task"
	}
	random := make([]byte, 3)
	_, _ = rand.Read(random)
	return words + "-" + hex.EncodeToString(random)
}

// CreateWorktree makes a worktree on a new branch agent/<slug>, cut from
// the repo's checked-out branch, with node_modules and sibling projects
// linked so it builds and reaches what the repo does.
func CreateWorktree(repo, worktreesRoot, label, slug string) (*Worktree, error) {
	name := filepath.Base(repo)
	if slug == "" {
		slug = SlugFor(strings.TrimPrefix(label, name+":"))
	}
	path := filepath.Join(worktreesRoot, name, slug)
	branch := "agent/" + slug
	if err := ensureFirstCommit(repo); err != nil {
		return nil, err
	}
	base, err := git(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, err
	}
	baseCommit, err := git(repo, "rev-parse", "--short", "HEAD")
	if err != nil {
		return nil, err
	}
	if _, err := git(repo, "worktree", "add", "-b", branch, path, "HEAD"); err != nil {
		return nil, err
	}
	linkModules(repo, path)
	linkSiblings(repo, path)
	return &Worktree{Repo: repo, Path: path, Branch: branch, Base: base, BaseCommit: baseCommit}, nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func linkModules(repo, path string) {
	modules := filepath.Join(repo, "node_modules")
	if exists(modules) && !exists(filepath.Join(path, "node_modules")) {
		_ = os.Symlink(modules, filepath.Join(path, "node_modules"))
		excludeFromGit(repo, "node_modules")
		runPostinstall(path)
	}
}

// runPostinstall runs the repo's postinstall script in a new worktree, since
// linking node_modules skips the install that would run it. It makes what
// the install generates and git ignores, like Nuxt's .nuxt folder.
func runPostinstall(path string) {
	if packageScripts(path)["postinstall"] == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	args := []string{"postinstall"}
	if managerOf(path) == "npm" {
		args = []string{"run", "postinstall"}
	}
	run := exec.CommandContext(ctx, managerOf(path), args...)
	run.Dir = path
	run.Env = append(os.Environ(), "CI=1", "FORCE_COLOR=0")
	_ = run.Run()
}

// linkSiblings puts the repo's sibling projects beside the worktree, so a
// path like ../pennvass resolves there too.
func linkSiblings(repo, path string) {
	parent := filepath.Dir(path)
	for _, name := range folders(filepath.Dir(repo)) {
		if name != filepath.Base(repo) && !exists(filepath.Join(parent, name)) {
			_ = os.Symlink(filepath.Join(filepath.Dir(repo), name), filepath.Join(parent, name))
		}
	}
}

func excludeFromGit(repo, pattern string) {
	common, err := git(repo, "rev-parse", "--git-common-dir")
	if err != nil {
		return
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(repo, common)
	}
	exclude := filepath.Join(common, "info", "exclude")
	current, _ := os.ReadFile(exclude)
	if slices.Contains(strings.Split(string(current), "\n"), pattern) {
		return
	}
	file, err := os.OpenFile(exclude, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	if len(current) > 0 && !strings.HasSuffix(string(current), "\n") {
		pattern = "\n" + pattern
	}
	_, _ = file.WriteString(pattern + "\n")
}

func folders(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			names = append(names, entry.Name())
		}
	}
	return names
}

// DefaultBranch is the repo's main branch: what origin's HEAD points at, main or master.
func DefaultBranch(repo string) string {
	remote, _ := tryGit(repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	for _, branch := range []string{strings.TrimPrefix(remote, "origin/"), "main", "master"} {
		if branch != "" && gitOK(repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch) {
			return branch
		}
	}
	return ""
}

// MoveToWorktree moves a branch checked out in a project's own checkout
// into a worktree, and puts the checkout back on base.
func MoveToWorktree(repo, branch, base, worktreesRoot string) (string, error) {
	if head, _ := tryGit(repo, "rev-parse", "--abbrev-ref", "HEAD"); head != branch {
		return "", fmt.Errorf("%s no longer has %s checked out", repo, branch)
	}
	if status, _ := tryGit(repo, "status", "--porcelain", "--untracked-files=no"); status != "" {
		return "", fmt.Errorf("%s has uncommitted changes; commit or stash them first", repo)
	}
	name := strings.Trim(notSlug.ReplaceAllString(strings.ToLower(branch), "-"), "-")
	if len(name) > 60 {
		name = name[:60]
	}
	if name == "" {
		name = "branch"
	}
	path := filepath.Join(worktreesRoot, filepath.Base(repo), name)
	for n := 2; exists(path); n++ {
		path = filepath.Join(worktreesRoot, filepath.Base(repo), fmt.Sprintf("%s-%d", name, n))
	}
	if _, err := git(repo, "switch", base); err != nil {
		return "", err
	}
	if _, err := git(repo, "worktree", "add", path, branch); err != nil {
		return "", err
	}
	linkModules(repo, path)
	linkSiblings(repo, path)
	return path, nil
}

// Project is one git repository under the root.
type Project struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// FindProjects is every repository directly under root, or one folder
// deeper; a top-level one wins a name.
func FindProjects(root string) []Project {
	found := map[string]string{}
	var nested []Project
	for _, name := range folders(root) {
		path := filepath.Join(root, name)
		if exists(filepath.Join(path, ".git")) {
			found[name] = path
			continue
		}
		for _, inner := range folders(path) {
			if exists(filepath.Join(path, inner, ".git")) {
				nested = append(nested, Project{Name: inner, Path: filepath.Join(path, inner)})
			}
		}
	}
	for _, project := range nested {
		if _, taken := found[project.Name]; !taken {
			found[project.Name] = project.Path
		}
	}
	projects := []Project{}
	for name, path := range found {
		projects = append(projects, Project{Name: name, Path: path})
	}
	slices.SortFunc(projects, func(a, b Project) int { return strings.Compare(a.Name, b.Name) })
	return projects
}

// ListedWorktree is one worktree of one project, or a branch checked out in
// a project's own checkout that is ahead of main.
type ListedWorktree struct {
	Project  string `json:"project"`
	Repo     string `json:"repo"`
	Path     string `json:"path"`
	Branch   string `json:"branch,omitempty"`
	Head     string `json:"head"`
	Base     string `json:"base"`
	Ahead    int    `json:"ahead"`
	Dirty    bool   `json:"dirty"`
	Missing  bool   `json:"missing"`
	Merged   bool   `json:"merged,omitempty"`
	Checkout bool   `json:"checkout,omitempty"`
	Agent    *struct {
		ID     string `json:"id"`
		Label  string `json:"label"`
		Status string `json:"status"`
	} `json:"agent,omitempty"`
}

func count(dir string, args ...string) int {
	out, _ := tryGit(dir, args...)
	n, _ := strconv.Atoi(out)
	return n
}

// ListWorktrees is every worktree of every project under root.
func ListWorktrees(root string) []ListedWorktree {
	out := []ListedWorktree{}
	for _, project := range FindProjects(root) {
		repo := project.Path
		porcelain, err1 := git(repo, "worktree", "list", "--porcelain")
		base, err2 := git(repo, "rev-parse", "--abbrev-ref", "HEAD")
		if err1 != nil || err2 != nil {
			continue
		}
		if main := DefaultBranch(repo); main != "" && base != main && base != "HEAD" {
			if ahead := count(repo, "rev-list", "--count", main+".."+base); ahead > 0 {
				head, _ := tryGit(repo, "rev-parse", "--short", "HEAD")
				status, _ := tryGit(repo, "status", "--porcelain", "--untracked-files=no")
				out = append(out, ListedWorktree{Project: project.Name, Repo: repo, Path: repo, Branch: base, Head: head, Base: main, Ahead: ahead, Dirty: status != "", Checkout: true})
			}
		}
		blocks := regexp.MustCompile(`\n\n+`).Split(porcelain, -1)
		for _, block := range blocks[1:] {
			field := func(name string) string {
				for _, line := range strings.Split(block, "\n") {
					if value, ok := strings.CutPrefix(line, name+" "); ok {
						return value
					}
				}
				return ""
			}
			path := field("worktree")
			if path == "" {
				continue
			}
			branch := strings.TrimPrefix(field("branch"), "refs/heads/")
			missing := !exists(path)
			ahead := 0
			if branch != "" {
				ahead = count(repo, "rev-list", "--count", base+".."+branch)
			}
			landed := branch != "" && ahead == 0 && count(repo, "rev-list", "--count", CutFrom(repo, branch, base)+".."+branch) > 0
			dirty := false
			if !missing {
				status, _ := tryGit(path, "status", "--porcelain", "--untracked-files=no")
				dirty = status != ""
			}
			head := field("HEAD")
			out = append(out, ListedWorktree{Project: project.Name, Repo: repo, Path: path, Branch: branch, Head: head[:min(len(head), 7)], Base: base, Ahead: ahead, Dirty: dirty, Missing: missing, Merged: landed})
		}
	}
	return out
}

var branchName = regexp.MustCompile(`^[\w./-]+$`)

// BranchesMade is the branches a shell command creates (git checkout -b,
// git switch -c), following cd and git -C.
func BranchesMade(command, cwd string) []Branch {
	var made []Branch
	dir := cwd
	home, _ := os.UserHomeDir()
	at := func(path string) string {
		if path == "~" || strings.HasPrefix(path, "~/") {
			path = home + path[1:]
		}
		if filepath.IsAbs(path) {
			return filepath.Clean(path)
		}
		return filepath.Join(dir, path)
	}
	for _, part := range regexp.MustCompile(`&&|\|\||;|\n`).Split(command, -1) {
		words := strings.Fields(part)
		for i, word := range words {
			words[i] = strings.Trim(word, `"'`)
		}
		if len(words) == 0 {
			continue
		}
		if words[0] == "cd" && len(words) > 1 {
			dir = at(words[1])
			continue
		}
		if words[0] != "git" {
			continue
		}
		i, where := 1, dir
		if i+1 < len(words) && words[i] == "-C" {
			where, i = at(words[i+1]), i+2
		}
		if i >= len(words) {
			continue
		}
		var create *regexp.Regexp
		switch words[i] {
		case "checkout":
			create = regexp.MustCompile(`^-[a-zA-Z]*[bB]$`)
		case "switch":
			create = regexp.MustCompile(`^-[a-zA-Z]*[cC]$`)
		default:
			continue
		}
		for n := i + 1; n+1 < len(words); n++ {
			if create.MatchString(words[n]) {
				if branch := words[n+1]; branchName.MatchString(branch) {
					if repo := RepoRoot(where); repo != "" {
						made = append(made, Branch{Repo: repo, Branch: branch})
					}
				}
				break
			}
		}
	}
	return made
}

// BranchPlace is where a branch is checked out, and whether it is merged
// into main; false when the branch is gone.
func BranchPlace(repo, branch string) (path string, merged bool, ok bool) {
	if !gitOK(repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch) {
		return "", false, false
	}
	list, _ := tryGit(repo, "worktree", "list", "--porcelain")
	for _, block := range regexp.MustCompile(`\n\n+`).Split(list, -1) {
		lines := strings.Split(block, "\n")
		if slices.Contains(lines, "branch refs/heads/"+branch) {
			path = strings.TrimPrefix(lines[0], "worktree ")
		}
	}
	main := DefaultBranch(repo)
	return path, main != "" && main != branch && gitOK(repo, "merge-base", "--is-ancestor", branch, main), true
}

// CutFrom is the commit a branch was cut from: its reflog's first entry, or
// where it meets base.
func CutFrom(repo, branch, base string) string {
	if reflog, ok := tryGit(repo, "reflog", "show", "--format=%h", "refs/heads/"+branch); ok {
		lines := strings.Fields(reflog)
		if len(lines) > 0 && gitOK(repo, "merge-base", "--is-ancestor", lines[len(lines)-1], branch) {
			return lines[len(lines)-1]
		}
	}
	met, ok := tryGit(repo, "merge-base", base, branch)
	if !ok {
		met = base
	}
	short, _ := tryGit(repo, "rev-parse", "--short", met)
	return short
}

// --- review ------------------------------------------------------------------

// DiffMax is the most diff a review carries.
const DiffMax = 400_000

// FileChange is one file in a review.
type FileChange struct {
	Path    string `json:"path"`
	Added   *int   `json:"added"`
	Removed *int   `json:"removed"`
}

// Review is a branch as the review page shows it.
type Review struct {
	Repo           string       `json:"repo"`
	Path           string       `json:"path"`
	Branch         string       `json:"branch"`
	Base           string       `json:"base"`
	BaseCommit     string       `json:"baseCommit"`
	WorktreeExists bool         `json:"worktreeExists"`
	BranchExists   bool         `json:"branchExists"`
	Merged         bool         `json:"merged"`
	Conflicts      []string     `json:"conflicts"`
	Commits        []Commit     `json:"commits"`
	Files          []FileChange `json:"files"`
	Commit         *Commit      `json:"commit,omitempty"`
	Diff           string       `json:"diff"`
	DiffTruncated  bool         `json:"diffTruncated"`
	Uncommitted    []string     `json:"uncommitted"`
	Checkout       struct {
		Branch string `json:"branch"`
		Dirty  bool   `json:"dirty"`
	} `json:"checkout"`
	Unpushed *struct {
		Upstream string `json:"upstream"`
		Count    int    `json:"count"`
	} `json:"unpushed,omitempty"`
	Screenshots []Screenshot `json:"screenshots,omitempty"`
}

func (w *Worktree) branchExists() bool {
	return gitOK(w.Repo, "show-ref", "--verify", "--quiet", "refs/heads/"+w.Branch)
}

func (w *Worktree) isMerged() bool {
	return gitOK(w.Repo, "merge-base", "--is-ancestor", w.Branch, w.Base)
}

// IsLanded is whether the branch is merged into base, or gone.
func (w *Worktree) IsLanded() bool {
	if !w.branchExists() {
		return true
	}
	tip, ok1 := tryGit(w.Repo, "rev-parse", w.Branch)
	start, ok2 := tryGit(w.Repo, "rev-parse", w.BaseCommit)
	return ok1 && ok2 && tip != start && w.isMerged()
}

func (w *Worktree) conflicts() []string {
	command := exec.Command("git", "-C", w.Repo, "merge-tree", "--write-tree", "--name-only", "--no-messages", w.Base, w.Branch)
	out, err := command.Output()
	var exit *exec.ExitError
	if err == nil || !errors.As(err, &exit) || exit.ExitCode() != 1 {
		return []string{}
	}
	files := []string{}
	for _, line := range strings.Split(string(out), "\n")[1:] {
		if line != "" && !slices.Contains(files, line) {
			files = append(files, line)
		}
	}
	return files
}

// RebasePrompt asks an agent to rebase its branch and resolve the conflicts.
func RebasePrompt(w *Worktree, conflicts []string) string {
	return strings.Join([]string{
		fmt.Sprintf("Your branch %s no longer merges cleanly into %s: it conflicts in %s.", w.Branch, w.Base, strings.Join(conflicts, ", ")),
		fmt.Sprintf("Rebase it onto %s here in your worktree (`git rebase %s`) and resolve each conflict so that both what %s changed and what you changed still work.", w.Base, w.Base, w.Base),
		"Run the project's checks, finish the rebase, and leave nothing uncommitted.",
		fmt.Sprintf("Do not merge into %s or push; it is merged from the review page. Say in a line or two how you resolved each file.", w.Base),
	}, " ")
}

// ConflictsToFix is the files a rebase must resolve; an error when there is nothing to fix.
func (w *Worktree) ConflictsToFix() ([]string, error) {
	switch {
	case !w.branchExists():
		return nil, reviewErr("branch %s is gone", w.Branch)
	case w.isMerged():
		return nil, reviewErr("%s is already merged into %s", w.Branch, w.Base)
	}
	conflicts := w.conflicts()
	if len(conflicts) == 0 {
		return nil, reviewErr("%s merges into %s without conflicts", w.Branch, w.Base)
	}
	return conflicts, nil
}

func (w *Worktree) commitsIn(rangeSpec string) []Commit {
	out, _ := tryGit(w.Repo, "log", "--format=%h%x00%s%x00%ct", rangeSpec)
	var commits []Commit
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\x00", 3)
		for len(fields) < 3 {
			fields = append(fields, "0")
		}
		at, _ := strconv.ParseInt(fields[2], 10, 64)
		commits = append(commits, Commit{Hash: fields[0], Subject: fields[1], At: at * 1000})
	}
	return commits
}

func (w *Worktree) numstat(spec ...string) []FileChange {
	out, _ := tryGit(w.Repo, append([]string{"diff", "--numstat"}, spec...)...)
	files := []FileChange{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		number := func(text string) *int {
			if n, err := strconv.Atoi(text); err == nil {
				return &n
			}
			return nil
		}
		files = append(files, FileChange{Path: strings.Join(fields[2:], "\t"), Added: number(fields[0]), Removed: number(fields[1])})
	}
	return files
}

func (w *Worktree) uncommitted() []string {
	if !exists(w.Path) {
		return []string{}
	}
	out, _ := tryGit(w.Path, "status", "--short")
	lines := []string{}
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func (w *Worktree) upstream() string {
	upstream, ok := tryGit(w.Repo, "rev-parse", "--abbrev-ref", w.Base+"@{upstream}")
	if !ok {
		return ""
	}
	return upstream
}

func findCommit(commits []Commit, hash, branch string) (*Commit, error) {
	if hash == "" {
		return nil, nil
	}
	if len(hash) >= 4 {
		for _, c := range commits {
			if strings.HasPrefix(c.Hash, hash) || strings.HasPrefix(hash, c.Hash) {
				return &c, nil
			}
		}
	}
	return nil, reviewErr("%s is not a commit on %s", hash, branch)
}

// ReviewOf is the branch's commits, files and diff, or one commit's.
func (w *Worktree) ReviewOf(commitHash string) (*Review, error) {
	review := &Review{Repo: w.Repo, Path: w.Path, Branch: w.Branch, Base: w.Base, BaseCommit: w.BaseCommit,
		WorktreeExists: exists(w.Path), Conflicts: []string{}, Commits: []Commit{}, Files: []FileChange{}, Uncommitted: w.uncommitted()}
	review.Checkout.Branch, _ = tryGit(w.Repo, "branch", "--show-current")
	status, _ := tryGit(w.Repo, "status", "--porcelain", "--untracked-files=no")
	review.Checkout.Dirty = status != ""
	if upstream := w.upstream(); upstream != "" {
		review.Unpushed = &struct {
			Upstream string `json:"upstream"`
			Count    int    `json:"count"`
		}{upstream, count(w.Repo, "rev-list", "--count", upstream+".."+w.Base)}
	}
	var spec []string
	if !w.branchExists() {
		if w.Merged == nil {
			review.Unpushed = nil
			return review, nil
		}
		review.Merged = true
		for _, c := range w.Merged.Commits {
			c.Merged = true
			review.Commits = append(review.Commits, c)
		}
		commit, err := findCommit(review.Commits, commitHash, w.Branch)
		if err != nil {
			return nil, err
		}
		review.Commit = commit
		spec = []string{w.Merged.From, w.Merged.To}
		if commit != nil {
			spec = []string{commit.Hash + "^", commit.Hash}
		}
	} else {
		review.BranchExists = true
		from := w.Base
		if gitOK(w.Repo, "merge-base", "--is-ancestor", w.BaseCommit, w.Branch) {
			from = w.BaseCommit
		}
		pending := map[string]bool{}
		for _, c := range w.commitsIn(w.Base + ".." + w.Branch) {
			pending[c.Hash] = true
		}
		for _, c := range w.commitsIn(from + ".." + w.Branch) {
			c.Merged = !pending[c.Hash]
			review.Commits = append(review.Commits, c)
		}
		review.Merged = w.isMerged()
		rangeSpec := w.Base + "..." + w.Branch
		if review.Merged {
			rangeSpec = from + "..." + w.Branch
		} else {
			review.Conflicts = w.conflicts()
		}
		commit, err := findCommit(review.Commits, commitHash, w.Branch)
		if err != nil {
			return nil, err
		}
		review.Commit = commit
		spec = []string{rangeSpec}
		if commit != nil {
			spec = []string{commit.Hash + "^", commit.Hash}
		}
	}
	review.Files = w.numstat(spec...)
	diff, _ := git(w.Repo, append([]string{"diff"}, spec...)...)
	if len(diff) > DiffMax {
		review.Diff, review.DiffTruncated = diff[:DiffMax], true
	} else {
		review.Diff = diff
	}
	return review, nil
}

// HasUnmergedCommits is whether the branch has work not yet in base.
func (w *Worktree) HasUnmergedCommits() bool {
	review, err := w.ReviewOf("")
	if err != nil {
		return false
	}
	for _, c := range review.Commits {
		if !c.Merged {
			return true
		}
	}
	return false
}

// PushBase pushes the branch the work merged into to the remote it tracks.
func (w *Worktree) PushBase() (string, error) {
	upstream := w.upstream()
	if upstream == "" {
		return "", reviewErr("%s in %s tracks no remote branch, so there is nowhere to push it", w.Base, w.Repo)
	}
	ahead := count(w.Repo, "rev-list", "--count", upstream+".."+w.Base)
	if ahead == 0 {
		return "", reviewErr("%s already has everything on %s", upstream, w.Base)
	}
	remote, branch, _ := strings.Cut(upstream, "/")
	if _, err := git(w.Repo, "push", remote, w.Base+":"+branch); err != nil {
		return "", reviewErr("the push of %s to %s was refused: %s", w.Base, remote, firstLine(err))
	}
	return fmt.Sprintf("pushed %d commit%s of %s to %s", ahead, plural(ahead), w.Base, upstream), nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Merge merges the branch into base in your checkout. Refused with
// uncommitted work on either side; a conflict leaves nothing changed.
func (w *Worktree) Merge() (string, error) {
	switch {
	case !w.branchExists():
		return "", reviewErr("branch %s is gone", w.Branch)
	case w.isMerged():
		return "", reviewErr("%s is already merged into %s", w.Branch, w.Base)
	}
	if pending := w.uncommitted(); len(pending) > 0 {
		return "", reviewErr("the worktree has %d uncommitted change%s, which a merge would leave behind; ask the agent to commit them first", len(pending), plural(len(pending)))
	}
	current, _ := tryGit(w.Repo, "branch", "--show-current")
	if current != w.Base {
		if current == "" {
			current = "a detached HEAD"
		}
		return "", reviewErr("your checkout of %s is on %s, not %s; switch it to %s first", w.Repo, current, w.Base, w.Base)
	}
	if status, _ := tryGit(w.Repo, "status", "--porcelain", "--untracked-files=no"); status != "" {
		return "", reviewErr("your checkout of %s has uncommitted changes; commit or stash them first", w.Repo)
	}
	from, _ := tryGit(w.Repo, "rev-parse", "--short", "HEAD")
	commits := w.commitsIn(w.Base + ".." + w.Branch)
	out, err := git(w.Repo, "merge", "--no-edit", w.Branch)
	if err != nil {
		conflicted, _ := tryGit(w.Repo, "diff", "--name-only", "--diff-filter=U")
		_, _ = tryGit(w.Repo, "merge", "--abort")
		if files := strings.Fields(conflicted); len(files) > 0 {
			return "", reviewErr("the merge conflicts in %s; nothing was changed. Merge it by hand, or ask the agent to rebase onto %s", strings.Join(files, ", "), w.Base)
		}
		return "", reviewErr("the merge failed: %s", firstLine(err))
	}
	to, _ := tryGit(w.Repo, "rev-parse", "--short", "HEAD")
	merged := &Merged{From: from, To: to, Commits: commits}
	if w.Merged != nil {
		merged.From = w.Merged.From
		merged.Commits = append(merged.Commits, w.Merged.Commits...)
	}
	w.Merged = merged
	lines := strings.Split(out, "\n")
	return lines[len(lines)-1], nil
}

var pullRequestURL = regexp.MustCompile(`https://\S+/pull/\d+`)

// PullRequest pushes the branch and opens a GitHub pull request into base with gh.
func (w *Worktree) PullRequest() (url, done string, err error) {
	switch {
	case !w.branchExists():
		return "", "", reviewErr("branch %s is gone", w.Branch)
	case w.isMerged():
		return "", "", reviewErr("%s is already merged into %s", w.Branch, w.Base)
	case !exists(w.Path):
		return "", "", reviewErr("the worktree is gone")
	}
	if pending := w.uncommitted(); len(pending) > 0 {
		return "", "", reviewErr("the worktree has %d uncommitted change%s, which the pull request would leave out; ask the agent to commit them first", len(pending), plural(len(pending)))
	}
	remotes := strings.Fields(must(tryGit(w.Repo, "remote")))
	remote := ""
	switch {
	case len(remotes) == 0:
		return "", "", reviewErr("%s has no remote to push to", w.Repo)
	case slices.Contains(remotes, "origin"):
		remote = "origin"
	case len(remotes) > 1:
		return "", "", reviewErr("%s has several remotes (%s) and none is called origin", w.Repo, strings.Join(remotes, ", "))
	default:
		remote = remotes[0]
	}
	remoteURL, _ := tryGit(w.Repo, "remote", "get-url", remote)
	if !regexp.MustCompile(`github\.com[:/]`).MatchString(remoteURL) {
		return "", "", reviewErr("%s is %s, which is not GitHub; only GitHub pull requests are supported", remote, remoteURL)
	}
	if _, err := git(w.Path, "push", "--set-upstream", remote, w.Branch); err != nil {
		return "", "", reviewErr("the push to %s failed: %s", remote, firstLine(err))
	}
	command := exec.Command("gh", "pr", "create", "--base", w.Base, "--head", w.Branch, "--fill")
	command.Dir = w.Path
	var stderr strings.Builder
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", "", reviewErr("the branch is pushed to %s, but the GitHub CLI (gh) is not installed here, so no pull request was opened", remote)
		}
		text := strings.TrimSpace(stderr.String())
		if existing := pullRequestURL.FindString(text); existing != "" && strings.Contains(strings.ToLower(text), "already exists") {
			return existing, fmt.Sprintf("pushed %s to %s; a pull request is already open: %s", w.Branch, remote, existing), nil
		}
		return "", "", reviewErr("the branch is pushed to %s, but gh could not open the pull request: %s", remote, text)
	}
	url = pullRequestURL.FindString(string(out))
	if url == "" {
		return "", "", reviewErr("gh did not say where the pull request is: %s", strings.TrimSpace(string(out)))
	}
	return url, fmt.Sprintf("pushed %s to %s and opened %s", w.Branch, remote, url), nil
}

func must(text string, _ bool) string { return text }

// Remove removes the worktree, and the branch once merged; force discards
// uncommitted and unmerged work.
func (w *Worktree) Remove(force bool) (string, error) {
	var done []string
	if exists(w.Path) {
		pending := w.uncommitted()
		if len(pending) > 0 && !force {
			return "", reviewErr("the worktree has %d uncommitted change%s; they would be lost", len(pending), plural(len(pending)))
		}
		args := []string{"worktree", "remove"}
		if force {
			args = append(args, "--force")
		}
		if _, err := git(w.Repo, append(args, w.Path)...); err != nil {
			return "", err
		}
		if len(pending) > 0 {
			done = append(done, fmt.Sprintf("removed %s, discarding %d uncommitted change%s", w.Path, len(pending), plural(len(pending))))
		} else {
			done = append(done, "removed "+w.Path)
		}
	}
	if w.branchExists() {
		switch {
		case w.isMerged():
			if _, err := git(w.Repo, "branch", "-d", w.Branch); err != nil {
				return "", err
			}
			done = append(done, "deleted "+w.Branch)
		case force:
			unmerged := count(w.Repo, "rev-list", "--count", w.Base+".."+w.Branch)
			if _, err := git(w.Repo, "branch", "-D", w.Branch); err != nil {
				return "", err
			}
			done = append(done, fmt.Sprintf("deleted %s with %d unmerged commit%s", w.Branch, unmerged, plural(unmerged)))
		default:
			done = append(done, "kept "+w.Branch+", which is not merged")
		}
	}
	if len(done) == 0 {
		return "nothing left to remove", nil
	}
	return strings.Join(done, "; "), nil
}

// IsEmpty is whether the branch holds no work: no commit past its base and
// nothing uncommitted. A branch already gone, or one git cannot read, is not.
func (w *Worktree) IsEmpty() bool {
	if !w.branchExists() {
		return false
	}
	ahead, ok := tryGit(w.Repo, "rev-list", "--count", w.Base+".."+w.Branch)
	return ok && ahead == "0" && len(w.uncommitted()) == 0
}

// Head is the worktree's current commit.
func (w *Worktree) Head() string {
	head, _ := tryGit(w.Path, "rev-parse", "--short", "HEAD")
	return head
}
