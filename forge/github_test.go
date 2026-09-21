package forge

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/karamble/omarchy-omagihu/accounts"
)

// testClient points a Client at a stub server standing in for GitHub.
func testClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := New(accounts.Account{
		ID:    "test@stub",
		Login: "tester",
		Host:  "stub.invalid",
		Token: "token",
	}, "test")
	// Route every request at the stub instead of the real host.
	c.http = srv.Client()
	return c
}

func TestWebURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "issue",
			in:   "https://api.github.com/repos/o/r/issues/5703",
			want: "https://github.com/o/r/issues/5703",
		},
		{
			name: "pull request loses the plural",
			in:   "https://api.github.com/repos/o/r/pulls/784",
			want: "https://github.com/o/r/pull/784",
		},
		{
			name: "repo named pulls is left alone",
			in:   "https://api.github.com/repos/o/pulls/issues/1",
			want: "https://github.com/o/pulls/issues/1",
		},
		{
			name: "unrecognised url passes through",
			in:   "https://example.org/thing",
			want: "https://example.org/thing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := webURL(tt.in); got != tt.want {
				t.Errorf("webURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestReadRate(t *testing.T) {
	reset := time.Now().Add(time.Hour).Unix()
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", "4321")
	h.Set("X-RateLimit-Limit", "5000")
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))

	got := readRate(h)
	if got.Remaining != 4321 || got.Limit != 5000 {
		t.Errorf("readRate() = %+v, want remaining 4321 limit 5000", got)
	}
	if !got.ResetsAt.Equal(time.Unix(reset, 0)) {
		t.Errorf("ResetsAt = %v, want %v", got.ResetsAt, time.Unix(reset, 0))
	}

	// Missing headers must not panic or invent values.
	if empty := readRate(http.Header{}); empty.Remaining != 0 || !empty.ResetsAt.IsZero() {
		t.Errorf("readRate(empty) = %+v, want zero value", empty)
	}
}

