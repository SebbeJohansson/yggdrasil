package sindri

// Releasing a project from the dashboard. A project can be released from
// here when its checkout has a release workflow that can be started by hand
// with a version: `.github/workflows/release.yml` (or .yaml) with
// `workflow_dispatch` and a `version` input, on a GitHub remote. Sindri asks
// GitHub through the GitHub CLI (gh), with the login it already has, and the
// workflow makes the tag and the release.
//
//	GET  /v1/projects/{name}/release  the latest release, the last runs, the next version
//	POST /v1/projects/{name}/release  {"version": "v0.3.6"} starts the workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"yggdrasil/internal/part"
	"yggdrasil/internal/release"
)

// runGH runs the GitHub CLI; tests replace it.
var runGH = func(args ...string) ([]byte, error) {
	command := exec.Command("gh", args...)
	var stderr strings.Builder
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, httpErr(409, "the GitHub CLI (gh) is not installed here, so Sindri can't reach GitHub")
		}
		if text := strings.TrimSpace(stderr.String()); text != "" {
			return out, fmt.Errorf("gh %s: %s", args[0], text)
		}
		return out, fmt.Errorf("gh %s: %w", args[0], err)
	}
	return out, nil
}

var githubRemote = regexp.MustCompile(`^(?:https?://|ssh://)?(?:[^@/]+@)?(github\.com)[:/]([^/]+)/([^/]+?)(?:\.git)?/?$`)

