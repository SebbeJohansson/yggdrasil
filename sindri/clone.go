package sindri

// Cloning a GitHub repo in as a project. A repo cloned by hand can land
// anywhere, and then Sindri, the widget and the phone never see it, and a
// cloud session started from a copy with no GitHub origin is seeded from a
// bundle instead of from GitHub. Cloning here always puts it at
// <root>/<name>, where FindProjects looks, with origin on github.com.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// cloneWait is how long one clone may take.
const cloneWait = 5 * time.Minute

// gitHubRepoArg is owner/name, alone or in a GitHub https or ssh link.
var gitHubRepoArg = regexp.MustCompile(`^(?:(?:https?://)?(?:www\.)?github\.com/|git@github\.com:)?([A-Za-z0-9][A-Za-z0-9-]*)/([A-Za-z0-9._-]+?)(?:\.git)?/?$`)

// ParseGitHubRepo is the owner/name in what was typed: "owner/name" or its link.
func ParseGitHubRepo(text string) (string, bool) {
	match := gitHubRepoArg.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil || match[2] == "." || match[2] == ".." {
		return "", false
	}
	return match[1] + "/" + match[2], true
}

// runClone clones owner/name into path; tests replace it. The GitHub CLI
// goes first, with the login it already has, so private repos clone too;
// without it, git over https (public repos, or git's own credentials).
var runClone = func(ctx context.Context, repo, path string) error {
	var stderr strings.Builder
	command := exec.CommandContext(ctx, "gh", "repo", "clone", repo, path)
	command.Stderr = &stderr
	err := command.Run()
	if errors.Is(err, exec.ErrNotFound) {
		stderr.Reset()
		command = exec.CommandContext(ctx, "git", "clone", "--quiet", "https://github.com/"+repo+".git", path)
		command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		command.Stderr = &stderr
		err = command.Run()
	}
	if err != nil {
		if ctx.Err() != nil {
			return httpErr(504, "cloning %s took longer than %s", repo, cloneWait)
		}
		if text := lastLines(stderr.String(), 3); text != "" {
			return httpErr(502, "could not clone %s: %s", repo, text)
		}
		return httpErr(502, "could not clone %s: %s", repo, err)
	}
	return nil
}

// CloneProject clones a GitHub repo into root as a project, named after the
// repo unless name is given.
func CloneProject(ctx context.Context, root, repoText, name string) (ProjectInfo, error) {
	repo, ok := ParseGitHubRepo(repoText)
	if !ok {
		return ProjectInfo{}, httpErr(400, "repo must be owner/name or a github.com link")
	}
	if name = strings.TrimSpace(name); name == "" {
		name = repo[strings.Index(repo, "/")+1:]
	}
	if !projectName.MatchString(name) || len(name) > 100 || strings.HasSuffix(name, ".") {
		return ProjectInfo{}, httpErr(400, `name must be letters, digits, ".", "_" or "-", starting with a letter or digit`)
	}
	for _, project := range FindProjects(root) {
		if strings.EqualFold(gitHubRepo(project.Path), repo) {
			return ProjectInfo{}, httpErr(409, "%s is already here, as %s", repo, project.Name)
		}
	}
	path := filepath.Join(root, name)
	if exists(path) {
		return ProjectInfo{}, httpErr(409, "%s already exists", name)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return ProjectInfo{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, cloneWait)
	defer cancel()
	if err := runClone(ctx, repo, path); err != nil {
		_ = os.RemoveAll(path) // a half clone would show as a broken project
		return ProjectInfo{}, err
	}
	if !exists(filepath.Join(path, ".git")) {
		return ProjectInfo{}, fmt.Errorf("the clone of %s left no repository at %s", repo, path)
	}
	return describeProject(name, path), nil
}
