package poll

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

	mu        sync.Mutex
	inboxCall int
	workCall  int

	inboxErr error
	workErr  error

	items []forge.Notification
	work  forge.Workload

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
	err, items := f.inboxErr, f.items
	f.mu.Unlock()

	select {
	case f.polled <- struct{}{}:
	default:
	}
	if err != nil {
		return nil, state, false, forge.Rate{}, err
	}
	return items, state, false, forge.Rate{Remaining: 4999, Limit: 5000}, nil
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

	// Both loops fire immediately, so wait for the work result to land too.
	deadline := time.Now().Add(5 * time.Second)
	var view AccountView
	for time.Now().Before(deadline) {
		snap := p.Snapshot()
		if len(snap.Accounts) == 1 && len(snap.Accounts[0].AuthoredPRs) == 1 {
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
