package local

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// resetGitRuns clears the invocation counter so a test measures only itself.
func resetGitRuns() {
	gitRuns.Lock()
	defer gitRuns.Unlock()
	gitRuns.n = make(map[string]int)
}

// gitRunCount reports how many times a git subcommand ran since the reset.
func gitRunCount(sub string) int {
	gitRuns.Lock()
	defer gitRuns.Unlock()
	return gitRuns.n[sub]
}

// worktreeFixture is one repository seen through three checkouts: the main
// one with an edited file, and two linked worktrees each holding a commit
// that exists only on this machine. It is the case #6 was filed on.
type worktreeFixture struct {
	root, origin, main, wtA, wtB string
}

func newWorktreeFixture(t *testing.T) worktreeFixture {
	t.Helper()
	f := worktreeFixture{root: filepath.Join(t.TempDir(), "repos")}
	f.origin = filepath.Join(t.TempDir(), "origin.git")
	git(t, t.TempDir(), "init", "--quiet", "--bare", "--initial-branch=main", f.origin)

	f.main = filepath.Join(f.root, "thing")
	write(t, filepath.Join(f.main, "README.md"), "hello\n")
	git(t, f.main, "init", "--quiet", "--initial-branch=main")
	git(t, f.main, "add", "README.md")
	git(t, f.main, "commit", "--quiet", "-m", "initial commit")
	git(t, f.main, "remote", "add", "origin", f.origin)
	git(t, f.main, "push", "--quiet", "-u", "origin", "main")

	f.wtA = filepath.Join(f.root, "wt-a")
	f.wtB = filepath.Join(f.root, "wt-b")
	git(t, f.main, "worktree", "add", "--quiet", "-b", "feature-a", f.wtA)
	git(t, f.main, "worktree", "add", "--quiet", "-b", "feature-b", f.wtB)
	for _, wt := range []string{f.wtA, f.wtB} {
		write(t, filepath.Join(wt, "work.txt"), "local only\n")
		git(t, wt, "add", "work.txt")
		git(t, wt, "commit", "--quiet", "-m", "local only work")
	}
	write(t, filepath.Join(f.main, "README.md"), "edited\n")
	return f
}

func testWatcher(root string) *Watcher {
	return NewWatcher(Config{Roots: []string{root}, MaxDepth: 2}, slog.New(slog.DiscardHandler), time.Minute, time.Hour)
}

// TestWorktreesShareAGroup is the fixture the plan calls mandatory: three
// discovered checkouts collapse to one repository, exactly one is the main
// checkout, and the repository counts once even though two checkouts hold
// unpushed commits and the third is dirty.
func TestWorktreesShareAGroup(t *testing.T) {
	f := newWorktreeFixture(t)
	paths := Discover(t.Context(), Config{Roots: []string{f.root}, MaxDepth: 2})
	if len(paths) != 3 {
		t.Fatalf("Discover found %v, want the main checkout and both worktrees", paths)
	}

	w := testWatcher(f.root)
	resetGitRuns()
	w.inspectAll(t.Context(), paths)
	repos := w.Snapshot().Repos
	if len(repos) != 3 {
		t.Fatalf("snapshot holds %d repos, want 3", len(repos))
	}

	groups := make(map[string]int)
	mains := 0
	byName := make(map[string]Repo)
	for _, r := range repos {
		if r.Error != "" {
			t.Fatalf("%s: %s", r.Path, r.Error)
		}
		groups[r.Group]++
		if r.Main {
			mains++
		}
		byName[r.Name] = r
	}
	if len(groups) != 1 {
		t.Errorf("groups = %v, want every checkout in one group", groups)
	}
	if mains != 1 || !byName["thing"].Main {
		t.Errorf("Main is set on %d checkouts, want exactly the main one", mains)
	}
	if got := byName["thing"]; !got.Dirty() || got.AtRisk() {
		t.Errorf("main checkout: Dirty %v AtRisk %v, want dirty only", got.Dirty(), got.AtRisk())
	}
	for _, name := range []string{"wt-a", "wt-b"} {
		if got := byName[name]; got.Unpushed != 1 || !got.AtRisk() || got.Dirty() {
			t.Errorf("%s = %+v, want one unpushed commit, at risk, clean", name, got)
		}
	}
	if got := RepositoriesAtRisk(repos); got != 1 {
		t.Errorf("RepositoriesAtRisk = %d, want 1: three checkouts are one repository", got)
	}

	// The listing runs once for the repository, not once per checkout, and
	// grouping itself costs no process at all.
	if got := gitRunCount("worktree"); got != 1 {
		t.Errorf("git worktree ran %d times, want once per repository", got)
	}
	if got := gitRunCount("rev-parse"); got != 0 {
		t.Errorf("git rev-parse ran %d times, want the group read from disk", got)
	}
	if got := gitRunCount("status"); got != 3 {
		t.Errorf("git status ran %d times, want once per checkout", got)
	}

	// The snapshot keeps the group together with the main checkout first.
	var order []string
	for _, r := range repos {
		order = append(order, r.Name)
	}
	if strings.Join(order, " ") != "thing wt-a wt-b" {
		t.Errorf("order = %v, want the main checkout then its worktrees", order)
	}
}

