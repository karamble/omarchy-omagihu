package local

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git runs a git command in dir and fails the test if it errors. No identity
// flags are passed: the machine's own git config supplies the author.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newRepo makes a repository with one commit on a branch called main.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "--quiet", "--initial-branch=main")
	write(t, filepath.Join(dir, "README.md"), "hello\n")
	git(t, dir, "add", "README.md")
	git(t, dir, "commit", "--quiet", "-m", "initial commit")
	return dir
}

func TestInspectCleanRepo(t *testing.T) {
	dir := newRepo(t)

	repo := Inspect(t.Context(), dir)
	if repo.Error != "" {
		t.Fatalf("Error = %q, want none", repo.Error)
	}
	if repo.Branch != "main" {
		t.Errorf("Branch = %q, want main", repo.Branch)
	}
	if repo.Dirty() {
		t.Errorf("Dirty() = true for a fresh repo: %+v", repo)
	}
	if repo.AtRisk() {
		t.Errorf("AtRisk() = true for a clean repo with no remote: %+v", repo)
	}
	if !repo.NoUpstream {
		t.Error("NoUpstream = false, want true for a branch with no upstream")
	}
	if repo.Last.Subject != "initial commit" {
		t.Errorf("Last.Subject = %q, want %q", repo.Last.Subject, "initial commit")
	}
	if repo.Last.SHA == "" || repo.Last.At.IsZero() {
		t.Errorf("Last = %+v, want a resolved commit", repo.Last)
	}
}

func TestInspectCountsWorkingTreeStates(t *testing.T) {
	dir := newRepo(t)

	write(t, filepath.Join(dir, "README.md"), "hello, changed\n") // modified
	write(t, filepath.Join(dir, "new.txt"), "fresh\n")            // untracked
	write(t, filepath.Join(dir, "staged.txt"), "staged\n")        // staged add
	git(t, dir, "add", "staged.txt")
	git(t, dir, "rm", "--quiet", "--cached", "README.md") // staged delete of a tracked file

	repo := Inspect(t.Context(), dir)
	if repo.Error != "" {
		t.Fatalf("Error = %q, want none", repo.Error)
	}
	if !repo.Dirty() {
		t.Fatalf("Dirty() = false, want true: %+v", repo)
	}
	if repo.Staged == 0 {
		t.Errorf("Staged = 0, want the staged add and delete counted: %+v", repo)
	}
	if repo.Untracked < 1 {
		t.Errorf("Untracked = %d, want at least 1", repo.Untracked)
	}
	if !repo.AtRisk() {
		t.Error("AtRisk() = false for a dirty repo")
	}
}

// TestInspectUnpushed is the signal that matters most: commits that exist only
// on this machine.
func TestInspectUnpushed(t *testing.T) {
	origin := t.TempDir()
	git(t, origin, "init", "--quiet", "--bare", "--initial-branch=main")

	work := newRepo(t)
	git(t, work, "remote", "add", "origin", origin)
	git(t, work, "push", "--quiet", "-u", "origin", "main")

	clean := Inspect(t.Context(), work)
	if clean.Unpushed != 0 {
		t.Errorf("Unpushed = %d straight after a push, want 0", clean.Unpushed)
	}
	if clean.NoUpstream {
		t.Error("NoUpstream = true after push -u, want false")
	}
	if clean.AtRisk() {
		t.Errorf("AtRisk() = true for a pushed clean repo: %+v", clean)
	}

	write(t, filepath.Join(work, "local-only.txt"), "not pushed\n")
	git(t, work, "add", "local-only.txt")
	git(t, work, "commit", "--quiet", "-m", "local only work")

	ahead := Inspect(t.Context(), work)
	if ahead.Unpushed != 1 {
		t.Errorf("Unpushed = %d after one local commit, want 1", ahead.Unpushed)
	}
	if ahead.Ahead != 1 {
		t.Errorf("Ahead = %d, want 1", ahead.Ahead)
	}
	if ahead.Dirty() {
		t.Error("Dirty() = true, but the commit left a clean tree")
	}
	if !ahead.AtRisk() {
		t.Error("AtRisk() = false with unpushed commits, which is the whole point")
	}
	if got := ahead.Remotes["origin"]; got != origin {
		t.Errorf("Remotes[origin] = %q, want %q", got, origin)
	}
}

// TestInspectNoRemoteIsNotAtRisk guards against flagging every scratch repo:
// with nowhere to push, unpushed is meaningless.
func TestInspectNoRemoteIsNotAtRisk(t *testing.T) {
	repo := Inspect(t.Context(), newRepo(t))
	if repo.Unpushed != 0 {
		t.Errorf("Unpushed = %d for a repo with no remotes, want 0", repo.Unpushed)
	}
}

func TestDetectInterruptedMerge(t *testing.T) {
	dir := newRepo(t)

	git(t, dir, "checkout", "--quiet", "-b", "other")
	write(t, filepath.Join(dir, "README.md"), "other side\n")
	git(t, dir, "commit", "--quiet", "-am", "other change")

	git(t, dir, "checkout", "--quiet", "main")
	write(t, filepath.Join(dir, "README.md"), "main side\n")
	git(t, dir, "commit", "--quiet", "-am", "main change")

	// Expected to fail with a conflict, leaving MERGE_HEAD behind.
	cmd := exec.Command("git", "merge", "other")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("merge unexpectedly succeeded, wanted a conflict:\n%s", out)
	}

	repo := Inspect(t.Context(), dir)
	if repo.Operation != OpMerge {
		t.Errorf("Operation = %q, want %q", repo.Operation, OpMerge)
	}
	if repo.Conflicted == 0 {
		t.Errorf("Conflicted = 0, want the unmerged path counted: %+v", repo)
	}
	if !repo.AtRisk() {
		t.Error("AtRisk() = false during an interrupted merge")
	}
}

func TestDiscoverDepthAndExcludes(t *testing.T) {
	root := t.TempDir()

	shallow := filepath.Join(root, "a")
	deep := filepath.Join(root, "a", "b", "c", "d", "e")
	excluded := filepath.Join(root, "node_modules", "pkg")
	for _, d := range []string{shallow, deep, excluded} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
		git(t, d, "init", "--quiet")
	}

	cfg := Config{Roots: []string{root}, MaxDepth: 2, Excludes: DefaultExcludes}
	found := Discover(t.Context(), cfg)

	if !contains(found, shallow) {
		t.Errorf("Discover missed the shallow repo %s: got %v", shallow, found)
	}
	if contains(found, deep) {
		t.Errorf("Discover returned %s, which is past MaxDepth 2", deep)
	}
	if contains(found, excluded) {
		t.Errorf("Discover returned %s from an excluded directory", excluded)
	}
}

// TestDiscoverDoesNotDescendIntoRepos keeps submodules and vendored checkouts
// from multiplying the list.
func TestDiscoverDoesNotDescendIntoRepos(t *testing.T) {
	root := t.TempDir()
	outer := filepath.Join(root, "outer")
	inner := filepath.Join(outer, "vendored")
	for _, d := range []string{outer, inner} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		git(t, d, "init", "--quiet")
	}

	found := Discover(t.Context(), Config{Roots: []string{root}, MaxDepth: 4})
	if len(found) != 1 || found[0] != outer {
		t.Errorf("Discover() = %v, want just the outer repo %s", found, outer)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
