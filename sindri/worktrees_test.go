package sindri

import "testing"

// One worktree listed by the repo's main checkout and by a linked checkout of
// the same repo (measured against that checkout's own branch) comes back once.
func TestOnePerPathKeepsTheAgentsRepoElseTheMainCheckout(t *testing.T) {
	list := []ListedWorktree{
		{Repo: "/p/app", Path: "/wt/owned", Base: "main", Merged: true},
		{Repo: "/p/app-wt/weave", Path: "/wt/owned", Base: "feature/weave", Ahead: 8, linked: true},
		{Repo: "/p/app-wt/weave", Path: "/wt/loose", Base: "feature/weave", Ahead: 3, linked: true},
		{Repo: "/p/app", Path: "/wt/loose", Base: "main"},
		{Repo: "/p/app", Path: "/wt/from-weave", Base: "main", Ahead: 9},
		{Repo: "/p/app-wt/weave", Path: "/wt/from-weave", Base: "feature/weave", Ahead: 1, linked: true},
	}
	owners := map[string]*Agent{
		"/wt/owned":      {Worktree: &Worktree{Repo: "/p/app"}},
		"/wt/from-weave": {Worktree: &Worktree{Repo: "/p/app-wt/weave"}},
	}
	got := onePerPath(list, owners)
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %d: %+v", len(got), got)
	}
	want := map[string]string{"/wt/owned": "main", "/wt/loose": "main", "/wt/from-weave": "feature/weave"}
	for _, w := range got {
		if w.Base != want[w.Path] {
			t.Errorf("%s: base %s, want %s", w.Path, w.Base, want[w.Path])
		}
	}
}
