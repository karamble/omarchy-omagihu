package alerts

import (
	"testing"
	"time"

	"github.com/karamble/omarchy-omagihu/attention"
	"github.com/karamble/omarchy-omagihu/correlate"
	"github.com/karamble/omarchy-omagihu/forge"
)

func ptr(f float64) *float64 { return &f }

func armed(path string, op Operator, p Params, where ...Where) *Trigger {
	return &Trigger{
		ID: "t-test", Path: path, Operator: op, Params: p, Where: where,
		DeliverTo: "you", ExpiresAt: time.Now().Add(24 * time.Hour),
		ArmedBy: "test", ArmedAt: time.Now(),
	}
}

func withUnpushed(n int) Snapshot {
	return Snapshot{Attention: attention.State{UnpushedTotal: n}}
}

func withReviews(prs ...forge.PullRequest) Snapshot {
	return Snapshot{Reviews: prs}
}

// TestFirstSampleNeverFires is the rule that keeps an alert system from
// shouting about the world as it already was.
func TestFirstSampleNeverFires(t *testing.T) {
	now := time.Now()

	crosses := armed("attention.unpushedTotal", OpCrosses, Params{Above: ptr(100)})
	if fires := Evaluate(crosses, withUnpushed(500), now); len(fires) != 0 {
		t.Errorf("crosses fired on the first sample: %+v", fires)
	}

	appears := armed("work.reviewRequests", OpAppears, Params{})
	if fires := Evaluate(appears, withReviews(forge.PullRequest{Repo: "o/r", Number: 1}), now); len(fires) != 0 {
		t.Errorf("appears fired on the first sample: %+v", fires)
	}
}

func TestCrossesFiresOnTheCarryingSample(t *testing.T) {
	now := time.Now()
	tr := armed("attention.unpushedTotal", OpCrosses, Params{Above: ptr(100)})
	tr.Standing = true

	Evaluate(tr, withUnpushed(90), now) // prime
	if fires := Evaluate(tr, withUnpushed(95), now); len(fires) != 0 {
		t.Fatalf("fired without crossing: %+v", fires)
	}

	fires := Evaluate(tr, withUnpushed(101), now)
	if len(fires) != 1 {
		t.Fatalf("got %d fires crossing the bound, want 1", len(fires))
	}

	// Sitting past the bound is not an event.
	if more := Evaluate(tr, withUnpushed(120), now); len(more) != 0 {
		t.Errorf("fired again while sitting past the bound: %+v", more)
	}
}

func TestCrossesRearmsOnlyPastTheMargin(t *testing.T) {
	now := time.Now()
	tr := armed("attention.unpushedTotal", OpCrosses, Params{Above: ptr(100)})
	tr.Standing = true // a one-shot would be spent by the first fire

	Evaluate(tr, withUnpushed(90), now)
	Evaluate(tr, withUnpushed(101), now) // fires

	// Back to the bound itself is not clear enough to re-arm.
	Evaluate(tr, withUnpushed(100), now)
	if tr.State.Ready {
		t.Error("re-armed at the bound, want it to require the margin")
	}

	// One whole unit below re-arms, and the re-arming sample never fires.
	if fires := Evaluate(tr, withUnpushed(99), now); len(fires) != 0 {
		t.Errorf("the re-arming sample fired: %+v", fires)
	}
	if !tr.State.Ready {
		t.Fatal("did not re-arm below the margin")
	}
	if fires := Evaluate(tr, withUnpushed(105), now); len(fires) != 1 {
		t.Errorf("got %d fires after re-arming, want 1", len(fires))
	}
}

func TestCrossesBelow(t *testing.T) {
	now := time.Now()
	tr := armed("health.rateLeft", OpCrosses, Params{Below: ptr(500)})
	snap := func(n int) Snapshot { return Snapshot{Health: Health{RateLeft: n}} }

	Evaluate(tr, snap(4000), now)
	if fires := Evaluate(tr, snap(499), now); len(fires) != 1 {
		t.Fatalf("got %d fires dropping below, want 1", len(fires))
	}
}

