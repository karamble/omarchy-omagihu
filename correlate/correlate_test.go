package correlate

import (
	"testing"

	"github.com/karamble/omarchy-omagihu/forge"
	"github.com/karamble/omarchy-omagihu/local"
	"github.com/karamble/omarchy-omagihu/poll"
)

func TestNormalizeRemote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"scp style ssh", "git@github.com:karamble/omarchy-omagihu.git", "karamble/omarchy-omagihu"},
		{"scp style without suffix", "git@github.com:karamble/demarchy", "karamble/demarchy"},
		{"https with suffix", "https://github.com/karamble/dcrpulse.git", "karamble/dcrpulse"},
		{"https without suffix", "https://github.com/Kiryuuki/oma-netscan", "Kiryuuki/oma-netscan"},
		{"ssh url", "ssh://git@github.com/companyzero/bisonrelay.git", "companyzero/bisonrelay"},
		{"trailing slash", "https://github.com/o/r/", "o/r"},
		{"enterprise host", "git@ghe.example.org:team/thing.git", "team/thing"},
		{"local path is not a forge", "/home/user/mirror.git", ""},
		{"empty", "", ""},
		{"nonsense", "not a url", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeRemote(tt.in); got != tt.want {
				t.Errorf("NormalizeRemote(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// snap builds a one-account remote snapshot.
func snap(open, merged []forge.PullRequest) *poll.Snapshot {
	return &poll.Snapshot{Accounts: []poll.AccountView{{
		AccountID:   "a",
		AuthoredPRs: open,
		MergedPRs:   merged,
	}}}
}

func repo(r local.Repo) *local.Snapshot {
	if r.Remotes == nil {
		r.Remotes = map[string]string{"origin": "git@github.com:o/r.git"}
	}
	if r.Name == "" {
		r.Name = "r"
	}
	if r.Path == "" {
		r.Path = "/checkout/r"
	}
	return &local.Snapshot{Repos: []local.Repo{r}}
}

func kinds(facts []Fact) []Kind {
	out := make([]Kind, 0, len(facts))
	for _, f := range facts {
		out = append(out, f.Kind)
	}
	return out
}

func hasKind(facts []Fact, k Kind) bool {
	for _, f := range facts {
		if f.Kind == k {
			return true
		}
	}
	return false
}

func TestMissingWork(t *testing.T) {
	pr := forge.PullRequest{Repo: "o/r", Number: 7, URL: "u", HeadRef: "topic", HeadSHA: "aaa"}

	facts := Correlate(snap([]forge.PullRequest{pr}, nil),
		repo(local.Repo{Branch: "topic", Unpushed: 3, Last: local.Commit{SHA: "bbb"}}))

	if !hasKind(facts, KindMissingWork) {
		t.Fatalf("kinds = %v, want missing-work", kinds(facts))
	}
	for _, f := range facts {
		if f.Kind != KindMissingWork {
			continue
		}
		if !f.Urgent() {
			t.Error("missing-work should be urgent: the PR is not what you think it is")
		}
		if f.Number != 7 || f.URL != "u" || f.Branch != "topic" {
			t.Errorf("fact = %+v, want it to carry the PR identity", f)
		}
	}
}

// TestMissingWorkNeedsTheSameBranch guards the join: a PR on another branch must
// not claim the commits sitting here.
func TestMissingWorkNeedsTheSameBranch(t *testing.T) {
	pr := forge.PullRequest{Repo: "o/r", Number: 7, URL: "u", HeadRef: "other"}

	facts := Correlate(snap([]forge.PullRequest{pr}, nil),
		repo(local.Repo{Branch: "topic", Unpushed: 3}))

	if hasKind(facts, KindMissingWork) {
		t.Errorf("kinds = %v, want no missing-work for a different branch", kinds(facts))
	}
}

// TestMissingWorkNeedsTheSameRepo guards the other half of the join.
func TestMissingWorkNeedsTheSameRepo(t *testing.T) {
	pr := forge.PullRequest{Repo: "other/repo", Number: 7, HeadRef: "topic"}

	facts := Correlate(snap([]forge.PullRequest{pr}, nil),
		repo(local.Repo{Branch: "topic", Unpushed: 3}))

	if len(facts) != 0 {
		t.Errorf("facts = %+v, want none: the PR belongs to another repository", facts)
	}
}

func TestCIRedOnlyWhenTheCommitMatches(t *testing.T) {
	pr := forge.PullRequest{
		Repo: "o/r", Number: 9, URL: "u", HeadRef: "topic",
		HeadSHA: "deadbeef", ChecksState: "FAILURE",
	}

	same := Correlate(snap([]forge.PullRequest{pr}, nil),
		repo(local.Repo{Branch: "topic", Last: local.Commit{SHA: "deadbeef"}}))
	if !hasKind(same, KindCIRedOnHead) {
		t.Errorf("kinds = %v, want ci-red-on-head when HEAD is the PR head", kinds(same))
	}

	// A different local commit means the red run was about something else.
	other := Correlate(snap([]forge.PullRequest{pr}, nil),
		repo(local.Repo{Branch: "topic", Last: local.Commit{SHA: "cafe"}}))
	if hasKind(other, KindCIRedOnHead) {
		t.Errorf("kinds = %v, want no ci-red-on-head for a different commit", kinds(other))
	}

	// An unknown head sha must never be treated as a match.
	blank := pr
	blank.HeadSHA = ""
	none := Correlate(snap([]forge.PullRequest{blank}, nil),
		repo(local.Repo{Branch: "topic", Last: local.Commit{SHA: ""}}))
	if hasKind(none, KindCIRedOnHead) {
		t.Error("two empty shas were treated as the same commit")
	}
}

func TestChangesRequested(t *testing.T) {
	pr := forge.PullRequest{
		Repo: "o/r", Number: 11, URL: "u", HeadRef: "topic",
		ReviewDecision: "CHANGES_REQUESTED",
	}

	facts := Correlate(snap([]forge.PullRequest{pr}, nil), repo(local.Repo{Branch: "topic"}))
	if !hasKind(facts, KindChangesRequested) {
		t.Fatalf("kinds = %v, want changes-requested", kinds(facts))
	}
}

func TestStaleBranch(t *testing.T) {
	merged := forge.PullRequest{Repo: "o/r", Number: 2, URL: "u", HeadRef: "range-picker"}

	clean := Correlate(snap(nil, []forge.PullRequest{merged}),
		repo(local.Repo{Branch: "range-picker"}))
	if !hasKind(clean, KindStaleBranch) {
		t.Fatalf("kinds = %v, want stale-branch for a merged PR on a clean tree", kinds(clean))
	}
	for _, f := range clean {
		if f.Kind == KindStaleBranch && f.Urgent() {
			t.Error("stale-branch should be a notice, not urgent: nothing is broken")
		}
	}

	// Still-uncommitted or still-unpushed work means the branch is not finished.
	dirty := Correlate(snap(nil, []forge.PullRequest{merged}),
		repo(local.Repo{Branch: "range-picker", Modified: 1}))
	if hasKind(dirty, KindStaleBranch) {
		t.Error("a dirty tree was called finished")
	}

	ahead := Correlate(snap(nil, []forge.PullRequest{merged}),
		repo(local.Repo{Branch: "range-picker", Unpushed: 1}))
	if hasKind(ahead, KindStaleBranch) {
		t.Error("a branch with unpushed commits was called finished")
	}
}

func TestForkBehind(t *testing.T) {
	facts := Correlate(&poll.Snapshot{}, repo(local.Repo{
		Branch:         "master",
		UpstreamBehind: 14,
		Remotes: map[string]string{
			"origin":   "git@github.com:karamble/thing.git",
			"upstream": "https://github.com/original/thing.git",
		},
	}))

	if !hasKind(facts, KindForkBehind) {
		t.Fatalf("kinds = %v, want fork-behind", kinds(facts))
	}
	for _, f := range facts {
		if f.Kind == KindForkBehind && f.Repo != "karamble/thing" {
			t.Errorf("Repo = %q, want the fork's own name", f.Repo)
		}
	}
}

// TestUrgentSortsFirst keeps the list readable: what is broken goes above what
// is merely tidy-up.
func TestUrgentSortsFirst(t *testing.T) {
	open := []forge.PullRequest{{Repo: "o/r", Number: 7, HeadRef: "topic"}}
	merged := []forge.PullRequest{{Repo: "o/r", Number: 2, HeadRef: "done"}}

	lcl := &local.Snapshot{Repos: []local.Repo{
		{Name: "r", Path: "/a", Branch: "done",
			Remotes: map[string]string{"origin": "git@github.com:o/r.git"}},
		{Name: "r2", Path: "/b", Branch: "topic", Unpushed: 2,
			Remotes: map[string]string{"origin": "git@github.com:o/r.git"}},
	}}

	facts := Correlate(snap(open, merged), lcl)
	if len(facts) < 2 {
		t.Fatalf("facts = %+v, want both a missing-work and a stale-branch", facts)
	}
	if !facts[0].Urgent() {
		t.Errorf("first fact = %+v, want the urgent one first", facts[0])
	}
}

func TestNoRemoteIsNotJoined(t *testing.T) {
	pr := forge.PullRequest{Repo: "o/r", Number: 7, HeadRef: "topic"}
	lcl := &local.Snapshot{Repos: []local.Repo{{Name: "scratch", Path: "/s", Branch: "topic", Unpushed: 4}}}

	if facts := Correlate(snap([]forge.PullRequest{pr}, nil), lcl); len(facts) != 0 {
		t.Errorf("facts = %+v, want none: a repo with no remote joins to nothing", facts)
	}
}

func TestNilSnapshotsAreSafe(t *testing.T) {
	if facts := Correlate(nil, nil); facts != nil {
		t.Errorf("Correlate(nil, nil) = %+v, want nil", facts)
	}
}
