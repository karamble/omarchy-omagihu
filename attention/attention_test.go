package attention

import (
	"testing"

	"github.com/karamble/omarchy-omagihu/forge"
	"github.com/karamble/omarchy-omagihu/local"
)

func TestResolvePicksTheMostSevereTierOnly(t *testing.T) {
	review := []forge.PullRequest{{Repo: "o/r", Number: 1}}
	broken := []forge.PullRequest{{Repo: "o/r", Number: 2, ChecksState: "FAILURE"}}
	unread := []forge.Notification{{ID: "1"}, {ID: "2"}}
	risky := []local.Repo{{Path: "/p", Unpushed: 3}}

	tests := []struct {
		name      string
		in        Input
		wantTier  Tier
		wantCount int
		wantLevel string
	}{
		{
			name:      "nothing waiting",
			in:        Input{},
			wantTier:  TierClear,
			wantCount: 0,
			wantLevel: "clear",
		},
		{
			name:      "local risk alone",
			in:        Input{Repos: risky},
			wantTier:  TierLocal,
			wantCount: 1,
			wantLevel: "notice",
		},
		{
			name:      "unread outranks local risk",
			in:        Input{Unread: unread, Repos: risky},
			wantTier:  TierInbox,
			wantCount: 2,
			wantLevel: "warn",
		},
		{
			name:      "a broken PR outranks unread",
			in:        Input{Authored: broken, Unread: unread, Repos: risky},
			wantTier:  TierBroken,
			wantCount: 1,
			wantLevel: "urgent",
		},
		{
			name:      "a requested review outranks everything",
			in:        Input{Reviews: review, Authored: broken, Unread: unread, Repos: risky},
			wantTier:  TierReview,
			wantCount: 1,
			wantLevel: "urgent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(tt.in)
			if got.Tier != tt.wantTier {
				t.Errorf("Tier = %d, want %d", got.Tier, tt.wantTier)
			}
			if got.Count != tt.wantCount {
				t.Errorf("Count = %d, want %d", got.Count, tt.wantCount)
			}
			if got.Level != tt.wantLevel {
				t.Errorf("Level = %q, want %q", got.Level, tt.wantLevel)
			}
			if got.Summary == "" {
				t.Error("Summary is empty")
			}
		})
	}
}

// TestResolveAlwaysReportsTheBreakdown proves the panel can show every number
// even though the bar shows only the winning tier.
func TestResolveAlwaysReportsTheBreakdown(t *testing.T) {
	got := Resolve(Input{
		Reviews:  []forge.PullRequest{{Number: 1}},
		Authored: []forge.PullRequest{{Number: 2, ReviewDecision: "CHANGES_REQUESTED"}},
		Unread:   []forge.Notification{{ID: "1"}},
		Repos: []local.Repo{
			{Path: "/a", Unpushed: 5},
			{Path: "/b", Operation: local.OpRebase},
			{Path: "/c"}, // clean, must not count
		},
	})

	if got.Tier != TierReview {
		t.Fatalf("Tier = %d, want TierReview", got.Tier)
	}
	if got.Reviews != 1 || got.BrokenPRs != 1 || got.Unread != 1 {
		t.Errorf("breakdown = %+v, want one of each", got)
	}
	if got.ReposAtRisk != 2 {
		t.Errorf("ReposAtRisk = %d, want 2: the clean repo must not count", got.ReposAtRisk)
	}
	if got.UnpushedTotal != 5 {
		t.Errorf("UnpushedTotal = %d, want 5", got.UnpushedTotal)
	}
	if got.Interrupted != 1 {
		t.Errorf("Interrupted = %d, want 1", got.Interrupted)
	}
}

func TestBroken(t *testing.T) {
	tests := []struct {
		name string
		pr   forge.PullRequest
		want bool
	}{
		{"failing checks", forge.PullRequest{ChecksState: "FAILURE"}, true},
		{"errored checks", forge.PullRequest{ChecksState: "ERROR"}, true},
		{"changes requested", forge.PullRequest{ReviewDecision: "CHANGES_REQUESTED"}, true},
		{"approved and green", forge.PullRequest{ChecksState: "SUCCESS", ReviewDecision: "APPROVED"}, false},
		{"pending checks", forge.PullRequest{ChecksState: "PENDING"}, false},
		{"nothing known", forge.PullRequest{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Broken(tt.pr); got != tt.want {
				t.Errorf("Broken(%+v) = %v, want %v", tt.pr, got, tt.want)
			}
		})
	}
}