// TestMissingSampleIsNotATransition covers a path the snapshot cannot resolve,
// which happens whenever a poll failed.
func TestMissingSampleIsNotATransition(t *testing.T) {
	tr := armed("attention.unpushedTotal", OpCrosses, Params{Above: ptr(10)})
	before := tr.State

	if fires := Evaluate(tr, Snapshot{}, time.Now()); len(fires) != 0 {
		t.Errorf("fired on a snapshot that resolves, unexpectedly: %+v", fires)
	}
	// A genuinely unresolvable path must leave the state untouched.
	missing := armed("nope.nothing", OpCrosses, Params{Above: ptr(1)})
	if fires := Evaluate(missing, Snapshot{}, time.Now()); len(fires) != 0 || missing.State.Primed {
		t.Errorf("an unresolvable path moved state: fires=%+v primed=%v", fires, missing.State.Primed)
	}
	_ = before
}

func TestAppearsFiresPerNewEntry(t *testing.T) {
	now := time.Now()
	tr := armed("work.reviewRequests", OpAppears, Params{})
	tr.Standing = true

	Evaluate(tr, withReviews(forge.PullRequest{Repo: "o/r", Number: 1}), now)

	fires := Evaluate(tr, withReviews(
		forge.PullRequest{Repo: "o/r", Number: 1},
		forge.PullRequest{Repo: "o/r", Number: 2},
		forge.PullRequest{Repo: "o/r", Number: 3},
	), now)
	if len(fires) != 2 {
		t.Fatalf("got %d fires for two new reviews, want 2", len(fires))
	}

	// The same set again is not news.
	if more := Evaluate(tr, withReviews(
		forge.PullRequest{Repo: "o/r", Number: 1},
		forge.PullRequest{Repo: "o/r", Number: 2},
		forge.PullRequest{Repo: "o/r", Number: 3},
	), now); len(more) != 0 {
		t.Errorf("an unchanged set fired: %+v", more)
	}
}

// TestOneShotStopsAtTheFirstEntry: a one-shot is spent by the first event, even
// when several arrive in the same sample.
func TestOneShotStopsAtTheFirstEntry(t *testing.T) {
	now := time.Now()
	tr := armed("work.reviewRequests", OpAppears, Params{})

	Evaluate(tr, withReviews(), now)
	fires := Evaluate(tr, withReviews(
		forge.PullRequest{Repo: "o/r", Number: 1},
		forge.PullRequest{Repo: "o/r", Number: 2},
	), now)
	if len(fires) != 1 {
		t.Fatalf("a one-shot produced %d fires, want 1", len(fires))
	}
	if tr.Status(now) != StatusFired {
		t.Errorf("status = %q, want fired", tr.Status(now))
	}
	if fires := Evaluate(tr, withReviews(forge.PullRequest{Repo: "o/r", Number: 9}), now); len(fires) != 0 {
		t.Errorf("a spent one-shot fired again: %+v", fires)
	}
}

func TestDisappears(t *testing.T) {
	now := time.Now()
	tr := armed("work.authoredPrs", OpDisappears, Params{})
	tr.Standing = true

	both := Snapshot{Authored: []forge.PullRequest{
		{Repo: "o/r", Number: 1}, {Repo: "o/r", Number: 2},
	}}
	Evaluate(tr, both, now)

	fires := Evaluate(tr, Snapshot{Authored: []forge.PullRequest{{Repo: "o/r", Number: 1}}}, now)
	if len(fires) != 1 {
		t.Fatalf("got %d fires when a PR left, want 1", len(fires))
	}
}

func TestWhereFiltersTheSet(t *testing.T) {
	now := time.Now()
	tr := armed("facts", OpAppears, Params{}, Where{Field: "kind", Op: "=", Value: "ci-red-on-head"})
	tr.Standing = true

	Evaluate(tr, Snapshot{}, now)

	// A fact of another kind must not wake anybody.
	quiet := Snapshot{Facts: []correlate.Fact{
		{Kind: correlate.KindForkBehind, Path: "/a", Branch: "master"},
	}}
	if fires := Evaluate(tr, quiet, now); len(fires) != 0 {
		t.Fatalf("a filtered-out fact fired: %+v", fires)
	}

	loud := Snapshot{Facts: []correlate.Fact{
		{Kind: correlate.KindForkBehind, Path: "/a", Branch: "master"},
		{Kind: correlate.KindCIRedOnHead, Path: "/b", Branch: "topic"},
	}}
	fires := Evaluate(tr, loud, now)
	if len(fires) != 1 {
		t.Fatalf("got %d fires for the matching fact, want 1", len(fires))
	}
}

