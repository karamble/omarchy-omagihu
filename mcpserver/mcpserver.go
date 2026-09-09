// Package mcpserver exposes omagihu over the Model Context Protocol, so an
// agent can ask what is waiting, what is in flight and what is at risk without
// shelling out to gh and guessing. Every tool is read only.
package mcpserver

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/karamble/omarchy-omagihu/attention"
	"github.com/karamble/omarchy-omagihu/correlate"
	"github.com/karamble/omarchy-omagihu/forge"
	"github.com/karamble/omarchy-omagihu/local"
	"github.com/karamble/omarchy-omagihu/poll"
)

// Remote is the poller as this package consumes it.
type Remote interface {
	Snapshot() *poll.Snapshot
}

// Local is the filesystem watcher as this package consumes it.
type Local interface {
	Snapshot() *local.Snapshot
}

// Source is everything the tools read. It is the same state the panel renders,
// so an agent and a person always see the same picture.
type Source struct {
	Remote Remote
	Local  Local
	// Monitoring reports the master switch, so a tool can say "asleep" rather
	// than quietly returning stale data as though it were current.
	Monitoring func() bool
	// Facts is the join between the two planes.
	Facts func() []correlate.Fact
	// Alerts resolves the trigger engine. It is a function because the daemon
	// builds this handler before the engine exists, the same reason Monitoring
	// and Facts are functions; capturing the value here would pin a nil.
	Alerts func() Alerts
}

// alerts resolves the engine, reporting false when there is not one to write to.
func (s Source) alerts() (Alerts, bool) {
	if s.Alerts == nil {
		return nil, false
	}
	a := s.Alerts()
	if a == nil {
		return nil, false
	}
	return a, true
}

// Handler builds the streamable HTTP handler to mount on the daemon's listener.
func Handler(src Source, version string) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "omagihu",
		Title:   "Omagihu: GitHub radar",
		Version: version,
	}, nil)
	register(server, src)
	registerAlerts(server, src)

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil)
}

type empty struct{}

// status rides along with every answer so a caller can tell current data from
// the last thing seen before the daemon was put to sleep.
type status struct {
	Monitoring bool   `json:"monitoring"`
	Note       string `json:"note,omitempty"`
}

func (s Source) status() status {
	if s.Monitoring == nil || s.Monitoring() {
		return status{Monitoring: true}
	}
	return status{
		Monitoring: false,
		Note:       "omagihu is asleep: nothing is being polled, this is the last known state",
	}
}

type inboxOut struct {
	Status        status               `json:"status"`
	Count         int                  `json:"count"`
	Notifications []forge.Notification `json:"notifications"`
}

type workOut struct {
	Status         status              `json:"status"`
	AuthoredPRs    []forge.PullRequest `json:"authoredPrs"`
	ReviewRequests []forge.PullRequest `json:"reviewRequests"`
	AssignedIssues []forge.Issue       `json:"assignedIssues"`
	AuthoredIssues []forge.Issue       `json:"authoredIssues"`
}

type reposOut struct {
	Status status       `json:"status"`
	Count  int          `json:"count"`
	Repos  []local.Repo `json:"repos"`
}

type factsOut struct {
	Status status           `json:"status"`
	Count  int              `json:"count"`
	Facts  []correlate.Fact `json:"facts"`
}

type attentionOut struct {
	Status    status          `json:"status"`
	Attention attention.State `json:"attention"`
}

func register(s *mcp.Server, src Source) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "omagihu_inbox",
		Description: "Unread GitHub notifications across every configured account, newest first.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, inboxOut, error) {
		items := mergedInbox(src)
		return nil, inboxOut{Status: src.status(), Count: len(items), Notifications: items}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "omagihu_work",
		Description: "Open pull requests you authored, pull requests awaiting your review, " +
			"and issues assigned to you. Pull requests carry their CI rollup and review decision.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, workOut, error) {
		authored, reviews, issues, opened := mergedWork(src)
		return nil, workOut{
			Status:         src.status(),
			AuthoredPRs:    authored,
			ReviewRequests: reviews,
			AssignedIssues: issues,
			AuthoredIssues: opened,
		}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "omagihu_repos",
		Description: "Every watched local checkout: branch, unpushed commits, uncommitted " +
			"changes, interrupted rebases or merges, stashes and distance from upstream.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, reposOut, error) {
		snap := src.Local.Snapshot()
		return nil, reposOut{Status: src.status(), Count: len(snap.Repos), Repos: snap.Repos}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "omagihu_risk",
		Description: "Only the local checkouts holding work at risk: unpushed commits, an " +
			"interrupted git operation, or uncommitted changes.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, reposOut, error) {
		snap := src.Local.Snapshot()
		var risky []local.Repo
		for _, r := range snap.Repos {
			if r.AtRisk() {
				risky = append(risky, r)
			}
		}
		return nil, reposOut{Status: src.status(), Count: len(risky), Repos: risky}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "omagihu_facts",
		Description: "Where GitHub and this machine disagree: pull requests missing commits " +
			"that exist only locally, checks failing on the commit currently checked out, " +
			"branches whose pull request already merged, and forks trailing upstream. " +
			"None of these are visible from the remote or the local side alone.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, factsOut, error) {
		var facts []correlate.Fact
		if src.Facts != nil {
			facts = src.Facts()
		}
		return nil, factsOut{Status: src.status(), Count: len(facts), Facts: facts}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "omagihu_attention",
		Description: "The one thing that most wants your attention right now, with the full " +
			"breakdown behind it. Use this to answer 'is anything waiting on me'.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, attentionOut, error) {
		authored, reviews, _, _ := mergedWork(src)
		var facts []correlate.Fact
		if src.Facts != nil {
			facts = src.Facts()
		}
		return nil, attentionOut{
			Status: src.status(),
			Attention: attention.Resolve(attention.Input{
				Reviews:  reviews,
				Authored: authored,
				Unread:   mergedInbox(src),
				Repos:    src.Local.Snapshot().Repos,
				Facts:    facts,
			}),
		}, nil
	})
}

// mergedInbox flattens every account's notifications, deduplicated by subject.
func mergedInbox(src Source) []forge.Notification {
	var out []forge.Notification
	seen := make(map[string]struct{})
	for _, a := range src.Remote.Snapshot().Accounts {
		for _, n := range a.Notifications {
			key := n.SubjectURL
			if key == "" {
				key = a.AccountID + "/" + n.ID
			}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, n)
		}
	}
	return out
}

func mergedWork(src Source) (authored, reviews []forge.PullRequest, issues, opened []forge.Issue) {
	seenPR := make(map[string]struct{})
	seenReview := make(map[string]struct{})
	seenIssue := make(map[string]struct{})
	seenOpened := make(map[string]struct{})

	for _, a := range src.Remote.Snapshot().Accounts {
		for _, pr := range a.AuthoredPRs {
			if _, dup := seenPR[pr.URL]; !dup {
				seenPR[pr.URL] = struct{}{}
				authored = append(authored, pr)
			}
		}
		for _, pr := range a.ReviewRequests {
			if _, dup := seenReview[pr.URL]; !dup {
				seenReview[pr.URL] = struct{}{}
				reviews = append(reviews, pr)
			}
		}
		for _, is := range a.AssignedIssues {
			if _, dup := seenIssue[is.URL]; !dup {
				seenIssue[is.URL] = struct{}{}
				issues = append(issues, is)
			}
		}
		for _, is := range a.AuthoredIssues {
			if _, dup := seenOpened[is.URL]; !dup {
				seenOpened[is.URL] = struct{}{}
				opened = append(opened, is)
			}
		}
	}
	return authored, reviews, issues, opened
}
