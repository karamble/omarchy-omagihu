package api

import (
	"log/slog"
	"testing"
	"time"

	"github.com/karamble/omarchy-omagihu/accounts"
	"github.com/karamble/omarchy-omagihu/forge"
	"github.com/karamble/omarchy-omagihu/local"
	"github.com/karamble/omarchy-omagihu/poll"
)

// shiftingPoller hands out a different snapshot on every call, so anything
// that takes more than one is visibly inconsistent.
type shiftingPoller struct {
	calls int
	snaps []*poll.Snapshot
}

func (p *shiftingPoller) Snapshot() *poll.Snapshot {
	p.calls++
	return p.snaps[min(p.calls, len(p.snaps))-1]
}

type shiftingWatcher struct {
	calls int
	snaps []*local.Snapshot
}

func (w *shiftingWatcher) Snapshot() *local.Snapshot {
	w.calls++
	return w.snaps[min(w.calls, len(w.snaps))-1]
}

func TestAlertSampleTakesOneSnapshotPair(t *testing.T) {
	first := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	later := first.Add(time.Minute)

	// The first pair yields one fact: a checkout of o/r on the branch of an
	// open pull request, with a commit that has not been pushed.
	poller := &shiftingPoller{snaps: []*poll.Snapshot{
		{TakenAt: first, Accounts: []poll.AccountView{{
			AccountID: "acct",
			Notifications: []forge.Notification{{
				ID: "n1", SubjectURL: "https://api.github.com/repos/o/r/pulls/1", Unread: true,
			}},
			AuthoredPRs: []forge.PullRequest{{
				Repo: "o/r", Number: 1, URL: "https://github.com/o/r/pull/1", HeadRef: "main",
			}},
		}}},
		// Everything afterwards is empty, so a field built from a later
		// snapshot comes out empty while its neighbours do not.
		{TakenAt: later},
	}}
	watcher := &shiftingWatcher{snaps: []*local.Snapshot{
		{TakenAt: first, Repos: []local.Repo{{
			Path: "/checkout/r", Name: "r", Branch: "main", Unpushed: 1,
			Remotes: map[string]string{"origin": "git@github.com:o/r.git"},
		}}},
		{TakenAt: later},
	}}

	s := NewServer(&accounts.Store{}, poller, watcher, slog.New(slog.DiscardHandler), "test")
	sample := s.AlertSample()

	if poller.calls != 1 || watcher.calls != 1 {
		t.Fatalf("AlertSample took %d remote and %d local snapshots, want 1 and 1",
			poller.calls, watcher.calls)
	}
	if len(sample.Repos) != 1 || len(sample.Facts) != 1 {
		t.Fatalf("got %d repos and %d facts, want 1 and 1", len(sample.Repos), len(sample.Facts))
	}
	if sample.Facts[0].Path != sample.Repos[0].Path {
		t.Fatalf("fact path %q does not match repo path %q", sample.Facts[0].Path, sample.Repos[0].Path)
	}
	if sample.Attention.FactsTotal != len(sample.Facts) {
		t.Fatalf("attention counted %d facts, sample carries %d", sample.Attention.FactsTotal, len(sample.Facts))
	}
	if len(sample.Inbox) != 1 || sample.Attention.Unread != len(sample.Inbox) {
		t.Fatalf("inbox has %d items, attention counted %d", len(sample.Inbox), sample.Attention.Unread)
	}
	if !sample.TakenAt.Equal(first) {
		t.Fatalf("TakenAt = %v, want %v", sample.TakenAt, first)
	}
}
