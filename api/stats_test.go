package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/karamble/omarchy-omagihu/accounts"
	"github.com/karamble/omarchy-omagihu/forge"
	"github.com/karamble/omarchy-omagihu/local"
	"github.com/karamble/omarchy-omagihu/poll"
)

// fakeStats records who asked for what and answers with canned figures.
type fakeStats struct {
	asked  []string // "login owner/name"
	stats  forge.RepoStats
	err    error
	byAcct string
}

func (f *fakeStats) RepoStats(_ context.Context, owner, name string) (forge.RepoStats, error) {
	f.asked = append(f.asked, f.byAcct+" "+owner+"/"+name)
	if f.err != nil {
		return forge.RepoStats{}, f.err
	}
	s := f.stats
	s.Owner, s.Name = owner, name
	return s, nil
}

// statsServer wires a server whose forge client is the fake and whose clock
// is a variable.
func statsServer(t *testing.T, store *accounts.Store, remote *poll.Snapshot, fake *fakeStats) (*Server, *time.Time) {
	t.Helper()
	s := NewServer(store, fixedPoller{remote}, fixedWatcher{&local.Snapshot{}},
		slog.New(slog.DiscardHandler), "test")
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	s.clock = func() time.Time { return now }
	s.statsClient = func(a accounts.Account) statsFetcher {
		fake.byAcct = a.Login
		return fake
	}
	return s, &now
}

func askStats(t *testing.T, s *Server, query string) (statsResponse, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleRepoStats(rec, httptest.NewRequest(http.MethodGet, "/api/repo-stats?"+query, nil))
	var resp statsResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
	}
	return resp, rec.Code
}

func oneAccount() *accounts.Store {
	return &accounts.Store{Accounts: []accounts.Account{{ID: "a", Login: "you", Enabled: true}}}
}

// TestRepoStatsCacheWindow pins the hour: the first ask fetches, a second
// inside the window is served from memory with the same expiry, and one past
// the window fetches again.
func TestRepoStatsCacheWindow(t *testing.T) {
	fake := &fakeStats{stats: forge.RepoStats{Stars: 42, Cost: 1}}
	s, now := statsServer(t, oneAccount(), &poll.Snapshot{}, fake)

	first, code := askStats(t, s, "repo=you/thing")
	if code != http.StatusOK || first.Cached || first.Stats == nil || first.Stats.Stars != 42 {
		t.Fatalf("first = %+v (%d), want fresh figures", first, code)
	}
	if !first.ExpiresAt.Equal(now.Add(statsFresh)) || !first.FetchedAt.Equal(*now) {
		t.Errorf("FetchedAt %v ExpiresAt %v, want now and now plus an hour", first.FetchedAt, first.ExpiresAt)
	}

	*now = now.Add(30 * time.Minute)
	second, _ := askStats(t, s, "repo=you/thing")
	if !second.Cached || second.Stale || !second.ExpiresAt.Equal(first.ExpiresAt) {
		t.Errorf("second = %+v, want cached with the original expiry", second)
	}
	if len(fake.asked) != 1 {
		t.Errorf("fetched %d times inside the window, want 1", len(fake.asked))
	}

	*now = now.Add(31 * time.Minute)
	third, _ := askStats(t, s, "repo=you/thing")
	if third.Cached || len(fake.asked) != 2 || !third.ExpiresAt.Equal(now.Add(statsFresh)) {
		t.Errorf("third = %+v after %d fetches, want a refetch with a new expiry", third, len(fake.asked))
	}
}

// TestRepoStatsPausedServesStale pins the pause: nothing is fetched while
// monitoring is off, what is cached is served and flagged, and nothing cached
// says why there is nothing.
func TestRepoStatsPausedServesStale(t *testing.T) {
	fake := &fakeStats{stats: forge.RepoStats{Stars: 1}}
	store := oneAccount()
	s, now := statsServer(t, store, &poll.Snapshot{}, fake)
	askStats(t, s, "repo=you/thing")

	off := false
	store.Monitoring = &off
	*now = now.Add(2 * time.Hour)
	got, _ := askStats(t, s, "repo=you/thing")
	if !got.Paused || !got.Stale || got.Stats == nil || len(fake.asked) != 1 {
		t.Errorf("paused with a stale cache = %+v after %d fetches, want stale figures served, nothing fetched", got, len(fake.asked))
	}
	none, _ := askStats(t, s, "repo=you/other")
	if !none.Paused || none.Stats != nil || none.Error == "" || len(fake.asked) != 1 {
		t.Errorf("paused with nothing cached = %+v, want no figures and a reason", none)
	}
}

