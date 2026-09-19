package forge

import (
	"errors"
	"net/http"
	"net/http/httptest"
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
}

// TestInboxPagination aggregates page one and page two over the Link header.
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

// TestInboxPagination304OnPoll asserts the conditional request still costs a
// single request and that the following poll returns unchanged on 304.
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

// TestInboxLoopingLinkStops asserts a self-referential Link cannot loop forever.
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

// TestInboxStatusMapping asserts 401 is ErrUnauthorized while 403 and 429 are
// recoverable errors that must NOT kill the account loop.
func TestInboxStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		status int
		class  string
	}{
		{http.StatusUnauthorized, "unauthorized"},
		{http.StatusForbidden, "recoverable"},
		{http.StatusTooManyRequests, "recoverable"},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			c := testClient(t, srv)
			_, _, _, _, err := c.inboxAt(t.Context(), srv.URL, InboxState{})
			if tc.class == "unauthorized" {
				if !errors.Is(err, ErrUnauthorized) {
					t.Fatalf("401 error = %v, want ErrUnauthorized", err)
				}
				return
			}
			if errors.Is(err, ErrUnauthorized) {
				t.Fatalf("%d must not be ErrUnauthorized, got %v", tc.status, err)
			}
			if err == nil {
				t.Fatalf("%d should surface an error so the poller backs off, got nil", tc.status)
			}
		})
	}
}

// TestWorkStatusMapping mirrors the inbox mapping for the workload query.
func TestWorkStatusMapping(t *testing.T) {
	t.Run("unauthorized", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()
		c := testClient(t, srv)
		_, _, err := c.workAt(t.Context(), srv.URL)
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("Work 401 error = %v, want ErrUnauthorized", err)
		}
	})
	t.Run("forbidden_is_recoverable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		c := testClient(t, srv)
		_, _, err := c.workAt(t.Context(), srv.URL)
		if errors.Is(err, ErrUnauthorized) {
			t.Fatalf("Work 403 must not be ErrUnauthorized, got %v", err)
		}
		if err == nil {
			t.Fatal("Work 403 should surface an error, got nil")
		}
	})
}