// TestInboxConditionalRequest is the rate-discipline guarantee: after a first
// fetch the client must send If-Modified-Since, and a 304 must be reported as
// unchanged rather than as an empty inbox.
func TestInboxConditionalRequest(t *testing.T) {
	const lastModified = "Tue, 09 Sep 2026 10:00:00 GMT"

	var sawIfModifiedSince string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		sawIfModifiedSince = r.Header.Get("If-Modified-Since")
		w.Header().Set("X-Poll-Interval", "90")
		w.Header().Set("X-RateLimit-Remaining", "4999")

		if sawIfModifiedSince == lastModified {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Last-Modified", lastModified)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{
			"id":"1","reason":"author","unread":true,
			"updated_at":"2026-09-08T15:43:11Z",
			"repository":{"full_name":"o/r"},
			"subject":{"title":"a thing","type":"Issue","url":"https://api.github.com/repos/o/r/issues/1"}
		}]`))
	}))
	defer srv.Close()

	c := testClient(t, srv)
	base := srv.URL

	// First poll: no validator, real payload.
	items, state, unchanged, rate, err := c.inboxAt(t.Context(), base, InboxState{})
	if err != nil {
		t.Fatalf("first Inbox: %v", err)
	}
	if unchanged {
		t.Error("first poll reported unchanged, want a fetch")
	}
	if len(items) != 1 || items[0].Repo != "o/r" || items[0].Title != "a thing" {
		t.Fatalf("first poll items = %+v, want one decoded notification", items)
	}
	if items[0].WebURL != "https://github.com/o/r/issues/1" {
		t.Errorf("WebURL = %q, want the browser url", items[0].WebURL)
	}
	if items[0].AccountID != "test@stub" {
		t.Errorf("AccountID = %q, want the owning account", items[0].AccountID)
	}
	if state.LastModified != lastModified {
		t.Errorf("state.LastModified = %q, want %q", state.LastModified, lastModified)
	}
	if state.PollInterval != 90*time.Second {
		t.Errorf("state.PollInterval = %v, want 90s from X-Poll-Interval", state.PollInterval)
	}
	if rate.Remaining != 4999 {
		t.Errorf("rate.Remaining = %d, want 4999", rate.Remaining)
	}

	// Second poll: the validator must be sent, and 304 means unchanged.
	items, state, unchanged, _, err = c.inboxAt(t.Context(), base, state)
	if err != nil {
		t.Fatalf("second Inbox: %v", err)
	}
	if !unchanged {
		t.Error("second poll reported changed, want unchanged from 304")
	}
	if items != nil {
		t.Errorf("second poll items = %+v, want nil so the caller keeps the previous list", items)
	}
	if sawIfModifiedSince != lastModified {
		t.Errorf("If-Modified-Since = %q, want %q", sawIfModifiedSince, lastModified)
	}
	if state.LastModified != lastModified {
		t.Errorf("state.LastModified = %q after 304, want it preserved", state.LastModified)
	}
	if calls != 2 {
		t.Errorf("server saw %d calls, want 2", calls)
	}
}

func TestInboxUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := testClient(t, srv)
	_, _, _, _, err := c.inboxAt(t.Context(), srv.URL, InboxState{})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Inbox error = %v, want ErrUnauthorized", err)
	}
}

func TestWorkDecodesChecksAndReviewDecision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{
			"viewer":{"login":"tester"},
			"authored":{"nodes":[{
				"number":767,"title":"a pr","url":"https://github.com/o/r/pull/767",
				"updatedAt":"2026-09-06T22:53:03Z","isDraft":false,
				"headRefName":"topic","reviewDecision":"APPROVED",
				"author":{"login":"tester"},
				"repository":{"nameWithOwner":"o/r"},
				"commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"FAILURE"}}}]}
			}]},
			"reviewing":{"nodes":[]},
			"assigned":{"nodes":[{
				"number":12,"title":"an issue","url":"https://github.com/o/r/issues/12",
				"updatedAt":"2026-09-01T00:00:00Z","repository":{"nameWithOwner":"o/r"}
			}]}
		}}`))
	}))
	defer srv.Close()

	c := testClient(t, srv)
	work, _, err := c.workAt(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	if work.Login != "tester" {
		t.Errorf("Login = %q, want tester", work.Login)
	}
	if len(work.AuthoredPRs) != 1 {
		t.Fatalf("AuthoredPRs = %+v, want one", work.AuthoredPRs)
	}
	pr := work.AuthoredPRs[0]
	if pr.ReviewDecision != "APPROVED" {
		t.Errorf("ReviewDecision = %q, want APPROVED", pr.ReviewDecision)
	}
	if pr.ChecksState != "FAILURE" {
		t.Errorf("ChecksState = %q, want FAILURE", pr.ChecksState)
	}
	if pr.Repo != "o/r" || pr.Number != 767 {
		t.Errorf("PR identity = %s#%d, want o/r#767", pr.Repo, pr.Number)
	}
	if len(work.AssignedIssues) != 1 || work.AssignedIssues[0].Number != 12 {
		t.Errorf("AssignedIssues = %+v, want issue 12", work.AssignedIssues)
	}
}

func TestWorkSurfacesGraphQLErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"errors":[{"message":"Field 'weeks' doesn't accept argument 'last'"}]}`))
	}))
	defer srv.Close()

	c := testClient(t, srv)
	_, _, err := c.workAt(t.Context(), srv.URL)
	if err == nil {
		t.Fatal("Work returned nil error for a GraphQL errors payload")
	}
	if got := err.Error(); !strings.Contains(got, "doesn't accept argument") {
		t.Errorf("error = %q, want it to carry the GraphQL message", got)
	}
	// A schema error is ours to fix. Backing off is harmless; stopping the
	// account would hide it.
	if !errors.Is(err, ErrTransient) {
		t.Errorf("error = %v, want ErrTransient", err)
	}
}

// TestWorkSurfacesIncomingPullRequests covers the case the workload query used
// to miss entirely: somebody else's pull request on a repository you own.
// GitHub cannot report those as review-requested, because requesting a
// reviewer needs write access the contributor does not have, so a maintainer
// saw nothing until they went looking.
func TestWorkSurfacesIncomingPullRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{
			"viewer":{"login":"tester"},
			"authored":{"nodes":[]},
			"reviewing":{"nodes":[{
				"number":1,"title":"asked for","url":"https://github.com/o/r/pull/1",
				"updatedAt":"2026-09-06T00:00:00Z","author":{"login":"colleague"},
				"repository":{"nameWithOwner":"o/r"}
			}]},
			"incoming":{"nodes":[
			  {"number":1,"title":"asked for","url":"https://github.com/o/r/pull/1",
			   "updatedAt":"2026-09-06T00:00:00Z","author":{"login":"colleague"},
			   "repository":{"nameWithOwner":"o/r"}},
			  {"number":2,"title":"unreviewed","url":"https://github.com/o/r/pull/2",
			   "updatedAt":"2026-09-07T00:00:00Z","author":{"login":"stranger"},
			   "repository":{"nameWithOwner":"o/r"}},
			  {"number":3,"title":"still a draft","url":"https://github.com/o/r/pull/3",
			   "isDraft":true,"updatedAt":"2026-09-07T00:00:00Z","author":{"login":"stranger"},
			   "repository":{"nameWithOwner":"o/r"}},
			  {"number":4,"title":"already approved","url":"https://github.com/o/r/pull/4",
			   "reviewDecision":"APPROVED","updatedAt":"2026-09-07T00:00:00Z",
			   "author":{"login":"stranger"},"repository":{"nameWithOwner":"o/r"}},
			  {"number":5,"title":"changes asked for","url":"https://github.com/o/r/pull/5",
			   "reviewDecision":"CHANGES_REQUESTED","updatedAt":"2026-09-07T00:00:00Z",
			   "author":{"login":"stranger"},"repository":{"nameWithOwner":"o/r"}}
			]},
			"assigned":{"nodes":[]}
		}}`))
	}))
	defer srv.Close()

	work, _, err := testClient(t, srv).workAt(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("Work: %v", err)
	}

	var got []int
	for _, pr := range work.ReviewRequests {
		got = append(got, pr.Number)
	}
	// 1 appears in both lists and must be listed once. 3 is the contributor's
	// own "not finished". 4 and 5 have been answered and are waiting on them.
	want := []int{1, 2}
	if len(got) != len(want) {
		t.Fatalf("ReviewRequests = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ReviewRequests = %v, want %v", got, want)
		}
	}
}