// TestPrunableWorktreeIsReported covers the registration discovery can never
// walk to: a worktree whose directory was deleted. git still lists it, with
// a reason, and pruning it makes the entry go away.
func TestPrunableWorktreeIsReported(t *testing.T) {
	f := newWorktreeFixture(t)
	if err := os.RemoveAll(f.wtB); err != nil {
		t.Fatal(err)
	}
	paths := Discover(t.Context(), Config{Roots: []string{f.root}, MaxDepth: 2})
	if len(paths) != 2 {
		t.Fatalf("Discover found %v, want the two checkouts that still exist", paths)
	}

	w := testWatcher(f.root)
	w.inspectAll(t.Context(), paths)
	var gone *Repo
	for _, r := range w.Snapshot().Repos {
		if r.Prunable != "" {
			gone = &r
		}
	}
	if gone == nil {
		t.Fatalf("snapshot = %+v, want the deleted worktree reported as prunable", w.Snapshot().Repos)
	}
	if realPath(gone.Path) != realPath(f.wtB) || gone.Branch != "feature-b" {
		t.Errorf("prunable entry = %+v, want the deleted worktree's path and branch", gone)
	}
	if !strings.Contains(gone.Prunable, "non-existent") {
		t.Errorf("Prunable = %q, want git's reason", gone.Prunable)
	}
	if gone.Group != Inspect(t.Context(), f.main).Group {
		t.Errorf("Group = %q, want the repository's group", gone.Group)
	}
	if gone.AtRisk() || gone.Dirty() {
		t.Error("a prunable registration has no state and must not count")
	}

	git(t, f.main, "worktree", "prune")
	w.inspectAll(t.Context(), paths)
	for _, r := range w.Snapshot().Repos {
		if r.Prunable != "" {
			t.Errorf("%s still reported after git worktree prune", r.Path)
		}
	}
}

// TestFetchOncePerRepository pins the #14 saving: three checkouts of one
// repository share an object store, so the remote is fetched once and every
// checkout is re-read afterwards.
func TestFetchOncePerRepository(t *testing.T) {
	f := newWorktreeFixture(t)
	w := testWatcher(f.root)
	w.inspectAll(t.Context(), Discover(t.Context(), Config{Roots: []string{f.root}, MaxDepth: 2}))
	w.fetchOn.Store(true)

	resetGitRuns()
	w.fetchAll(context.Background())
	if got := gitRunCount("fetch"); got != 1 {
		t.Errorf("git fetch ran %d times, want once for the repository", got)
	}
	if got := gitRunCount("status"); got != 3 {
		t.Errorf("git status ran %d times after the fetch, want every checkout re-read", got)
	}
}

// TestSortReposKeepsGroupsTogether pins the order the panel trusts: groups
// at risk first, a group's checkouts adjacent with the main one first, and
// names otherwise.
func TestSortReposKeepsGroupsTogether(t *testing.T) {
	repos := []Repo{
		{Path: "/z", Name: "zeta", Group: "/z/.git", Main: true, Modified: 1},
		{Path: "/wt-b", Name: "wt-b", Group: "/thing/.git", Unpushed: 1},
		{Path: "/alpha", Name: "alpha", Group: "/alpha/.git", Main: true},
		{Path: "/thing", Name: "thing", Group: "/thing/.git", Main: true, Modified: 1},
		{Path: "/wt-a", Name: "wt-a", Group: "/thing/.git", Unpushed: 1},
		{Path: "/beta", Name: "beta", Group: "/beta/.git", Main: true, Operation: OpRebase},
	}
	sortRepos(repos)
	var got []string
	for _, r := range repos {
		got = append(got, r.Name)
	}
	want := "beta thing wt-a wt-b alpha zeta"
	if strings.Join(got, " ") != want {
		t.Errorf("order = %v, want %s", got, want)
	}
}

// TestRepoJSONCarriesTheClassification pins the wire form the panel reads:
// atRisk and dirty are written from the same predicates the daemon uses, so
// the two cannot disagree.
func TestRepoJSONCarriesTheClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		repo   Repo
		atRisk bool
		dirty  bool
	}{
		{"clean", Repo{Path: "/a"}, false, false},
		{"dirty only", Repo{Path: "/a", Modified: 2}, false, true},
		{"unpushed", Repo{Path: "/a", Unpushed: 1}, true, false},
		{"interrupted and dirty", Repo{Path: "/a", Operation: OpMerge, Conflicted: 1}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.repo)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Path   string `json:"path"`
				AtRisk bool   `json:"atRisk"`
				Dirty  bool   `json:"dirty"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got.Path != "/a" || got.AtRisk != tc.atRisk || got.Dirty != tc.dirty {
				t.Errorf("json = %s, want atRisk %v dirty %v with the plain fields kept", raw, tc.atRisk, tc.dirty)
			}
		})
	}
}

func TestParseWorktrees(t *testing.T) {
	out := "worktree /p/main\nHEAD 1111\nbranch refs/heads/master\n\n" +
		"worktree /p/wt-a\nHEAD 2222\nbranch refs/heads/feature-a\n\n" +
		"worktree /p/wt-b\nHEAD 3333\ndetached\nprunable gitdir file points to non-existent location\n\n"
	got := parseWorktrees(out)
	if len(got) != 3 {
		t.Fatalf("parsed %d entries, want 3", len(got))
	}
	if !got[0].Main || got[0].Branch != "master" || got[1].Main || got[1].Branch != "feature-a" {
		t.Errorf("entries = %+v, want the first main and branches without refs/heads", got)
	}
	if got[2].Prunable != "gitdir file points to non-existent location" || got[2].Branch != "" {
		t.Errorf("prunable entry = %+v, want git's reason and no branch", got[2])
	}
}
