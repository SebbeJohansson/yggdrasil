package sindri

// Cleaning projects nobody has touched in a long while. A project left for
// months still holds its node_modules, build output and caches, often
// gigabytes on a small VPS, and all of it comes back with one install or
// build. Once a project has had no commit, checkout or file change for
// CleanAfter (SINDRI_CLEAN_AFTER_DAYS, 90), those folders are removed:
//
//   - only folders named in cleanNames (dependencies, build output, caches),
//   - only when git ignores them, so nothing tracked or kept by hand goes,
//   - never inside .git, and never by following a link.
//
//	GET  /v1/cleanup                       what would go now, and the last run
//	POST /v1/cleanup                       clean those now (owner)
//	POST /v1/cleanup {"project": "x"}      clean one project now, however recent (owner)
//
// A loop does the same once a day when CleanAfter is set; 0 only lists.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/part"
)

// cleanNames are folders that only hold what an install, build or tool run
// makes again. A folder by one of these names is removed only when git
// ignores it.
var cleanNames = []string{
	// JavaScript
	"node_modules", ".nuxt", ".output", ".next", ".svelte-kit", ".turbo", ".parcel-cache", ".vite", ".angular", ".expo",
	// Build output
	"dist", "build", "out", "target", ".gradle",
	// Python
	"__pycache__", ".venv", "venv", ".pytest_cache", ".mypy_cache", ".ruff_cache", ".tox",
	// Coverage and other caches
	"coverage", ".nyc_output", ".cache",
}

// cleanDepth is how deep under a project folders are looked for (a monorepo's packages/x/node_modules).
const cleanDepth = 6

// CleanTarget is one folder that goes, or went.
type CleanTarget struct {
	Project string `json:"project"`
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
}

// CleanProject is a project with what can go, and since when it is quiet.
type CleanProject struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// LastActive is the newest commit, checkout or change seen, in ms.
	LastActive int64         `json:"lastActive"`
	Inactive   bool          `json:"inactive"`
	Targets    []CleanTarget `json:"targets"`
	Bytes      int64         `json:"bytes"`
}

// CleanRun is what one cleaning removed.
type CleanRun struct {
	At      int64         `json:"at"`
	Removed []CleanTarget `json:"removed"`
	Bytes   int64         `json:"bytes"`
	Errors  []string      `json:"errors,omitempty"`
}

// Cleaner looks after the projects under Root.
type Cleaner struct {
	Root string
	// After is how long a project must be quiet; 0 never cleans by itself.
	After time.Duration
	// StatePath keeps the last run across restarts.
	StatePath string
	Now       func() time.Time

	mu   sync.Mutex
	last *CleanRun
}

func (c *Cleaner) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Scan lists every project with its cleanable folders; inactive ones are those Clean takes.
func (c *Cleaner) Scan(ctx context.Context) []CleanProject {
	out := []CleanProject{}
	for _, p := range FindProjects(c.Root) {
		if ctx.Err() != nil {
			break
		}
		last := lastActive(p.Path)
		project := CleanProject{Name: p.Name, Path: p.Path, LastActive: last.UnixMilli()}
		project.Inactive = c.After > 0 && !last.IsZero() && c.now().Sub(last) >= c.After
		for _, dir := range ignoredBuildDirs(p.Path) {
			size := dirSize(dir)
			project.Targets = append(project.Targets, CleanTarget{Project: p.Name, Path: dir, Bytes: size})
			project.Bytes += size
		}
		if project.Targets == nil {
			project.Targets = []CleanTarget{}
		}
		out = append(out, project)
	}
	return out
}

// Clean removes the cleanable folders of the inactive projects, or of the
// one named in only, however recent.
func (c *Cleaner) Clean(ctx context.Context, only string) (*CleanRun, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	run := &CleanRun{At: c.now().UnixMilli(), Removed: []CleanTarget{}}
	found := false
	for _, project := range c.Scan(ctx) {
		if only != "" && project.Name != only {
			continue
		}
		found = found || project.Name == only
		if only == "" && !project.Inactive {
			continue
		}
		for _, target := range project.Targets {
			if err := removeInside(project.Path, target.Path); err != nil {
				run.Errors = append(run.Errors, err.Error())
				continue
			}
			run.Removed = append(run.Removed, target)
			run.Bytes += target.Bytes
		}
	}
	if only != "" && !found {
		return nil, httpErr(404, "no project called %s here", only)
	}
	c.last = run
	c.save()
	return run, nil
}

// Last is the last cleaning, here or before a restart.
func (c *Cleaner) Last() *CleanRun {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil && c.StatePath != "" {
		if raw, err := os.ReadFile(c.StatePath); err == nil {
			var run CleanRun
			if json.Unmarshal(raw, &run) == nil {
				c.last = &run
			}
		}
	}
	return c.last
}

func (c *Cleaner) save() {
	if c.StatePath == "" || c.last == nil {
		return
	}
	raw, _ := json.Marshal(c.last)
	_ = os.MkdirAll(filepath.Dir(c.StatePath), 0o700)
	_ = os.WriteFile(c.StatePath, raw, 0o600)
}

