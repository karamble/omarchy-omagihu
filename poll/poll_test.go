package poll

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/karamble/omarchy-omagihu/accounts"
	"github.com/karamble/omarchy-omagihu/forge"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeClient is a hand-written stand-in for forge.Client. Manual fakes keep the
// test readable and the dependency list short.
type fakeClient struct {
	account accounts.Account

	mu          sync.Mutex
	inboxCall   int
	workCall    int
	inboxStates []string // tracks LastModified sent in each Inbox call

	// What to return on the next call
	inboxErr    error
	items       []forge.Notification
	nextLastMod string

	workErr error
	work    forge.Workload

	polled chan struct{} // signalled once per inbox call
}

func newFake(id string) *fakeClient {
	return &fakeClient{
		account: accounts.Account{ID: id, Login: id, Enabled: true},
		polled:  make(chan struct{}, 16),
	}
}

func (f *fakeClient) Account() accounts.Account { return f.account }

func (f *fakeClient) Inbox(ctx context.Context, state forge.InboxState) ([]forge.Notification, forge.InboxState, bool, forge.Rate, error) {
	f.mu.Lock()
	f.inboxCall++
	f.inboxStates = append(f.inboxStates, state.LastModified)

	err := f.inboxErr
	items := f.items
	nextMod := f.nextLastMod
	f.mu.Unlock()

	select {
	case f.polled <- struct{}{}:
	default:
	}
	if err != nil {
		return nil, state, false, forge.Rate{}, err
	}

	// Real 304 semantics: unchanged only when the caller sent an
	// If-Modified-Since matching our current data. An unconditional fetch
	// (empty state.LastModified) always returns the fresh list. On 304 the
	// incoming LastModified is preserved (the real forge client returns next
	// = state on NotModified), so the loop can send it again next time.
	unchanged := state.LastModified != "" && state.LastModified == nextMod

	var out forge.InboxState
	if unchanged {
		out = state
	} else {
		out = forge.InboxState{LastModified: nextMod}
	}
	return items, out, unchanged, forge.Rate{Remaining: 4999, Limit: 5000}, nil
}

func (f *fakeClient) Work(ctx context.Context) (forge.Workload, forge.Rate, error) {
	f.mu.Lock()
	f.workCall++
	err, work := f.workErr, f.work
	f.mu.Unlock()

	if err != nil {
		return forge.Workload{}, forge.Rate{}, err
	}
	return work, forge.Rate{Remaining: 4999, Limit: 5000}, nil
}

func (f *fakeClient) calls() (inbox, work int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inboxCall, f.workCall
}

func (f *fakeClient) inboxCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inboxCall
}

func (f *fakeClient) inboxStatesSent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.inboxStates...)
}

func (f *fakeClient) resetInboxStates() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inboxStates = nil
}

// SetNextResponse configures what the next Inbox call returns.
// If nextLastMod is non-empty, the fake will return unchanged=true when
// the caller sends that exact LastModified value (simulating 304).
func (f *fakeClient) SetNextResponse(items []forge.Notification, nextLastMod string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = items
	f.nextLastMod = nextLastMod
}

func TestNewPublishesEmptyViewPerAccount(t *testing.T) {
	p := New([]Client{newFake("a"), newFake("b")}, quietLogger(), time.Minute, time.Minute)

	snap := p.Snapshot()
	if len(snap.Accounts) != 2 {
		t.Fatalf("Snapshot has %d accounts, want 2", len(snap.Accounts))
	}
	for _, v := range snap.Accounts {
		if v.AccountID == "" {
			t.Error("account view published without an id")
		}
		if v.Notifications != nil {
			t.Errorf("account %s starts with notifications %+v, want none", v.AccountID, v.Notifications)
		}
	}
}

