package sindri

import (
	"os"
	"os/exec"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestAWorktreeCutFromAMergedFeatureBranchIsReviewedAgainstMain(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-b", "main")
	gitIn(t, repo, "commit", "--allow-empty", "-m", "first")
	gitIn(t, repo, "checkout", "-b", "feature")
	gitIn(t, repo, "commit", "--allow-empty", "-m", "feature work")
	w := &Worktree{Repo: repo, Branch: "agent/x", Base: "feature"}

	w.Retarget()
	if w.Base != "feature" {
		t.Fatalf("retargeted while the checkout is still on feature: %s", w.Base)
	}

	gitIn(t, repo, "checkout", "main")
	w.Retarget()
	if w.Base != "feature" {
		t.Fatalf("retargeted before feature was merged: %s", w.Base)
	}

	gitIn(t, repo, "merge", "--no-ff", "feature", "-m", "merge feature")
	w.Retarget()
	if w.Base != "main" {
		t.Fatalf("still on %s after feature merged into main", w.Base)
	}

	w.Base = "deleted"
	w.Retarget()
	if w.Base != "main" {
		t.Fatalf("a deleted base stayed: %s", w.Base)
	}
}

func TestMoveOntoReplaysOnlyTheBranchsOwnCommitsOntoTheCheckoutsBranch(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-b", "main")
	gitIn(t, repo, "config", "user.name", "t")
	gitIn(t, repo, "config", "user.email", "t@example.com")
	gitIn(t, repo, "commit", "--allow-empty", "-m", "first")
	gitIn(t, repo, "checkout", "-b", "old-feature")
	gitIn(t, repo, "commit", "--allow-empty", "-m", "old feature work")
	cut, _ := tryGit(repo, "rev-parse", "--short", "HEAD")
	path := t.TempDir() + "/wt"
	gitIn(t, repo, "worktree", "add", "-b", "agent/x", path)
	if err := os.WriteFile(path+"/fix.txt", []byte("fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, path, "add", "fix.txt")
	gitIn(t, path, "commit", "-m", "the fix")
	gitIn(t, repo, "checkout", "main")
	w := &Worktree{Repo: repo, Path: path, Branch: "agent/x", Base: "old-feature", BaseCommit: cut}

	done, conflicts, err := w.MoveOnto("")
	if err != nil || len(conflicts) > 0 {
		t.Fatalf("move failed: %v %v", err, conflicts)
	}
	if w.Base != "main" {
		t.Fatalf("base is %s, not main: %s", w.Base, done)
	}
	if subjects, _ := tryGit(repo, "log", "--format=%s", "main..agent/x"); subjects != "the fix" {
		t.Fatalf("branch carries more than its own commit: %q", subjects)
	}
	if _, err := w.Merge(); err != nil {
		t.Fatalf("merge after the move: %v", err)
	}
}
