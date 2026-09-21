package local

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// watchedFixture wires a watcher and a real fsnotify watcher over the given
// checkouts, inspected once so each entry carries its branch.
func watchedFixture(t *testing.T, root string, paths []string) (*Watcher, *fsnotify.Watcher) {
	t.Helper()
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsw.Close() })
	w := testWatcher(root)
	for _, p := range paths {
		w.watch(fsw, p)
	}
	w.inspectAll(context.Background(), paths)
	return w, fsw
}

// attributed collects which checkouts the events raised by act are mapped to,
// draining the stream until it goes quiet.
func attributed(t *testing.T, w *Watcher, fsw *fsnotify.Watcher, act func()) []string {
	t.Helper()
	act()
	var hit []string
	quiet := time.NewTimer(400 * time.Millisecond)
	defer quiet.Stop()
	for {
		select {
		case e := <-fsw.Events:
			for _, p := range w.reposFor(e.Name) {
				if !slices.Contains(hit, p) {
					hit = append(hit, p)
				}
			}
			quiet.Reset(400 * time.Millisecond)
		case err := <-fsw.Errors:
			t.Fatal(err)
		case <-quiet.C:
			slices.Sort(hit)
			return hit
		}
	}
}

// TestWorktreeGitDirWriteIsItsOwn is the measured bug: a write inside a
// linked worktree's git directory is attributed to that worktree, and to
// nothing else. It used to name the main checkout, whose path contains it.
func TestWorktreeGitDirWriteIsItsOwn(t *testing.T) {
	f := newWorktreeFixture(t)
	paths := Discover(t.Context(), Config{Roots: []string{f.root}, MaxDepth: 2})
	w, fsw := watchedFixture(t, f.root, paths)

	got := attributed(t, w, fsw, func() {
		write(t, filepath.Join(f.main, ".git", "worktrees", "wt-b", "OMAGIHU_TEST"), "x\n")
	})
	if !slices.Equal(got, []string{f.wtB}) {
		t.Errorf("a write in wt-b's git directory was attributed to %v, want only %s", got, f.wtB)
	}

	got = attributed(t, w, fsw, func() {
		write(t, filepath.Join(f.main, ".git", "OMAGIHU_TEST"), "x\n")
	})
	if !slices.Equal(got, []string{f.main}) {
		t.Errorf("a write in the main git directory was attributed to %v, want only %s", got, f.main)
	}
}

// TestWorktreeCommitIsItsOwn pins the ref rule: a commit in a worktree moves
// its branch under the common heads, and that write names the worktree on
// that branch and nothing else. The packed-refs.lock git takes alongside
// moves no ref and wakes nobody.
func TestWorktreeCommitIsItsOwn(t *testing.T) {
	f := newWorktreeFixture(t)
	paths := Discover(t.Context(), Config{Roots: []string{f.root}, MaxDepth: 2})
	w, fsw := watchedFixture(t, f.root, paths)

	got := attributed(t, w, fsw, func() {
		write(t, filepath.Join(f.wtA, "more.txt"), "more\n")
		git(t, f.wtA, "add", "more.txt")
		git(t, f.wtA, "commit", "--quiet", "-m", "more work")
	})
	if !slices.Equal(got, []string{f.wtA}) {
		t.Fatalf("a commit in wt-a was attributed to %v, want wt-a and nothing else", got)
	}

	// The branch write alone, without the packed-refs touch, names wt-a only.
	heads := filepath.Join(f.main, ".git", "refs", "heads")
	if only := w.reposFor(filepath.Join(heads, "feature-a.lock")); !slices.Equal(only, []string{f.wtA}) {
		t.Errorf("refs/heads/feature-a.lock mapped to %v, want wt-a", only)
	}
	if all := w.reposFor(filepath.Join(heads, "nobody")); len(all) != 3 {
		t.Errorf("an unowned branch mapped to %v, want every checkout of the repository", all)
	}
	// A rewrite of packed-refs can move any branch, so it names the group;
	// the lock alone names nobody.
	if all := w.reposFor(filepath.Join(f.main, ".git", "packed-refs")); len(all) != 3 {
		t.Errorf("packed-refs mapped to %v, want every checkout of the repository", all)
	}
	if lock := w.reposFor(filepath.Join(f.main, ".git", "packed-refs.lock")); lock != nil {
		t.Errorf("packed-refs.lock mapped to %v, want nothing", lock)
	}
}

// TestWorktreeWithoutItsMainStillSeesItsBranch covers a linked worktree
// discovered on its own: its heads live in a directory outside the watched
// roots, and they are watched anyway.
func TestWorktreeWithoutItsMainStillSeesItsBranch(t *testing.T) {
	f := newWorktreeFixture(t)
	w, fsw := watchedFixture(t, f.root, []string{f.wtA})

	got := attributed(t, w, fsw, func() {
		write(t, filepath.Join(f.wtA, "alone.txt"), "alone\n")
		git(t, f.wtA, "add", "alone.txt")
		git(t, f.wtA, "commit", "--quiet", "-m", "alone")
	})
	if !slices.Equal(got, []string{f.wtA}) {
		t.Errorf("a commit in an orphaned worktree was attributed to %v, want wt-a", got)
	}

	// Forgetting the last member drops the shared watch; a write there is
	// nobody's.
	w.unwatch(fsw, f.wtA)
	if got := w.reposFor(filepath.Join(f.main, ".git", "refs", "heads", "feature-a")); got != nil {
		t.Errorf("after unwatch a ref write mapped to %v, want nothing", got)
	}
	if _, err := os.Stat(filepath.Join(f.main, ".git", "refs", "heads")); err != nil {
		t.Fatal(err)
	}
}

// TestRunReinspectsTheWorktreeThatChanged is the whole loop end to end: a
// write in a worktree's git directory re-reads that worktree within the
// debounce, and leaves the main checkout's observation alone.
func TestRunReinspectsTheWorktreeThatChanged(t *testing.T) {
	f := newWorktreeFixture(t)
	w := testWatcher(f.root)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go w.Run(ctx)

	observed := func(path string) time.Time {
		for _, r := range w.Snapshot().Repos {
			if r.Path == path {
				return r.ObservedAt
			}
		}
		return time.Time{}
	}
	deadline := time.Now().Add(5 * time.Second)
	for observed(f.wtB).IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("the watcher never inspected the fixture")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mainBefore, wtBefore := observed(f.main), observed(f.wtB)

	write(t, filepath.Join(f.main, ".git", "worktrees", "wt-b", "MERGE_HEAD"), "0000\n")
	deadline = time.Now().Add(5 * time.Second)
	for !observed(f.wtB).After(wtBefore) {
		if time.Now().After(deadline) {
			t.Fatal("wt-b was not re-inspected after a write in its git directory")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if observed(f.main).After(mainBefore) {
		t.Error("the main checkout was re-inspected for a change that belongs to wt-b")
	}
}