func TestWaitingOnOwner(t *testing.T) {
	cases := []struct {
		name string
		pr   PullRequest
		want bool
	}{
		{"nobody has looked at it", PullRequest{}, true},
		{"the contributor is still working", PullRequest{IsDraft: true}, false},
		{"already approved", PullRequest{ReviewDecision: "APPROVED"}, false},
		{"changes are with the author", PullRequest{ReviewDecision: "CHANGES_REQUESTED"}, false},
		{"review required but not given", PullRequest{ReviewDecision: "REVIEW_REQUIRED"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := waitingOnOwner(tc.pr); got != tc.want {
				t.Errorf("waitingOnOwner = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInboxPagination(t *testing.T) {
	const lastModified = "Tue, 09 Sep 2026 10:00:00 GMT"

	// page returns a JSON array of n notifications whose ids run 0..n-1.
	page := func(n int) string {
		var out strings.Builder
		out.WriteByte('[')
		for i := 0; i < n; i++ {
			if i > 0 {
				out.WriteByte(',')
			}
			out.WriteString(`{"id":"` + strconv.Itoa(i) + `","reason":"author","unread":true,`)
			out.WriteString(`"updated_at":"2026-09-08T15:43:11Z","repository":{"full_name":"o/r"},`)
			out.WriteString(`"subject":{"title":"n` + strconv.Itoa(i) + `","type":"Issue",`)
			out.WriteString(`"url":"https://api.github.com/repos/o/r/issues/1"}}`)
		}
		out.WriteByte(']')
		return out.String()
	}

	var page2Calls int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			page2Calls++
			w.Write([]byte(page(12))) // page two: 12 notifications
			return
		}
		// page one (no page param): 50, with a rel="next" link.
		w.Header().Set("Link",
			`<`+srv.URL+`/notifications?all=false&per_page=50&page=2>; rel="next", `+
				`<`+srv.URL+`/notifications?page=1>; rel="first"`)
		w.Header().Set("Last-Modified", lastModified)
		w.Write([]byte(page(50)))
	}))
	defer srv.Close()

	c := testClient(t, srv)
	items, state, _, _, err := c.inboxAt(t.Context(), srv.URL, InboxState{})
	if err != nil {
		t.Fatalf("inboxAt: %v", err)
	}
	if len(items) != 62 {
		t.Fatalf("len(items) = %d, want 62 (50 + 12)", len(items))
	}
	if page2Calls != 1 {
		t.Errorf("page 2 fetched %d times, want 1", page2Calls)
	}
	if state.LastModified != lastModified {
		t.Errorf("LastModified = %q, want %q", state.LastModified, lastModified)
	}
}

func TestInboxPagination304OnPoll(t *testing.T) {
	const lastModified = "Tue, 09 Sep 2026 10:00:00 GMT"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("If-Modified-Since") == lastModified {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Last-Modified", lastModified)
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := testClient(t, srv)

	// First poll: fetch, capture Last-Modified.
	items, state, unchanged, _, err := c.inboxAt(t.Context(), srv.URL, InboxState{})
	if err != nil {
		t.Fatalf("first inboxAt: %v", err)
	}
	if unchanged {
		t.Error("first poll reported unchanged, want a real fetch")
	}
	if len(items) != 0 {
		t.Fatalf("first poll items = %d, want empty list", len(items))
	}

	// Second poll sends the validator and gets 304.
	items, state, unchanged, _, err = c.inboxAt(t.Context(), srv.URL, state)
	if err != nil {
		t.Fatalf("second inboxAt: %v", err)
	}
	if !unchanged {
		t.Error("second poll reported changed, want unchanged from 304")
	}
	if items != nil {
		t.Errorf("second poll items = %+v, want nil so the caller keeps the previous list", items)
	}
	if state.LastModified != lastModified {
		t.Errorf("LastModified after 304 = %q, want preserved", state.LastModified)
	}
}

func TestInboxLoopingLinkStops(t *testing.T) {
	calls := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		// Always point "next" back at page 1.
		w.Header().Set("Link", `<`+srv.URL+`/notifications?page=1>; rel="next"`)
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := testClient(t, srv)
	items, _, _, _, err := c.inboxAt(t.Context(), srv.URL, InboxState{})
	if err != nil {
		t.Fatalf("inboxAt: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %+v, want empty", items)
	}
	if calls > maxPages {
		t.Errorf("server received %d requests, want at most %d (link loop bounded)", calls, maxPages)
	}
}

// statusCases is the classification both transports share. wait says what
// RetryAt must hold: nothing, the X-RateLimit-Reset time, or now plus
// Retry-After.
var statusCases = []struct {
	name    string
	status  int
	headers map[string]string
	class   error
	wait    string
}{
	{"401", http.StatusUnauthorized, nil, ErrUnauthorized, "none"},
	// A bare 403 is a missing scope, SSO enforcement or a suspended token.
	// The token still works, so the account keeps polling and the error is
	// reported rather than the loop stopped.
	{"403 with no rate signal", http.StatusForbidden, nil, ErrBlocked, "none"},
	{"403 primary limit", http.StatusForbidden,
		map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "RESET"}, ErrThrottled, "reset"},
	{"403 secondary limit", http.StatusForbidden,
		map[string]string{"X-RateLimit-Remaining": "482", "Retry-After": "60"}, ErrThrottled, "after"},
	// Retry-After wins when both are present.
	{"403 with both signals", http.StatusForbidden,
		map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "RESET", "Retry-After": "60"}, ErrThrottled, "after"},
	{"429 with no hint", http.StatusTooManyRequests, nil, ErrThrottled, "none"},
	{"404", http.StatusNotFound, nil, ErrBlocked, "none"},
	{"422", http.StatusUnprocessableEntity, nil, ErrTransient, "none"},
	{"500", http.StatusInternalServerError, nil, ErrTransient, "none"},
	// The regression #5 exists for: a bad gateway used to read as a dead token.
	{"502", http.StatusBadGateway, nil, ErrTransient, "none"},
	{"503", http.StatusServiceUnavailable, nil, ErrTransient, "none"},
	{"504", http.StatusGatewayTimeout, nil, ErrTransient, "none"},
}

