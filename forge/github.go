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
	neturl "net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/karamble/omarchy-omagihu/accounts"
)

// maxBody caps how much of a response we will read, so a hostile or broken
// endpoint cannot exhaust memory.
const maxBody = 8 << 20

// The poller acts on the class of a failure and nothing else.
var (
	// ErrUnauthorized means the token was rejected. Retrying will not help,
	// so the account stops.
	ErrUnauthorized = errors.New("token rejected")
	// ErrThrottled means the budget is spent. GitHub names the time it will
	// answer again, and the poller waits for it.
	ErrThrottled = errors.New("rate limited")
	// ErrTransient means GitHub or the network failed. The poller backs off.
	ErrTransient = errors.New("temporary failure")
	// ErrBlocked means the token works but this resource is not accessible:
	// a missing scope, a private repository, a permission GitHub hides
	// behind a 404. The poller reports it and keeps polling.
	ErrBlocked = errors.New("not accessible")
)

// APIError carries what the class alone cannot: the status, and the time
// GitHub said it would answer again.
type APIError struct {
	Op      string
	Status  int
	RetryAt time.Time // zero when GitHub gave no hint
	Message string
	Class   error // one of the sentinels above, returned by Unwrap
	cause   error // the transport error, when there was one
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString(e.Op + ": " + e.Class.Error())
	if e.Status != 0 {
		fmt.Fprintf(&b, " (%d)", e.Status)
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	return b.String()
}

// Unwrap exposes the class to errors.Is, and the transport error behind a
// transient failure when there was one.
func (e *APIError) Unwrap() []error {
	if e.cause != nil {
		return []error{e.Class, e.cause}
	}
	return []error{e.Class}
}

// transportError wraps a failure to reach GitHub at all.
func transportError(op string, err error) *APIError {
	return &APIError{Op: op, Message: err.Error(), Class: ErrTransient, cause: err}
}

// classify turns a response other than 200 into an APIError. Only 401 is
// permanent. A spent budget carries its reset time. 400 and 422 mean the
// request itself is wrong, which backing off cannot fix but stopping would
// hide. Every other 3xx and 4xx, including a 403 with no rate signal and the
// 404 GitHub answers for anything private, is the token working and this
// resource not being reachable. The rest is GitHub's problem and passes.
func classify(op string, resp *http.Response) *APIError {
	e := &APIError{Op: op, Status: resp.StatusCode, Message: apiMessage(resp.Body)}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		e.Class = ErrUnauthorized
	case rateLimited(resp.StatusCode, resp.Header):
		e.Class = ErrThrottled
		e.RetryAt = retryAt(resp.Header, time.Now())
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity:
		e.Class = ErrTransient
		e.Message = "the request was rejected, this is a bug: " + e.Message
	case resp.StatusCode >= 500:
		e.Class = ErrTransient
	case resp.StatusCode >= 300:
		e.Class = ErrBlocked
	default:
		e.Class = ErrTransient
	}
	return e
}

// apiMessage reads the message GitHub puts in an error body, or "" when the
// body is not that shape.
func apiMessage(body io.Reader) string {
	var out struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 64<<10)).Decode(&out); err != nil {
		return ""
	}
	return strings.TrimSpace(out.Message)
}

// retryAt is when GitHub said to try again. Retry-After, which is how a
// secondary limit speaks, wins over the primary limit's reset time.
func retryAt(h http.Header, now time.Time) time.Time {
	if secs, err := strconv.Atoi(h.Get("Retry-After")); err == nil && secs > 0 {
		return now.Add(time.Duration(secs) * time.Second)
	}
	return readRate(h).ResetsAt
}

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

// maxPages caps how many notification pages one Inbox poll follows, so a broken
// endpoint that points at itself cannot make a poll loop forever. 50 per page
// means a full sweep is 5000 notifications, far beyond what any panel shows.
const maxPages = 100