func TestCountCrossesOverFilteredEntries(t *testing.T) {
	now := time.Now()
	tr := armed("facts", OpCount, Params{Above: ptr(2)},
		Where{Field: "severity", Op: "=", Value: "urgent"})

	facts := func(n int) Snapshot {
		var out []correlate.Fact
		for i := range n {
			out = append(out, correlate.Fact{
				Kind: correlate.KindMissingWork, Severity: correlate.Urgent,
				Path: string(rune('a' + i)), Branch: "b",
			})
		}
		// A notice must not be counted.
		out = append(out, correlate.Fact{Kind: correlate.KindForkBehind,
			Severity: correlate.Notice, Path: "/z", Branch: "b"})
		return Snapshot{Facts: out}
	}

	Evaluate(tr, facts(1), now)
	if fires := Evaluate(tr, facts(2), now); len(fires) != 0 {
		t.Fatalf("fired at the bound rather than past it: %+v", fires)
	}
	if fires := Evaluate(tr, facts(3), now); len(fires) != 1 {
		t.Fatalf("got %d fires crossing above 2, want 1", len(fires))
	}
}

func TestBecomes(t *testing.T) {
	now := time.Now()
	tr := armed("attention.level", OpBecomes, Params{Value: "urgent"})
	level := func(l string) Snapshot { return Snapshot{Attention: attention.State{Level: l}} }

	Evaluate(tr, level("clear"), now)
	if fires := Evaluate(tr, level("urgent"), now); len(fires) != 1 {
		t.Fatalf("got %d fires becoming urgent, want 1", len(fires))
	}
	// Still urgent is not a new event.
	if fires := Evaluate(tr, level("urgent"), now); len(fires) != 0 {
		t.Errorf("fired while still urgent: %+v", fires)
	}
}

// TestBecomesHoldsOffFlapping: away briefly then back must not ring twice.
func TestBecomesHoldsOffFlapping(t *testing.T) {
	start := time.Now()
	tr := armed("attention.level", OpBecomes, Params{Value: "urgent", Hold: "5m"})
	tr.Standing = true
	level := func(l string) Snapshot { return Snapshot{Attention: attention.State{Level: l}} }

	Evaluate(tr, level("clear"), start)
	Evaluate(tr, level("urgent"), start) // fires

	Evaluate(tr, level("clear"), start.Add(time.Minute))
	if fires := Evaluate(tr, level("urgent"), start.Add(2*time.Minute)); len(fires) != 0 {
		t.Fatalf("a flap within the hold rang again: %+v", fires)
	}

	// Away long enough, and it is a fresh episode.
	Evaluate(tr, level("clear"), start.Add(10*time.Minute))
	if fires := Evaluate(tr, level("urgent"), start.Add(20*time.Minute)); len(fires) != 1 {
		t.Errorf("got %d fires after the hold elapsed, want 1", len(fires))
	}
}

func TestAges(t *testing.T) {
	now := time.Now()
	tr := armed("work.authoredPrs", OpAges, Params{Field: "updatedAt", OlderThan: "7d"})
	tr.Standing = true

	fresh := Snapshot{Authored: []forge.PullRequest{
		{Repo: "o/r", Number: 1, UpdatedAt: now.Add(-time.Hour)},
	}}
	Evaluate(tr, fresh, now)
	if fires := Evaluate(tr, fresh, now); len(fires) != 0 {
		t.Fatalf("a fresh pull request aged: %+v", fires)
	}

	stale := Snapshot{Authored: []forge.PullRequest{
		{Repo: "o/r", Number: 1, UpdatedAt: now.Add(-8 * 24 * time.Hour)},
	}}
	fires := Evaluate(tr, stale, now)
	if len(fires) != 1 {
		t.Fatalf("got %d fires for a PR untouched for eight days, want 1", len(fires))
	}

	// Still stale is not a second event.
	if more := Evaluate(tr, stale, now); len(more) != 0 {
		t.Errorf("the same stale entry fired twice: %+v", more)
	}

	// Touched, then stale again, is a new episode.
	Evaluate(tr, fresh, now)
	if fires := Evaluate(tr, stale, now); len(fires) != 1 {
		t.Errorf("got %d fires after it went stale again, want 1", len(fires))
	}
}

