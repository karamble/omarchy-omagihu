package local

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sync/errgroup"
)

const (
	// DefaultRefresh re-inspects every repository on a timer. fsnotify covers
	// git operations, but a plain editor save never touches .git, so working
	// tree dirt still needs a sweep.
	DefaultRefresh = time.Minute
	// DefaultRediscover looks for repositories that appeared or vanished.
	DefaultRediscover = 10 * time.Minute

	debounce      = 500 * time.Millisecond
	inspectLimit  = 8
	minRefresh    = 5 * time.Second
	minRediscover = time.Minute
)

// Watcher keeps the local view current. fsnotify reacts to git operations
// immediately; a timer catches working tree edits, which never touch .git.
type Watcher struct {
	// cfg is swapped rather than mutated, so the roots can change while the
	// watcher runs: every pass re-reads it, and a reconfigure takes effect on
	// the next one with nothing to restart.
	cfg        atomic.Pointer[Config]
	logger     *slog.Logger
	refresh    time.Duration
	rediscover time.Duration

	mu    sync.Mutex
	repos map[string]Repo
	// watched maps each watched checkout to the git directories its events
	// arrive from, which for a linked worktree are not under its own path.
	watched map[string]gitDirs
	// prunable holds the worktree registrations git reports whose directory
	// is gone, keyed by that path. Discovery cannot find them, so they live
	// beside the checkouts it did find.
	prunable map[string]Repo

	snap   atomic.Pointer[Snapshot]
	paused atomic.Bool

	// Background fetch settings, read live by the fetch loop.
	fetchOn    atomic.Bool
	fetchEvery atomic.Int64
	// fetchNow carries a single nudge, so a refresh brings the remote refs up
	// to date instead of waiting out the fetch cadence.
	fetchNow chan struct{}
	// resumed carries a single nudge so waking re-inspects at once instead of
	// waiting out the refresh timer with a stale or empty view.
	resumed chan struct{}
}

// config is the settings as they stand right now.
func (w *Watcher) config() Config { return *w.cfg.Load() }

// SetRoots changes where checkouts are looked for. It takes effect on the next
// discovery pass, which is what lets the roots be reconfigured from the panel
// without restarting anything.
func (w *Watcher) SetRoots(roots []string) {
	next := w.config()
	next.Roots = slices.Clone(roots)
	w.cfg.Store(&next)
	w.Refresh()
}

// SetPaused stops or resumes inspection. While paused no git process is
// spawned and filesystem events are dropped, so the watcher is idle.
func (w *Watcher) SetPaused(paused bool) {
	was := w.paused.Swap(paused)
	if was && !paused {
		select {
		case w.resumed <- struct{}{}:
		default:
		}
	}
}

// Paused reports the current state of the switch.
func (w *Watcher) Paused() bool { return w.paused.Load() }

// Refresh asks for an immediate re-inspection, and for the remote refs to be
// brought up to date, instead of waiting out either timer.
func (w *Watcher) Refresh() {
	select {
	case w.resumed <- struct{}{}:
	default:
	}
	select {
	case w.fetchNow <- struct{}{}:
	default:
	}
}

// ForceRefresh satisfies ForceRefreshable for the API's user-initiated
// refresh. For the local watcher this is equivalent to Refresh() since
// there is no conditional GitHub cache to bypass.
func (w *Watcher) ForceRefresh() { w.Refresh() }

// NewWatcher builds a watcher. Nothing runs until Run is called.
func NewWatcher(cfg Config, logger *slog.Logger, refresh, rediscover time.Duration) *Watcher {
	w := &Watcher{
		logger:     logger,
		refresh:    max(refresh, minRefresh),
		rediscover: max(rediscover, minRediscover),
		repos:      make(map[string]Repo),
		prunable:   make(map[string]Repo),
		watched:    make(map[string]gitDirs),
		resumed:    make(chan struct{}, 1),
		fetchNow:   make(chan struct{}, 1),
	}
	w.cfg.Store(&cfg)
	w.publish()
	return w
}

