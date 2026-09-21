package api

import (
	"time"

	"github.com/karamble/omarchy-omagihu/attention"
	"github.com/karamble/omarchy-omagihu/correlate"
	"github.com/karamble/omarchy-omagihu/forge"
	"github.com/karamble/omarchy-omagihu/local"
	"github.com/karamble/omarchy-omagihu/notify"
	"github.com/karamble/omarchy-omagihu/poll"
)

// Demo is a fabricated dashboard, for building the panel and for the screenshot
// in the README. It is built from the same structs the daemon serves, so it
// cannot drift from the real document, and it deliberately carries one of every
// state the panel can draw: each CI verdict, each review decision, each kind of
// drift, and each notification reason.
//
// Nothing here is real. The accounts, repositories and people are invented.
func Demo() any {
	now := time.Now()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	// Deliberately small: the panel is a card on a screen, and a demonstration
	// that does not fit into one is demonstrating the wrong thing.
	reviews := []forge.PullRequest{
		{
			AccountID: "demo", Repo: "octosmith/harbourmaster", Number: 412,
			Title:  "Retry the manifest fetch on a partial read",
			Author: "wrenkeeper", HeadRef: "fix/partial-manifest-read",
			URL:       "https://github.com/octosmith/harbourmaster/pull/412",
			UpdatedAt: ago(35 * time.Minute),
		},
	}

	// One pull request per verdict the panel knows how to draw.
	authored := []forge.PullRequest{
		{
			AccountID: "demo", Repo: "octosmith/harbourmaster", Number: 407,
			Title:  "Carry the request id through the worker pool",
			Author: "you", HeadRef: "feat/request-id",
			URL:         "https://github.com/octosmith/harbourmaster/pull/407",
			ChecksState: "FAILURE", ReviewDecision: "APPROVED",
			HeadSHA: "9f21c4a", UpdatedAt: ago(18 * time.Minute),
		},
		{
			AccountID: "demo", Repo: "octosmith/tidewatch", Number: 91,
			Title:  "Teach the poller to back off on 429",
			Author: "you", HeadRef: "fix/backoff-429",
			URL:         "https://github.com/octosmith/tidewatch/pull/91",
			ChecksState: "SUCCESS", ReviewDecision: "CHANGES_REQUESTED",
			UpdatedAt: ago(2 * time.Hour),
		},
		{
			AccountID: "demo", Repo: "octosmith/harbourmaster", Number: 410,
			Title:  "Document the retry budget",
			Author: "you", HeadRef: "docs/retry-budget",
			URL:         "https://github.com/octosmith/harbourmaster/pull/410",
			ChecksState: "PENDING",
			UpdatedAt:   ago(9 * time.Minute),
		},
	}

	// One fact per kind of drift, with the two severities represented.
	facts := []correlate.Fact{
		{
			Kind: correlate.KindCIRedOnHead, Severity: "urgent",
			Repo: "octosmith/harbourmaster", Path: "~/src/harbourmaster-request-id",
			Branch: "feat/request-id", Number: 407,
			Summary: "checks failed on the commit you have checked out",
			URL:     "https://github.com/octosmith/harbourmaster/pull/407",
		},
		{
			Kind: correlate.KindMissingWork, Severity: "urgent",
			Repo: "octosmith/tidewatch", Path: "~/src/tidewatch",
			Branch: "fix/backoff-429", Number: 91,
			Summary: "pull request #91 is 3 commits behind your checkout",
			URL:     "https://github.com/octosmith/tidewatch/pull/91",
		},
		{
			Kind: correlate.KindStaleBranch, Severity: "notice",
			Repo: "octosmith/lanternfish", Path: "~/src/lanternfish",
			Branch:  "spike/storage-iface",
			Summary: "merged upstream in #19; the branch is finished and safe to delete",
		},
		{
			Kind: correlate.KindForkBehind, Severity: "notice",
			Repo: "octosmith/pilotlight", Path: "~/go/src/pilotlight",
			Branch:  "main",
			Summary: "4 commits behind upstream",
		},
	}

	// The reason is the badge: one that means a person is blocked, one that
	// means a machine is talking.
	inbox := []forge.Notification{
		{
			AccountID: "demo", ID: "n1", Repo: "octosmith/harbourmaster",
			Type: "PullRequest", Title: "Retry the manifest fetch on a partial read",
			Reason: "review_requested", Unread: true, UpdatedAt: ago(35 * time.Minute),
			WebURL: "https://github.com/octosmith/harbourmaster/pull/412",
		},
		{
			AccountID: "demo", ID: "n4", Repo: "octosmith/harbourmaster",
			Type: "PullRequest", Title: "Carry the request id through the worker pool",
			Reason: "ci_activity", Unread: true, UpdatedAt: ago(18 * time.Minute),
			WebURL: "https://github.com/octosmith/harbourmaster/pull/407",
		},
	}

	// An issue you opened, carrying the labels that say where it stands. This is
	// the state a notification cannot show.
	opened := []forge.Issue{
		{
			AccountID: "demo", Repo: "octosmith/harbourmaster", Number: 44,
			Title: "Publish the manifest schema alongside the release",
			URL:   "https://github.com/octosmith/harbourmaster/issues/44",
			Labels: []forge.Label{
				{Name: "accepted", Color: "0e8a16"},
				{Name: "needs-docs", Color: "d4c5f9"},
			},
			UpdatedAt: ago(3 * time.Hour),
		},
	}

	issues := []forge.Issue{
		{
			AccountID: "demo", Repo: "octosmith/lanternfish", Number: 31,
			Title:     "Decide the on-disk format before the spike lands",
			URL:       "https://github.com/octosmith/lanternfish/issues/31",
			UpdatedAt: ago(4 * time.Hour),
		},
		{
			AccountID: "demo", Repo: "octosmith/tidewatch", Number: 77,
			Title:     "Poller wedges after a DNS failure",
			URL:       "https://github.com/octosmith/tidewatch/issues/77",
			UpdatedAt: ago(26 * time.Hour),
		},
		// Reported by somebody else on a repository you own. Nobody could
		// assign it to you, so it arrives marked incoming.
		{
			AccountID: "demo", Repo: "octosmith/harbourmaster", Number: 51,
			Title:  "First install fails when the config directory is missing",
			URL:    "https://github.com/octosmith/harbourmaster/issues/51",
			Author: "driftwood", Incoming: true,
			Labels:    []forge.Label{{Name: "bug", Color: "d73a4a"}},
			UpdatedAt: ago(50 * time.Minute),
		},
	}

	// harbourmaster is one repository with three registered worktrees: the
	// main checkout, a feature branch holding unpushed work, and a stale
	// registration whose directory is gone. The rest are plain repositories,
	// each the main checkout of its own group.
	repos := []local.Repo{
		{
			Name: "harbourmaster", Path: "~/src/harbourmaster", Branch: "main",
			Upstream: "origin/main", Modified: 3, Untracked: 1,
			Group: "~/src/harbourmaster/.git", Main: true,
			ObservedAt: ago(20 * time.Second),
		},
		{
			Name: "harbourmaster-request-id", Path: "~/src/harbourmaster-request-id", Branch: "feat/request-id",
			Upstream: "origin/feat/request-id", Unpushed: 2,
			Group:      "~/src/harbourmaster/.git",
			ObservedAt: ago(20 * time.Second),
		},
		{
			Name: "harbourmaster-hotfix", Path: "~/src/harbourmaster-hotfix", Branch: "hotfix/tls",
			Group:    "~/src/harbourmaster/.git",
			Prunable: "gitdir file points to non-existent location",
		},
		{
			Name: "tidewatch", Path: "~/src/tidewatch", Branch: "fix/backoff-429",
			Upstream: "origin/fix/backoff-429", Unpushed: 3,
			Group: "~/src/tidewatch/.git", Main: true,
			Operation: local.OpRebase, ObservedAt: ago(20 * time.Second),
		},
		{
			Name: "lanternfish", Path: "~/src/lanternfish", Branch: "spike/storage-iface",
			Upstream: "origin/spike/storage-iface",
			Group:    "~/src/lanternfish/.git", Main: true,
			ObservedAt: ago(20 * time.Second),
		},
		{
			Name: "pilotlight", Path: "~/go/src/pilotlight", Branch: "main",
			Upstream: "origin/main", UpstreamBehind: 4,
			Group: "~/go/src/pilotlight/.git", Main: true,
			ObservedAt: ago(20 * time.Second),
		},
		// Somebody else's repository, cloned to read. Trailing it is its
		// normal state, so it sorts last and says what it is.
		{
			Name: "driftnet", Path: "~/src/driftnet", Branch: "main",
			Upstream: "origin/main", Behind: 243,
			Remotes: map[string]string{"origin": "https://github.com/driftwood/driftnet"},
			Group:   "~/src/driftnet/.git", Main: true, Followed: true,
			ObservedAt: ago(20 * time.Second),
		},
	}

	att := attention.Resolve(attention.Input{
		Reviews:  reviews,
		Authored: authored,
		Unread:   inbox,
		Repos:    repos,
		Facts:    facts,
	})

	return dashboardResponse{
		Health: healthResponse{
			Status: "ok", Version: "demo", Uptime: "3h12m",
			Accounts: 2, Enabled: 2, PolledAt: ago(41 * time.Second),
			InboxRateLeft: 4837, WorkRateLeft: 4991, Errors: []string{},
			Monitoring: true, IntervalMin: 5,
			MCPEnabled: true, FetchEnabled: true, FetchMin: 30,
			Notify: notify.Prefs{Reviews: true, Broken: true, Local: true, Reconcile: true},
			Repos:  len(repos), ReposRisk: local.RepositoriesAtRisk(repos), ScannedAt: ago(20 * time.Second),
		},
		Attention: att,
		Inbox:     inbox,
		Facts:     facts,
		Alerts:    []alertRow{},
		Work: workResponse{
			AuthoredPRs:    authored,
			ReviewRequests: reviews,
			AssignedIssues: issues,
			AuthoredIssues: opened,
			MergedPRs:      []forge.PullRequest{},
		},
		Repos:     repos,
		LocalOnly: []string{},
		Accounts: []poll.AccountView{
			{
				AccountID: "demo", Login: "you",
				InboxRate: forge.Rate{Limit: 5000, Remaining: 4837},
				WorkRate:  forge.Rate{Limit: 5000, Remaining: 4991},
				InboxAt:   ago(41 * time.Second), WorkAt: ago(2 * time.Minute),
			},
			{
				AccountID: "demo-work", Login: "you-at-work",
				InboxRate: forge.Rate{Limit: 5000, Remaining: 4991},
				WorkRate:  forge.Rate{Limit: 5000, Remaining: 4988},
				InboxAt:   ago(58 * time.Second), WorkAt: ago(3 * time.Minute),
			},
		},
	}
}
