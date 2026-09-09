package local

import (
	"context"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	// DefaultFetchEvery is deliberately slow. Fetching is the only outbound
	// traffic the local plane makes, and nothing here needs to be current to
	// the second.
	DefaultFetchEvery = 30 * time.Minute
	minFetchEvery     = 5 * time.Minute

	// Two at a time: enough to get through a large tree in a couple of
	// minutes, few enough that a laptop on a phone tether does not notice.
	fetchLimit   = 2
	fetchTimeout = 45 * time.Second
)

// SetFetch turns background fetching on or off and sets its cadence.
func (w *Watcher) SetFetch(enabled bool, every time.Duration) {
	w.fetchOn.Store(enabled)
	w.fetchEvery.Store(int64(max(every, minFetchEvery)))
	w.logger.Info("background fetch configured",
		"enabled", enabled, "every", max(every, minFetchEvery))
}

// RunFetch keeps remote-tracking refs current until ctx is done.
//
// This is what makes "unpushed", "behind" and fork divergence mean anything: a
// commit looks unpushed for as long as the local copy of the remote refs
// predates the push. It writes only to .git. No branch is moved, nothing is
// merged, and the working tree is never touched.
func (w *Watcher) RunFetch(ctx context.Context) {
	for {
		// Jitter so a machine with several git tools on timers does not have
		// them all wake together.
		wait := time.Duration(w.fetchEvery.Load())
		if wait <= 0 {
			wait = DefaultFetchEvery
		}
		wait += rand.N(wait / 4)

		if !w.waitFetch(ctx, wait) {
			return
		}
		w.fetchAll(ctx)
	}
}

// waitFetch sleeps until the cadence elapses or a refresh asks for the refs
// now, reporting false only when the context ends.
func (w *Watcher) waitFetch(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-w.fetchNow:
		return true
	case <-t.C:
		return true
	}
}

func (w *Watcher) fetchAll(ctx context.Context) {
	if !w.fetchOn.Load() {
		return
	}
	// The master switch means no outbound traffic at all, and a fetch is
	// outbound traffic, so "nothing leaves this machine" stays literally true.
	if w.paused.Load() {
		return
	}

	w.mu.Lock()
	paths := make([]string, 0, len(w.repos))
	for path, repo := range w.repos {
		if len(repo.Remotes) > 0 {
			paths = append(paths, path)
		}
	}
	w.mu.Unlock()

	if len(paths) == 0 {
		return
	}

	started := time.Now()
	var ok atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(fetchLimit)
	for _, path := range paths {
		g.Go(func() error {
			fctx, cancel := context.WithTimeout(gctx, fetchTimeout)
			defer cancel()
			// --no-tags keeps it small, --prune drops refs for branches that
			// were deleted after their pull request merged.
			if _, err := runGit(fctx, path, "fetch", "--quiet", "--no-tags", "--prune", "--all"); err != nil {
				// A private repo without credentials, or an offline moment, is
				// ordinary. It must not stop the rest.
				w.logger.Debug("fetch failed", "repo", path, "err", err)
				return nil
			}
			ok.Add(1)
			return nil
		})
	}
	_ = g.Wait()

	if ctx.Err() != nil {
		return
	}
	w.logger.Info("background fetch done",
		"repos", len(paths), "ok", ok.Load(), "took", time.Since(started).Round(time.Second))

	// Counts computed before the fetch are now out of date, so re-read them.
	w.inspectAll(ctx, paths)
}
