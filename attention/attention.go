// Package attention turns everything omagihu knows into the one thing a bar
// icon can say. The failure mode to avoid is a badge that is permanently lit
// and therefore ignored, so exactly one tier is ever reported: the most severe
// one that is currently active, counted on its own terms.
package attention

import (
	"strconv"

	"github.com/karamble/omarchy-omagihu/correlate"
	"github.com/karamble/omarchy-omagihu/forge"
	"github.com/karamble/omarchy-omagihu/local"
)

// Tier ranks what deserves the icon. Lower is more urgent.
type Tier int

const (
	// TierReview: someone else is blocked waiting on you.
	TierReview Tier = iota + 1
	// TierBroken: your own pull request is failing or was sent back.
	TierBroken
	// TierReconcile: what GitHub believes and what this machine holds have
	// drifted apart, such as a pull request missing commits you have here.
	TierReconcile
	// TierInbox: unread notifications.
	TierInbox
	// TierLocal: work at risk on this machine only.
	TierLocal
	// TierClear: nothing wants you.
	TierClear
)

// State is the resolved bar signal.
type State struct {
	Tier    Tier   `json:"tier"`
	Level   string `json:"level"`
	Count   int    `json:"count"`
	Summary string `json:"summary"`

	// The full breakdown, so the panel can show every number even though the
	// bar shows only the winning tier.
	Reviews       int `json:"reviews"`
	BrokenPRs     int `json:"brokenPrs"`
	Reconcile     int `json:"reconcile"`
	FactsTotal    int `json:"factsTotal"`
	Unread        int `json:"unread"`
	ReposAtRisk   int `json:"reposAtRisk"`
	UnpushedTotal int `json:"unpushedTotal"`
	Interrupted   int `json:"interrupted"`
}

// Input is everything the ladder considers.
type Input struct {
	Reviews  []forge.PullRequest
	Authored []forge.PullRequest
	Unread   []forge.Notification
	Repos    []local.Repo
	Facts    []correlate.Fact
}

// Broken reports whether a pull request of yours needs attention: its checks
// failed, or a reviewer asked for changes.
func Broken(pr forge.PullRequest) bool {
	return pr.ChecksState == "FAILURE" || pr.ChecksState == "ERROR" ||
		pr.ReviewDecision == "CHANGES_REQUESTED"
}

// Resolve picks the winning tier.
func Resolve(in Input) State {
	s := State{
		Tier:    TierClear,
		Level:   "clear",
		Reviews: len(in.Reviews),
		Unread:  len(in.Unread),
	}

	for _, pr := range in.Authored {
		if Broken(pr) {
			s.BrokenPRs++
		}
	}
	s.FactsTotal = len(in.Facts)
	for _, f := range in.Facts {
		if f.Urgent() {
			s.Reconcile++
		}
	}
	for _, r := range in.Repos {
		if r.AtRisk() {
			s.ReposAtRisk++
		}
		s.UnpushedTotal += r.Unpushed
		if r.Operation != local.OpNone {
			s.Interrupted++
		}
	}

	switch {
	case s.Reviews > 0:
		s.Tier, s.Level, s.Count = TierReview, "urgent", s.Reviews
		s.Summary = plural(s.Reviews, "review waiting on you", "reviews waiting on you")
	case s.BrokenPRs > 0:
		s.Tier, s.Level, s.Count = TierBroken, "urgent", s.BrokenPRs
		s.Summary = plural(s.BrokenPRs, "pull request needs you", "pull requests need you")
	case s.Reconcile > 0:
		s.Tier, s.Level, s.Count = TierReconcile, "urgent", s.Reconcile
		s.Summary = plural(s.Reconcile, "thing needs reconciling", "things need reconciling")
	case s.Unread > 0:
		s.Tier, s.Level, s.Count = TierInbox, "warn", s.Unread
		s.Summary = plural(s.Unread, "unread notification", "unread notifications")
	case s.ReposAtRisk > 0:
		s.Tier, s.Level, s.Count = TierLocal, "notice", s.ReposAtRisk
		s.Summary = plural(s.ReposAtRisk, "repo with local work", "repos with local work")
	default:
		s.Summary = "nothing waiting"
	}
	return s
}

func plural(n int, one, many string) string {
	word := many
	if n == 1 {
		word = one
	}
	return strconv.Itoa(n) + " " + word
}