// Snapshot returns the current view. It never blocks and never returns nil.
func (w *Watcher) Snapshot() *Snapshot {
	if s := w.snap.Load(); s != nil {
		return s
	}
	return &Snapshot{TakenAt: time.Now()}
}

// Run discovers repositories, watches them, and returns when ctx is done.
func (w *Watcher) Run(ctx context.Context) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		// Without fsnotify the timer still works, so degrade rather than fail.
		w.logger.Warn("filesystem watch unavailable, falling back to polling", "err", err)
		return w.pollOnly(ctx)
	}
	defer fsw.Close()

	paths := w.rescan(ctx, fsw, nil)
	w.logger.Info("watching repositories", "count", len(paths), "roots", w.config().Roots)

	refresh := time.NewTicker(w.refresh)
	defer refresh.Stop()
	rediscover := time.NewTicker(w.rediscover)
	defer rediscover.Stop()

	// Coalesce bursts: a single git command touches several files under .git.
	pending := make(map[string]struct{})
	var debounceC <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return nil

		case event, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			for _, repo := range w.reposFor(event.Name) {
				pending[repo] = struct{}{}
				debounceC = time.After(debounce)
			}

		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			w.logger.Warn("filesystem watch error", "err", err)

		case <-debounceC:
			debounceC = nil
			changed := make([]string, 0, len(pending))
			for p := range pending {
				changed = append(changed, p)
			}
			clear(pending)
			w.inspectAll(ctx, changed)

		case <-refresh.C:
			w.inspectAll(ctx, paths)

		case <-w.resumed:
			// Waking should show current state immediately, not in a minute.
			paths = w.rescan(ctx, fsw, paths)

		case <-rediscover.C:
			paths = w.rescan(ctx, fsw, paths)
		}
	}
}

// pollOnly is the degraded loop used when fsnotify is unavailable.
func (w *Watcher) pollOnly(ctx context.Context) error {
	paths := Discover(ctx, w.config())
	w.inspectAll(ctx, paths)

	refresh := time.NewTicker(w.refresh)
	defer refresh.Stop()
	rediscover := time.NewTicker(w.rediscover)
	defer rediscover.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-refresh.C:
			w.inspectAll(ctx, paths)
		case <-w.resumed:
			w.inspectAll(ctx, paths)
		case <-rediscover.C:
			paths = Discover(ctx, w.config())
			w.inspectAll(ctx, paths)
		}
	}
}

// rescan re-runs discovery, moves the fsnotify watches to match, and inspects
// everything found.
func (w *Watcher) rescan(ctx context.Context, fsw *fsnotify.Watcher, old []string) []string {
	paths := Discover(ctx, w.config())

	for _, p := range old {
		if !slices.Contains(paths, p) {
			w.unwatch(fsw, p)
			w.forget(p)
		}
	}
	for _, p := range paths {
		if !slices.Contains(old, p) {
			w.watch(fsw, p)
		}
	}
	w.inspectAll(ctx, paths)
	return paths
}

// gitDirs is where one checkout's git state lives: its own git directory,
// and the common one its branches are written under. For a plain checkout
// the two are the same.
type gitDirs struct {
	own    string
	common string
}

// watch adds the checkout's git directory and the common heads, which is
// where git operations leave their traces. A linked worktree's own directory
// sits under the main checkout's, and its branch moves in the common
// directory, so both are watched and both are remembered for reposFor. The
// working tree itself is deliberately not watched: it would mean recursive
// watches over every source file for little gain, since the refresh timer
// already catches edits.
func (w *Watcher) watch(fsw *fsnotify.Watcher, repoPath string) {
	dir, err := resolveGitDir(repoPath)
	if err != nil {
		return
	}
	common, _ := commonDir(repoPath)
	if common == "" {
		common = realPath(dir)
	}
	dirs := gitDirs{own: realPath(dir), common: common}
	for _, p := range []string{dirs.own, filepath.Join(dirs.common, "refs", "heads")} {
		if err := fsw.Add(p); err != nil {
			w.logger.Debug("cannot watch path", "path", p, "err", err)
		}
	}
	w.mu.Lock()
	w.watched[repoPath] = dirs
	w.mu.Unlock()
}

