package sindri

import (
	"os/exec"
	"strings"
	"testing"
)

func TestReviewOfBranchWithNoCommitsIsNotMerged(t *testing.T) {
	_, repo := setup(t)
	base, _ := tryGit(repo, "rev-parse", "HEAD")
	if out, err := exec.Command("git", "-C", repo, "branch", "agent/empty").CombinedOutput(); err != nil {
		t.Fatalf("git branch: %v %s", err, out)
	}
	w := &Worktree{Repo: repo, Path: t.TempDir(), Branch: "agent/empty", Base: "main", BaseCommit: base}
	review, err := w.ReviewOf("")
	if err != nil {
		t.Fatal(err)
	}
	if review.Merged || len(review.Commits) != 0 {
		t.Fatalf("merged=%v commits=%d, want an unmerged branch with no commits", review.Merged, len(review.Commits))
	}
	if _, err := w.Merge(); err == nil || !strings.Contains(err.Error(), "no commits yet") {
		t.Fatalf("Merge() = %v, want it refused for having no commits", err)
	}
}