// statusStub answers every request with one status and the case's headers,
// substituting a real reset time for RESET.
func statusStub(t *testing.T, status int, headers map[string]string, reset time.Time) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			if v == "RESET" {
				v = strconv.FormatInt(reset.Unix(), 10)
			}
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		w.Write([]byte(`{"message":"from github"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// checkClass asserts the class, the status, the message and the wait of one
// classified error. before is the instant just before the request was made.
func checkClass(t *testing.T, err error, class error, wait string, reset, before time.Time) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !errors.Is(err, class) {
		t.Fatalf("errors.Is(%v, %v) = false", err, class)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("%v is not an *APIError", err)
	}
	if !strings.Contains(apiErr.Message, "from github") {
		t.Errorf("Message = %q, want the body's message", apiErr.Message)
	}
	switch wait {
	case "none":
		if !apiErr.RetryAt.IsZero() {
			t.Errorf("RetryAt = %v, want zero", apiErr.RetryAt)
		}
	case "reset":
		if !apiErr.RetryAt.Equal(reset) {
			t.Errorf("RetryAt = %v, want the reset time %v", apiErr.RetryAt, reset)
		}
	case "after":
		low, high := before.Add(60*time.Second), time.Now().Add(60*time.Second)
		if apiErr.RetryAt.Before(low) || apiErr.RetryAt.After(high) {
			t.Errorf("RetryAt = %v, want now plus Retry-After, between %v and %v", apiErr.RetryAt, low, high)
		}
	}
}

func TestInboxStatusMapping(t *testing.T) {
	reset := time.Unix(time.Now().Add(time.Hour).Unix(), 0)
	for _, tc := range statusCases {
		t.Run(tc.name, func(t *testing.T) {
			srv := statusStub(t, tc.status, tc.headers, reset)
			before := time.Now()
			_, _, _, _, err := testClient(t, srv).inboxAt(t.Context(), srv.URL, InboxState{})
			checkClass(t, err, tc.class, tc.wait, reset, before)
			var apiErr *APIError
			if errors.As(err, &apiErr) && (apiErr.Op != "notifications" || apiErr.Status != tc.status) {
				t.Errorf("APIError = %+v, want op notifications and status %d", apiErr, tc.status)
			}
		})
	}
}

func TestWorkStatusMapping(t *testing.T) {
	reset := time.Unix(time.Now().Add(time.Hour).Unix(), 0)
	for _, tc := range statusCases {
		t.Run(tc.name, func(t *testing.T) {
			srv := statusStub(t, tc.status, tc.headers, reset)
			before := time.Now()
			_, _, err := testClient(t, srv).workAt(t.Context(), srv.URL)
			checkClass(t, err, tc.class, tc.wait, reset, before)
			var apiErr *APIError
			if errors.As(err, &apiErr) && (apiErr.Op != "workload" || apiErr.Status != tc.status) {
				t.Errorf("APIError = %+v, want op workload and status %d", apiErr, tc.status)
			}
		})
	}
}

// TestTransportFailureIsTransient pins a server that cannot be reached at all
// as a failure to back off from, with the dial error still in the chain.
func TestTransportFailureIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	c := testClient(t, srv)
	url := srv.URL
	srv.Close()

	_, _, _, _, err := c.inboxAt(t.Context(), url, InboxState{})
	if !errors.Is(err, ErrTransient) {
		t.Errorf("inbox against a dead server = %v, want ErrTransient", err)
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		t.Errorf("inbox error %v no longer carries the dial error", err)
	}
	if _, _, err := c.workAt(t.Context(), url); !errors.Is(err, ErrTransient) {
		t.Errorf("work against a dead server = %v, want ErrTransient", err)
	}
}

// TestWorkRateLimitArrivesAs200 covers the shape GitHub documents for a
// GraphQL primary limit: status 200, a RATE_LIMITED error and the budget
// headers. It used to read as an ordinary query error.
func TestWorkRateLimitArrivesAs200(t *testing.T) {
	reset := time.Unix(time.Now().Add(30*time.Minute).Unix(), 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded for user ID 1."}]}`))
	}))
	defer srv.Close()

	_, rate, err := testClient(t, srv).workAt(t.Context(), srv.URL)
	if !errors.Is(err, ErrThrottled) {
		t.Fatalf("error = %v, want ErrThrottled", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.RetryAt.Equal(reset) {
		t.Errorf("RetryAt = %v, want the reset time %v", apiErr.RetryAt, reset)
	}
	if rate.Remaining != 0 || !rate.ResetsAt.Equal(reset) {
		t.Errorf("rate = %+v, want the headers read", rate)
	}
}

// TestWorkAccessErrorsAreBlocked pins the typed errors that mean the token
// works but cannot see something: reported, never a reason to stop.
func TestWorkAccessErrorsAreBlocked(t *testing.T) {
	for _, typ := range []string{"FORBIDDEN", "NOT_FOUND", "INSUFFICIENT_SCOPES"} {
		t.Run(typ, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"data":null,"errors":[{"type":"` + typ + `","message":"nope"}]}`))
			}))
			defer srv.Close()

			_, _, err := testClient(t, srv).workAt(t.Context(), srv.URL)
			if !errors.Is(err, ErrBlocked) {
				t.Fatalf("error = %v, want ErrBlocked", err)
			}
			if !strings.Contains(err.Error(), "nope") {
				t.Errorf("error = %q, want it to carry the GraphQL message", err)
			}
		})
	}
}

// TestWorkKeepsPartialData pins that a response carrying data and errors
// yields the data, with the errors kept as warnings rather than discarding
// the sub-queries that worked.
func TestWorkKeepsPartialData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{
			"viewer":{"login":"tester"},
			"authored":{"nodes":[{
				"number":1,"title":"a pr","url":"https://github.com/o/r/pull/1",
				"author":{"login":"tester"},"repository":{"nameWithOwner":"o/r"}
			}]},
			"reviewing":null
		},"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration","path":["reviewing"]}]}`))
	}))
	defer srv.Close()

	work, _, err := testClient(t, srv).workAt(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("Work = %v, want the partial data with no error", err)
	}
	if work.Login != "tester" || len(work.AuthoredPRs) != 1 {
		t.Errorf("work = %+v, want the viewer and the one authored pr", work)
	}
	if len(work.Warnings) != 1 || !strings.Contains(work.Warnings[0], "not accessible") {
		t.Errorf("Warnings = %v, want the one GraphQL error", work.Warnings)
	}
	// reviewing was nulled, so reviewRequests is unresolved. The lists the
	// fixture leaves out entirely count too, since nothing came back for them.
	want := []string{"assignedIssues", "authoredIssues", "mergedPrs", "reviewRequests"}
	if !slices.Equal(work.Unresolved, want) {
		t.Errorf("Unresolved = %v, want %v", work.Unresolved, want)
	}
	if work.Resolved("reviewRequests") || !work.Resolved("authoredPrs") {
		t.Error("Resolved does not follow Unresolved")
	}
}

