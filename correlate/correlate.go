// Package correlate introduces the two planes to each other. Everything here is
// invisible to either one alone: GitHub cannot see the commits sitting on your
// disk, and your disk does not know a pull request exists.
package correlate

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/karamble/omarchy-omagihu/forge"
	"github.com/karamble/omarchy-omagihu/local"
	"github.com/karamble/omarchy-omagihu/poll"
)

// Kind names one shape of mismatch between what GitHub believes and what this
// machine holds.
type Kind string

const (
	// KindMissingWork: commits exist locally on a branch that already has an
	// open pull request, so the pull request is not the whole story.
	KindMissingWork Kind = "missing-work"
	// KindCIRedOnHead: the commit checked out here is the pull request head,
	// and its checks failed.
	KindCIRedOnHead Kind = "ci-red-on-head"
	// KindChangesRequested: the branch in the working tree was sent back.
	KindChangesRequested Kind = "changes-requested"
	// KindStaleBranch: the pull request merged, so the local branch is done.
	KindStaleBranch Kind = "stale-branch"
	// KindForkBehind: a fork trailing the repository it was forked from.
	KindForkBehind Kind = "fork-behind"
	// KindDetachedWork: commits on a detached HEAD. No branch names them, so
	// the next checkout leaves them reachable only through the reflog.
	KindDetachedWork Kind = "detached-work"
	// KindNoRemote: a repository holding commits with nowhere to push them.
	KindNoRemote Kind = "no-remote"
	// KindPRBaseMoved: an approved pull request whose base advanced after the
	// branch diverged, so it may want a rebase before it merges.
	KindPRBaseMoved Kind = "pr-base-moved"
)

// Severity levels, matching the vocabulary the panel and the bar already use.
const (
	Urgent = "urgent"
	Notice = "notice"
)

// Fact is one correlation, phrased so it can be read straight off a row.
type Fact struct {
	Kind     Kind   `json:"kind"`
	Severity string `json:"severity"`
	Repo     string `json:"repo"`
	Path     string `json:"path"`
	Branch   string `json:"branch"`
	Summary  string `json:"summary"`
	Detail   string `json:"detail,omitempty"`
	URL      string `json:"url,omitempty"`
	Number   int    `json:"number,omitempty"`
}

// Urgent reports whether this fact should be able to colour the bar.
func (f Fact) Urgent() bool { return f.Severity == Urgent }

// remotePattern pulls owner and repository out of any of the forms a git remote
// is written in: scp-style ssh, ssh:// urls, https, and bare host/owner/repo.
var remotePattern = regexp.MustCompile(`(?:^|[/@])([^/@:]+\.[^/@:]+)[:/]+([^/]+)/([^/]+?)(?:\.git)?/?$`)

// NormalizeRemote reduces a git remote to "owner/repo", or returns an empty
// string when it is not a recognisable forge url. The host is deliberately
// dropped: the join only has to be consistent, and every account already knows
// its own host.
func NormalizeRemote(remote string) string {
	r := strings.TrimSpace(remote)
	if r == "" {
		return ""
	}
	m := remotePattern.FindStringSubmatch(r)
	if m == nil {
		return ""
	}
	owner, repo := m[2], m[3]
	if owner == "" || repo == "" {
		return ""
	}
	return owner + "/" + repo
}

// checksFailed reports a red or errored check rollup.
func checksFailed(pr forge.PullRequest) bool {
	return pr.ChecksState == "FAILURE" || pr.ChecksState == "ERROR"
}

// Options tunes Correlate. The zero value applies every rule to every
// checkout.
type Options struct {
	// DeliberatelyLocal reports a repository that is meant to have no remote,
	// which silences the no-remote fact for it. Nil means none are.
	DeliberatelyLocal func(repo local.Repo) bool
}

// Correlate joins the account view to the checkouts on this machine.
func Correlate(remote *poll.Snapshot, lcl *local.Snapshot) []Fact {
	return CorrelateWith(remote, lcl, Options{})
}