// ReleaseRun is one run of a project's release workflow.
type ReleaseRun struct {
	ID         int64  `json:"id"`
	Title      string `json:"title"`
	Event      string `json:"event"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	URL        string `json:"url"`
	CreatedAt  string `json:"createdAt"`
}

// PublishedRelease is a project's newest GitHub release.
type PublishedRelease struct {
	Tag         string `json:"tag"`
	URL         string `json:"url"`
	PublishedAt string `json:"publishedAt"`
	Prerelease  bool   `json:"prerelease"`
}

// ProjectRelease is what GET /v1/projects/{name}/release answers.
type ProjectRelease struct {
	// Releasable: a release workflow that takes a version, on a GitHub remote.
	Releasable bool `json:"releasable"`
	// Why, when it is not.
	Reason      string            `json:"reason,omitempty"`
	Repo        string            `json:"repo,omitempty"`
	Workflow    string            `json:"workflow,omitempty"`
	ReleasesURL string            `json:"releasesUrl,omitempty"`
	RunsURL     string            `json:"runsUrl,omitempty"`
	Latest      *PublishedRelease `json:"latest"`
	Next        string            `json:"next,omitempty"`
	Runs        []ReleaseRun      `json:"runs"`
}

type releaseTarget struct {
	host, repo, workflow string
}

// releaseWorkflow is the project's release workflow file name when it can be
// started by hand with a version input; "" when not.
func releaseWorkflow(path string) string {
	for _, name := range []string{"release.yml", "release.yaml"} {
		raw, err := os.ReadFile(filepath.Join(path, ".github", "workflows", name))
		if err != nil {
			continue
		}
		var file struct {
			On yaml.Node `yaml:"on"`
		}
		if yaml.Unmarshal(raw, &file) != nil {
			continue
		}
		var on struct {
			Dispatch *struct {
				Inputs map[string]any `yaml:"inputs"`
			} `yaml:"workflow_dispatch"`
		}
		if file.On.Kind == yaml.MappingNode && file.On.Decode(&on) == nil && on.Dispatch != nil {
			if _, ok := on.Dispatch.Inputs["version"]; ok {
				return name
			}
		}
	}
	return ""
}

func findReleaseTarget(root, name string) (string, releaseTarget, error) {
	path := ""
	for _, project := range FindProjects(root) {
		if project.Name == name {
			path = project.Path
		}
	}
	if path == "" {
		return "", releaseTarget{}, httpErr(404, "no project called %s", name)
	}
	workflow := releaseWorkflow(path)
	if workflow == "" {
		return path, releaseTarget{}, httpErr(409, "%s has no .github/workflows/release.yml that can be run by hand with a version (workflow_dispatch with a version input)", name)
	}
	remote, ok := tryGit(path, "remote", "get-url", "origin")
	match := githubRemote.FindStringSubmatch(remote)
	if !ok || match == nil {
		return path, releaseTarget{}, httpErr(409, "%s's origin is not on GitHub", name)
	}
	return path, releaseTarget{host: match[1], repo: match[2] + "/" + match[3], workflow: workflow}, nil
}

func ghJSON(target releaseTarget, endpoint string, into any) error {
	out, err := runGH("api", "--hostname", target.host, endpoint)
	if err != nil {
		return err
	}
	return json.Unmarshal(out, into)
}

// NextVersion is the version after tag: its release when tag is a
// prerelease, else the next patch. v0.1.0 when there is none.
func NextVersion(tag string) string {
	if !release.ValidVersion(tag) {
		return "v0.1.0"
	}
	core, pre, _ := strings.Cut(tag[1:], "-")
	if pre != "" {
		return "v" + core
	}
	fields := strings.Split(core, ".")
	patch, _ := strconv.Atoi(fields[2])
	return fmt.Sprintf("v%s.%s.%d", fields[0], fields[1], patch+1)
}

// ReadProjectRelease is the project's release state from GitHub.
func ReadProjectRelease(root, name string) (ProjectRelease, error) {
	_, target, err := findReleaseTarget(root, name)
	if err != nil {
		var refused *HTTPError
		if errors.As(err, &refused) && refused.Status == 409 {
			return ProjectRelease{Reason: refused.Message, Runs: []ReleaseRun{}}, nil
		}
		return ProjectRelease{}, err
	}
	web := "https://" + target.host + "/" + target.repo
	out := ProjectRelease{
		Releasable:  true,
		Repo:        target.repo,
		Workflow:    target.workflow,
		ReleasesURL: web + "/releases",
		RunsURL:     web + "/actions/workflows/" + target.workflow,
		Runs:        []ReleaseRun{},
	}
	var releases []struct {
		Tag         string `json:"tag_name"`
		URL         string `json:"html_url"`
		PublishedAt string `json:"published_at"`
		Draft       bool   `json:"draft"`
		Prerelease  bool   `json:"prerelease"`
	}
	if err := ghJSON(target, "repos/"+target.repo+"/releases?per_page=10", &releases); err != nil {
		return ProjectRelease{}, err
	}
	for _, r := range releases {
		if !r.Draft {
			out.Latest = &PublishedRelease{Tag: r.Tag, URL: r.URL, PublishedAt: r.PublishedAt, Prerelease: r.Prerelease}
			break
		}
	}
	if out.Latest != nil {
		out.Next = NextVersion(out.Latest.Tag)
	} else {
		out.Next = "v0.1.0"
	}
	var runs struct {
		Runs []struct {
			ID         int64  `json:"id"`
			Title      string `json:"display_title"`
			Event      string `json:"event"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			URL        string `json:"html_url"`
			CreatedAt  string `json:"created_at"`
		} `json:"workflow_runs"`
	}
	if err := ghJSON(target, "repos/"+target.repo+"/actions/workflows/"+target.workflow+"/runs?per_page=3", &runs); err != nil {
		return ProjectRelease{}, err
	}
	for _, r := range runs.Runs {
		out.Runs = append(out.Runs, ReleaseRun{ID: r.ID, Title: r.Title, Event: r.Event, Status: r.Status, Conclusion: r.Conclusion, URL: r.URL, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

// StartProjectRelease starts the project's release workflow for version, on
// the repo's default branch; the workflow makes the tag.
func StartProjectRelease(root, name, version string) (map[string]string, error) {
	version = strings.TrimSpace(version)
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	if !release.ValidVersion(version) {
		return nil, httpErr(400, "%q is not a version like v0.3.6 or v0.4.0-rc.1", version)
	}
	_, target, err := findReleaseTarget(root, name)
	if err != nil {
		return nil, err
	}
	if _, err := runGH("api", "--hostname", target.host, "repos/"+target.repo+"/git/ref/tags/"+version); err == nil {
		return nil, httpErr(409, "%s already has a tag %s: pick a new version", name, version)
	} else if !strings.Contains(err.Error(), "404") && !strings.Contains(err.Error(), "Not Found") {
		return nil, err
	}
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := ghJSON(target, "repos/"+target.repo, &repo); err != nil {
		return nil, err
	}
	if repo.DefaultBranch == "" {
		repo.DefaultBranch = "main"
	}
	if _, err := runGH("workflow", "run", target.workflow, "-R", target.host+"/"+target.repo, "--ref", repo.DefaultBranch, "-f", "version="+version); err != nil {
		return nil, err
	}
	return map[string]string{
		"version": version,
		"ref":     repo.DefaultBranch,
		"runsUrl": "https://" + target.host + "/" + target.repo + "/actions/workflows/" + target.workflow,
	}, nil
}

func (s *Sindri) projectRelease(w http.ResponseWriter, r *http.Request) {
	answer(w)(ReadProjectRelease(s.Root, r.PathValue("name")))
}

func (s *Sindri) startProjectRelease(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Version == "" {
		part.Error(w, http.StatusBadRequest, "say which version: {\"version\": \"v0.3.6\"}")
		return
	}
	answer(w)(StartProjectRelease(s.Root, r.PathValue("name"), body.Version))
}