// TestWorkDiscardsUnattributableErrors pins the fallback: an error that
// names no path while every field came back leaves nothing known to be
// whole, so the answer is refused rather than trusted in part.
func TestWorkDiscardsUnattributableErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{
			"viewer":{"login":"tester"},
			"authored":{"nodes":[]},"reviewing":{"nodes":[]},"incoming":{"nodes":[]},
			"merged":{"nodes":[]},"assigned":{"nodes":[]},"authoredIssues":{"nodes":[]}
		},"errors":[{"message":"something went wrong while executing your query"}]}`))
	}))
	defer srv.Close()

	_, _, err := testClient(t, srv).workAt(t.Context(), srv.URL)
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("Work = %v, want the answer refused as ErrTransient", err)
	}
}

// TestUnresolvedLists pins how errors are pinned to lists.
func TestUnresolvedLists(t *testing.T) {
	whole := map[string]json.RawMessage{}
	for field := range workloadLists {
		whole[field] = json.RawMessage(`{}`)
	}
	withNull := map[string]json.RawMessage{}
	for field := range workloadLists {
		withNull[field] = json.RawMessage(`{}`)
	}
	withNull["merged"] = json.RawMessage(`null`)

	for _, tc := range []struct {
		name       string
		fields     map[string]json.RawMessage
		errs       []gqlError
		want       []string
		attributed bool
	}{
		{"no errors, every field present", whole, nil, nil, true},
		{"a nulled field with no path", withNull, []gqlError{{Message: "x"}}, []string{"mergedPrs"}, true},
		{"a path into a present field", whole, []gqlError{{Path: []any{"incoming", "nodes", 3.0}}}, []string{"reviewRequests"}, true},
		{"a path nobody asked for", whole, []gqlError{{Path: []any{"weeks"}}}, nil, false},
		{"no path and nothing nulled", whole, []gqlError{{Message: "x"}}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, attributed := unresolvedLists(tc.fields, tc.errs)
			if attributed != tc.attributed || !slices.Equal(got, tc.want) {
				t.Errorf("unresolvedLists = %v, %v; want %v, %v", got, attributed, tc.want, tc.attributed)
			}
		})
	}
}

// TestNextLinkStaysOnTheSameHost pins the one place a response decides where
// the next request goes. newRequest attaches the bearer token to whatever URL
// it is handed, so a Link header pointing somewhere else would send the
// account's GitHub token there. Pagination stops instead: a short list is a
// better failure than an authenticated request nobody asked for.
func TestNextLinkStaysOnTheSameHost(t *testing.T) {
	const base = "https://api.github.com"
	for _, tc := range []struct {
		name   string
		header string
		want   string
	}{
		{"same host", `<https://api.github.com/notifications?page=2>; rel="next"`,
			"https://api.github.com/notifications?page=2"},
		{"unquoted rel", `<https://api.github.com/n?page=2>; rel=next`,
			"https://api.github.com/n?page=2"},
		{"last page", `<https://api.github.com/n?page=1>; rel="first"`, ""},
		{"empty", "", ""},
		{"another host", `<https://evil.invalid/notifications?page=2>; rel="next"`, ""},
		{"downgraded to http", `<http://api.github.com/n?page=2>; rel="next"`, ""},
		{"credentials in the url", `<https://api.github.com@evil.invalid/n>; rel="next"`, ""},
		{"unparseable", `<://nonsense>; rel="next"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextLink(tc.header, base); got != tc.want {
				t.Errorf("nextLink(%q) = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}
