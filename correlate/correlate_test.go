package correlate

import (
	"strings"
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

// find returns the first fact of a kind, or fails the test.
func find(t *testing.T, facts []Fact, k Kind) Fact {
	t.Helper()
	for _, f := range facts {
		if f.Kind == k {
			return f
		}
	}
	t.Fatalf("kinds = %v, want %s", kinds(facts), k)
	return Fact{}
}

func TestDetachedWork(t *testing.T) {
	facts := Correlate(&poll.Snapshot{}, repo(local.Repo{
		Branch: "(detached)", Detached: true, Unpushed: 1, Path: "/checkout/r",
	}))
	f := find(t, facts, KindDetachedWork)
	if f.Urgent() {
		t.Error("detached-work must not be urgent: that would lift the bar off the local tier")
	}
	if f.Path != "/checkout/r" || f.Branch != "(detached)" {
		t.Errorf("fact = %+v, want it keyed on the checkout's path and branch", f)
	}

	// The adjacent cases: a detached HEAD with nothing unpushed, and ordinary
	// unpushed work on a named branch, which is a badge and not a fact.
	if got := Correlate(&poll.Snapshot{}, repo(local.Repo{Branch: "(detached)", Detached: true})); hasKind(got, KindDetachedWork) {
		t.Errorf("kinds = %v, want no detached-work when nothing is unpushed", kinds(got))
	}
	if got := Correlate(&poll.Snapshot{}, repo(local.Repo{Branch: "topic", Unpushed: 3})); hasKind(got, KindDetachedWork) {
		t.Errorf("kinds = %v, want no detached-work on a named branch", kinds(got))
	}
}

func TestNoRemote(t *testing.T) {
	committed := local.Repo{
		Name: "scratch", Path: "/s", Branch: "main",
		Remotes: map[string]string{}, Last: local.Commit{SHA: "abc"},
	}
	f := find(t, Correlate(&poll.Snapshot{}, repo(committed)), KindNoRemote)
	if f.Urgent() {
		t.Error("no-remote is informational: a local-only repository may be deliberate")
	}
	if f.Path != "/s" || f.Repo != "scratch" {
		t.Errorf("fact = %+v, want the checkout's path and, with no forge name, its own name", f)
	}

	// A fresh init has nothing to lose, and a repository with a remote is
	// somebody else's rule.
	empty := committed
	empty.Last = local.Commit{}
	if got := Correlate(&poll.Snapshot{}, repo(empty)); hasKind(got, KindNoRemote) {
		t.Errorf("kinds = %v, want no no-remote for a repository with no commits", kinds(got))
	}
	pushed := committed
	pushed.Remotes = map[string]string{"origin": "git@github.com:o/r.git"}
	if got := Correlate(&poll.Snapshot{}, repo(pushed)); hasKind(got, KindNoRemote) {
		t.Errorf("kinds = %v, want no no-remote when a remote exists", kinds(got))
	}

	// The seam the suppression list will plug into.
	opts := Options{DeliberatelyLocal: func(r local.Repo) bool { return r.Path == "/s" }}
	if got := CorrelateWith(&poll.Snapshot{}, repo(committed), opts); hasKind(got, KindNoRemote) {
		t.Errorf("kinds = %v, want no no-remote for a repository marked deliberately local", kinds(got))
	}
}

func TestPRBaseMoved(t *testing.T) {
	approved := forge.PullRequest{
		Repo: "original/thing", Number: 12, URL: "u12", HeadRef: "topic", BaseRef: "main",
		ReviewDecision: "APPROVED",
	}
	// A fork checkout: origin is the fork, upstream is the repository the
	// pull request belongs to. UpstreamBase names the branch the distance was
	// counted against, and it has to be the one the pull request merges into.
	fork := local.Repo{
		Name: "thing", Path: "/fork", Branch: "topic",
		UpstreamBehind: 8, UpstreamBase: "main",
		Remotes: map[string]string{
			"origin":   "git@github.com:karamble/thing.git",
			"upstream": "https://github.com/original/thing.git",
		},
	}

	f := find(t, Correlate(snap([]forge.PullRequest{approved}, nil), repo(fork)), KindPRBaseMoved)
	if f.Number != 12 || f.URL != "u12" || f.Path != "/fork" || f.Repo != "original/thing" {
		t.Errorf("fact = %+v, want it to name the pull request and the fork checkout", f)
	}
	if f.Urgent() {
		t.Error("pr-base-moved is a notice: nothing is broken, the base moved")
	}

	// Still being reviewed: expected to drift, so quiet.
	pending := approved
	pending.ReviewDecision = "REVIEW_REQUIRED"
	if got := Correlate(snap([]forge.PullRequest{pending}, nil), repo(fork)); hasKind(got, KindPRBaseMoved) {
		t.Errorf("kinds = %v, want no pr-base-moved while the review is pending", kinds(got))
	}
	// Approved and current.
	current := fork
	current.UpstreamBehind = 0
	if got := Correlate(snap([]forge.PullRequest{approved}, nil), repo(current)); hasKind(got, KindPRBaseMoved) {
		t.Errorf("kinds = %v, want no pr-base-moved when the base has not moved", kinds(got))
	}
	// Another branch of the same fork.
	other := fork
	other.Branch = "elsewhere"
	if got := Correlate(snap([]forge.PullRequest{approved}, nil), repo(other)); hasKind(got, KindPRBaseMoved) {
		t.Errorf("kinds = %v, want no pr-base-moved on a branch that is not the pull request head", kinds(got))
	}

	// A clone of the repository itself, with an upstream remote, joins through
	// origin as every other kind does.
	clone := fork
	clone.Path = "/clone"
	clone.Remotes = map[string]string{
		"origin":   "git@github.com:original/thing.git",
		"upstream": "https://github.com/original/thing.git",
	}
	find(t, Correlate(snap([]forge.PullRequest{approved}, nil), repo(clone)), KindPRBaseMoved)
}

// TestPRBaseMovedWithoutAnUpstreamRemote is the bug this kind was filed for.
// The only distance collected used to be the one measured against an upstream
// remote, which is the fork convention, so a pull request opened from a branch
// in the repository itself could never raise the fact however far its base had
// moved. That is the more common shape.
func TestPRBaseMovedWithoutAnUpstreamRemote(t *testing.T) {
	approved := forge.PullRequest{
		Repo: "o/r", Number: 30, URL: "u30", HeadRef: "topic", BaseRef: "main",
		ReviewDecision: "APPROVED",
	}
	// origin and nothing else: a repository you can push to directly.
	own := local.Repo{
		Name: "r", Path: "/own", Branch: "topic",
		BaseBehind: 6, BaseBranch: "main",
		Remotes: map[string]string{"origin": "git@github.com:o/r.git"},
	}

	f := find(t, Correlate(snap([]forge.PullRequest{approved}, nil), repo(own)), KindPRBaseMoved)
	if f.Number != 30 || f.Path != "/own" {
		t.Errorf("fact = %+v, want it to name the pull request and the checkout", f)
	}
	if !strings.Contains(f.Summary, "6 commits") {
		t.Errorf("summary = %q, want the distance origin's default branch has moved", f.Summary)
	}
	if !strings.Contains(f.Detail, "main") {
		t.Errorf("detail = %q, want it to name the branch the distance was counted against", f.Detail)
	}

	// Current against its base.
	current := own
	current.BaseBehind = 0
	if got := Correlate(snap([]forge.PullRequest{approved}, nil), repo(current)); hasKind(got, KindPRBaseMoved) {
		t.Errorf("kinds = %v, want no pr-base-moved when the base has not moved", kinds(got))
	}
}

// TestPRBaseMovedChecksTheBaseBranch pins that a distance is only quoted when
// it was measured against the branch the pull request actually merges into. A
// pull request onto a release branch, or stacked on another, is not told how
// far it is from a branch it is not going to.
func TestPRBaseMovedChecksTheBaseBranch(t *testing.T) {
	own := local.Repo{
		Name: "r", Path: "/own", Branch: "topic",
		BaseBehind: 6, BaseBranch: "main",
		Remotes: map[string]string{"origin": "git@github.com:o/r.git"},
	}
	stacked := forge.PullRequest{
		Repo: "o/r", Number: 31, URL: "u31", HeadRef: "topic", BaseRef: "release-2",
		ReviewDecision: "APPROVED",
	}
	if got := Correlate(snap([]forge.PullRequest{stacked}, nil), repo(own)); hasKind(got, KindPRBaseMoved) {
		t.Errorf("kinds = %v, want no pr-base-moved when the base is not the measured branch", kinds(got))
	}

	// A distance with no branch behind it says nothing about any base. This is
	// the checkout that was never cloned and so has no origin/HEAD to read.
	unnamed := own
	unnamed.BaseBranch = ""
	onMain := stacked
	onMain.BaseRef = "main"
	if got := Correlate(snap([]forge.PullRequest{onMain}, nil), repo(unnamed)); hasKind(got, KindPRBaseMoved) {
		t.Errorf("kinds = %v, want no pr-base-moved when nothing names the branch measured", kinds(got))
	}
}

// TestPullRequestKindsMatchAForkCheckout pins the second gap the base work
// uncovered. A pull request names its base repository, so a fork's checkout is
// only reachable through its upstream remote. pr-base-moved looked there;
// every other pull request kind indexed on origin alone and was silently blind
// to forks. All of these are quiet before the widening.
func TestPullRequestKindsMatchAForkCheckout(t *testing.T) {
	fork := local.Repo{
		Name: "thing", Path: "/fork", Branch: "topic",
		Unpushed: 3,
		Last:     local.Commit{SHA: "deadbeef"},
		Remotes: map[string]string{
			"origin":   "git@github.com:karamble/thing.git",
			"upstream": "https://github.com/original/thing.git",
		},
	}
	open := forge.PullRequest{
		Repo: "original/thing", Number: 12, URL: "u12", HeadRef: "topic", BaseRef: "main",
		HeadSHA: "deadbeef", ChecksState: "FAILURE", ReviewDecision: "CHANGES_REQUESTED",
	}
	got := Correlate(snap([]forge.PullRequest{open}, nil), repo(fork))
	for _, want := range []Kind{KindMissingWork, KindCIRedOnHead, KindChangesRequested} {
		if !hasKind(got, want) {
			t.Errorf("kinds = %v, want %s for a fork checkout reached through upstream", kinds(got), want)
		}
	}

	// The merged loop indexed on origin too, so stale-branch missed forks the
	// same way.
	done := forge.PullRequest{
		Repo: "original/thing", Number: 12, URL: "u12", HeadRef: "topic", BaseRef: "main",
	}
	clean := fork
	clean.Unpushed = 0
	if got := Correlate(snap(nil, []forge.PullRequest{done}), repo(clean)); !hasKind(got, KindStaleBranch) {
		t.Errorf("kinds = %v, want stale-branch for a merged pull request from a fork", kinds(got))
	}
}
