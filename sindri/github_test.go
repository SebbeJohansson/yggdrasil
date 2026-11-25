package sindri

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// githubRoot is a root with app (on GitHub as someone/app), web (someone/web)
// and local (no remote).
func githubRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, remote := range map[string]string{"app": "git@github.com:someone/app.git", "web": "https://github.com/someone/web", "local": ""} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := git(root, "init", "--quiet", path); err != nil {
			t.Fatal(err)
		}
		if remote != "" {
			if _, err := git(path, "remote", "add", "origin", remote); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func fakeSearch(t *testing.T, answer string) *[]string {
	t.Helper()
	was := runGH
	t.Cleanup(func() { runGH = was })
	var args []string
	runGH = func(a ...string) ([]byte, error) {
		args = a
		return []byte(answer), nil
	}
	return &args
}

const searchAnswer = `[
 {"number":7,"title":"Fix login","body":"It fails on Safari.","url":"https://github.com/someone/app/issues/7","repository":{"nameWithOwner":"someone/app"},"labels":[{"name":"bug"}]},
 {"number":3,"title":"Dark mode","body":"","url":"https://github.com/someone/web/issues/3","repository":{"nameWithOwner":"someone/web"},"labels":[]}
]`

func TestGitHubIssuesAreCandidatesWithTheirProjectPicked(t *testing.T) {
	args := fakeSearch(t, searchAnswer)
	known := map[string]int64{"github:someone/web#3": 12}
	got, err := GitHubCandidates(context.Background(), githubRoot(t), GitHubFilter{}, known)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	first, second := got[0], got[1]
	if first.Source != "github:someone/app#7" || first.Title != "Fix login" || !slices.Equal(first.Repos, []string{"app"}) ||
		first.Context != "It fails on Safari.\n\nhttps://github.com/someone/app/issues/7" || !slices.Equal(first.Labels, []string{"bug"}) || first.Goal != nil {
		t.Fatalf("first: %+v", first)
	}
	if second.Context != "https://github.com/someone/web/issues/3" || second.Goal == nil || *second.Goal != 12 {
		t.Fatalf("second: %+v", second)
	}
	joined := strings.Join(*args, " ")
	for _, want := range []string{"search issues", "--state open", "--repo someone/app", "--repo someone/web", "--assignee @me"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in gh %s", want, joined)
		}
	}
}

func TestGitHubFiltersByProjectTitleAndAssignee(t *testing.T) {
	args := fakeSearch(t, "[]")
	if _, err := GitHubCandidates(context.Background(), githubRoot(t), GitHubFilter{Project: "web", Query: " dark ", Assignee: "unassigned"}, nil); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(*args, " ")
	if strings.Contains(joined, "someone/app") || !strings.Contains(joined, "--repo someone/web") || !strings.Contains(joined, "--no-assignee") ||
		!strings.HasSuffix(joined, "--match title -- dark") {
		t.Fatalf("gh %s", joined)
	}
	fakeSearch(t, "[]")
	if _, err := GitHubCandidates(context.Background(), githubRoot(t), GitHubFilter{Assignee: "all"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAProjectNotOnGitHubHasNoIssues(t *testing.T) {
	fakeSearch(t, "[]")
	_, err := GitHubCandidates(context.Background(), githubRoot(t), GitHubFilter{Project: "local"}, nil)
	if refused, ok := err.(*HTTPError); !ok || refused.Status != 404 {
		t.Fatalf("%v", err)
	}
}

func TestAPullRequestClosesItsGoalsIssueOnlyInTheIssuesRepo(t *testing.T) {
	root := githubRoot(t)
	if got := closesLine(filepath.Join(root, "app"), "github:someone/app#7"); got != "Closes someone/app#7" {
		t.Fatalf("%q", got)
	}
	for _, c := range []struct{ repo, source string }{{"web", "github:someone/app#7"}, {"app", "linear:AI-3"}, {"app", "manual"}, {"local", "github:someone/app#7"}} {
		if got := closesLine(filepath.Join(root, c.repo), c.source); got != "" {
			t.Fatalf("%v: %q", c, got)
		}
	}
}