// inboxAt is Inbox against an explicit API root, so tests can point it at a stub.
// It aggregates every page GitHub returns rather than trusting page one: the
// notifications list is paginated and a busy account can outgrow page one.
func (c *Client) inboxAt(ctx context.Context, base string, state InboxState) (items []Notification, next InboxState, unchanged bool, rate Rate, err error) {
	next = state
	url := base + "/notifications?all=false&per_page=50"

	for page := 0; page < maxPages; page++ {
		req, reqErr := c.newRequest(ctx, http.MethodGet, url, nil)
		if reqErr != nil {
			return nil, state, false, Rate{}, reqErr
		}
		// Only the first request of a poll carries the conditional validator: it
		// covers the list as a whole. Page 2+ are already known-stale (page 1
		// answered 200), so sending the validator again would risk a confusing
		// per-page 304.
		if page == 0 && state.LastModified != "" {
			req.Header.Set("If-Modified-Since", state.LastModified)
		}

		resp, doErr := c.http.Do(req)
		if doErr != nil {
			return nil, state, false, Rate{}, transportError("notifications", doErr)
		}

		if v := resp.Header.Get("X-Poll-Interval"); v != "" {
			if secs, convErr := strconv.Atoi(v); convErr == nil && secs > 0 {
				next.PollInterval = time.Duration(secs) * time.Second
			}
		}
		rate = readRate(resp.Header)

		switch resp.StatusCode {
		case http.StatusNotModified:
			// The conditional validator covers the list as a whole, so a 304 on
			// the first request means nothing changed: keep the existing list.
			resp.Body.Close()
			return nil, next, true, rate, nil
		case http.StatusOK:
		default:
			// next, not state: GitHub sends X-Poll-Interval precisely on a
			// throttled answer to say how long to wait, and it was read above.
			apiErr := classify("notifications", resp)
			resp.Body.Close()
			return nil, next, false, rate, apiErr
		}

		// Last-Modified is a property of the whole list, taken from the first
		// (validated) request; page 2+ must not clobber it.
		if page == 0 {
			if v := resp.Header.Get("Last-Modified"); v != "" {
				next.LastModified = v
			}
		}

		pageItems, decErr := decodeNotifications(resp.Body, c.account.ID)
		resp.Body.Close()
		if decErr != nil {
			return nil, state, false, rate, fmt.Errorf("decoding notifications: %w", decErr)
		}
		items = append(items, pageItems...)

		url = nextLink(resp.Header.Get("Link"), base)
		if url == "" || page+1 >= maxPages {
			return items, next, false, rate, nil
		}
	}
	return items, next, false, rate, nil
}

