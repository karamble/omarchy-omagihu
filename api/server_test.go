package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/karamble/omarchy-omagihu/accounts"
	"github.com/karamble/omarchy-omagihu/correlate"
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

// localOnlyServer serves one remote-less, committed checkout at /s that also
// carries synthetic detached work, so the mark can be shown to silence the
// missing remote and nothing else.
func localOnlyServer(t *testing.T) (*Server, *accounts.Store) {
	t.Helper()
	store := &accounts.Store{APIToken: "tok"}
	store.SetPath(filepath.Join(t.TempDir(), "accounts.json"))
	repos := &local.Snapshot{Repos: []local.Repo{{
		Name: "scratch", Path: "/s", Branch: "(detached)", Detached: true, Unpushed: 1,
		Last: local.Commit{SHA: "abc"},
	}}}
	s := NewServer(store, fixedPoller{&poll.Snapshot{}}, fixedWatcher{repos}, slog.New(slog.DiscardHandler), "test")
	return s, store
}

func factKinds(facts []correlate.Fact) []string {
	out := []string{}
	for _, f := range facts {
		out = append(out, string(f.Kind))
	}
	slices.Sort(out)
	return out
}

func TestLocalOnlySilencesOnlyTheMissingRemote(t *testing.T) {
	s, store := localOnlyServer(t)

	if got := factKinds(s.facts()); !slices.Equal(got, []string{"detached-work", "no-remote"}) {
		t.Fatalf("unmarked kinds = %v, want both facts", got)
	}
	store.SetLocalOnly("/s", true)
	if got := factKinds(s.facts()); !slices.Equal(got, []string{"detached-work"}) {
		t.Fatalf("marked kinds = %v, want the missing remote silenced and nothing else", got)
	}
	store.SetLocalOnly("/s", false)
	if got := factKinds(s.facts()); !slices.Equal(got, []string{"detached-work", "no-remote"}) {
		t.Fatalf("unmarked again kinds = %v, want both facts back", got)
	}
}