func TestRunPopulatesSnapshot(t *testing.T) {
	fake := newFake("a")
	fake.items = []forge.Notification{{AccountID: "a", ID: "1", Title: "hello", Repo: "o/r"}}
	fake.nextLastMod = "LM1"
	fake.work = forge.Workload{
		Login:       "a",
		AuthoredPRs: []forge.PullRequest{{Repo: "o/r", Number: 7, ChecksState: "FAILURE"}},
	}

	p := New([]Client{fake}, quietLogger(), time.Hour, time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()

	select {
	case <-fake.polled:
	case <-time.After(5 * time.Second):
		t.Fatal("poller never called Inbox")
	}

	// Both loops fire immediately, so wait for BOTH the work result and the
	// inbox result to land (they publish independently and concurrently).
	deadline := time.Now().Add(5 * time.Second)
	var view AccountView
	for time.Now().Before(deadline) {
		snap := p.Snapshot()
		if len(snap.Accounts) == 1 && len(snap.Accounts[0].AuthoredPRs) == 1 &&
			len(snap.Accounts[0].Notifications) == 1 {
			view = snap.Accounts[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if len(view.Notifications) != 1 || view.Notifications[0].Title != "hello" {
		t.Errorf("Notifications = %+v, want the fake's single item", view.Notifications)
	}
	if len(view.AuthoredPRs) != 1 || view.AuthoredPRs[0].ChecksState != "FAILURE" {
		t.Errorf("AuthoredPRs = %+v, want the fake's failing PR", view.AuthoredPRs)
	}
	if view.Rate.Remaining != 4999 {
		t.Errorf("Rate.Remaining = %d, want 4999", view.Rate.Remaining)
	}
	if view.Error != "" {
		t.Errorf("Error = %q, want empty", view.Error)
	}
}

// TestRunStopsAccountOnUnauthorized proves a rejected token does not spin: the
// loop exits instead of retrying on its cadence.
func TestRunStopsAccountOnUnauthorized(t *testing.T) {
	fake := newFake("a")
	fake.inboxErr = fmt.Errorf("notifications: %w (401)", forge.ErrUnauthorized)
	fake.workErr = fmt.Errorf("workload: %w (401)", forge.ErrUnauthorized)

	p := New([]Client{fake}, quietLogger(), time.Millisecond, time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(t.Context())
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after an unauthorized account gave up")
	}

	inbox, work := fake.calls()
	if inbox != 1 || work != 1 {
		t.Errorf("calls = inbox %d work %d, want exactly 1 each: a rejected token must not be retried", inbox, work)
	}

	snap := p.Snapshot()
	if len(snap.Accounts) != 1 || snap.Accounts[0].Error == "" {
		t.Errorf("snapshot = %+v, want the account carrying its error", snap.Accounts)
	}
	if !errors.Is(fake.inboxErr, forge.ErrUnauthorized) {
		t.Error("fixture no longer wraps ErrUnauthorized")
	}
}

func TestBackoff(t *testing.T) {
	base := time.Minute
	tests := []struct {
		fails int
		want  time.Duration
	}{
		{1, 2 * time.Minute},
		{2, 4 * time.Minute},
		{3, 8 * time.Minute},
		{4, backoffMax},
		{50, backoffMax},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("fails=%d", tt.fails), func(t *testing.T) {
			if got := backoff(base, tt.fails); got != tt.want {
				t.Errorf("backoff(%v, %d) = %v, want %v", base, tt.fails, got, tt.want)
			}
		})
	}
}

func TestNewClampsIntervals(t *testing.T) {
	p := New([]Client{newFake("a")}, quietLogger(), time.Second, time.Second)
	inbox := time.Duration(p.inboxEvery.Load())
	work := time.Duration(p.workEvery.Load())
	if inbox != minInterval || work != minInterval {
		t.Errorf("intervals = %v/%v, want both clamped to %v", inbox, work, minInterval)
	}
}

func TestSetIntervalMinutes(t *testing.T) {
	p := New([]Client{newFake("a")}, quietLogger(), time.Minute, 5*time.Minute)

	p.SetIntervalMinutes(15)
	if got := time.Duration(p.inboxEvery.Load()); got != 15*time.Minute {
		t.Errorf("inbox interval = %v, want 15m", got)
	}
	if got := time.Duration(p.workEvery.Load()); got != 75*time.Minute {
		t.Errorf("work interval = %v, want 75m: the work query runs at five times the inbox", got)
	}

	// A nonsense rhythm must not wipe the running one.
	p.SetIntervalMinutes(0)
	if got := time.Duration(p.inboxEvery.Load()); got != 15*time.Minute {
		t.Errorf("inbox interval = %v after SetIntervalMinutes(0), want it unchanged", got)
	}
}

func TestSleepReturnsFalseOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if sleep(ctx, time.Hour) {
		t.Error("sleep returned true for a cancelled context")
	}
}

// TestForceRefreshIncrementsGeneration verifies that ForceRefresh()
// increments the generation counter and Wake() does not.
func TestForceRefreshIncrementsGeneration(t *testing.T) {
	fake := newFake("a")
	fake.items = []forge.Notification{{AccountID: "a", ID: "1", Title: "hello", Repo: "o/r"}}
	fake.nextLastMod = "LM1"

	p := New([]Client{fake}, quietLogger(), time.Hour, time.Hour)

	initialGen := p.refreshGeneration.Load()
	if initialGen != 0 {
		t.Errorf("initial generation = %d, want 0", initialGen)
	}

	p.Wake()
	if gen := p.refreshGeneration.Load(); gen != 0 {
		t.Errorf("generation after Wake() = %d, want 0", gen)
	}

	p.ForceRefresh()
	if gen := p.refreshGeneration.Load(); gen != 1 {
		t.Errorf("generation after ForceRefresh() = %d, want 1", gen)
	}

	p.ForceRefresh()
	if gen := p.refreshGeneration.Load(); gen != 2 {
		t.Errorf("generation after second ForceRefresh() = %d, want 2", gen)
	}
}

// TestForceRefreshRefreshesEveryAccount verifies that a single ForceRefresh()
// causes ALL account loops to perform unconditional fetches, not just the
// first one to observe the generation counter.
func TestForceRefreshRefreshesEveryAccount(t *testing.T) {
	fakeA := newFake("a")
	fakeA.items = []forge.Notification{{AccountID: "a", ID: "1", Title: "A1", Repo: "o/r"}}
	fakeA.nextLastMod = "LM-A1"

	fakeB := newFake("b")
	fakeB.items = []forge.Notification{{AccountID: "b", ID: "2", Title: "B1", Repo: "o/r"}}
	fakeB.nextLastMod = "LM-B1"

	p := New([]Client{fakeA, fakeB}, quietLogger(), time.Hour, time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()

	// Wait for both initial inbox fetches
	timeout1 := time.After(5 * time.Second)
	for {
		select {
		case <-fakeA.polled:
		case <-fakeB.polled:
		case <-timeout1:
			cancel()
			<-done
			t.Fatal("poller never called Inbox for both accounts")
		}
		if fakeA.inboxCalls() > 0 && fakeB.inboxCalls() > 0 {
			break
		}
	}

	// Verify initial fetches were unconditional (empty LastModified)
	statesA1 := fakeA.inboxStatesSent()
	statesB1 := fakeB.inboxStatesSent()
	if len(statesA1) != 1 || statesA1[0] != "" {
		cancel()
		<-done
		t.Errorf("account A initial fetch LastModified = %q, want empty (unconditional)", statesA1)
	}
	if len(statesB1) != 1 || statesB1[0] != "" {
		cancel()
		<-done
		t.Errorf("account B initial fetch LastModified = %q, want empty (unconditional)", statesB1)
	}

	fakeA.resetInboxStates()
	fakeB.resetInboxStates()

	// Configure fresh responses for the forced fetch
	fakeA.SetNextResponse([]forge.Notification{{AccountID: "a", ID: "3", Title: "A2", Repo: "o/r"}}, "LM-A2")
	fakeB.SetNextResponse([]forge.Notification{{AccountID: "b", ID: "4", Title: "B2", Repo: "o/r"}}, "LM-B2")

	// Trigger a FORCED refresh
	p.ForceRefresh()

	// Wait for both accounts to be fetched again
	timeout2 := time.After(5 * time.Second)
	for {
		select {
		case <-fakeA.polled:
		case <-fakeB.polled:
		case <-timeout2:
			cancel()
			<-done
			t.Fatal("poller never called Inbox for both accounts after ForceRefresh")
		}
		if fakeA.inboxCalls() > 1 && fakeB.inboxCalls() > 1 {
			break
		}
	}

	// BOTH accounts should have made an unconditional fetch (empty LastModified)
	statesA2 := fakeA.inboxStatesSent()
	statesB2 := fakeB.inboxStatesSent()

	if len(statesA2) != 1 || statesA2[0] != "" {
		cancel()
		<-done
		t.Errorf("account A after ForceRefresh: LastModified = %q, want empty (unconditional)", statesA2)
	}
	if len(statesB2) != 1 || statesB2[0] != "" {
		cancel()
		<-done
		t.Errorf("account B after ForceRefresh: LastModified = %q, want empty (unconditional)", statesB2)
	}

	// Verify poller snapshot contains fresh data for BOTH accounts. Poll the
	// snapshot (which lands asynchronously after the Inbox call signals polled)
	// rather than reading it once and racing the publish.
	deadline := time.Now().Add(5 * time.Second)
	var gotA, gotB *AccountView
	for time.Now().Before(deadline) {
		snap := p.Snapshot()
		gotA, gotB = nil, nil
		for _, v := range snap.Accounts {
			if v.AccountID == "a" {
				gotA = &v
			}
			if v.AccountID == "b" {
				gotB = &v
			}
		}
		if gotA != nil && len(gotA.Notifications) == 1 && gotA.Notifications[0].ID == "3" &&
			gotB != nil && len(gotB.Notifications) == 1 && gotB.Notifications[0].ID == "4" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if gotA == nil || len(gotA.Notifications) != 1 || gotA.Notifications[0].ID != "3" {
		cancel()
		<-done
		t.Errorf("account A snapshot = %+v, want fresh notification A2", gotA.Notifications)
	}
	if gotB == nil || len(gotB.Notifications) != 1 || gotB.Notifications[0].ID != "4" {
		cancel()
		<-done
		t.Errorf("account B snapshot = %+v, want fresh notification B2", gotB.Notifications)
	}

	cancel()
	<-done
}

// TestPostForceConditionalRestored verifies that after a forced refresh,
// normal conditional polling resumes (If-Modified-Since is used again).
func TestPostForceConditionalRestored(t *testing.T) {
	fakeA := newFake("a")
	fakeA.items = []forge.Notification{{AccountID: "a", ID: "1", Title: "A1", Repo: "o/r"}}
	fakeA.nextLastMod = "LM-A1"

	fakeB := newFake("b")
	fakeB.items = []forge.Notification{{AccountID: "b", ID: "2", Title: "B1", Repo: "o/r"}}
	fakeB.nextLastMod = "LM-B1"

	p := New([]Client{fakeA, fakeB}, quietLogger(), time.Hour, time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()

	// Wait for both initial fetches
	timeoutInit := time.After(5 * time.Second)
	for {
		select {
		case <-fakeA.polled:
		case <-fakeB.polled:
		case <-timeoutInit:
			cancel()
			<-done
			t.Fatal("poller never called Inbox for both accounts")
		}
		if fakeA.inboxCalls() > 0 && fakeB.inboxCalls() > 0 {
			break
		}
	}

	fakeA.resetInboxStates()
	fakeB.resetInboxStates()

	// Configure fresh responses for forced fetch
	fakeA.SetNextResponse([]forge.Notification{{AccountID: "a", ID: "3", Title: "A2", Repo: "o/r"}}, "LM-A2")
	fakeB.SetNextResponse([]forge.Notification{{AccountID: "b", ID: "4", Title: "B2", Repo: "o/r"}}, "LM-B2")

	// Force refresh
	p.ForceRefresh()

	// Wait for both forced fetches
	timeoutForce := time.After(5 * time.Second)
	for {
		select {
		case <-fakeA.polled:
		case <-fakeB.polled:
		case <-timeoutForce:
			cancel()
			<-done
			t.Fatal("poller never called Inbox for both accounts after ForceRefresh")
		}
		if fakeA.inboxCalls() > 1 && fakeB.inboxCalls() > 1 {
			break
		}
	}

	// Verify forced fetches were unconditional
	statesAForced := fakeA.inboxStatesSent()
	statesBForced := fakeB.inboxStatesSent()
	if len(statesAForced) != 1 || statesAForced[0] != "" {
		cancel()
		<-done
		t.Errorf("account A forced fetch LastModified = %q, want empty", statesAForced)
	}
	if len(statesBForced) != 1 || statesBForced[0] != "" {
		cancel()
		<-done
		t.Errorf("account B forced fetch LastModified = %q, want empty", statesBForced)
	}

	fakeA.resetInboxStates()
	fakeB.resetInboxStates()

	// Configure for conditional 304: return same LastModified as we just got
	fakeA.SetNextResponse([]forge.Notification{{AccountID: "a", ID: "3", Title: "A2", Repo: "o/r"}}, "LM-A2")
	fakeB.SetNextResponse([]forge.Notification{{AccountID: "b", ID: "4", Title: "B2", Repo: "o/r"}}, "LM-B2")

	// Wake() must NOT advance the generation, so the next poll is conditional
	// (keeps LastModified) rather than forced. This is the "normal poll resumes
	// after a forced refresh" behaviour. Retry the wake until the loops are
	// parked again, since the loops may not be in waitForced yet right after
	// their fetch.
	timeoutNormal := time.After(5 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		p.Wake()
		select {
		case <-fakeA.polled:
		case <-fakeB.polled:
		case <-ticker.C:
		case <-timeoutNormal:
			cancel()
			<-done
			t.Fatal("poller never called Inbox for normal poll after forced fetch")
		}
		if fakeA.inboxCalls() > 2 && fakeB.inboxCalls() > 2 {
			break
		}
	}

	// Next normal polls should be CONDITIONAL (send LastModified)
	statesANormal := fakeA.inboxStatesSent()
	statesBNormal := fakeB.inboxStatesSent()

	// Both should have sent their new LastModified values
	if len(statesANormal) == 0 {
		cancel()
		<-done
		t.Errorf("account A did no conditional poll after forced refresh")
	}
	for _, lm := range statesANormal {
		if lm != "LM-A2" {
			cancel()
			<-done
			t.Errorf("account A normal poll after forced: LastModified = %q, want LM-A2 (conditional)", statesANormal)
		}
	}
	if len(statesBNormal) == 0 {
		cancel()
		<-done
		t.Errorf("account B did no conditional poll after forced refresh")
	}
	for _, lm := range statesBNormal {
		if lm != "LM-B2" {
			cancel()
			<-done
			t.Errorf("account B normal poll after forced: LastModified = %q, want LM-B2 (conditional)", statesBNormal)
		}
	}

	// Verify they got 304 (unchanged=true) by checking snapshot still has A2/B2
	snap := p.Snapshot()
	for _, v := range snap.Accounts {
		if v.AccountID == "a" {
			if len(v.Notifications) != 1 || v.Notifications[0].ID != "3" {
				cancel()
				<-done
				t.Errorf("account A after conditional poll: %+v, want unchanged A2", v.Notifications)
			}
		}
		if v.AccountID == "b" {
			if len(v.Notifications) != 1 || v.Notifications[0].ID != "4" {
				cancel()
				<-done
				t.Errorf("account B after conditional poll: %+v, want unchanged B2", v.Notifications)
			}
		}
	}

	cancel()
	<-done
}

// TestWaitForcedNoGoroutineLeak proves that waitForced does not accumulate
// goroutines over many calls: the transient per-call bridge goroutine must
// terminate on its own (timer/ctx/stop). A broken implementation that leaked
// would keep NumGoroutine climbing with the iteration count.
func TestWaitForcedNoGoroutineLeak(t *testing.T) {
	p := New([]Client{newFake("a")}, quietLogger(), time.Hour, time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	runtime.GC()
	before := runtime.NumGoroutine()

	// 2000 sequential short waits: each spawns one bridge goroutine. If any
	// leaked, the count would grow roughly one per call.
	for i := 0; i < 2000; i++ {
		if _, ok := p.waitForced(ctx, time.Millisecond, 0); !ok {
			t.Fatal("waitForced cancelled unexpectedly")
		}
	}
	// Give any straggler goroutines a moment to be scheduled out.
	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()

	const tolerance = 8 // small slack for runtime/runtime internals
	if after > before+tolerance {
		t.Fatalf("goroutines grew from %d to %d over %d waits; waitForced likely leaks",
			before, after, 2000)
	}
}

// TestRapidForceRefreshBounded proves many back-to-back ForceRefresh calls do
// not deadlock, stall the loops, or accumulate waitForced goroutines across a
// running poller.
func TestRapidForceRefreshBounded(t *testing.T) {
	fakeA := newFake("a")
	fakeA.nextLastMod = "LM-A1"
	fakeB := newFake("b")
	fakeB.nextLastMod = "LM-B1"

	p := New([]Client{fakeA, fakeB}, quietLogger(), time.Hour, time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()

	// Wait for both to be up and parked.
	timeout := time.After(5 * time.Second)
	for {
		select {
		case <-fakeA.polled:
		case <-fakeB.polled:
		case <-timeout:
			cancel()
			<-done
			t.Fatal("poller never called Inbox for both accounts")
		}
		if fakeA.inboxCalls() > 0 && fakeB.inboxCalls() > 0 {
			break
		}
	}

	// A burst of forces, faster than the loops can consume, must not wedge the
	// loops or deadlock: each woken loop must perform an unconditional fetch for
	// the latest generation, and a later poll must still work.
	const burst = 500
	for i := 0; i < burst; i++ {
		p.ForceRefresh()
	}

	// Each loop must do its initial fetch plus at least one forced (unconditional)
	// fetch. Break as soon as both have advanced, keeping the deadline as a
	// safety net rather than letting it run to completion.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fakeA.inboxCalls() >= 2 && fakeB.inboxCalls() >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	// Initial fetch is unconditional; the forced fetch after the burst must also
	// have no If-Modified-Since, proving the force reached the account.
	for _, lm := range fakeA.inboxStatesSent() {
		if lm != "" {
			t.Fatalf("account A sent conditional LastModified=%q under a force burst", lm)
		}
	}
	for _, lm := range fakeB.inboxStatesSent() {
		if lm != "" {
			t.Fatalf("account B sent conditional LastModified=%q under a force burst", lm)
		}
	}
}
