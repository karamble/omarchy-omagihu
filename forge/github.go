// Package forge talks to GitHub on behalf of one account. Everything here is
// account scoped and read only: the cost of a poll cycle is a fixed handful of
// requests no matter how many repositories the account can see.
package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/karamble/omarchy-omagihu/accounts"
)

// maxBody caps how much of a response we will read, so a hostile or broken
// endpoint cannot exhaust memory.
const maxBody = 8 << 20

// ErrUnauthorized means the token was rejected. The poller stops retrying on
// its normal cadence when it sees this, because retrying will not help.
var ErrUnauthorized = errors.New("token rejected")

// Client is a read-only GitHub client for a single account.
type Client struct {
	account accounts.Account
	http    *http.Client
	agent   string
}

// New builds a client. The account's Token is held in memory only.
//
// The transport is spelled out rather than left to http.DefaultTransport,
// which proxies according to HTTP_PROXY and friends. This client carries a
// GitHub token, so where it connects should be decided here and not by an
// environment variable: Proxy is nil, meaning never proxy.
func New(account accounts.Account, version string) *Client {
	return &Client{
		account: account,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				Proxy:                 nil,
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 20 * time.Second,
				MaxIdleConns:          10,
				IdleConnTimeout:       90 * time.Second,
			},
		},
		agent: "omagihu/" + version,
	}
}

// Account reports which identity this client speaks for.
func (c *Client) Account() accounts.Account { return c.account }

// Rate is what the response headers said about the remaining budget.
type Rate struct {
	Remaining int       `json:"remaining"`
	Limit     int       `json:"limit"`
	ResetsAt  time.Time `json:"resetsAt,omitzero"`
}

// Notification is one entry from the account's inbox.
type Notification struct {
	AccountID  string    `json:"accountId"`
	ID         string    `json:"id"`
	Repo       string    `json:"repo"`
	Type       string    `json:"type"`
	Title      string    `json:"title"`
	Reason     string    `json:"reason"`
	Unread     bool      `json:"unread"`
	UpdatedAt  time.Time `json:"updatedAt,omitzero"`
	SubjectURL string    `json:"subjectUrl"`
	WebURL     string    `json:"webUrl"`
}

// InboxState carries the conditional-request bookkeeping between polls. A zero
// value is a valid first poll.
type InboxState struct {
	LastModified string
	PollInterval time.Duration
}

// Inbox fetches unread notifications. When GitHub answers 304 the previous list
// still stands and the call costs nothing against the rate limit, which is what
// makes a 60 second cadence affordable.
func (c *Client) Inbox(ctx context.Context, state InboxState) (items []Notification, next InboxState, unchanged bool, rate Rate, err error) {
	return c.inboxAt(ctx, c.account.APIBase(), state)
}

// inboxAt is Inbox against an explicit API root, so tests can point it at a stub.
func (c *Client) inboxAt(ctx context.Context, base string, state InboxState) (items []Notification, next InboxState, unchanged bool, rate Rate, err error) {
	req, err := c.newRequest(ctx, http.MethodGet, base+"/notifications?all=false", nil)
	if err != nil {
		return nil, state, false, Rate{}, err
	}
	if state.LastModified != "" {
		req.Header.Set("If-Modified-Since", state.LastModified)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, state, false, Rate{}, fmt.Errorf("fetching notifications: %w", err)
	}
	defer resp.Body.Close()

	next = state
	if v := resp.Header.Get("X-Poll-Interval"); v != "" {
		if secs, convErr := strconv.Atoi(v); convErr == nil && secs > 0 {
			next.PollInterval = time.Duration(secs) * time.Second
		}
	}
	rate = readRate(resp.Header)

	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, next, true, rate, nil
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, next, false, rate, fmt.Errorf("notifications: %w (%s)", ErrUnauthorized, resp.Status)
	default:
		return nil, next, false, rate, fmt.Errorf("notifications returned %s", resp.Status)
	}

	if v := resp.Header.Get("Last-Modified"); v != "" {
		next.LastModified = v
	}

	var raw []struct {
		ID         string    `json:"id"`
		Reason     string    `json:"reason"`
		Unread     bool      `json:"unread"`
		UpdatedAt  time.Time `json:"updated_at"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Subject struct {
			Title string `json:"title"`
			Type  string `json:"type"`
			URL   string `json:"url"`
		} `json:"subject"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&raw); err != nil {
		return nil, next, false, rate, fmt.Errorf("decoding notifications: %w", err)
	}

	items = make([]Notification, 0, len(raw))
	for _, n := range raw {
		items = append(items, Notification{
			AccountID:  c.account.ID,
			ID:         n.ID,
			Repo:       n.Repository.FullName,
			Type:       n.Subject.Type,
			Title:      n.Subject.Title,
			Reason:     n.Reason,
			Unread:     n.Unread,
			UpdatedAt:  n.UpdatedAt,
			SubjectURL: n.Subject.URL,
			WebURL:     webURL(n.Subject.URL),
		})
	}
	return items, next, false, rate, nil
}

