package api

import (
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
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

// fixedPoller serves one snapshot on every call.
type fixedPoller struct{ snap *poll.Snapshot }

func (p fixedPoller) Snapshot() *poll.Snapshot { return p.snap }

type fixedWatcher struct{ snap *local.Snapshot }

func (w fixedWatcher) Snapshot() *local.Snapshot { return w.snap }

func TestHealthCarriesEveryAccountError(t *testing.T) {
	remote := &poll.Snapshot{Accounts: []poll.AccountView{
		{AccountID: "id-a", Login: "a", InboxError: "notifications: 502",
			InboxRate: forge.Rate{Remaining: 10}, WorkRate: forge.Rate{Remaining: 20}},
		{AccountID: "id-b", WorkError: "workload: 401",
			InboxRate: forge.Rate{Remaining: 5}, WorkRate: forge.Rate{Remaining: 7}},
	}}
	s := NewServer(&accounts.Store{}, fixedPoller{remote}, fixedWatcher{&local.Snapshot{}},
		slog.New(slog.DiscardHandler), "test")

	h := s.health(remote, &local.Snapshot{})
	want := []string{"a inbox: notifications: 502", "id-b work: workload: 401"}
	if !slices.Equal(h.Errors, want) {
		t.Fatalf("Errors = %q, want %q: every account and plane, login first, id as fallback", h.Errors, want)
	}
	if h.InboxRateLeft != 15 || h.WorkRateLeft != 27 {
		t.Fatalf("rate left = inbox %d work %d, want 15 and 27: buckets summed separately", h.InboxRateLeft, h.WorkRateLeft)
	}

	sample := s.AlertSample()
	if sample.Health.LastError != "a inbox: notifications: 502; id-b work: workload: 401" {
		t.Fatalf("alert LastError = %q, want the joined list", sample.Health.LastError)
	}
	if got, _ := sample.Number("health.rateLeft"); got != 15 {
		t.Fatalf("health.rateLeft = %v, want 15: the tighter bucket", got)
	}
}

func TestHealthErrorsIsAnEmptyListWhenHealthy(t *testing.T) {
	remote := &poll.Snapshot{Accounts: []poll.AccountView{{AccountID: "a", Login: "a"}}}
	s := NewServer(&accounts.Store{}, fixedPoller{remote}, fixedWatcher{&local.Snapshot{}},
		slog.New(slog.DiscardHandler), "test")

	raw, err := json.Marshal(s.health(remote, &local.Snapshot{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"errors":[]`, `"inboxRateLeft":0`, `"workRateLeft":0`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("health JSON lacks %s: %s", key, raw)
		}
	}
	for _, gone := range []string{`"lastError"`, `"rateLeft"`} {
		if strings.Contains(string(raw), gone) {
			t.Errorf("health JSON still carries %s: %s", gone, raw)
		}
	}
}

// TestHealthCountsCheckoutsAndRepositories pins the two local numbers in the
// health document: repos counts the checkouts being watched, so a prunable
// worktree registration is listed but not counted, and reposAtRisk counts
// repositories, so a group with two at-risk checkouts is one and a checkout
// that is merely dirty is none.
func TestHealthCountsCheckoutsAndRepositories(t *testing.T) {
	repos := &local.Snapshot{Repos: []local.Repo{
		{Path: "/thing", Name: "thing", Group: "/thing/.git", Main: true, Modified: 1},
		{Path: "/wt-a", Name: "wt-a", Group: "/thing/.git", Unpushed: 1},
		{Path: "/wt-b", Name: "wt-b", Group: "/thing/.git", Unpushed: 1},
		{Path: "/wt-gone", Name: "wt-gone", Group: "/thing/.git",
			Prunable: "gitdir file points to non-existent location"},
	}}
	remote := &poll.Snapshot{}
	s := NewServer(&accounts.Store{}, fixedPoller{remote}, fixedWatcher{repos},
		slog.New(slog.DiscardHandler), "test")

	h := s.health(remote, repos)
	if h.Repos != 3 {
		t.Errorf("Repos = %d, want 3: a prunable registration is not a checkout", h.Repos)
	}
	if h.ReposRisk != 1 {
		t.Errorf("ReposRisk = %d, want 1: two at-risk checkouts of one repository count once", h.ReposRisk)
	}

	// A repository that is only dirty adds nothing at the health level either.
	dirty := &local.Snapshot{Repos: []local.Repo{
		{Path: "/other", Name: "other", Group: "/other/.git", Main: true, Modified: 2, Untracked: 1},
	}}
	if h := s.health(remote, dirty); h.Repos != 1 || h.ReposRisk != 0 {
		t.Errorf("dirty-only: Repos = %d ReposRisk = %d, want 1 and 0", h.Repos, h.ReposRisk)
	}
}
