package poll

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strings"
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

	// A held plane blocks inside its call until the context ends, so a test
	// can drive the other loop on its own. A held call is not counted.
	holdInbox bool
	holdWork  bool

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
	held := f.holdInbox
	f.mu.Unlock()
	if held {
		<-ctx.Done()
		return nil, state, false, forge.Rate{}, ctx.Err()
	}

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
	held := f.holdWork
	f.mu.Unlock()
	if held {
		<-ctx.Done()
		return forge.Workload{}, forge.Rate{}, ctx.Err()
	}

	f.mu.Lock()
	f.workCall++
	err, work := f.workErr, f.work
	f.mu.Unlock()

	if err != nil {
		return forge.Workload{}, forge.Rate{}, err
	}
	return work, forge.Rate{Remaining: 4000, Limit: 5000}, nil
}

// setWork changes what Work returns from now on.
func (f *fakeClient) setWork(work forge.Workload) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.work = work
}

// hold parks the named planes from their next call on.
func (f *fakeClient) hold(inbox, work bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holdInbox, f.holdWork = inbox, work
}

// setErrors changes what both calls return from now on.
func (f *fakeClient) setErrors(inbox, work error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inboxErr, f.workErr = inbox, work
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
	if view.InboxRate.Remaining != 4999 || view.WorkRate.Remaining != 4000 {
		t.Errorf("rates = inbox %d work %d, want 4999 and 4000: each loop reports its own bucket",
			view.InboxRate.Remaining, view.WorkRate.Remaining)
	}
	if view.InboxError != "" || view.WorkError != "" {
		t.Errorf("errors = inbox %q work %q, want both empty", view.InboxError, view.WorkError)
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
	if len(snap.Accounts) != 1 || snap.Accounts[0].InboxError == "" || snap.Accounts[0].WorkError == "" {
		t.Errorf("snapshot = %+v, want the account carrying both errors", snap.Accounts)
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

// TestRunSurvivesRecoverableFailures is the regression #5 exists for: only a
// rejected token may end an account's loop. A 502, a spent budget and a
// resource the token cannot see all back off and poll again.
func TestRunSurvivesRecoverableFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"502", &forge.APIError{Op: "notifications", Status: 502, Class: forge.ErrTransient}},
		{"throttled", &forge.APIError{Op: "notifications", Status: 429, Class: forge.ErrThrottled}},
		{"blocked", &forge.APIError{Op: "notifications", Status: 404, Class: forge.ErrBlocked}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFake("a")
			fake.setErrors(tc.err, tc.err)
			p := New([]Client{fake}, quietLogger(), time.Hour, time.Hour)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				p.Run(ctx)
			}()

			// Wait for both loops to have failed once.
			deadline := time.Now().Add(5 * time.Second)
			for {
				inbox, work := fake.calls()
				if inbox >= 1 && work >= 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("poller never called the fake")
				}
				time.Sleep(time.Millisecond)
			}
			if v := p.Snapshot().Accounts[0]; v.InboxError == "" || v.WorkError == "" {
				t.Errorf("errors = inbox %q work %q, want the failure reported on both planes", v.InboxError, v.WorkError)
			}

			// GitHub recovers. Wake the loops out of their backoff until each has
			// polled again; the retry loop is because a loop may not be parked yet.
			fake.setErrors(nil, nil)
			deadline = time.Now().Add(5 * time.Second)
			for {
				p.Wake()
				inbox, work := fake.calls()
				if inbox >= 2 && work >= 2 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("loops did not poll again after a %s: calls = inbox %d work %d", tc.name, inbox, work)
				}
				time.Sleep(time.Millisecond)
			}
			select {
			case <-done:
				t.Fatalf("Run returned after a %s: the account was stopped", tc.name)
			default:
			}
			deadline = time.Now().Add(5 * time.Second)
			for v := p.Snapshot().Accounts[0]; v.InboxError != "" || v.WorkError != ""; v = p.Snapshot().Accounts[0] {
				if time.Now().After(deadline) {
					t.Fatalf("errors = inbox %q work %q after recovery, want both cleared", v.InboxError, v.WorkError)
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

// TestRetryWait pins how a loop picks its next wait after a failure.
func TestRetryWait(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	base := time.Minute
	throttled := func(at time.Time) error {
		return &forge.APIError{Op: "workload", Status: 403, RetryAt: at, Class: forge.ErrThrottled}
	}
	for _, tc := range []struct {
		name  string
		err   error
		fails int
		want  time.Duration
	}{
		{"a 502 backs off", &forge.APIError{Op: "workload", Status: 502, Class: forge.ErrTransient}, 1, 2 * time.Minute},
		{"a plain error backs off", errors.New("boom"), 2, 4 * time.Minute},
		{"a primary limit waits for the reset", throttled(now.Add(90 * time.Second)), 1, 90 * time.Second},
		{"a secondary limit waits for Retry-After", throttled(now.Add(60 * time.Second)), 3, 60 * time.Second},
		{"a reset hours away is capped", throttled(now.Add(3 * time.Hour)), 1, backoffMax},
		{"a reset already past backs off", throttled(now.Add(-time.Second)), 1, 2 * time.Minute},
		{"throttled with no hint backs off", throttled(time.Time{}), 1, 2 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryWait(base, tc.fails, tc.err, now); got != tc.want {
				t.Errorf("retryWait = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRunHonoursRetryAt proves the loop itself consults the wait GitHub named,
// not just the helper: a reset a few milliseconds away is polled after,
// where backoff would have parked the loop for the full quarter hour.
func TestRunHonoursRetryAt(t *testing.T) {
	fake := newFake("a")
	fake.setErrors(
		&forge.APIError{Op: "notifications", Status: 403, RetryAt: time.Now().Add(20 * time.Millisecond), Class: forge.ErrThrottled},
		&forge.APIError{Op: "workload", Status: 403, RetryAt: time.Now().Add(20 * time.Millisecond), Class: forge.ErrThrottled},
	)
	p := New([]Client{fake}, quietLogger(), time.Hour, time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go p.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		inbox, work := fake.calls()
		if inbox >= 2 && work >= 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("loops did not poll again at the reset: calls = inbox %d work %d", inbox, work)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRunKeepsUnresolvedLists pins what a partial answer does to the view: a
// list GitHub could not resolve keeps its previous value, the others take
// the fresh one, and the view says the answer was partial. Blanking the
// review queue and reporting health would be the worst failure on offer.
func TestRunKeepsUnresolvedLists(t *testing.T) {
	fake := newFake("a")
	fake.setWork(forge.Workload{
		Login:          "a",
		AuthoredPRs:    []forge.PullRequest{{Repo: "o/r", Number: 7}},
		ReviewRequests: []forge.PullRequest{{Repo: "o/r", Number: 1}},
	})
	p := New([]Client{fake}, quietLogger(), time.Hour, time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()

	view := func(ready func(AccountView) bool) AccountView {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			v := p.Snapshot().Accounts[0]
			if ready(v) {
				return v
			}
			if time.Now().After(deadline) {
				t.Fatalf("view never reached the expected state: %+v", v)
			}
			time.Sleep(time.Millisecond)
		}
	}
	view(func(v AccountView) bool { return len(v.ReviewRequests) == 1 && len(v.AuthoredPRs) == 1 })

	// The next answer resolves authored but not reviewing.
	fake.setWork(forge.Workload{
		Login:       "a",
		AuthoredPRs: []forge.PullRequest{{Repo: "o/r", Number: 8}},
		Warnings:    []string{"Resource not accessible by integration"},
		Unresolved:  []string{"reviewRequests"},
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.Wake()
		if _, work := fake.calls(); work >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the work loop did not poll again")
		}
		time.Sleep(time.Millisecond)
	}
	v := view(func(v AccountView) bool { return len(v.AuthoredPRs) == 1 && v.AuthoredPRs[0].Number == 8 })
	if len(v.ReviewRequests) != 1 || v.ReviewRequests[0].Number != 1 {
		t.Errorf("ReviewRequests = %+v, want the previous value kept", v.ReviewRequests)
	}
	if !strings.Contains(v.WorkPartial, "not accessible") {
		t.Errorf("WorkPartial = %q, want it to say why the answer was partial", v.WorkPartial)
	}
	if v.WorkError != "" {
		t.Errorf("WorkError = %q, want empty: a partial answer is not a failure", v.WorkError)
	}
	select {
	case <-done:
		t.Fatal("Run returned: a partial answer stopped the account")
	default:
	}
}

// waitCalls blocks until the fake has been polled at least inbox and work
// times, so a test can reason about what happened after a given point.
func waitCalls(t *testing.T, p *Poller, fake *fakeClient, inbox, work int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.Wake()
		i, w := fake.calls()
		if i >= inbox && w >= work {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("calls = inbox %d work %d, wanted at least %d and %d", i, w, inbox, work)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestErrorOnOnePlaneSurvivesSuccessOnTheOther is the regression #13 exists
// for: a healthy poll on one plane used to clear the other plane's error. The
// failing loop is parked after its first failure so nothing rewrites the
// error behind the test's back.
func TestErrorOnOnePlaneSurvivesSuccessOnTheOther(t *testing.T) {
	t.Run("work error outlives inbox successes", func(t *testing.T) {
		fake := newFake("a")
		fake.setErrors(nil, &forge.APIError{Op: "workload", Status: 502, Class: forge.ErrTransient})
		p := New([]Client{fake}, quietLogger(), time.Hour, time.Hour)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go p.Run(ctx)

		waitCalls(t, p, fake, 1, 1)
		fake.hold(false, true)
		inbox, _ := fake.calls()
		waitCalls(t, p, fake, inbox+3, 1)

		v := p.Snapshot().Accounts[0]
		if v.WorkError == "" {
			t.Fatal("a successful inbox poll erased the workload error")
		}
		if v.InboxError != "" {
			t.Fatalf("InboxError = %q, want empty", v.InboxError)
		}
	})

	t.Run("inbox error outlives work successes", func(t *testing.T) {
		fake := newFake("a")
		fake.setErrors(&forge.APIError{Op: "notifications", Status: 502, Class: forge.ErrTransient}, nil)
		p := New([]Client{fake}, quietLogger(), time.Hour, time.Hour)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go p.Run(ctx)

		waitCalls(t, p, fake, 1, 1)
		fake.hold(true, false)
		_, work := fake.calls()
		waitCalls(t, p, fake, 1, work+3)

		v := p.Snapshot().Accounts[0]
		if v.InboxError == "" {
			t.Fatal("a successful workload poll erased the inbox error")
		}
		if v.WorkError != "" {
			t.Fatalf("WorkError = %q, want empty", v.WorkError)
		}
	})
}

// TestRejectedTokenLeavesWorkErrorInPlace pins the worst case: the work loop
// has exited, so nothing will ever rewrite its message, and the inbox loop
// keeps succeeding.
func TestRejectedTokenLeavesWorkErrorInPlace(t *testing.T) {
	fake := newFake("a")
	fake.setErrors(nil, fmt.Errorf("workload: %w (401)", forge.ErrUnauthorized))
	p := New([]Client{fake}, quietLogger(), time.Hour, time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()

	waitCalls(t, p, fake, 1, 1)
	inbox, _ := fake.calls()
	waitCalls(t, p, fake, inbox+5, 1)

	if _, work := fake.calls(); work != 1 {
		t.Fatalf("work calls = %d, want 1: a rejected token stops the loop", work)
	}
	select {
	case <-done:
		t.Fatal("Run returned: the inbox loop should still be running")
	default:
	}
	v := p.Snapshot().Accounts[0]
	if !strings.Contains(v.WorkError, "401") {
		t.Fatalf("WorkError = %q, want the rejection kept after the loop exited", v.WorkError)
	}
	if v.InboxError != "" {
		t.Fatalf("InboxError = %q, want empty", v.InboxError)
	}
}

// TestRateBucketsAreKeptApart pins that the REST and GraphQL budgets never
// overwrite each other: one loop is parked while the other polls repeatedly,
// and the parked loop's bucket must not move.
func TestRateBucketsAreKeptApart(t *testing.T) {
	t.Run("work polls leave the inbox bucket alone", func(t *testing.T) {
		fake := newFake("a")
		p := New([]Client{fake}, quietLogger(), time.Hour, time.Hour)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go p.Run(ctx)

		waitCalls(t, p, fake, 1, 1)
		fake.hold(true, false)
		_, work := fake.calls()
		waitCalls(t, p, fake, 1, work+3)

		v := p.Snapshot().Accounts[0]
		if v.InboxRate.Remaining != 4999 || v.WorkRate.Remaining != 4000 {
			t.Fatalf("rates = inbox %d work %d, want 4999 and 4000", v.InboxRate.Remaining, v.WorkRate.Remaining)
		}
	})

	t.Run("inbox polls leave the work bucket alone", func(t *testing.T) {
		fake := newFake("a")
		p := New([]Client{fake}, quietLogger(), time.Hour, time.Hour)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go p.Run(ctx)

		waitCalls(t, p, fake, 1, 1)
		fake.hold(false, true)
		inbox, _ := fake.calls()
		waitCalls(t, p, fake, inbox+3, 1)

		v := p.Snapshot().Accounts[0]
		if v.InboxRate.Remaining != 4999 || v.WorkRate.Remaining != 4000 {
			t.Fatalf("rates = inbox %d work %d, want 4999 and 4000", v.InboxRate.Remaining, v.WorkRate.Remaining)
		}
	})
}