// PullRequest is one of the account's own PRs or one awaiting its review.
type PullRequest struct {
	AccountID string `json:"accountId"`
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Author    string `json:"author"`
	HeadRef   string `json:"headRef"`
	// HeadSHA is the tip of the PR branch on the forge. Comparing it with a
	// local HEAD is what makes "CI is red on the commit you have checked out"
	// exact rather than a guess.
	HeadSHA        string    `json:"headSha,omitempty"`
	MergedAt       time.Time `json:"mergedAt,omitzero"`
	IsDraft        bool      `json:"isDraft"`
	ReviewDecision string    `json:"reviewDecision,omitempty"`
	ChecksState    string    `json:"checksState,omitempty"`
	UpdatedAt      time.Time `json:"updatedAt,omitzero"`
}

// Label is one label on an issue. The colour is GitHub's own six-character hex
// for that label, carried through so the panel can paint each one as its
// repository defines it rather than inventing a palette.
type Label struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Issue is one issue the account is assigned to or opened.
type Issue struct {
	AccountID string    `json:"accountId"`
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	Labels    []Label   `json:"labels,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
}

// Workload is everything the account currently owes or is owed.
type Workload struct {
	Login          string        `json:"login"`
	AuthoredPRs    []PullRequest `json:"authoredPrs"`
	ReviewRequests []PullRequest `json:"reviewRequests"`
	AssignedIssues []Issue       `json:"assignedIssues"`
	// AuthoredIssues are the issues you opened. A submission under review, a
	// bug you filed upstream: the labels on them are where their state lives.
	AuthoredIssues []Issue `json:"authoredIssues"`
	// MergedPRs are recently merged pull requests of yours. They are what makes
	// a finished local branch identifiable as finished.
	MergedPRs []PullRequest `json:"mergedPrs"`
}

// workloadQuery asks for the three attention lists plus the head-commit check
// rollup in a single round trip. Per-repository polling would need hundreds.
const workloadQuery = `
query {
  viewer { login }
  authored: search(query: "is:open is:pr author:@me archived:false", type: ISSUE, first: 50) {
    nodes { ...prFields }
  }
  reviewing: search(query: "is:open is:pr review-requested:@me archived:false", type: ISSUE, first: 50) {
    nodes { ...prFields }
  }
  merged: search(query: "is:merged is:pr author:@me", type: ISSUE, first: 30) {
    nodes { ...prFields }
  }
  assigned: search(query: "is:open is:issue assignee:@me archived:false", type: ISSUE, first: 50) {
    nodes { ...issueFields }
  }
  authoredIssues: search(query: "is:open is:issue author:@me archived:false", type: ISSUE, first: 50) {
    nodes { ...issueFields }
  }
}

fragment issueFields on Issue {
  number title url updatedAt
  repository { nameWithOwner }
  labels(first: 10) { nodes { name color } }
}

fragment prFields on PullRequest {
  number title url updatedAt isDraft headRefName headRefOid reviewDecision mergedAt
  author { login }
  repository { nameWithOwner }
  commits(last: 1) {
    nodes { commit { statusCheckRollup { state } } }
  }
}`

type gqlPR struct {
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	URL            string    `json:"url"`
	UpdatedAt      time.Time `json:"updatedAt"`
	IsDraft        bool      `json:"isDraft"`
	HeadRefName    string    `json:"headRefName"`
	HeadRefOid     string    `json:"headRefOid"`
	MergedAt       time.Time `json:"mergedAt"`
	ReviewDecision string    `json:"reviewDecision"`
	Author         struct {
		Login string `json:"login"`
	} `json:"author"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// Work runs the batched attention query.
func (c *Client) Work(ctx context.Context) (Workload, Rate, error) {
	return c.workAt(ctx, c.account.GraphQLURL())
}

// workAt is Work against an explicit GraphQL endpoint, so tests can stub it.
func (c *Client) workAt(ctx context.Context, endpoint string) (Workload, Rate, error) {
	body, err := json.Marshal(map[string]string{"query": workloadQuery})
	if err != nil {
		return Workload{}, Rate{}, fmt.Errorf("encoding query: %w", err)
	}
	req, err := c.newRequest(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Workload{}, Rate{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return Workload{}, Rate{}, fmt.Errorf("running workload query: %w", err)
	}
	defer resp.Body.Close()

	rate := readRate(resp.Header)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return Workload{}, rate, fmt.Errorf("workload: %w (%s)", ErrUnauthorized, resp.Status)
	default:
		return Workload{}, rate, fmt.Errorf("workload query returned %s", resp.Status)
	}

	var out struct {
		Data struct {
			Viewer struct {
				Login string `json:"login"`
			} `json:"viewer"`
			Authored       struct{ Nodes []gqlPR }    `json:"authored"`
			Reviewing      struct{ Nodes []gqlPR }    `json:"reviewing"`
			Merged         struct{ Nodes []gqlPR }    `json:"merged"`
			Assigned       struct{ Nodes []gqlIssue } `json:"assigned"`
			AuthoredIssues struct{ Nodes []gqlIssue } `json:"authoredIssues"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&out); err != nil {
		return Workload{}, rate, fmt.Errorf("decoding workload: %w", err)
	}
	if len(out.Errors) > 0 {
		return Workload{}, rate, fmt.Errorf("workload query: %s", out.Errors[0].Message)
	}

	work := Workload{Login: out.Data.Viewer.Login}
	for _, n := range out.Data.Authored.Nodes {
		work.AuthoredPRs = append(work.AuthoredPRs, c.toPR(n))
	}
	for _, n := range out.Data.Reviewing.Nodes {
		work.ReviewRequests = append(work.ReviewRequests, c.toPR(n))
	}
	for _, n := range out.Data.Merged.Nodes {
		work.MergedPRs = append(work.MergedPRs, c.toPR(n))
	}
	for _, n := range out.Data.Assigned.Nodes {
		work.AssignedIssues = append(work.AssignedIssues, c.toIssue(n))
	}
	for _, n := range out.Data.AuthoredIssues.Nodes {
		work.AuthoredIssues = append(work.AuthoredIssues, c.toIssue(n))
	}
	return work, rate, nil
}

// gqlIssue is the issueFields fragment as it comes back, shared by both issue
// searches so the two cannot decode differently.
type gqlIssue struct {
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	URL        string    `json:"url"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Labels struct {
		Nodes []Label `json:"nodes"`
	} `json:"labels"`
}

// toIssue converts one decoded node, the way toPR does for pull requests.
func (c *Client) toIssue(n gqlIssue) Issue {
	return Issue{
		AccountID: c.account.ID,
		Repo:      n.Repository.NameWithOwner,
		Number:    n.Number,
		Title:     n.Title,
		URL:       n.URL,
		Labels:    n.Labels.Nodes,
		UpdatedAt: n.UpdatedAt,
	}
}

func (c *Client) toPR(n gqlPR) PullRequest {
	pr := PullRequest{
		AccountID:      c.account.ID,
		Repo:           n.Repository.NameWithOwner,
		Number:         n.Number,
		Title:          n.Title,
		URL:            n.URL,
		Author:         n.Author.Login,
		HeadRef:        n.HeadRefName,
		HeadSHA:        n.HeadRefOid,
		MergedAt:       n.MergedAt,
		IsDraft:        n.IsDraft,
		ReviewDecision: n.ReviewDecision,
		UpdatedAt:      n.UpdatedAt,
	}
	if nodes := n.Commits.Nodes; len(nodes) > 0 {
		pr.ChecksState = nodes[0].Commit.StatusCheckRollup.State
	}
	return pr
}

func (c *Client) newRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("building request to %s: %w", url, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.account.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.agent)
	return req, nil
}

func readRate(h http.Header) Rate {
	var r Rate
	r.Remaining, _ = strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	r.Limit, _ = strconv.Atoi(h.Get("X-RateLimit-Limit"))
	if v := h.Get("X-RateLimit-Reset"); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			r.ResetsAt = time.Unix(secs, 0)
		}
	}
	return r
}

// webURL turns an API subject URL into the browser URL a person can open.
// Subject URLs are shaped owner/repo/kind/number, so only the kind segment is
// rewritten: a repository actually named "pulls" must survive untouched.
func webURL(apiURL string) string {
	rest, ok := strings.CutPrefix(apiURL, "https://api.github.com/repos/")
	if !ok {
		return apiURL
	}
	parts := strings.Split(rest, "/")
	if len(parts) >= 4 && parts[2] == "pulls" {
		parts[2] = "pull"
	}
	return "https://github.com/" + strings.Join(parts, "/")
}
