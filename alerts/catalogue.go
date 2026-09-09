package alerts

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/karamble/omarchy-omagihu/attention"
	"github.com/karamble/omarchy-omagihu/correlate"
	"github.com/karamble/omarchy-omagihu/forge"
	"github.com/karamble/omarchy-omagihu/local"
)

// Kind is the shape of a leaf, which decides the operators it accepts.
type Kind string

const (
	KindNumber Kind = "number"
	KindText   Kind = "text"
	KindBool   Kind = "bool"
	KindList   Kind = "list"
)

// Leaf is one watchable path.
type Leaf struct {
	Path      string     `json:"path"`
	Kind      Kind       `json:"kind"`
	Operators []Operator `json:"operators"`
	Describes string     `json:"describes"`
	// Fields are what --where can filter on, for a list.
	Fields []string `json:"fields,omitempty"`
	// Identity is what makes two entries the same entry, which is what makes
	// appears and disappears honest.
	Identity []string `json:"identity,omitempty"`
	// TimeFields are the timestamps ages can measure.
	TimeFields []string `json:"timeFields,omitempty"`
}

// Accepts reports whether this leaf takes the given operator.
func (l Leaf) Accepts(op Operator) bool { return slices.Contains(l.Operators, op) }

var (
	numberOps = []Operator{OpCrosses}
	textOps   = []Operator{OpBecomes}
	listOps   = []Operator{OpAppears, OpDisappears, OpCount, OpAges}
	// Facts are recomputed from scratch every pass and carry no timestamp of
	// their own, so there is nothing for ages to measure.
	listOpsNoAge = []Operator{OpAppears, OpDisappears, OpCount}
)

// prFields are shared by every pull request list.
var (
	prFields   = []string{"repo", "number", "title", "author", "headRef", "reviewDecision", "checksState", "isDraft"}
	prIdentity = []string{"repo", "number"}
	prTimes    = []string{"updatedAt", "mergedAt"}
)

// Catalogue is every path a trigger can watch. It mirrors what the panel
// renders, so anything visible there can be waited on.
func Catalogue() []Leaf {
	return []Leaf{
		// ---- what most wants you, as numbers
		{Path: "attention.reviews", Kind: KindNumber, Operators: numberOps,
			Describes: "reviews requested of you"},
		{Path: "attention.brokenPrs", Kind: KindNumber, Operators: numberOps,
			Describes: "your pull requests failing checks or sent back"},
		{Path: "attention.reconcile", Kind: KindNumber, Operators: numberOps,
			Describes: "urgent drift between GitHub and this machine"},
		{Path: "attention.unread", Kind: KindNumber, Operators: numberOps,
			Describes: "unread notifications"},
		{Path: "attention.reposAtRisk", Kind: KindNumber, Operators: numberOps,
			Describes: "checkouts holding uncommitted or unpushed work"},
		{Path: "attention.unpushedTotal", Kind: KindNumber, Operators: numberOps,
			Describes: "commits that exist only on this machine"},
		{Path: "attention.interrupted", Kind: KindNumber, Operators: numberOps,
			Describes: "checkouts left mid rebase, merge or bisect"},
		{Path: "attention.level", Kind: KindText, Operators: textOps,
			Describes: "the bar's severity: urgent, warn, notice, clear or asleep"},

		// ---- the daemon itself
		{Path: "health.repos", Kind: KindNumber, Operators: numberOps,
			Describes: "how many checkouts are watched"},
		{Path: "health.rateLeft", Kind: KindNumber, Operators: numberOps,
			Describes: "GitHub rate budget remaining"},
		{Path: "health.monitoring", Kind: KindBool, Operators: textOps,
			Describes: "whether polling is on at all"},
		{Path: "health.lastError", Kind: KindText, Operators: textOps,
			Describes: "the most recent poll error, empty when healthy"},

		// ---- the lists, where most of the interesting waiting happens
		{Path: "inbox", Kind: KindList, Operators: listOps,
			Describes:  "unread GitHub notifications",
			Fields:     []string{"repo", "type", "title", "reason", "accountId"},
			Identity:   []string{"id"},
			TimeFields: []string{"updatedAt"}},
		{Path: "work.reviewRequests", Kind: KindList, Operators: listOps,
			Describes: "pull requests waiting on your review",
			Fields:    prFields, Identity: prIdentity, TimeFields: prTimes},
		{Path: "work.authoredPrs", Kind: KindList, Operators: listOps,
			Describes: "your open pull requests",
			Fields:    prFields, Identity: prIdentity, TimeFields: prTimes},
		{Path: "work.mergedPrs", Kind: KindList, Operators: listOps,
			Describes: "your recently merged pull requests",
			Fields:    prFields, Identity: prIdentity, TimeFields: prTimes},
		{Path: "work.assignedIssues", Kind: KindList, Operators: listOps,
			Describes:  "issues assigned to you",
			Fields:     []string{"repo", "number", "title"},
			Identity:   []string{"repo", "number"},
			TimeFields: []string{"updatedAt"}},
		{Path: "facts", Kind: KindList, Operators: listOpsNoAge,
			Describes: "where GitHub and this machine disagree",
			Fields:    []string{"kind", "severity", "repo", "path", "branch", "number"},
			Identity:  []string{"kind", "path", "branch"}},
		{Path: "repos", Kind: KindList, Operators: listOps,
			Describes: "watched local checkouts",
			Fields: []string{"name", "path", "branch", "upstream", "operation",
				"unpushed", "behind", "upstreamBehind", "staged", "modified", "untracked", "conflicted"},
			Identity:   []string{"path"},
			TimeFields: []string{"observedAt"}},
	}
}