// unwatch drops a checkout's watches, keeping the common heads while another
// checkout of the same repository still needs them.
func (w *Watcher) unwatch(fsw *fsnotify.Watcher, repoPath string) {
	w.mu.Lock()
	dirs, ok := w.watched[repoPath]
	delete(w.watched, repoPath)
	shared := false
	for _, other := range w.watched {
		if other.common == dirs.common {
			shared = true
		}
	}
	w.mu.Unlock()
	if !ok {
		return
	}
	_ = fsw.Remove(dirs.own)
	if !shared {
		_ = fsw.Remove(filepath.Join(dirs.common, "refs", "heads"))
	}
}

// reposFor maps a changed path back to the checkouts it belongs to. A write
// inside a git directory names its checkout, the deepest match winning so a
// worktree's directory is not read as its main checkout's. A branch written
// under the common heads names the checkout on that branch, or every
// checkout of the repository when none is, and so does a rewrite of
// packed-refs, which can move any branch at once.
func (w *Watcher) reposFor(changed string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Branch writes come first: the common heads sit inside the main
	// checkout's own directory, so they would otherwise read as its own.
	for _, dirs := range w.watched {
		heads := filepath.Join(dirs.common, "refs", "heads")
		var branch string
		switch {
		case under(changed, heads):
			branch = strings.TrimSuffix(strings.TrimPrefix(changed, heads+string(filepath.Separator)), ".lock")
		// Only the packed file itself: git takes packed-refs.lock on every
		// commit from any checkout without moving a ref, and that must wake
		// nobody, least of all the main checkout it happens to sit in.
		case filepath.Base(changed) == "packed-refs.lock" && filepath.Dir(changed) == dirs.common:
			return nil
		case filepath.Base(changed) == "packed-refs" && filepath.Dir(changed) == dirs.common:
		default:
			continue
		}
		var members, onBranch []string
		for path, d := range w.watched {
			if d.common != dirs.common {
				continue
			}
			members = append(members, path)
			if branch != "" && w.repos[path].Branch == branch {
				onBranch = append(onBranch, path)
			}
		}
		if len(onBranch) > 0 {
			return onBranch
		}
		return members
	}

	// Otherwise the deepest git directory containing the path owns it, so a
	// worktree's directory is not read as the main checkout's around it.
	var owner string
	for path, dirs := range w.watched {
		if under(changed, dirs.own) && (owner == "" || len(dirs.own) > len(w.watched[owner].own)) {
			owner = path
		}
	}
	if owner == "" {
		return nil
	}
	return []string{owner}
}

// under reports whether path lies inside dir.
func under(path, dir string) bool {
	return dir != "" && strings.HasPrefix(path, dir+string(filepath.Separator))
}

// inspectAll re-reads the given repositories with bounded concurrency, then
// republishes the snapshot once.
func (w *Watcher) inspectAll(ctx context.Context, paths []string) {
	if w.paused.Load() {
		return
	}
	if len(paths) == 0 {
		w.publish()
		return
	}

	results := make([]Repo, len(paths))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(inspectLimit)
	for i, p := range paths {
		g.Go(func() error {
			results[i] = Inspect(gctx, p)
			return nil
		})
	}
	_ = g.Wait()
	listed := w.groupWorktrees(ctx, results)

	w.mu.Lock()
	for _, r := range results {
		if r.Path != "" {
			w.repos[r.Path] = r
		}
	}
	for group, entries := range listed {
		for path, r := range w.prunable {
			if r.Group == group {
				delete(w.prunable, path)
			}
		}
		for _, r := range entries {
			w.prunable[r.Path] = r
		}
	}
	w.mu.Unlock()
	w.publish()
}

