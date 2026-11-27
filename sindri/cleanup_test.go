package sindri

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func cleanRepo(t *testing.T, root, name string) string {
	t.Helper()
	repo := filepath.Join(root, name)
	write := func(rel, text string) {
		path := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "node_modules/\n.nuxt/\n")
	write("build/keep.txt", "tracked on purpose")
	write("src/app.js", "app")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "add", "."},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "first"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write("node_modules/left-pad/index.js", "module.exports = 1")
	write("packages/ui/node_modules/x/index.js", "x")
	write(".nuxt/cache.json", "{}")
	write("dist/bundle.js", "not ignored: someone keeps it")
	return repo
}

func TestCleanerRemovesIgnoredBuildFoldersOfQuietProjects(t *testing.T) {
	root := t.TempDir()
	repo := cleanRepo(t, root, "old-site")
	later := time.Now().Add(120 * 24 * time.Hour)
	c := &Cleaner{Root: root, After: 90 * 24 * time.Hour, Now: func() time.Time { return later }, StatePath: filepath.Join(t.TempDir(), "last.json")}

	projects := c.Scan(context.Background())
	if len(projects) != 1 || !projects[0].Inactive {
		t.Fatalf("projects = %+v", projects)
	}
	var names []string
	for _, target := range projects[0].Targets {
		rel, _ := filepath.Rel(repo, target.Path)
		names = append(names, rel)
	}
	want := map[string]bool{".nuxt": true, "node_modules": true, filepath.Join("packages", "ui", "node_modules"): true}
	if len(names) != len(want) {
		t.Fatalf("targets = %v", names)
	}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("unexpected target %s in %v", n, names)
		}
	}

	run, err := c.Clean(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Removed) != 3 || run.Bytes == 0 || len(run.Errors) != 0 {
		t.Fatalf("run = %+v", run)
	}
	for _, gone := range []string{"node_modules", ".nuxt", "packages/ui/node_modules"} {
		if exists(filepath.Join(repo, gone)) {
			t.Errorf("%s is still there", gone)
		}
	}
	for _, kept := range []string{"dist/bundle.js", "build/keep.txt", "src/app.js", ".git"} {
		if !exists(filepath.Join(repo, kept)) {
			t.Errorf("%s was removed", kept)
		}
	}
	if last := (&Cleaner{StatePath: c.StatePath}).Last(); last == nil || len(last.Removed) != 3 {
		t.Fatalf("the last run was not kept: %+v", last)
	}
}

func TestCleanerLeavesRecentProjectsUnlessAsked(t *testing.T) {
	root := t.TempDir()
	repo := cleanRepo(t, root, "busy")
	c := &Cleaner{Root: root, After: 90 * 24 * time.Hour}

	run, err := c.Clean(context.Background(), "")
	if err != nil || len(run.Removed) != 0 {
		t.Fatalf("a recent project was cleaned: %+v %v", run, err)
	}
	if !exists(filepath.Join(repo, "node_modules")) {
		t.Fatal("node_modules went")
	}
	// Asked by name, it goes now.
	run, err = c.Clean(context.Background(), "busy")
	if err != nil || len(run.Removed) != 3 {
		t.Fatalf("cleaning by name: %+v %v", run, err)
	}
	if _, err := c.Clean(context.Background(), "nope"); err == nil {
		t.Fatal("an unknown project was not refused")
	}
}

func TestCleanerNeverFollowsALink(t *testing.T) {
	root := t.TempDir()
	repo := cleanRepo(t, root, "linked")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "precious.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(repo, ".nuxt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ".nuxt")); err != nil {
		t.Fatal(err)
	}
	if err := removeInside(repo, filepath.Join(repo, ".nuxt")); err == nil {
		t.Fatal("removed through a link")
	}
	if err := removeInside(repo, filepath.Join(repo, "src")); err == nil {
		t.Fatal("removed a folder not named for cleaning")
	}
	if !exists(filepath.Join(outside, "precious.txt")) {
		t.Fatal("a file outside the project went")
	}
}