// CorrelateWith is Correlate with its options spelled out.
func CorrelateWith(remote *poll.Snapshot, lcl *local.Snapshot, opts Options) []Fact {
	if remote == nil || lcl == nil {
		return nil
	}

	// Two checkouts can point at the same repository, a fork and a clone of the
	// parent, so the index holds every one of them.
	byRepo := make(map[string][]local.Repo)
	// A fork's checkout names the parent through its upstream remote, and the
	// parent is the repository a pull request from the fork belongs to.
	byUpstream := make(map[string][]local.Repo)
	for _, r := range lcl.Repos {
		if key := NormalizeRemote(r.Remotes["origin"]); key != "" {
			byRepo[key] = append(byRepo[key], r)
		}
		if key := NormalizeRemote(r.Remotes["upstream"]); key != "" {
			byUpstream[key] = append(byUpstream[key], r)
		}
	}

	open, merged := dedupePRs(remote)

	var facts []Fact
	for _, pr := range open {
		// Approval is the gate: a branch still being worked on is expected to
		// drift, an approved one is a click from merging and the stale base
		// is what stops it.
		if pr.ReviewDecision == "APPROVED" {
			for _, repo := range prCheckouts(pr, byRepo, byUpstream) {
				behind, base := baseDistance(pr, repo)
				// A distance is only worth quoting when it was measured
				// against the branch this pull request actually merges into.
				// One onto a release branch, or stacked on another, stays
				// quiet rather than being told how far it is from a branch it
				// is not going to.
				if behind <= 0 || base == "" || base != pr.BaseRef {
					continue
				}
				facts = append(facts, Fact{
					Kind:     KindPRBaseMoved,
					Severity: Notice,
					Repo:     pr.Repo, Path: repo.Path, Branch: repo.Branch,
					URL: pr.URL, Number: pr.Number,
					Summary: fmt.Sprintf("%s #%d is approved but %s behind its base",
						repo.Name, pr.Number, plural(behind, "commit", "commits")),
					Detail: fmt.Sprintf("counted against %s as last fetched; a rebase brings it current", base),
				})
			}
		}

		for _, repo := range prCheckouts(pr, byRepo, byUpstream) {
			if repo.Unpushed > 0 {
				facts = append(facts, Fact{
					Kind:     KindMissingWork,
					Severity: Urgent,
					Repo:     pr.Repo, Path: repo.Path, Branch: repo.Branch,
					URL: pr.URL, Number: pr.Number,
					Summary: fmt.Sprintf("%s #%d is missing %s",
						repo.Name, pr.Number, plural(repo.Unpushed, "local commit", "local commits")),
					Detail: "the branch here is ahead of what the pull request shows",
				})
			}

			// Only claim the checks are about this commit when it is provably
			// the same commit.
			if checksFailed(pr) && pr.HeadSHA != "" && repo.Last.SHA == pr.HeadSHA {
				facts = append(facts, Fact{
					Kind:     KindCIRedOnHead,
					Severity: Urgent,
					Repo:     pr.Repo, Path: repo.Path, Branch: repo.Branch,
					URL: pr.URL, Number: pr.Number,
					Summary: fmt.Sprintf("%s #%d: checks failed on the commit you have checked out",
						repo.Name, pr.Number),
					Detail: "HEAD here is the pull request head",
				})
			}

			if pr.ReviewDecision == "CHANGES_REQUESTED" {
				facts = append(facts, Fact{
					Kind:     KindChangesRequested,
					Severity: Urgent,
					Repo:     pr.Repo, Path: repo.Path, Branch: repo.Branch,
					URL: pr.URL, Number: pr.Number,
					Summary: fmt.Sprintf("%s #%d: changes requested on the branch you are on",
						repo.Name, pr.Number),
				})
			}
		}
	}

	for _, pr := range merged {
		for _, repo := range prCheckouts(pr, byRepo, byUpstream) {
			// Finished means finished: nothing uncommitted and nothing unpushed,
			// otherwise the branch still holds something.
			if repo.Dirty() || repo.Unpushed > 0 {
				continue
			}
			facts = append(facts, Fact{
				Kind:     KindStaleBranch,
				Severity: Notice,
				Repo:     pr.Repo, Path: repo.Path, Branch: repo.Branch,
				URL: pr.URL, Number: pr.Number,
				Summary: fmt.Sprintf("%s: %s was merged in #%d",
					repo.Name, repo.Branch, pr.Number),
				Detail: "the branch is finished and safe to leave behind",
			})
		}
	}

	for _, repo := range lcl.Repos {
		if repo.UpstreamBehind > 0 {
			facts = append(facts, Fact{
				Kind:     KindForkBehind,
				Severity: Notice,
				Repo:     NormalizeRemote(repo.Remotes["origin"]),
				Path:     repo.Path, Branch: repo.Branch,
				Summary: fmt.Sprintf("%s is %s behind upstream",
					repo.Name, plural(repo.UpstreamBehind, "commit", "commits")),
				Detail: "counted against the refs last fetched",
			})
		}

		// Unpushed counts commits reachable from HEAD and from no remote. On a
		// detached HEAD that is work no branch is known to name. Notice, not
		// urgent: an urgent fact lifts the bar to the reconcile tier, and this
		// is local work, which stays at the local tier; the badge is loud
		// instead.
		if repo.Detached && repo.Unpushed > 0 {
			facts = append(facts, Fact{
				Kind:     KindDetachedWork,
				Severity: Notice,
				Repo:     NormalizeRemote(repo.Remotes["origin"]),
				Path:     repo.Path, Branch: repo.Branch,
				Summary: fmt.Sprintf("%s: %s on a detached HEAD",
					repo.Name, plural(repo.Unpushed, "commit", "commits")),
				Detail: "no branch names this work and the next checkout leaves it to the reflog; git switch -c <name> keeps it",
			})
		}

		// Commits with nowhere to go. A commit has to exist: a fresh init with
		// nothing committed has nothing to lose.
		if len(repo.Remotes) == 0 && repo.Last.SHA != "" &&
			!(opts.DeliberatelyLocal != nil && opts.DeliberatelyLocal(repo)) {
			facts = append(facts, Fact{
				Kind:     KindNoRemote,
				Severity: Notice,
				Repo:     repo.Name,
				Path:     repo.Path, Branch: repo.Branch,
				Summary: fmt.Sprintf("%s has commits and no remote", repo.Name),
				Detail:  "everything here exists on this disk only",
			})
		}
	}

	// Urgent first, then stable by repository and kind so the list does not
	// reshuffle between polls.
	slices.SortFunc(facts, func(a, b Fact) int {
		if a.Urgent() != b.Urgent() {
			if a.Urgent() {
				return -1
			}
			return 1
		}
		if c := strings.Compare(a.Repo, b.Repo); c != 0 {
			return c
		}
		return strings.Compare(string(a.Kind), string(b.Kind))
	})
	return facts
}