// groupWorktrees asks git, once per repository that has ever registered a
// worktree, which checkouts belong to it. It confirms which member is the
// main one and returns, per group listed, the registrations whose directory
// is gone: those are the checkouts discovery can never walk to.
func (w *Watcher) groupWorktrees(ctx context.Context, results []Repo) map[string][]Repo {
	members := make(map[string][]int)
	for i, r := range results {
		if r.Group != "" && r.Error == "" {
			members[r.Group] = append(members[r.Group], i)
		}
	}
	listed := make(map[string][]Repo)
	for group, idx := range members {
		if !hasLinkedWorktrees(group) {
			continue
		}
		trees, err := listWorktrees(ctx, results[idx[0]].Path)
		if err != nil {
			w.logger.Debug("worktree list failed", "repo", results[idx[0]].Path, "err", err)
			continue
		}
		listed[group] = nil
		for _, t := range trees {
			if t.Prunable != "" {
				listed[group] = append(listed[group], Repo{
					Path: t.Path, Name: filepath.Base(t.Path), Branch: t.Branch,
					Group: group, Prunable: t.Prunable, ObservedAt: time.Now(),
				})
				continue
			}
			for _, i := range idx {
				if realPath(results[i].Path) == t.Path {
					results[i].Main = t.Main
				}
			}
		}
	}
	return listed
}

// forget drops a checkout that discovery no longer finds, and the prunable
// registrations of its repository once no member is left to list them.
func (w *Watcher) forget(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	group := w.repos[path].Group
	delete(w.repos, path)
	for _, r := range w.repos {
		if r.Group == group {
			return
		}
	}
	for p, r := range w.prunable {
		if r.Group == group {
			delete(w.prunable, p)
		}
	}
}

// publish rebuilds the immutable snapshot readers see.
func (w *Watcher) publish() {
	w.mu.Lock()
	snap := &Snapshot{
		TakenAt: time.Now(),
		Roots:   slices.Clone(w.config().Roots),
		Repos:   make([]Repo, 0, len(w.repos)),
	}
	for _, r := range w.repos {
		snap.Repos = append(snap.Repos, r)
	}
	for _, r := range w.prunable {
		snap.Repos = append(snap.Repos, r)
	}
	w.mu.Unlock()

	sortRepos(snap.Repos)
	w.snap.Store(snap)
}

// sortRepos orders repositories at risk first, then by name, and keeps a
// repository's checkouts together with the main one first, so the panel has
// a stable order it can group without re-sorting.
func sortRepos(repos []Repo) {
	// A group sorts by its main checkout's name, or its first name when the
	// main one is not watched, and is at risk when any member is.
	risk := make(map[string]bool)
	name := make(map[string]string)
	for _, r := range repos {
		if r.Main {
			name[r.GroupKey()] = r.Name
		}
	}
	for _, r := range repos {
		key := r.GroupKey()
		risk[key] = risk[key] || r.AtRisk()
		if n, ok := name[key]; !ok || (!hasMain(repos, key) && r.Name < n) {
			name[key] = r.Name
		}
	}
	slices.SortFunc(repos, func(a, b Repo) int {
		ka, kb := a.GroupKey(), b.GroupKey()
		if risk[ka] != risk[kb] {
			if risk[ka] {
				return -1
			}
			return 1
		}
		if c := strings.Compare(name[ka], name[kb]); c != 0 {
			return c
		}
		if c := strings.Compare(a.GroupKey(), b.GroupKey()); c != 0 {
			return c
		}
		if a.Main != b.Main {
			if a.Main {
				return -1
			}
			return 1
		}
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Path, b.Path)
	})
}

// hasMain reports whether a group's main checkout is among the repos.
func hasMain(repos []Repo, key string) bool {
	for _, r := range repos {
		if r.Main && r.GroupKey() == key {
			return true
		}
	}
	return false
}