// lastActive is the newest of: the last commit, the index (a checkout, an
// add) and any change git sees in the working tree, ignored files aside.
func lastActive(repo string) time.Time {
	var newest time.Time
	seen := func(t time.Time) {
		if t.After(newest) {
			newest = t
		}
	}
	if out, ok := tryGit(repo, "log", "-1", "--format=%ct"); ok {
		if s, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64); err == nil {
			seen(time.Unix(s, 0))
		}
	}
	if gitDir, ok := tryGit(repo, "rev-parse", "--absolute-git-dir"); ok {
		if info, err := os.Stat(filepath.Join(gitDir, "index")); err == nil {
			seen(info.ModTime())
		}
	}
	if out, ok := tryGit(repo, "status", "--porcelain", "-z", "--untracked-files=all"); ok {
		for _, entry := range strings.Split(out, "\x00") {
			if len(entry) < 4 {
				continue
			}
			if info, err := os.Lstat(filepath.Join(repo, entry[3:])); err == nil {
				seen(info.ModTime())
			}
		}
	}
	return newest
}

// ignoredBuildDirs are the folders under repo named in cleanNames that git
// ignores, outermost only (node_modules inside node_modules is not listed twice).
func ignoredBuildDirs(repo string) []string {
	var found []string
	root := filepath.Clean(repo)
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() || path == root {
			return nil
		}
		if d.Name() == ".git" {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(root, path)
		if strings.Count(rel, string(filepath.Separator)) >= cleanDepth {
			return filepath.SkipDir
		}
		if slices.Contains(cleanNames, d.Name()) {
			found = append(found, path)
			return filepath.SkipDir
		}
		return nil
	})
	return gitIgnored(repo, found)
}

// gitIgnored keeps the paths git ignores, asked in one call.
func gitIgnored(repo string, paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	command := exec.Command("git", "-C", repo, "check-ignore", "--stdin", "-z")
	command.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	out, err := command.Output()
	// 1: none ignored. Anything else that failed: keep nothing, to be safe.
	if err != nil {
		return nil
	}
	ignored := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			if !filepath.IsAbs(p) {
				p = filepath.Join(repo, p)
			}
			ignored[filepath.Clean(p)] = true
		}
	}
	var kept []string
	for _, p := range paths {
		if ignored[filepath.Clean(p)] {
			kept = append(kept, p)
		}
	}
	return kept
}

// dirSize adds up the files under dir, not following links.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// removeInside removes target, which must be a real folder inside repo
// (no link anywhere on the way) named in cleanNames.
func removeInside(repo, target string) error {
	rel, err := filepath.Rel(repo, target)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || !slices.Contains(cleanNames, filepath.Base(target)) {
		return fmt.Errorf("%s: not a folder to clean in %s", target, repo)
	}
	resolvedRepo, err1 := filepath.EvalSymlinks(repo)
	resolved, err2 := filepath.EvalSymlinks(target)
	if err := errors.Join(err1, err2); err != nil {
		return fmt.Errorf("%s: %w", target, err)
	}
	if resolved != filepath.Join(resolvedRepo, rel) {
		return fmt.Errorf("%s: a link on the way, left alone", target)
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("%s: %w", target, err)
	}
	return nil
}

// CleanStatus is GET /v1/cleanup's answer.
type CleanStatus struct {
	AfterDays int            `json:"afterDays"`
	Projects  []CleanProject `json:"projects"`
	// Bytes is what cleaning now would free: the inactive projects' folders.
	Bytes int64     `json:"bytes"`
	Last  *CleanRun `json:"last"`
}

func (c *Cleaner) status(ctx context.Context) CleanStatus {
	status := CleanStatus{AfterDays: int(c.After / (24 * time.Hour)), Projects: c.Scan(ctx), Last: c.Last()}
	for _, p := range status.Projects {
		if p.Inactive {
			status.Bytes += p.Bytes
		}
	}
	return status
}

func (s *Sindri) mountCleanup(p *part.Part) {
	if s.Cleaner == nil {
		return
	}
	p.Handle("GET /v1/cleanup", func(w http.ResponseWriter, r *http.Request) {
		part.JSON(w, http.StatusOK, s.Cleaner.status(r.Context()))
	})
	p.Handle("POST /v1/cleanup", only(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Project string `json:"project"`
		}
		if r.ContentLength != 0 {
			if err := part.Decode(r, &body); err != nil {
				part.Error(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		answer(w)(s.Cleaner.Clean(r.Context(), body.Project))
	}))
	if s.Cleaner.After > 0 {
		p.Loop("cleanup", 24*time.Hour, func(ctx context.Context) error {
			run, err := s.Cleaner.Clean(ctx, "")
			if err != nil {
				return err
			}
			if len(run.Removed) > 0 {
				p.Logf("cleanup: removed %d folders from inactive projects, %d MB", len(run.Removed), run.Bytes>>20)
			}
			if len(run.Errors) > 0 {
				return fmt.Errorf("cleanup: %s", strings.Join(run.Errors, "; "))
			}
			return nil
		})
	}
}