func TestExpiredNeverFires(t *testing.T) {
	now := time.Now()
	tr := armed("attention.unpushedTotal", OpCrosses, Params{Above: ptr(10)})
	tr.ExpiresAt = now.Add(-time.Minute)

	Evaluate(tr, withUnpushed(1), now)
	if fires := Evaluate(tr, withUnpushed(100), now); len(fires) != 0 {
		t.Errorf("an expired trigger fired: %+v", fires)
	}
	if tr.Status(now) != StatusExpired {
		t.Errorf("status = %q, want expired", tr.Status(now))
	}
}

func TestParseSpanAndExpiry(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{in: "4d", want: 96 * time.Hour},
		{in: "12h", want: 12 * time.Hour},
		{in: "90m", want: 90 * time.Minute},
		{in: "30s", want: 30 * time.Second},
		{in: "", bad: true},
		{in: "soon", bad: true},
		{in: "-1h", bad: true},
	}
	for _, tt := range tests {
		got, err := ParseSpan(tt.in)
		if tt.bad {
			if err == nil {
				t.Errorf("ParseSpan(%q) accepted a bad span", tt.in)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ParseSpan(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
		}
	}

	if _, err := ParseExpiry("", time.Now()); err == nil {
		t.Error("an empty expiry was accepted: nothing may stay armed for ever")
	}
	if _, err := ParseExpiry("2026-10-01", time.Now()); err != nil {
		t.Errorf("a bare date was rejected: %v", err)
	}
}

func TestParseWhere(t *testing.T) {
	tests := []struct {
		in    string
		field string
		op    string
		value string
		bad   bool
	}{
		{in: "kind=ci-red-on-head", field: "kind", op: "=", value: "ci-red-on-head"},
		{in: "title~=flake", field: "title", op: "~=", value: "flake"},
		{in: "nofilter", bad: true},
		{in: "=value", bad: true},
	}
	for _, tt := range tests {
		got, err := ParseWhere(tt.in)
		if tt.bad {
			if err == nil {
				t.Errorf("ParseWhere(%q) accepted nonsense", tt.in)
			}
			continue
		}
		if err != nil || got.Field != tt.field || got.Op != tt.op || got.Value != tt.value {
			t.Errorf("ParseWhere(%q) = %+v, %v", tt.in, got, err)
		}
	}
}

func TestCatalogueOperatorsMatchKinds(t *testing.T) {
	for _, leaf := range Catalogue() {
		if len(leaf.Operators) == 0 {
			t.Errorf("%s offers no operators", leaf.Path)
		}
		if leaf.Kind == KindList {
			if len(leaf.Identity) == 0 {
				t.Errorf("%s is a list with no identity: appears could not work", leaf.Path)
			}
			if leaf.Accepts(OpAges) && len(leaf.TimeFields) == 0 {
				t.Errorf("%s accepts ages but names no time field", leaf.Path)
			}
		}
		if leaf.Kind == KindNumber && !leaf.Accepts(OpCrosses) {
			t.Errorf("%s is a number that cannot cross", leaf.Path)
		}
	}
}

// TestEveryCataloguePathResolves guards against a leaf that names a path the
// snapshot cannot produce, which would make a trigger permanently no-sample.
func TestEveryCataloguePathResolves(t *testing.T) {
	snap := Snapshot{}
	for _, leaf := range Catalogue() {
		var ok bool
		switch leaf.Kind {
		case KindNumber:
			_, ok = snap.Number(leaf.Path)
		case KindText, KindBool:
			_, ok = snap.Text(leaf.Path)
		case KindList:
			_, ok = snap.List(leaf.Path)
		}
		if !ok {
			t.Errorf("catalogue path %q does not resolve against a snapshot", leaf.Path)
		}
	}
}
