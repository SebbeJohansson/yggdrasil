package sindri

// Goals from GitHub issues. The open issues of the projects here whose origin
// is on GitHub are goal candidates (source "github:<owner>/<repo>#<number>"),
// each with its project already picked. GitHub is asked through the GitHub
// CLI (gh), with the login it already has, in one search over those repos.
//
// The issue is never moved or commented on. A pull request Sindri opens for
// such a goal, in the issue's own repo, says "Closes <owner>/<repo>#<number>",
// so GitHub closes the issue when it merges.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// GitHubFilter narrows the issues listed.
type GitHubFilter struct {
	// Project is a project here; "" is every project on GitHub.
	Project string
	// Query is part of the title.
	Query string
	// Assignee is mine (the default), unassigned or all.
	Assignee string
}

// GitHubAssignees are the assignee filters.
var GitHubAssignees = []string{"mine", "unassigned", "all"}

// gitHubLimit is the most issues one search lists.
const gitHubLimit = 100

var githubSource = regexp.MustCompile(`^github:([\w.-]+/[\w.-]+)#(\d+)$`)

// GitHubIssueOf is the repo and number of a goal source from GitHub; ok is false for any other.
func GitHubIssueOf(source string) (repo string, number int, ok bool) {
	match := githubSource.FindStringSubmatch(source)
	if match == nil {
		return "", 0, false
	}
	number, _ = strconv.Atoi(match[2])
	return match[1], number, true
}

// gitHubRepo is the owner/repo of a checkout's origin on github.com; "" when it is elsewhere.
func gitHubRepo(path string) string {
	remote, ok := tryGit(path, "remote", "get-url", "origin")
	match := githubRemote.FindStringSubmatch(remote)
	if !ok || match == nil {
		return ""
	}
	return match[2] + "/" + match[3]
}

// GitHubProjects maps owner/repo to the project here whose origin it is.
func GitHubProjects(root string) map[string]string {
	repos := map[string]string{}
	for _, project := range FindProjects(root) {
		if repo := gitHubRepo(project.Path); repo != "" {
			if _, taken := repos[strings.ToLower(repo)]; !taken {
				repos[strings.ToLower(repo)] = project.Name
			}
		}
	}
	return repos
}

type gitHubIssue struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	URL        string `json:"url"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// GitHubCandidates are the open issues of the projects under root as goal candidates.
func GitHubCandidates(_ context.Context, root string, filter GitHubFilter, known map[string]int64) ([]Candidate, error) {
	projects := GitHubProjects(root)
	var repos []string
	for repo, name := range projects {
		if filter.Project == "" || name == filter.Project {
			repos = append(repos, repo)
		}
	}
	if len(repos) == 0 {
		if filter.Project != "" {
			return nil, httpErr(404, "%s is not a project here with its origin on GitHub", filter.Project)
		}
		return []Candidate{}, nil
	}
	sort.Strings(repos)
	args := []string{"search", "issues", "--state", "open", "--limit", strconv.Itoa(gitHubLimit),
		"--json", "number,title,body,url,repository,labels"}
	for _, repo := range repos {
		args = append(args, "--repo", repo)
	}
	switch filter.Assignee {
	case "", "mine":
		args = append(args, "--assignee", "@me")
	case "unassigned":
		args = append(args, "--no-assignee")
	}
	if query := strings.TrimSpace(filter.Query); query != "" {
		args = append(args, "--match", "title", "--", query)
	}
	out, err := runGH(args...)
	if err != nil {
		return nil, err
	}
	var issues []gitHubIssue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("gh search issues: %w", err)
	}
	found := make([]Candidate, 0, len(issues))
	for _, issue := range issues {
		repo := issue.Repository.NameWithOwner
		source := fmt.Sprintf("github:%s#%d", repo, issue.Number)
		context := issue.URL
		if body := strings.TrimSpace(issue.Body); body != "" {
			context = body + "\n\n" + issue.URL
		}
		var labels []string
		for _, label := range issue.Labels {
			labels = append(labels, label.Name)
		}
		c := Candidate{Source: source, Title: clip(issue.Title, 200), Context: clip(context, MaxGoalContext), Repos: []string{}, URL: issue.URL, Labels: labels}
		if name, ok := projects[strings.ToLower(repo)]; ok {
			c.Repos = []string{name}
		}
		if id, ok := known[source]; ok {
			c.Goal = &id
		}
		found = append(found, c)
	}
	return found, nil
}

// closesLine is the line a pull request from path says for a goal from a
// GitHub issue in the same repo, so the issue closes when it merges; "" otherwise.
func closesLine(path, source string) string {
	repo, number, ok := GitHubIssueOf(source)
	if !ok || !strings.EqualFold(repo, gitHubRepo(path)) {
		return ""
	}
	return fmt.Sprintf("Closes %s#%d", repo, number)
}

// closes is what a pull request for agent says about its goal's GitHub issue; nil without goals.
func (s *Sindri) closes(agent string) func(path string) string {
	if s.Work == nil {
		return nil
	}
	return func(path string) string {
		task, _ := s.Work.Store.TaskOfAgent(agent)
		if task == nil || task.GoalID == nil {
			return ""
		}
		goal, _ := s.Work.Store.Goal(*task.GoalID)
		if goal == nil {
			return ""
		}
		return closesLine(path, goal.Source)
	}
}
