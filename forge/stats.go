package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// RepoStats is what a repository says about itself: the figures a card
// shows, and what GitHub charged to ask.
type RepoStats struct {
	Owner         string    `json:"owner"`
	Name          string    `json:"name"`
	URL           string    `json:"url"`
	Stars         int       `json:"stars"`
	Forks         int       `json:"forks"`
	Watchers      int       `json:"watchers"`
	OpenIssues    int       `json:"openIssues"`
	OpenPRs       int       `json:"openPrs"`
	Release       string    `json:"release,omitempty"`
	ReleasedAt    time.Time `json:"releasedAt,omitzero"`
	Archived      bool      `json:"archived,omitempty"`
	Private       bool      `json:"private,omitempty"`
	PushedAt      time.Time `json:"pushedAt,omitzero"`
	DefaultBranch string    `json:"defaultBranch,omitempty"`
	License       string    `json:"license,omitempty"`
	Description   string    `json:"description,omitempty"`
	Topics        []string  `json:"topics"`
	// Cost is the points GitHub charged for this query, and Remaining the
	// budget left afterwards. The first query whose cost scales with what a
	// person does, so it is measured rather than estimated.
	Cost      int `json:"cost"`
	Remaining int `json:"remaining"`
}

// statsQuery asks one repository for exactly what the card shows.
const statsQuery = `
query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    url stargazerCount forkCount isArchived isPrivate pushedAt description
    watchers { totalCount }
    issues(states: OPEN) { totalCount }
    pullRequests(states: OPEN) { totalCount }
    latestRelease { tagName publishedAt }
    defaultBranchRef { name }
    licenseInfo { spdxId }
    repositoryTopics(first: 6) { nodes { topic { name } } }
  }
  rateLimit { cost remaining }
}`

// RepoStats fetches one repository's statistics.
func (c *Client) RepoStats(ctx context.Context, owner, name string) (RepoStats, error) {
	return c.repoStatsAt(ctx, c.account.GraphQLURL(), owner, name)
}

// repoStatsAt is RepoStats against an explicit endpoint, so tests can stub it.
func (c *Client) repoStatsAt(ctx context.Context, endpoint, owner, name string) (RepoStats, error) {
	body, err := json.Marshal(map[string]any{
		"query":     statsQuery,
		"variables": map[string]string{"owner": owner, "name": name},
	})
	if err != nil {
		return RepoStats{}, fmt.Errorf("encoding query: %w", err)
	}
	req, err := c.newRequest(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return RepoStats{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return RepoStats{}, transportError("repository", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return RepoStats{}, classify("repository", resp)
	}

	var out struct {
		Data struct {
			Repository *struct {
				URL           string    `json:"url"`
				Stars         int       `json:"stargazerCount"`
				Forks         int       `json:"forkCount"`
				Archived      bool      `json:"isArchived"`
				Private       bool      `json:"isPrivate"`
				PushedAt      time.Time `json:"pushedAt"`
				Description   string    `json:"description"`
				Watchers      struct{ TotalCount int }
				Issues        struct{ TotalCount int }
				PullRequests  struct{ TotalCount int }
				LatestRelease *struct {
					TagName     string    `json:"tagName"`
					PublishedAt time.Time `json:"publishedAt"`
				} `json:"latestRelease"`
				DefaultBranchRef *struct{ Name string }   `json:"defaultBranchRef"`
				LicenseInfo      *struct{ SpdxID string } `json:"licenseInfo"`
				RepositoryTopics struct {
					Nodes []struct {
						Topic struct{ Name string } `json:"topic"`
					} `json:"nodes"`
				} `json:"repositoryTopics"`
			} `json:"repository"`
			RateLimit struct {
				Cost      int `json:"cost"`
				Remaining int `json:"remaining"`
			} `json:"rateLimit"`
		} `json:"data"`
		Errors []gqlError `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&out); err != nil {
		return RepoStats{}, fmt.Errorf("decoding repository: %w", err)
	}
	// A renamed, deleted or invisible repository comes back null with a
	// NOT_FOUND error, which classifies as blocked: the token works, this
	// resource does not.
	if out.Data.Repository == nil {
		if len(out.Errors) > 0 {
			return RepoStats{}, classifyGraphQL("repository", out.Errors, resp.Header)
		}
		return RepoStats{}, &APIError{Op: "repository", Status: http.StatusOK,
			Message: owner + "/" + name + " is not visible to this account", Class: ErrBlocked}
	}

	r := out.Data.Repository
	stats := RepoStats{
		Owner: owner, Name: name, URL: r.URL,
		Stars: r.Stars, Forks: r.Forks, Watchers: r.Watchers.TotalCount,
		OpenIssues: r.Issues.TotalCount, OpenPRs: r.PullRequests.TotalCount,
		Archived: r.Archived, Private: r.Private, PushedAt: r.PushedAt,
		Description: r.Description, Topics: []string{},
		Cost: out.Data.RateLimit.Cost, Remaining: out.Data.RateLimit.Remaining,
	}
	if r.LatestRelease != nil {
		stats.Release, stats.ReleasedAt = r.LatestRelease.TagName, r.LatestRelease.PublishedAt
	}
	if r.DefaultBranchRef != nil {
		stats.DefaultBranch = r.DefaultBranchRef.Name
	}
	if r.LicenseInfo != nil {
		stats.License = r.LicenseInfo.SpdxID
	}
	for _, n := range r.RepositoryTopics.Nodes {
		stats.Topics = append(stats.Topics, n.Topic.Name)
	}
	return stats, nil
}

// remoteParts reads the host, owner and name out of a forge remote in its ssh
// or https spelling.
var remoteParts = regexp.MustCompile(`^(?:[a-z+]+://)?(?:[^@/]+@)?([^/:]+)[:/]+([^/]+)/([^/]+?)(?:\.git)?/?$`)

// ParseRemote splits a git remote into host, owner and name. A remote that is
// not a forge url, a filesystem path for one, yields ok false.
func ParseRemote(remote string) (host, owner, name string, ok bool) {
	m := remoteParts.FindStringSubmatch(strings.TrimSpace(remote))
	if m == nil || !strings.Contains(m[1], ".") {
		return "", "", "", false
	}
	return strings.ToLower(m[1]), m[2], m[3], true
}
