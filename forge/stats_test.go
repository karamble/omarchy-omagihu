package forge

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

const statsBody = `{"data":{"repository":{
	"url":"https://github.com/o/r","stargazerCount":42,"forkCount":7,
	"isArchived":false,"isPrivate":true,"pushedAt":"2026-09-20T10:00:00Z",
	"description":"a thing","watchers":{"totalCount":9},
	"issues":{"totalCount":3},"pullRequests":{"totalCount":2},
	"latestRelease":{"tagName":"v0.2.0","publishedAt":"2026-09-01T00:00:00Z"},
	"defaultBranchRef":{"name":"master"},"licenseInfo":{"spdxId":"ISC"},
	"repositoryTopics":{"nodes":[{"topic":{"name":"omarchy"}},{"topic":{"name":"plugin"}}]}
},"rateLimit":{"cost":1,"remaining":4990}}}`

// TestRepoStatsDecodes pins the query's shape: variables carry the target,
// every figure lands, and the cost GitHub reports comes back with them.
func TestRepoStatsDecodes(t *testing.T) {
	var variables map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Variables map[string]string `json:"variables"`
		}
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		variables = body.Variables
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(statsBody))
	}))
	defer srv.Close()

	got, err := testClient(t, srv).repoStatsAt(t.Context(), srv.URL, "o", "r")
	if err != nil {
		t.Fatalf("RepoStats: %v", err)
	}
	if variables["owner"] != "o" || variables["name"] != "r" {
		t.Errorf("variables = %v, want owner o and name r", variables)
	}
	if got.Stars != 42 || got.Forks != 7 || got.Watchers != 9 || got.OpenIssues != 3 || got.OpenPRs != 2 {
		t.Errorf("counts = %+v, want 42 7 9 3 2", got)
	}
	if got.Release != "v0.2.0" || got.ReleasedAt.IsZero() || got.DefaultBranch != "master" || got.License != "ISC" {
		t.Errorf("release/branch/licence = %+v", got)
	}
	if !got.Private || got.Archived || got.Description != "a thing" || got.URL != "https://github.com/o/r" {
		t.Errorf("state = %+v", got)
	}
	if !slices.Equal(got.Topics, []string{"omarchy", "plugin"}) {
		t.Errorf("Topics = %v", got.Topics)
	}
	if got.PushedAt != time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC) {
		t.Errorf("PushedAt = %v", got.PushedAt)
	}
	if got.Cost != 1 || got.Remaining != 4990 {
		t.Errorf("Cost = %d Remaining = %d, want the rateLimit figures", got.Cost, got.Remaining)
	}
}

// TestRepoStatsMissingRepositoryIsBlocked pins the renamed, deleted or
// invisible case: GitHub answers a null repository with NOT_FOUND, which is
// blocked, not a reason to stop anything.
func TestRepoStatsMissingRepositoryIsBlocked(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"not found", `{"data":{"repository":null,"rateLimit":{"cost":1,"remaining":4999}},"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a Repository with the name 'o/gone'.","path":["repository"]}]}`},
		{"null with no error", `{"data":{"repository":null,"rateLimit":{"cost":1,"remaining":4999}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := testClient(t, srv).repoStatsAt(t.Context(), srv.URL, "o", "gone")
			if !errors.Is(err, ErrBlocked) {
				t.Fatalf("error = %v, want ErrBlocked", err)
			}
			if errors.Is(err, ErrUnauthorized) {
				t.Fatal("a missing repository must not read as a dead token")
			}
		})
	}

	// Topics stay a list, never null, when the repository has none.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"repository":{"url":"u","repositoryTopics":{"nodes":[]}},"rateLimit":{"cost":1}}}`))
	}))
	defer srv.Close()
	got, err := testClient(t, srv).repoStatsAt(t.Context(), srv.URL, "o", "bare")
	if err != nil || got.Topics == nil {
		t.Errorf("bare repository = %+v, %v; want empty topics and no error", got, err)
	}
}

func TestParseRemote(t *testing.T) {
	for _, tc := range []struct {
		in                string
		host, owner, name string
		ok                bool
	}{
		{"git@github.com:karamble/omarchy-omagihu.git", "github.com", "karamble", "omarchy-omagihu", true},
		{"https://github.com/karamble/omarchy-omagihu", "github.com", "karamble", "omarchy-omagihu", true},
		{"https://GitHub.com/Karamble/thing.git", "github.com", "Karamble", "thing", true},
		{"ssh://git@git.example.org/team/thing.git", "git.example.org", "team", "thing", true},
		{"/tmp/lab/origin.git", "", "", "", false},
		{"../origin.git", "", "", "", false},
		{"", "", "", "", false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			host, owner, name, ok := ParseRemote(tc.in)
			if ok != tc.ok || host != tc.host || owner != tc.owner || name != tc.name {
				t.Errorf("ParseRemote(%q) = %q %q %q %v, want %q %q %q %v",
					tc.in, host, owner, name, ok, tc.host, tc.owner, tc.name, tc.ok)
			}
		})
	}
}
