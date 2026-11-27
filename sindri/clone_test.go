package sindri

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParseGitHubRepo(t *testing.T) {
	for text, want := range map[string]string{
		"owner/name":                        "owner/name",
		" owner/name.js ":                   "owner/name.js",
		"https://github.com/owner/name":     "owner/name",
		"https://github.com/owner/name.git": "owner/name",
		"github.com/owner/name/":            "owner/name",
		"git@github.com:owner/name.git":     "owner/name",
		"https://gitlab.com/owner/name":     "",
		"owner":                             "",
		"owner/name/tree/main":              "",
		"-owner/name":                       "",
		"owner/..":                          "",
	} {
		got, ok := ParseGitHubRepo(text)
		if ok != (want != "") || got != want {
			t.Errorf("ParseGitHubRepo(%q) = %q, %v; want %q", text, got, ok, want)
		}
	}
}

// fakeClone makes a repo with a GitHub origin at path, as gh repo clone would.
func fakeClone(t *testing.T, calls *[]string) func(context.Context, string, string) error {
	return func(_ context.Context, repo, path string) error {
		*calls = append(*calls, repo+" "+path)
		for _, args := range [][]string{{"init", "--quiet", path}, {"-C", path, "remote", "add", "origin", "https://github.com/" + repo + ".git"}} {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v %s", args, err, out)
			}
		}
		return nil
	}
}

func TestCloneProject(t *testing.T) {
	var calls []string
	before := runClone
	runClone = fakeClone(t, &calls)
	t.Cleanup(func() { runClone = before })
	root := t.TempDir()

	project, err := CloneProject(context.Background(), root, "https://github.com/Owner/front.git", "")
	if err != nil {
		t.Fatal(err)
	}
	if project.Name != "front" || project.Path != filepath.Join(root, "front") {
		t.Fatalf("project = %+v", project)
	}
	if len(calls) != 1 || calls[0] != "Owner/front "+filepath.Join(root, "front") {
		t.Fatalf("calls = %v", calls)
	}
	// The same repo again, under another name, is refused: it is already here.
	if _, err := CloneProject(context.Background(), root, "owner/front", "front-2"); err == nil {
		t.Fatal("a second clone of the same repo was not refused")
	}
	// A name that is taken is refused before anything is cloned.
	if _, err := CloneProject(context.Background(), root, "owner/other", "front"); err == nil {
		t.Fatal("a taken name was not refused")
	}
	if _, err := CloneProject(context.Background(), root, "not a repo", ""); err == nil {
		t.Fatal("a bad repo was not refused")
	}
	if len(calls) != 1 {
		t.Fatalf("cloned again: %v", calls)
	}
	named, err := CloneProject(context.Background(), root, "owner/other", "mine")
	if err != nil || named.Name != "mine" {
		t.Fatalf("named = %+v, %v", named, err)
	}
	if got := len(ProjectInfos(root)); got != 2 {
		t.Fatalf("projects = %d, want 2", got)
	}
}

func TestCloneProjectFailureLeavesNothing(t *testing.T) {
	before := runClone
	runClone = func(_ context.Context, repo, path string) error {
		_ = exec.Command("git", "init", "--quiet", path).Run()
		return httpErr(502, "could not clone %s: not found", repo)
	}
	t.Cleanup(func() { runClone = before })
	root := t.TempDir()
	if _, err := CloneProject(context.Background(), root, "owner/gone", ""); err == nil {
		t.Fatal("no error")
	}
	if exists(filepath.Join(root, "gone")) {
		t.Fatal("a failed clone left its folder")
	}
}
