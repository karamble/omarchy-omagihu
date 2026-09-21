package local

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func newFetchWatcher(t *testing.T) *Watcher {
	t.Helper()
	return NewWatcher(Config{}, slog.New(slog.DiscardHandler), time.Minute, time.Hour)
}

// nudged reports whether a wake-up is waiting on fetchNow, consuming it.
func nudged(w *Watcher) bool {
	select {
	case <-w.fetchNow:
		return true
	default:
		return false
	}
}

func TestSetFetchShorterCadenceAppliesAtOnce(t *testing.T) {
	w := newFetchWatcher(t)
	w.SetFetch(true, time.Hour)
	if nudged(w) {
		t.Fatal("first configuration must not wake the loop")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The loop is asleep on the hour it was configured with.
	woke := make(chan bool, 1)
	go func() { woke <- w.waitFetch(ctx, time.Hour) }()

	w.SetFetch(true, 5*time.Minute)

	select {
	case ok := <-woke:
		if !ok {
			t.Fatal("waitFetch reported the context ended")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shortening the cadence did not wake the fetch loop")
	}
	if got := time.Duration(w.fetchEvery.Load()); got != 5*time.Minute {
		t.Fatalf("cadence after change = %v, want 5m", got)
	}
}

func TestSetFetchSwitchAloneLeavesScheduleAlone(t *testing.T) {
	w := newFetchWatcher(t)
	w.SetFetch(true, time.Hour)

	w.SetFetch(false, time.Hour)
	if nudged(w) {
		t.Fatal("turning fetching off must not wake the loop")
	}
	w.SetFetch(false, 5*time.Minute)
	if nudged(w) {
		t.Fatal("a cadence change while off must not wake the loop")
	}
	w.SetFetch(true, 5*time.Minute)
	if nudged(w) {
		t.Fatal("turning fetching on without changing the cadence must not wake the loop")
	}
}
