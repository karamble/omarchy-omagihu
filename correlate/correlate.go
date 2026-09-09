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

// Correlate joins the account view to the checkouts on this machine.
func Correlate(remote *poll.Snapshot, lcl *local.Snapshot) []Fact {
	if remote == nil || lcl == nil {
		return nil
	}

	// Two checkouts can point at the same repository, a fork and a clone of the
	// parent, so the index holds every one of them.
	byRepo := make(map[string][]local.Repo)
	for _, r := range lcl.Repos {
		key := NormalizeRemote(r.Remotes["origin"])
		if key == "" {
			continue
		}
		byRepo[key] = append(byRepo[key], r)
	}

	open, merged := dedupePRs(remote)

	var facts []Fact
	for _, pr := range open {
		for _, repo := range byRepo[pr.Repo] {
			if repo.Branch != pr.HeadRef {
				continue
			}

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
		for _, repo := range byRepo[pr.Repo] {
			// Finished means finished: nothing uncommitted and nothing unpushed,
			// otherwise the branch still holds something.
			if repo.Branch != pr.HeadRef || repo.Dirty() || repo.Unpushed > 0 {
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
		if repo.UpstreamBehind <= 0 {
			continue
		}
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