// TestLocalOnlyReachesEveryFactReader is the #10 lesson applied: a mark set
// through the endpoint must change the dashboard, the alert sample, the facts
// route and the exported Facts alike, or a trigger and the panel disagree.
func TestLocalOnlyReachesEveryFactReader(t *testing.T) {
	s, store := localOnlyServer(t)
	h := s.Handler()

	call := func(method, path, body string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	kindsOf := func(doc map[string]any) []string {
		out := []string{}
		for _, f := range doc["facts"].([]any) {
			out = append(out, f.(map[string]any)["kind"].(string))
		}
		slices.Sort(out)
		return out
	}

	call(http.MethodPost, "/api/local-only", `{"path":"/s","on":true}`)
	if !store.IsLocalOnly("/s") {
		t.Fatal("the endpoint did not mark the checkout")
	}
	dash := call(http.MethodGet, "/api/dashboard", "")
	if got := kindsOf(dash); !slices.Equal(got, []string{"detached-work"}) {
		t.Errorf("dashboard kinds = %v, want no-remote silenced", got)
	}
	if got := dash["localOnly"]; !slices.Equal(anyStrings(got), []string{"/s"}) {
		t.Errorf("dashboard localOnly = %v, want [/s]", got)
	}
	if got := kindsOf(call(http.MethodGet, "/api/facts", "")); !slices.Equal(got, []string{"detached-work"}) {
		t.Errorf("facts route kinds = %v, want no-remote silenced", got)
	}
	if got := factKinds(s.AlertSample().Facts); !slices.Equal(got, []string{"detached-work"}) {
		t.Errorf("alert sample kinds = %v, want no-remote silenced", got)
	}
	if got := factKinds(s.Facts()); !slices.Equal(got, []string{"detached-work"}) {
		t.Errorf("exported Facts kinds = %v, want no-remote silenced", got)
	}

	// Lifting the mark goes through the same endpoint and reaches the same
	// readers.
	call(http.MethodPost, "/api/local-only", `{"path":"/s","on":false}`)
	if got := kindsOf(call(http.MethodGet, "/api/dashboard", "")); !slices.Equal(got, []string{"detached-work", "no-remote"}) {
		t.Errorf("dashboard kinds after unmarking = %v, want both facts back", got)
	}
	if got := factKinds(s.AlertSample().Facts); !slices.Equal(got, []string{"detached-work", "no-remote"}) {
		t.Errorf("alert sample kinds after unmarking = %v, want both facts back", got)
	}
	if got := call(http.MethodGet, "/api/dashboard", "")["localOnly"]; !slices.Equal(anyStrings(got), []string{}) {
		t.Errorf("dashboard localOnly after unmarking = %v, want an empty list, not null", got)
	}
}

func anyStrings(v any) []string {
	out := []string{}
	if list, ok := v.([]any); ok {
		for _, s := range list {
			out = append(out, s.(string))
		}
	}
	return out
}

// TestAnnotateMarksFollowedRepositories pins ownership as the API derives
// it: a repository whose origin names another account is followed, one whose
// origin is you is yours (a fork included, whatever its upstream), one with no
// origin is yours, a group answers as one, followed sorts below your own, and
// followed never means less at risk.
func TestAnnotateMarksFollowedRepositories(t *testing.T) {
	remote := &poll.Snapshot{Accounts: []poll.AccountView{{AccountID: "a", Login: "You"}}}
	s := NewServer(&accounts.Store{}, fixedPoller{remote}, fixedWatcher{&local.Snapshot{}},
		slog.New(slog.DiscardHandler), "test")

	// In the collector's order: at-risk groups first, then names.
	repos := &local.Snapshot{Repos: []local.Repo{
		{Path: "/lib", Name: "lib", Group: "/lib/.git", Main: true, Unpushed: 1,
			Remotes: map[string]string{"origin": "https://github.com/other/lib"}},
		{Path: "/thing", Name: "thing", Group: "/thing/.git", Main: true, Unpushed: 2,
			Remotes: map[string]string{"origin": "git@github.com:you/thing.git"}},
		{Path: "/fork", Name: "fork", Group: "/fork/.git", Main: true,
			Remotes: map[string]string{"origin": "git@github.com:you/fork.git", "upstream": "https://github.com/other/fork"}},
		{Path: "/scratch", Name: "scratch", Group: "/scratch/.git", Main: true},
		{Path: "/tool", Name: "tool", Group: "/tool/.git", Main: true, Behind: 243,
			Remotes: map[string]string{"origin": "https://github.com/someone/tool"}},
		{Path: "/tool-wt", Name: "tool-wt", Group: "/tool/.git",
			Remotes: map[string]string{"origin": "https://github.com/someone/tool"}},
		{Path: "/tool-gone", Name: "tool-gone", Group: "/tool/.git", Prunable: "gitdir file points to non-existent location"},
	}}

	got := s.annotate(repos, remote)
	var order []string
	followed := make(map[string]bool)
	for _, r := range got.Repos {
		order = append(order, r.Name)
		followed[r.Name] = r.Followed
	}
	want := "thing lib fork scratch tool tool-wt tool-gone"
	if strings.Join(order, " ") != want {
		t.Errorf("order = %v, want %s: yours first within each tier, at-risk followed above clean yours", order, want)
	}
	for name, f := range map[string]bool{"lib": true, "thing": false, "fork": false, "scratch": false,
		"tool": true, "tool-wt": true, "tool-gone": true} {
		if followed[name] != f {
			t.Errorf("%s followed = %v, want %v", name, followed[name], f)
		}
	}
	if got := local.RepositoriesAtRisk(got.Repos); got != 2 {
		t.Errorf("RepositoriesAtRisk = %d, want 2: the followed clone with unpushed work still counts", got)
	}
	if repos.Repos[0].Followed {
		t.Error("annotate wrote into the collector's snapshot instead of a copy")
	}

	// With no login known, nothing can be called somebody else's.
	for _, r := range s.annotate(repos, &poll.Snapshot{}).Repos {
		if r.Followed {
			t.Errorf("%s followed with no account login to compare against", r.Name)
		}
	}
}

// TestAnnotateCountsOrganisationsAsYours pins the organisation rule: an origin
// under an organisation an enabled account belongs to is owned, under one it
// does not belong to is followed, and the comparison ignores case.
func TestAnnotateCountsOrganisationsAsYours(t *testing.T) {
	remote := &poll.Snapshot{Accounts: []poll.AccountView{
		{AccountID: "a", Login: "you", Organizations: []string{"Decred"}},
	}}
	s := NewServer(&accounts.Store{}, fixedPoller{remote}, fixedWatcher{&local.Snapshot{}},
		slog.New(slog.DiscardHandler), "test")
	repos := &local.Snapshot{Repos: []local.Repo{
		{Path: "/dcrd", Name: "dcrd", Group: "/dcrd/.git", Main: true,
			Remotes: map[string]string{"origin": "git@github.com:decred/dcrd.git"}},
		{Path: "/lib", Name: "lib", Group: "/lib/.git", Main: true,
			Remotes: map[string]string{"origin": "https://github.com/other-org/lib"}},
	}}
	got := s.annotate(repos, remote)
	for _, r := range got.Repos {
		switch r.Name {
		case "dcrd":
			if r.Followed {
				t.Error("dcrd followed, want owned through the organisation")
			}
		case "lib":
			if !r.Followed {
				t.Error("lib owned, want followed: not one of your organisations")
			}
		}
	}
	if got.Repos[0].Name != "dcrd" {
		t.Errorf("order = %s first, want the organisation's repository above the followed one", got.Repos[0].Name)
	}
}