// decodeNotifications parses one page of the notifications endpoint into
// Notification values owned by the given account.
func decodeNotifications(body io.Reader, accountID string) ([]Notification, error) {
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
	if err := json.NewDecoder(io.LimitReader(body, maxBody)).Decode(&raw); err != nil {
		return nil, err
	}
	items := make([]Notification, 0, len(raw))
	for _, n := range raw {
		items = append(items, Notification{
			AccountID:  accountID,
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
	return items, nil
}

// nextLink returns the URL to ask for the next page (the rel="next" entry of
// the Link header), or "" on the last page.
//
// It is the only place a response decides where the next request goes, and
// newRequest puts the bearer token on whatever it is handed, so the answer is
// held to the scheme and host already being talked to. Anything else ends the
// sweep: a short list is a better failure than an authenticated request
// somewhere nobody chose.
func nextLink(header, base string) string {
	want, err := neturl.Parse(base)
	if err != nil {
		return ""
	}
	for _, part := range strings.Split(header, ",") {
		link, params, found := strings.Cut(part, ";")
		if !found {
			continue
		}
		if !strings.Contains(params, `rel="next"`) && !strings.Contains(params, "rel=next") {
			continue
		}
		raw := strings.Trim(strings.TrimSpace(link), "<> ")
		got, err := neturl.Parse(raw)
		if err != nil || got.Scheme != want.Scheme || got.Host != want.Host {
			return ""
		}
		return raw
	}
	return ""
}

// rateLimited reports whether a refusal is the quota running out rather than
// the token being wrong. GitHub answers 403 for both, and also for a missing
// scope, for SSO enforcement and for a suspended token, none of which clear on
// their own. Those are classified as blocked: reported on the account every
// cycle and polled on, rather than mistaken for a spent budget with a reset
// time to wait for.
// Reads the headers rather than the parsed Rate on purpose: readRate turns a
// missing X-RateLimit-Remaining into 0 through Atoi, so "we were not told"
// would be indistinguishable from "the budget is spent" and every 403 would
// look throttled again. Only a header that says so counts, plus Retry-After,
// which is how a secondary limit announces itself while Remaining is still
// positive.
func rateLimited(status int, h http.Header) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	if status != http.StatusForbidden {
		return false
	}
	return h.Get("X-RateLimit-Remaining") == "0" || h.Get("Retry-After") != ""
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
	// BaseRef is the branch the pull request merges into. A distance to the
	// base is only honest when it was measured against this branch, so the
	// correlation checks it rather than assuming the default.
	BaseRef string `json:"baseRef,omitempty"`
	// HeadSHA is the tip of the PR branch on the forge. Comparing it with a
	// local HEAD is what makes "CI is red on the commit you have checked out"
	// exact rather than a guess.
	HeadSHA        string    `json:"headSha,omitempty"`
	MergedAt       time.Time `json:"mergedAt,omitzero"`
	IsDraft        bool      `json:"isDraft"`
	ReviewDecision string    `json:"reviewDecision,omitempty"`
	ChecksState    string    `json:"checksState,omitempty"`
	UpdatedAt      time.Time `json:"updatedAt,omitzero"`
	// Incoming marks a pull request somebody else opened on a repository the
	// account owns, as opposed to one whose review was actually requested.
	// Both wait on the same person, so they share a list, but only one of them
	// is a request and saying otherwise in a notification is a small lie.
	Incoming bool `json:"incoming,omitempty"`
}

// Label is one label on an issue. The colour is GitHub's own six-character hex
// for that label, carried through so the panel can paint each one as its
// repository defines it rather than inventing a palette.
type Label struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Issue is one issue the account is assigned to, opened, or was handed by
// somebody else on a repository it owns.
type Issue struct {
	AccountID string    `json:"accountId"`
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	Author    string    `json:"author,omitempty"`
	Labels    []Label   `json:"labels,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
	// Incoming marks an issue somebody else opened on a repository the
	// account owns. Assigning needs triage permission, so a reporter cannot
	// put themselves on your radar; this is how they get there.
	Incoming bool `json:"incoming,omitempty"`
}

// Workload is everything the account currently owes or is owed.
type Workload struct {
	Login string `json:"login"`
	// Organizations are the logins of the organisations the account belongs
	// to, as far as the token can see. OrgsHidden is set when the token's
	// scopes say the list is incomplete, so an absent organisation is not
	// mistaken for not being a member.
	Organizations  []string      `json:"organizations"`
	OrgsHidden     bool          `json:"orgsHidden,omitempty"`
	AuthoredPRs    []PullRequest `json:"authoredPrs"`
	ReviewRequests []PullRequest `json:"reviewRequests"`
	// AssignedIssues are the issues waiting on you: assigned to you, or
	// opened by somebody else on a repository you own, marked Incoming.
	AssignedIssues []Issue `json:"assignedIssues"`
	// AuthoredIssues are the issues you opened. A submission under review, a
	// bug you filed upstream: the labels on them are where their state lives.
	AuthoredIssues []Issue `json:"authoredIssues"`
	// MergedPRs are recently merged pull requests of yours. They are what makes
	// a finished local branch identifiable as finished.
	MergedPRs []PullRequest `json:"mergedPrs"`
	// Warnings are the errors GitHub sent alongside the data, and Unresolved
	// names the lists those errors nulled, by their JSON names above. A
	// caller keeps whatever it already had for an unresolved list rather
	// than reading the empty one here as the truth.
	Warnings   []string `json:"warnings,omitempty"`
	Unresolved []string `json:"unresolved,omitempty"`
}

// Resolved reports whether the named list came back from GitHub this time.
func (w Workload) Resolved(list string) bool {
	return !slices.Contains(w.Unresolved, list)
}

// workloadQuery asks for the three attention lists plus the head-commit check
// rollup in a single round trip. Per-repository polling would need hundreds.
const workloadQuery = `
query {
  viewer { login }
  orgs: viewer { organizations(first: 100) { nodes { login } } }
  authored: search(query: "is:open is:pr author:@me archived:false", type: ISSUE, first: 50) {
    nodes { ...prFields }
  }
  reviewing: search(query: "is:open is:pr review-requested:@me archived:false", type: ISSUE, first: 50) {
    nodes { ...prFields }
  }
  incoming: search(query: "is:open is:pr user:@me -author:@me archived:false", type: ISSUE, first: 50) {
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
  incomingIssues: search(query: "is:open is:issue user:@me -author:@me archived:false", type: ISSUE, first: 50) {
    nodes { ...issueFields }
  }
}

fragment issueFields on Issue {
  number title url updatedAt
  author { login }
  repository { nameWithOwner }
  labels(first: 10) { nodes { name color } }
}

fragment prFields on PullRequest {
  number title url updatedAt isDraft headRefName headRefOid baseRefName reviewDecision mergedAt
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
	BaseRefName    string    `json:"baseRefName"`
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
		return Workload{}, Rate{}, transportError("workload", err)
	}
	defer resp.Body.Close()

	rate := readRate(resp.Header)
	if resp.StatusCode != http.StatusOK {
		return Workload{}, rate, classify("workload", resp)
	}

	// Data is kept raw so a response carrying both data and errors can be
	// told from one carrying errors alone.
	var out struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&out); err != nil {
		return Workload{}, rate, fmt.Errorf("decoding workload: %w", err)
	}
	if len(out.Errors) > 0 && !hasData(out.Data) {
		return Workload{}, rate, classifyGraphQL("workload", out.Errors, resp.Header)
	}
	var data gqlWorkload
	var fields map[string]json.RawMessage
	if hasData(out.Data) {
		if err := json.Unmarshal(out.Data, &data); err != nil {
			return Workload{}, rate, fmt.Errorf("decoding workload: %w", err)
		}
		if err := json.Unmarshal(out.Data, &fields); err != nil {
			return Workload{}, rate, fmt.Errorf("decoding workload: %w", err)
		}
	}
	// Partial data is used only when every error can be pinned to a list.
	// Otherwise nothing in the answer can be trusted, and the previous
	// answer is better than this one.
	unresolved, attributed := unresolvedLists(fields, out.Errors)
	if !attributed {
		return Workload{}, rate, classifyGraphQL("workload", out.Errors, resp.Header)
	}

	work := Workload{Login: data.Viewer.Login, Unresolved: unresolved, Organizations: []string{}}
	for _, o := range data.Orgs.Organizations.Nodes {
		work.Organizations = append(work.Organizations, o.Login)
	}
	work.OrgsHidden = orgsHidden(resp.Header)
	for _, e := range out.Errors {
		work.Warnings = append(work.Warnings, e.Message)
	}
	for _, n := range data.Authored.Nodes {
		work.AuthoredPRs = append(work.AuthoredPRs, c.toPR(n))
	}
	seen := make(map[string]struct{})
	for _, n := range data.Reviewing.Nodes {
		pr := c.toPR(n)
		seen[pr.URL] = struct{}{}
		work.ReviewRequests = append(work.ReviewRequests, pr)
	}
	// Somebody else's pull request on a repository you own. GitHub will never
	// put these in review-requested: setting a reviewer needs write access, so
	// an outside contributor cannot ask, and without a CODEOWNERS file nobody
	// asks on their behalf. For a solo maintainer that is the whole of the
	// inbound work, and it was invisible.
	for _, n := range data.Incoming.Nodes {
		pr := c.toPR(n)
		if _, already := seen[pr.URL]; already {
			continue
		}
		if !waitingOnOwner(pr) {
			continue
		}
		pr.Incoming = true
		seen[pr.URL] = struct{}{}
		work.ReviewRequests = append(work.ReviewRequests, pr)
	}
	for _, n := range data.Merged.Nodes {
		work.MergedPRs = append(work.MergedPRs, c.toPR(n))
	}
	seenIssue := make(map[string]struct{})
	for _, n := range data.Assigned.Nodes {
		issue := c.toIssue(n)
		seenIssue[issue.URL] = struct{}{}
		work.AssignedIssues = append(work.AssignedIssues, issue)
	}
	// Somebody else's issue on a repository you own. Assigning needs triage
	// permission, so a reporter cannot appear through assignee:@me any more
	// than a contributor can through review-requested:@me. An issue already
	// assigned to you is listed as that: somebody triaged it, which is the
	// more specific state.
	for _, n := range data.IncomingIssues.Nodes {
		issue := c.toIssue(n)
		if _, already := seenIssue[issue.URL]; already {
			continue
		}
		issue.Incoming = true
		seenIssue[issue.URL] = struct{}{}
		work.AssignedIssues = append(work.AssignedIssues, issue)
	}
	for _, n := range data.AuthoredIssues.Nodes {
		work.AuthoredIssues = append(work.AuthoredIssues, c.toIssue(n))
	}
	return work, rate, nil
}

// gqlWorkload is the shape of the data field of the workload query.
type gqlWorkload struct {
	Viewer struct {
		Login string `json:"login"`
	} `json:"viewer"`
	Orgs struct {
		Organizations struct {
			Nodes []struct {
				Login string `json:"login"`
			} `json:"nodes"`
		} `json:"organizations"`
	} `json:"orgs"`
	Authored       struct{ Nodes []gqlPR }    `json:"authored"`
	Reviewing      struct{ Nodes []gqlPR }    `json:"reviewing"`
	Incoming       struct{ Nodes []gqlPR }    `json:"incoming"`
	Merged         struct{ Nodes []gqlPR }    `json:"merged"`
	Assigned       struct{ Nodes []gqlIssue } `json:"assigned"`
	AuthoredIssues struct{ Nodes []gqlIssue } `json:"authoredIssues"`
	IncomingIssues struct{ Nodes []gqlIssue } `json:"incomingIssues"`
}

// gqlError is one entry of a GraphQL errors array. Type is how GitHub says
// what went wrong; a schema error has none. Path leads to the field the
// error nulled, starting with one of the query's top-level names.
type gqlError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    []any  `json:"path"`
}

// orgsHidden reports whether the token cannot see private organisation
// memberships. A classic token announces its scopes in X-OAuth-Scopes and
// needs read:org, or write:org or admin:org which contain it, for the full
// list. An answer carrying no scopes at all cannot be judged and is trusted:
// that is a fine-grained token or a GitHub App, but also anything that
// strips the header on the way back.
func orgsHidden(h http.Header) bool {
	scopes := h.Get("X-OAuth-Scopes")
	if scopes == "" {
		return false
	}
	for _, s := range strings.Split(scopes, ",") {
		switch strings.TrimSpace(s) {
		case "read:org", "admin:org", "write:org":
			return false
		}
	}
	return true
}

// hasData reports whether a GraphQL data field holds anything to decode.
func hasData(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// workloadLists maps each top-level field of the query to the Workload list
// it feeds, by JSON name. reviewing and incoming both feed reviewRequests;
// assigned and incomingIssues both feed assignedIssues.
var workloadLists = map[string]string{
	"viewer":         "login",
	"orgs":           "organizations",
	"authored":       "authoredPrs",
	"reviewing":      "reviewRequests",
	"incoming":       "reviewRequests",
	"merged":         "mergedPrs",
	"assigned":       "assignedIssues",
	"authoredIssues": "authoredIssues",
	"incomingIssues": "assignedIssues",
}

// unresolvedLists names the lists the answer did not deliver: a top-level
// field that is null or absent, or one an error's path points into. It
// reports false when an error cannot be pinned to any list, because then no
// part of the data is known to be whole.
func unresolvedLists(fields map[string]json.RawMessage, errs []gqlError) ([]string, bool) {
	var out []string
	add := func(list string) {
		if !slices.Contains(out, list) {
			out = append(out, list)
		}
	}
	nulled := false
	for field, list := range workloadLists {
		if raw, ok := fields[field]; !ok || string(raw) == "null" {
			add(list)
			nulled = true
		}
	}
	for _, e := range errs {
		if len(e.Path) == 0 {
			// No path: the nulled field says what failed, if there is one.
			if !nulled {
				return nil, false
			}
			continue
		}
		top, _ := e.Path[0].(string)
		list, known := workloadLists[top]
		if !known {
			return nil, false
		}
		add(list)
	}
	slices.Sort(out)
	return out, true
}

// classifyGraphQL maps the typed errors a 200 can carry. A primary rate limit
// arrives this way, as HTTP 200 with RATE_LIMITED and the budget headers, and
// it wins over anything else in the array. Access failures are blocked, and
// an untyped error is a schema problem, which is ours.
func classifyGraphQL(op string, errs []gqlError, h http.Header) *APIError {
	e := &APIError{Op: op, Status: http.StatusOK, Message: errs[0].Message, Class: ErrTransient}
	for _, ge := range errs {
		switch ge.Type {
		case "RATE_LIMITED":
			e.Class = ErrThrottled
			e.RetryAt = retryAt(h, time.Now())
			e.Message = ge.Message
			return e
		case "FORBIDDEN", "NOT_FOUND", "INSUFFICIENT_SCOPES":
			e.Class = ErrBlocked
			e.Message = ge.Message
		}
	}
	return e
}

// gqlIssue is the issueFields fragment as it comes back, shared by both issue
// searches so the two cannot decode differently.
type gqlIssue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	UpdatedAt time.Time `json:"updatedAt"`
	Author    struct {
		Login string `json:"login"`
	} `json:"author"`
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
		Author:    n.Author.Login,
		Labels:    n.Labels.Nodes,
		UpdatedAt: n.UpdatedAt,
	}
}

// waitingOnOwner reports whether an incoming pull request still needs the
// maintainer. A draft says the contributor is not finished, and a review that
// has already been given puts the ball back in their court; neither should
// hold the badge open. A pull request nobody has reviewed carries no decision
// at all, which is the case this exists for.
func waitingOnOwner(pr PullRequest) bool {
	if pr.IsDraft {
		return false
	}
	switch pr.ReviewDecision {
	case "APPROVED", "CHANGES_REQUESTED":
		return false
	}
	return true
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
		BaseRef:        n.BaseRefName,
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
