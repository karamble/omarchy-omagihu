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

// NewWatcher builds a watcher. Nothing runs until Run is called.
func NewWatcher(cfg Config, logger *slog.Logger, refresh, rediscover time.Duration) *Watcher {
	w := &Watcher{
		logger:     logger,
		refresh:    max(refresh, minRefresh),
		rediscover: max(rediscover, minRediscover),
		repos:      make(map[string]Repo),
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
			if repo := w.repoFor(event.Name, paths); repo != "" {
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

// watch adds the .git directory and its heads, which is where git operations
// leave their traces. The working tree itself is deliberately not watched: it
// would mean recursive watches over every source file for little gain, since
// the refresh timer already catches edits.
func (w *Watcher) watch(fsw *fsnotify.Watcher, repoPath string) {
	dir, err := resolveGitDir(repoPath)
	if err != nil {
		return
	}
	for _, p := range []string{dir, filepath.Join(dir, "refs", "heads")} {
		if err := fsw.Add(p); err != nil {
			w.logger.Debug("cannot watch path", "path", p, "err", err)
		}
	}
}

func (w *Watcher) unwatch(fsw *fsnotify.Watcher, repoPath string) {
	dir, err := resolveGitDir(repoPath)
	if err != nil {
		return
	}
	for _, p := range []string{dir, filepath.Join(dir, "refs", "heads")} {
		_ = fsw.Remove(p)
	}
}

// repoFor maps a changed path back to the repository that owns it.
func (w *Watcher) repoFor(changed string, paths []string) string {
	for _, p := range paths {
		if strings.HasPrefix(changed, p+string(filepath.Separator)) {
			return p
		}
	}
	return ""
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

	w.mu.Lock()
	for _, r := range results {
		if r.Path != "" {
			w.repos[r.Path] = r
		}
	}
	w.mu.Unlock()
	w.publish()
}

func (w *Watcher) forget(path string) {
	w.mu.Lock()
	delete(w.repos, path)
	w.mu.Unlock()
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
	w.mu.Unlock()

	// At-risk repositories first, then by name, so the panel has a stable order
	// that puts the things needing attention at the top.
	slices.SortFunc(snap.Repos, func(a, b Repo) int {
		if a.AtRisk() != b.AtRisk() {
			if a.AtRisk() {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	w.snap.Store(snap)
}
