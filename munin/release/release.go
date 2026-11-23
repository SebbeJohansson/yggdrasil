// Package release is Munin's source for your repos' builds and deploys: the
// latest workflow run on each repo's default branch, and its latest
// deployment's status.
//
// Settings (in munin.env, with release in MUNIN_SOURCES):
//
//	MUNIN_RELEASE_WATCH  the repos to watch: owner/name,owner/name
//	MUNIN_RELEASE_TOKEN  a GitHub token that can read their Actions and deployments
//	MUNIN_RELEASE_API    the GitHub API (https://api.github.com)
//
// One check per repo, raised on Gjallar as release:<owner/name> (warning). It
// fails when the latest finished run on the default branch failed, or the
// latest deployment ended in failure or error, and passes again once the
// next run or deployment does. The alert's detail has the run's link.
package release

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/watch"
	"yggdrasil/munin/source"
)

// Name is the source's name in MUNIN_SOURCES.
const Name = "release"

// DefaultAPI is GitHub.
const DefaultAPI = "https://api.github.com"

const every = 5 * time.Minute

// Source polls GitHub for each watched repo.
type Source struct {
	API   string
	Token string
	Repos []string
	HTTP  *http.Client
}

// New is the source.Factory.
func New(settings source.Settings) (source.Source, error) {
	api := strings.TrimRight(settings("API"), "/")
	if api == "" {
		api = DefaultAPI
	}
	if parsed, err := url.Parse(api); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("MUNIN_RELEASE_API: %q is not an http(s) URL", api)
	}
	var repos []string
	for _, repo := range strings.Split(settings("WATCH"), ",") {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		if owner, name, ok := strings.Cut(repo, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("MUNIN_RELEASE_WATCH: %q is not owner/name", repo)
		}
		repos = append(repos, repo)
	}
	if len(repos) == 0 {
		return nil, fmt.Errorf("MUNIN_RELEASE_WATCH: name the repos to watch, owner/name,…")
	}
	return &Source{API: api, Token: settings("TOKEN"), Repos: repos, HTTP: &http.Client{Timeout: 15 * time.Second}}, nil
}

func (s *Source) Name() string         { return Name }
func (s *Source) Every() time.Duration { return every }

// Run is one workflow run, as much as the check needs.
type Run struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
}

// Deployment is one deployment with its latest status.
type Deployment struct {
	Environment string `json:"environment"`
	State       string `json:"state"`
	URL         string `json:"url"`
}

// Repo is what Munin last saw of one repo, for the dashboard.
type Repo struct {
	Repo       string      `json:"repo"`
	OK         bool        `json:"ok"`
	Run        *Run        `json:"run,omitempty"`
	Deployment *Deployment `json:"deployment,omitempty"`
	Error      string      `json:"error,omitempty"`
}

// Failed is whether a finished run did not pass: cancelled and skipped runs
// are nobody's fault.
func Failed(conclusion string) bool {
	switch conclusion {
	case "failure", "timed_out", "startup_failure":
		return true
	}
	return false
}

// Check turns what one repo showed into its check.
func Check(repo string, run *Run, dep *Deployment) watch.Result {
	result := watch.Result{Key: repo, OK: true, Severity: alert.Warning}
	switch {
	case run != nil && Failed(run.Conclusion):
		result.OK = false
		result.Title = fmt.Sprintf("%s: %s failed on the default branch", repo, run.Name)
		result.Detail = run.Conclusion + " " + run.HTMLURL
	case dep != nil && (dep.State == "failure" || dep.State == "error"):
		result.OK = false
		result.Title = fmt.Sprintf("%s: deploy to %s failed", repo, dep.Environment)
		result.Detail = "deployment " + dep.State
	}
	return result
}

func (s *Source) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.API+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.Unmarshal(body, out)
}

// latest reads a repo's newest finished run on its default branch and its
// newest deployment with that deployment's state.
func (s *Source) latest(ctx context.Context, repo string) (*Run, *Deployment, error) {
	var info struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := s.get(ctx, "/repos/"+repo, &info); err != nil {
		return nil, nil, err
	}
	var runs struct {
		WorkflowRuns []Run `json:"workflow_runs"`
	}
	if err := s.get(ctx, "/repos/"+repo+"/actions/runs?per_page=1&status=completed&branch="+url.QueryEscape(info.DefaultBranch), &runs); err != nil {
		return nil, nil, err
	}
	var run *Run
	if len(runs.WorkflowRuns) > 0 {
		run = &runs.WorkflowRuns[0]
	}
	var deps []struct {
		ID          int64  `json:"id"`
		Environment string `json:"environment"`
	}
	if err := s.get(ctx, "/repos/"+repo+"/deployments?per_page=1", &deps); err != nil {
		return run, nil, err
	}
	var dep *Deployment
	if len(deps) > 0 {
		var statuses []struct {
			State string `json:"state"`
		}
		if err := s.get(ctx, fmt.Sprintf("/repos/%s/deployments/%d/statuses?per_page=1", repo, deps[0].ID), &statuses); err != nil {
			return run, nil, err
		}
		dep = &Deployment{Environment: deps[0].Environment}
		if len(statuses) > 0 {
			dep.State = statuses[0].State
		}
	}
	return run, dep, nil
}

// Poll checks every repo once.
func (s *Source) Poll(ctx context.Context, _ string) source.Report {
	var report source.Report
	var seen []Repo
	failing, unread := 0, 0
	for _, repo := range s.Repos {
		run, dep, err := s.latest(ctx, repo)
		if err != nil {
			// Unreadable is not failed: the check stays as it was.
			unread++
			report.Checks = append(report.Checks, watch.Result{Key: repo, Skip: true})
			seen = append(seen, Repo{Repo: repo, Error: err.Error()})
			continue
		}
		check := Check(repo, run, dep)
		if !check.OK {
			failing++
		}
		report.Checks = append(report.Checks, check)
		seen = append(seen, Repo{Repo: repo, OK: check.OK, Run: run, Deployment: dep})
	}
	report.Data = seen
	report.Up = failing == 0 && unread == 0
	switch {
	case failing > 0:
		report.Detail = fmt.Sprintf("%d of %d repos failing", failing, len(s.Repos))
	case unread > 0:
		report.Detail = fmt.Sprintf("could not read %d of %d repos", unread, len(s.Repos))
	default:
		report.Detail = fmt.Sprintf("%d repos passing", len(s.Repos))
	}
	return report
}