// prCheckouts finds the checkouts sitting on a pull request's branch, whether
// they clone the repository the pull request belongs to or a fork of it.
// baseDistance reports how far a checkout trails the base of pr, and the
// branch that distance was measured against. It picks the remote that is
// actually the pull request's base repository rather than guessing: upstream
// for a pull request opened from a fork, origin for one opened from a branch
// in the repository itself. A checkout that is neither answers with nothing.
func baseDistance(pr forge.PullRequest, repo local.Repo) (int, string) {
	if NormalizeRemote(repo.Remotes["upstream"]) == pr.Repo {
		return repo.UpstreamBehind, repo.UpstreamBase
	}
	if NormalizeRemote(repo.Remotes["origin"]) == pr.Repo {
		return repo.BaseBehind, repo.BaseBranch
	}
	return 0, ""
}

func prCheckouts(pr forge.PullRequest, byRepo, byUpstream map[string][]local.Repo) []local.Repo {
	var out []local.Repo
	seen := make(map[string]struct{})
	for _, repo := range slices.Concat(byRepo[pr.Repo], byUpstream[pr.Repo]) {
		if repo.Branch != pr.HeadRef {
			continue
		}
		if _, dup := seen[repo.Path]; dup {
			continue
		}
		seen[repo.Path] = struct{}{}
		out = append(out, repo)
	}
	return out
}

// dedupePRs flattens the accounts, since the same pull request can be visible
// to more than one identity.
func dedupePRs(remote *poll.Snapshot) (open, merged []forge.PullRequest) {
	seenOpen := make(map[string]struct{})
	seenMerged := make(map[string]struct{})

	for _, acct := range remote.Accounts {
		for _, pr := range acct.AuthoredPRs {
			if _, dup := seenOpen[pr.URL]; dup {
				continue
			}
			seenOpen[pr.URL] = struct{}{}
			open = append(open, pr)
		}
		for _, pr := range acct.MergedPRs {
			if _, dup := seenMerged[pr.URL]; dup {
				continue
			}
			seenMerged[pr.URL] = struct{}{}
			merged = append(merged, pr)
		}
	}
	return open, merged
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