// Lookup finds one leaf by path.
func Lookup(path string) (Leaf, bool) {
	for _, l := range Catalogue() {
		if l.Path == path {
			return l, true
		}
	}
	return Leaf{}, false
}

// Snapshot is everything a trigger can be evaluated against. The daemon fills
// it from the same state the panel renders, so an alert and a person can never
// disagree about what was true.
type Snapshot struct {
	Attention attention.State
	Health    Health
	Inbox     []forge.Notification
	Reviews   []forge.PullRequest
	Authored  []forge.PullRequest
	Merged    []forge.PullRequest
	Issues    []forge.Issue
	Facts     []correlate.Fact
	Repos     []local.Repo
	// TakenAt anchors the ages operator.
	TakenAt time.Time
}

// Health is the handful of daemon numbers worth watching.
type Health struct {
	Repos      int
	RateLeft   int
	Monitoring bool
	LastError  string
}

// Number resolves a numeric path, reporting false when the path is not one.
func (s Snapshot) Number(path string) (float64, bool) {
	switch path {
	case "attention.reviews":
		return float64(s.Attention.Reviews), true
	case "attention.brokenPrs":
		return float64(s.Attention.BrokenPRs), true
	case "attention.reconcile":
		return float64(s.Attention.Reconcile), true
	case "attention.unread":
		return float64(s.Attention.Unread), true
	case "attention.reposAtRisk":
		return float64(s.Attention.ReposAtRisk), true
	case "attention.unpushedTotal":
		return float64(s.Attention.UnpushedTotal), true
	case "attention.interrupted":
		return float64(s.Attention.Interrupted), true
	case "health.repos":
		return float64(s.Health.Repos), true
	case "health.rateLeft":
		return float64(s.Health.RateLeft), true
	}
	return 0, false
}

// Text resolves a text or bool path as a string.
func (s Snapshot) Text(path string) (string, bool) {
	switch path {
	case "attention.level":
		return s.Attention.Level, true
	case "health.monitoring":
		return fmt.Sprint(s.Health.Monitoring), true
	case "health.lastError":
		return s.Health.LastError, true
	}
	return "", false
}

// List resolves a list path into filterable entries.
func (s Snapshot) List(path string) ([]map[string]any, bool) {
	switch path {
	case "inbox":
		out := make([]map[string]any, 0, len(s.Inbox))
		for _, n := range s.Inbox {
			out = append(out, map[string]any{
				"id": n.ID, "repo": n.Repo, "type": n.Type, "title": n.Title,
				"reason": n.Reason, "accountId": n.AccountID,
				"updatedAt": n.UpdatedAt, "url": n.WebURL,
			})
		}
		return out, true
	case "work.reviewRequests":
		return pullRequests(s.Reviews), true
	case "work.authoredPrs":
		return pullRequests(s.Authored), true
	case "work.mergedPrs":
		return pullRequests(s.Merged), true
	case "work.assignedIssues":
		out := make([]map[string]any, 0, len(s.Issues))
		for _, i := range s.Issues {
			out = append(out, map[string]any{
				"repo": i.Repo, "number": i.Number, "title": i.Title,
				"updatedAt": i.UpdatedAt, "url": i.URL,
			})
		}
		return out, true
	case "facts":
		out := make([]map[string]any, 0, len(s.Facts))
		for _, f := range s.Facts {
			out = append(out, map[string]any{
				"kind": string(f.Kind), "severity": f.Severity, "repo": f.Repo,
				"path": f.Path, "branch": f.Branch, "number": f.Number,
				"summary": f.Summary, "url": f.URL,
			})
		}
		return out, true
	case "repos":
		out := make([]map[string]any, 0, len(s.Repos))
		for _, r := range s.Repos {
			out = append(out, map[string]any{
				"name": r.Name, "path": r.Path, "branch": r.Branch,
				"upstream": r.Upstream, "operation": string(r.Operation),
				"unpushed": r.Unpushed, "behind": r.Behind,
				"upstreamBehind": r.UpstreamBehind, "staged": r.Staged,
				"modified": r.Modified, "untracked": r.Untracked,
				"conflicted": r.Conflicted, "observedAt": r.ObservedAt,
			})
		}
		return out, true
	}
	return nil, false
}

func pullRequests(prs []forge.PullRequest) []map[string]any {
	out := make([]map[string]any, 0, len(prs))
	for _, pr := range prs {
		out = append(out, map[string]any{
			"repo": pr.Repo, "number": pr.Number, "title": pr.Title,
			"author": pr.Author, "headRef": pr.HeadRef,
			"reviewDecision": pr.ReviewDecision, "checksState": pr.ChecksState,
			"isDraft": pr.IsDraft, "updatedAt": pr.UpdatedAt,
			"mergedAt": pr.MergedAt, "url": pr.URL,
		})
	}
	return out
}

// identityOf builds the key that decides when two entries are the same entry.
func identityOf(entry map[string]any, fields []string) string {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, fmt.Sprint(entry[f]))
	}
	return strings.Join(parts, "\x1f")
}

// filter keeps the entries every where clause accepts.
func filter(entries []map[string]any, wheres []Where) []map[string]any {
	if len(wheres) == 0 {
		return entries
	}
	var out []map[string]any
	for _, e := range entries {
		keep := true
		for _, w := range wheres {
			if !w.Match(e) {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, e)
		}
	}
	return out
}