// TestRepoStatsFailedRefreshKeepsFigures pins that a refresh that fails
// leaves the previous figures in place, marked stale, with the error beside
// them, and that a missing repository is an error on the row, not a failure
// of the endpoint.
func TestRepoStatsFailedRefreshKeepsFigures(t *testing.T) {
	fake := &fakeStats{stats: forge.RepoStats{Stars: 5}}
	s, now := statsServer(t, oneAccount(), &poll.Snapshot{}, fake)
	askStats(t, s, "repo=you/thing")

	*now = now.Add(2 * time.Hour)
	fake.err = &forge.APIError{Op: "repository", Status: 200, Message: "gone", Class: forge.ErrBlocked}
	got, code := askStats(t, s, "repo=you/thing")
	if code != http.StatusOK || !got.Stale || got.Stats == nil || got.Stats.Stars != 5 || got.Error == "" {
		t.Errorf("failed refresh = %+v (%d), want the old figures, stale, with the error", got, code)
	}
	if !errors.Is(fake.err, forge.ErrBlocked) {
		t.Fatal("fixture is not blocked")
	}
}

// TestRepoStatsPicksTheOwnersAccount pins account selection: the account
// whose login or organisations own the repository asks, else the first
// enabled account on the host.
func TestRepoStatsPicksTheOwnersAccount(t *testing.T) {
	store := &accounts.Store{Accounts: []accounts.Account{
		{ID: "a", Login: "you", Enabled: true},
		{ID: "b", Login: "work", Enabled: true},
		{ID: "c", Login: "off", Enabled: false},
	}}
	remote := &poll.Snapshot{Accounts: []poll.AccountView{
		{AccountID: "a", Login: "you"},
		{AccountID: "b", Login: "work", Organizations: []string{"Decred"}},
	}}
	fake := &fakeStats{}
	s, _ := statsServer(t, store, remote, fake)
	for _, tc := range []struct{ repo, want string }{
		{"you/thing", "you you/thing"},
		{"decred/dcrd", "work decred/dcrd"},
		{"Work/tool", "work Work/tool"},
		{"stranger/lib", "you stranger/lib"},
		{"off/mine", "you off/mine"},
	} {
		fake.asked = nil
		askStats(t, s, "repo="+tc.repo)
		if len(fake.asked) != 1 || fake.asked[0] != tc.want {
			t.Errorf("%s asked as %v, want %q", tc.repo, fake.asked, tc.want)
		}
	}
}

// TestRepoStatsRefusesWhatNoAccountCanAsk pins the host filter: a remote that
// is not a forge url, or one on a host no enabled account speaks to, is
// refused before any request is made.
func TestRepoStatsRefusesWhatNoAccountCanAsk(t *testing.T) {
	fake := &fakeStats{}
	s, _ := statsServer(t, oneAccount(), &poll.Snapshot{}, fake)
	for _, q := range []string{
		"origin=/tmp/lab/origin.git",
		"origin=../origin.git",
		"origin=https://gitlab.com/someone/thing.git",
		"repo=you/thing&host=git.example.org",
		"repo=nonsense",
		"repo=a/b/c",
		"",
	} {
		if _, code := askStats(t, s, q); code != http.StatusBadRequest {
			t.Errorf("%q answered %d, want 400", q, code)
		}
	}
	if len(fake.asked) != 0 {
		t.Errorf("a refused target still reached the forge: %v", fake.asked)
	}

	// A github origin in either spelling resolves to one cache entry.
	askStats(t, s, "origin=git@github.com:you/thing.git")
	got, _ := askStats(t, s, "origin=https://github.com/you/thing")
	if !got.Cached || got.Repo != "you/thing" || got.Host != "github.com" || len(fake.asked) != 1 {
		t.Errorf("two spellings of one origin = %+v after %d fetches, want one entry", got, len(fake.asked))
	}
}
