package notify

import (
	"io"
	"log/slog"
	"testing"

	"github.com/karamble/omarchy-omagihu/forge"
	"github.com/karamble/omarchy-omagihu/local"
	"github.com/karamble/omarchy-omagihu/poll"
)

type fakeRemote struct{ snap *poll.Snapshot }

func (f *fakeRemote) Snapshot() *poll.Snapshot { return f.snap }

type fakeLocal struct{ snap *local.Snapshot }

func (f *fakeLocal) Snapshot() *local.Snapshot { return f.snap }

type sent struct {
	urgency string
	title   string
	body    string
}

// harness wires a notifier to fakes and captures what it would announce.
func harness(t *testing.T, prefs Prefs) (*Notifier, *fakeRemote, *fakeLocal, *[]sent) {
	t.Helper()
	remote := &fakeRemote{snap: &poll.Snapshot{}}
	lcl := &fakeLocal{snap: &local.Snapshot{}}
	var out []sent

	n := New(remote, lcl, func() Prefs { return prefs }, nil, func() bool { return true },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	n.send = func(urgency, title, body string) error {
		out = append(out, sent{urgency, title, body})
		return nil
	}
	return n, remote, lcl, &out
}

func withReview(url string) *poll.Snapshot {
	return &poll.Snapshot{Accounts: []poll.AccountView{{
		AccountID:      "a",
		ReviewRequests: []forge.PullRequest{{Repo: "o/r", Number: 1, URL: url, Title: "a change"}},
	}}}
}

// TestFirstPassIsSilent is the guard against a burst of notifications every
// time the daemon restarts: what is already true is history, not news.
func TestFirstPassIsSilent(t *testing.T) {
	n, remote, _, out := harness(t, Defaults())
	remote.snap = withReview("https://example/1")

	n.check()
	if len(*out) != 0 {
		t.Fatalf("first pass sent %d notifications, want 0: startup state is not news", len(*out))
	}

	// The same state on the next pass is still not news.
	n.check()
	if len(*out) != 0 {
		t.Fatalf("unchanged state sent %d notifications, want 0", len(*out))
	}
}

func TestAnnouncesOnlyNewItems(t *testing.T) {
	n, remote, _, out := harness(t, Defaults())
	remote.snap = withReview("https://example/1")
	n.check() // prime

	remote.snap = &poll.Snapshot{Accounts: []poll.AccountView{{
		AccountID: "a",
		ReviewRequests: []forge.PullRequest{
			{Repo: "o/r", Number: 1, URL: "https://example/1", Title: "a change"},
			{Repo: "o/r", Number: 2, URL: "https://example/2", Title: "another change"},
		},
	}}}
	n.check()

	if len(*out) != 1 {
		t.Fatalf("sent %d notifications, want exactly 1 for the new review: %+v", len(*out), *out)
	}
	if got := (*out)[0]; got.urgency != "critical" || got.title != "Review requested" {
		t.Errorf("notification = %+v, want a critical review request", got)
	}

	// Repeating the same state must stay quiet.
	n.check()
	if len(*out) != 1 {
		t.Errorf("sent %d notifications after an unchanged pass, want still 1", len(*out))
	}
}

// TestBrokenAgainIsNews covers a PR that fails, is fixed, then fails again.
func TestBrokenAgainIsNews(t *testing.T) {
	prefs := Prefs{Broken: true}
	n, remote, _, out := harness(t, prefs)

	failing := &poll.Snapshot{Accounts: []poll.AccountView{{
		AccountID:   "a",
		AuthoredPRs: []forge.PullRequest{{Repo: "o/r", Number: 7, URL: "u", ChecksState: "FAILURE"}},
	}}}
	green := &poll.Snapshot{Accounts: []poll.AccountView{{
		AccountID:   "a",
		AuthoredPRs: []forge.PullRequest{{Repo: "o/r", Number: 7, URL: "u", ChecksState: "SUCCESS"}},
	}}}

	remote.snap = green
	n.check() // prime on a healthy state

	remote.snap = failing
	n.check()
	if len(*out) != 1 {
		t.Fatalf("sent %d, want 1 when checks first fail", len(*out))
	}

	remote.snap = green
	n.check()
	remote.snap = failing
	n.check()
	if len(*out) != 2 {
		t.Errorf("sent %d, want 2: a repeat failure after a fix is news again", len(*out))
	}
}

func TestDomainsAreIndependent(t *testing.T) {
	// Reviews muted, broken allowed.
	n, remote, _, out := harness(t, Prefs{Reviews: false, Broken: true})
	remote.snap = &poll.Snapshot{}
	n.check() // prime empty

	remote.snap = &poll.Snapshot{Accounts: []poll.AccountView{{
		AccountID:      "a",
		ReviewRequests: []forge.PullRequest{{Repo: "o/r", Number: 1, URL: "r1"}},
		AuthoredPRs:    []forge.PullRequest{{Repo: "o/r", Number: 2, URL: "p2", ChecksState: "FAILURE"}},
	}}}
	n.check()

	if len(*out) != 1 {
		t.Fatalf("sent %d, want 1: the muted review must not speak", len(*out))
	}
	if (*out)[0].title != "Your pull request needs you" {
		t.Errorf("notification = %+v, want the broken pull request", (*out)[0])
	}
}

func TestAsleepSaysNothing(t *testing.T) {
	remote := &fakeRemote{snap: &poll.Snapshot{}}
	lcl := &fakeLocal{snap: &local.Snapshot{}}
	var out []sent

	// Awake for the priming pass, then asleep.
	awake := true
	n := New(remote, lcl, Defaults, nil, func() bool { return awake },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	n.send = func(urgency, title, body string) error {
		out = append(out, sent{urgency, title, body})
		return nil
	}
	n.check()

	awake = false
	remote.snap = withReview("https://example/1")
	n.check()

	if len(out) != 0 {
		t.Errorf("sent %d notifications while asleep, want 0: the switch must silence it too", len(out))
	}
}

func TestInterruptedOperationAnnounced(t *testing.T) {
	n, _, lcl, out := harness(t, Prefs{Local: true})
	lcl.snap = &local.Snapshot{}
	n.check() // prime

	lcl.snap = &local.Snapshot{Repos: []local.Repo{
		{Path: "/p", Name: "demo", Branch: "main", Operation: local.OpRebase},
		{Path: "/q", Name: "clean", Branch: "main"},
	}}
	n.check()

	if len(*out) != 1 {
		t.Fatalf("sent %d, want 1 for the interrupted rebase: %+v", len(*out), *out)
	}
	if got := (*out)[0].title; got != "Unfinished rebase" {
		t.Errorf("title = %q, want %q", got, "Unfinished rebase")
	}
}

func TestParseDomain(t *testing.T) {
	for _, name := range []string{"reviews", "broken", "inbox", "local"} {
		if _, ok := ParseDomain(name); !ok {
			t.Errorf("ParseDomain(%q) rejected a real domain", name)
		}
	}
	if _, ok := ParseDomain("bogus"); ok {
		t.Error("ParseDomain accepted an unknown domain")
	}
}

func TestPrefsSetAndEnabled(t *testing.T) {
	p := Prefs{}
	for _, d := range []Domain{DomainReviews, DomainBroken, DomainInbox, DomainLocal} {
		if p.Enabled(d) {
			t.Errorf("zero Prefs has %s enabled", d)
		}
		p = p.Set(d, true)
		if !p.Enabled(d) {
			t.Errorf("Set(%s, true) did not take", d)
		}
	}
}
