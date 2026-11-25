package sindri

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func releaseProject(t *testing.T, workflow string) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "app")
	if err := os.MkdirAll(filepath.Join(path, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := git(root, "init", "--quiet", path); err != nil {
		t.Fatal(err)
	}
	if _, err := git(path, "remote", "add", "origin", "git@github.com:someone/app.git"); err != nil {
		t.Fatal(err)
	}
	if workflow != "" {
		if err := os.WriteFile(filepath.Join(path, ".github", "workflows", "release.yml"), []byte(workflow), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func fakeGH(t *testing.T, answers map[string]string) *[][]string {
	t.Helper()
	was := runGH
	t.Cleanup(func() { runGH = was })
	var calls [][]string
	runGH = func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		if args[0] == "workflow" {
			return nil, nil
		}
		endpoint := args[len(args)-1]
		if body, ok := answers[endpoint]; ok {
			return []byte(body), nil
		}
		return nil, errors.New("gh api: gh: Not Found (HTTP 404)")
	}
	return &calls
}

const dispatchable = `name: release
on:
  push:
    tags: ["v*.*.*"]
  workflow_dispatch:
    inputs:
      version:
        required: true
`

func TestAProjectWithoutAVersionInputCannotBeReleased(t *testing.T) {
	fakeGH(t, nil)
	for _, workflow := range []string{"", "on:\n  push:\n    tags: [\"v*\"]\n", "on: workflow_dispatch\n"} {
		got, err := ReadProjectRelease(releaseProject(t, workflow), "app")
		if err != nil || got.Releasable || got.Reason == "" {
			t.Fatalf("workflow %q: %+v %v", workflow, got, err)
		}
	}
}

func TestReadsTheLatestReleaseAndSuggestsTheNext(t *testing.T) {
	fakeGH(t, map[string]string{
		"repos/someone/app/releases?per_page=10":                          `[{"tag_name":"v0.4.0","draft":true},{"tag_name":"v0.3.5","html_url":"https://github.com/someone/app/releases/tag/v0.3.5"}]`,
		"repos/someone/app/actions/workflows/release.yml/runs?per_page=3": `{"workflow_runs":[{"id":7,"status":"waiting","event":"workflow_dispatch","html_url":"https://github.com/someone/app/actions/runs/7"}]}`,
	})
	got, err := ReadProjectRelease(releaseProject(t, dispatchable), "app")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Releasable || got.Repo != "someone/app" || got.Latest == nil || got.Latest.Tag != "v0.3.5" || got.Next != "v0.3.6" {
		t.Fatalf("%+v", got)
	}
	if len(got.Runs) != 1 || got.Runs[0].Status != "waiting" {
		t.Fatalf("runs: %+v", got.Runs)
	}
}

func TestStartingAReleaseRunsTheWorkflowOnTheDefaultBranch(t *testing.T) {
	calls := fakeGH(t, map[string]string{
		"repos/someone/app":                     `{"default_branch":"trunk"}`,
		"repos/someone/app/git/ref/tags/v0.3.5": `{"ref":"refs/tags/v0.3.5"}`,
	})
	root := releaseProject(t, dispatchable)
	if _, err := StartProjectRelease(root, "app", "nonsense"); err == nil {
		t.Fatal("a version that is not one was accepted")
	}
	if _, err := StartProjectRelease(root, "app", "v0.3.5"); err == nil || !strings.Contains(err.Error(), "already has a tag") {
		t.Fatalf("an existing tag was released again: %v", err)
	}
	started, err := StartProjectRelease(root, "app", "0.3.6")
	if err != nil {
		t.Fatal(err)
	}
	last := strings.Join((*calls)[len(*calls)-1], " ")
	if started["version"] != "v0.3.6" || last != "workflow run release.yml -R github.com/someone/app --ref trunk -f version=v0.3.6" {
		t.Fatalf("%v; ran %s", started, last)
	}
}

func TestNextVersion(t *testing.T) {
	for tag, want := range map[string]string{"v0.3.5": "v0.3.6", "v1.0.0-rc.2": "v1.0.0", "": "v0.1.0"} {
		if got := NextVersion(tag); got != want {
			t.Errorf("NextVersion(%q) = %s, want %s", tag, got, want)
		}
	}
}
