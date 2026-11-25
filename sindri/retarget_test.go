package sindri

import (
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
