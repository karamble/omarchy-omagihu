// Package poll runs one scheduler goroutine per account and keeps the most
// recent view of every account in memory. Readers take a snapshot; nothing
// blocks on the network.
package poll

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/karamble/omarchy-omagihu/accounts"
	"github.com/karamble/omarchy-omagihu/forge"
)

// Defaults chosen so an idle machine costs almost nothing: notifications lean on
// conditional requests and GitHub's own X-Poll-Interval, and the workload query
// is a single GraphQL call.
const (
	DefaultInboxInterval = time.Minute
	DefaultWorkInterval  = 5 * time.Minute
	minInterval          = 30 * time.Second
	backoffMax           = 15 * time.Minute
	// How often a paused loop wakes to re-read the switch. This costs a flag
	// read and nothing else: no request leaves the machine while paused.
	pausedCheck = 5 * time.Second
)

// Client is what the poller needs from a forge. Declared here, in the consumer,
// so forge stays free of interfaces nobody implements twice.
type Client interface {
	Account() accounts.Account
	Inbox(ctx context.Context, state forge.InboxState) (items []forge.Notification, next forge.InboxState, unchanged bool, rate forge.Rate, err error)
	Work(ctx context.Context) (forge.Workload, forge.Rate, error)
}

// AccountView is the latest known state for one identity.
type AccountView struct {
	AccountID      string               `json:"accountId"`
	Login          string               `json:"login"`
	Notifications  []forge.Notification `json:"notifications"`
	AuthoredPRs    []forge.PullRequest  `json:"authoredPrs"`
	ReviewRequests []forge.PullRequest  `json:"reviewRequests"`
	AssignedIssues []forge.Issue        `json:"assignedIssues"`
	MergedPRs      []forge.PullRequest  `json:"mergedPrs"`
	Rate           forge.Rate           `json:"rate"`
	InboxAt        time.Time            `json:"inboxAt,omitzero"`
	WorkAt         time.Time            `json:"workAt,omitzero"`
	Error          string               `json:"error,omitempty"`
}

// Snapshot is a consistent read across every account.
type Snapshot struct {
	TakenAt  time.Time     `json:"takenAt"`
	Accounts []AccountView `json:"accounts"`
}

// Poller owns the per-account goroutines and the shared snapshot.
type Poller struct {
	clients []Client
	logger  *slog.Logger

	// Held as nanosecond counters so the running loops can pick up a new
	// rhythm without a restart.
	inboxEvery atomic.Int64
	workEvery  atomic.Int64

	mu    sync.Mutex
	views map[string]*AccountView

	snap   atomic.Pointer[Snapshot]
	paused atomic.Bool

	// wakeMu guards wakeCh, which is closed to broadcast a wake to every
	// waiting loop and then replaced. A buffered channel would only release
	// one waiter; closing releases them all, which is what "refresh now" has
	// to mean when several accounts are asleep.
	wakeMu sync.Mutex
	wakeCh chan struct{}
}

// Refresh cuts short every sleeping loop so the next poll happens now. A rhythm
// of an hour used to mean an hour, even after the network came back.
func (p *Poller) Refresh() {
	p.wakeMu.Lock()
	defer p.wakeMu.Unlock()
	if p.wakeCh == nil {
		return
	}
	close(p.wakeCh)
	p.wakeCh = make(chan struct{})
}

// wakeSignal hands a loop the current broadcast channel to wait on.
func (p *Poller) wakeSignal() <-chan struct{} {
	p.wakeMu.Lock()
	defer p.wakeMu.Unlock()
	return p.wakeCh
}

// wait sleeps for d, returning false only when the context ends. A wake
// broadcast returns early, so the caller polls immediately.
func (p *Poller) wait(ctx context.Context, d time.Duration) bool {
	wake := p.wakeSignal()
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-wake:
		return true
	case <-t.C:
		return true
	}
}

// SetPaused stops or resumes every account loop. While paused the poller makes
// no network calls whatsoever and the last snapshot stands.
func (p *Poller) SetPaused(paused bool) {
	p.paused.Store(paused)
	if paused {
		p.logger.Info("remote polling paused: no outbound requests")
		return
	}
	p.logger.Info("remote polling resumed")
	// Waking should poll now, not when the interrupted sleep would have ended.
	p.Refresh()
}

// Paused reports the current state of the switch.
func (p *Poller) Paused() bool { return p.paused.Load() }

// New builds a poller over already constructed clients.
func New(clients []Client, logger *slog.Logger, inboxEvery, workEvery time.Duration) *Poller {
	p := &Poller{
		clients: clients,
		logger:  logger,
		views:   make(map[string]*AccountView, len(clients)),
		wakeCh:  make(chan struct{}),
	}
	p.SetIntervals(inboxEvery, workEvery)
	for _, c := range clients {
		a := c.Account()
		p.views[a.ID] = &AccountView{AccountID: a.ID, Login: a.Login}
	}
	p.publish()
	return p
}

// SetIntervals changes the polling rhythm of the running loops. The work query
// is deliberately slower than the inbox: it is one GraphQL call against a much
// smaller budget, and it changes far less often.
func (p *Poller) SetIntervals(inbox, work time.Duration) {
	p.inboxEvery.Store(int64(max(inbox, minInterval)))
	p.workEvery.Store(int64(max(work, minInterval)))
	p.logger.Info("polling rhythm set",
		"inbox", max(inbox, minInterval), "work", max(work, minInterval))
	// A new rhythm applies from now, not after the old one finishes waiting.
	p.Refresh()
}

// SetIntervalMinutes applies one rhythm to both loops, the shape the panel's
// chips expose. The work loop runs at five times the inbox interval.
func (p *Poller) SetIntervalMinutes(minutes int) {
	if minutes <= 0 {
		return
	}
	inbox := time.Duration(minutes) * time.Minute
	p.SetIntervals(inbox, inbox*5)
}

// Snapshot returns the current view. It never blocks and never returns nil.
func (p *Poller) Snapshot() *Snapshot {
	if s := p.snap.Load(); s != nil {
		return s
	}
	return &Snapshot{TakenAt: time.Now()}
}

// Run starts every account loop and returns when ctx is done. Each loop owns its
// own goroutine and exits with the context, so nothing is left running.
func (p *Poller) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, c := range p.clients {
		wg.Go(func() { p.inboxLoop(ctx, c) })
		wg.Go(func() { p.workLoop(ctx, c) })
	}
	wg.Wait()
}

func (p *Poller) inboxLoop(ctx context.Context, c Client) {
	var (
		state forge.InboxState
		fails int
	)
	for {
		if p.paused.Load() {
			if !p.wait(ctx, pausedCheck) {
				return
			}
			continue
		}
		items, next, unchanged, rate, err := c.Inbox(ctx, state)
		if ctx.Err() != nil {
			return
		}
		state = next

		switch {
		case err != nil:
			fails++
			p.update(c, func(v *AccountView) {
				v.Error = err.Error()
				v.Rate = rate
			})
			if errors.Is(err, forge.ErrUnauthorized) {
				p.logger.Error("inbox: token rejected, pausing this account",
					"account", c.Account().ID, "err", err)
				return
			}
			p.logger.Warn("inbox poll failed", "account", c.Account().ID, "err", err, "fails", fails)
		case unchanged:
			fails = 0
			p.logger.Debug("inbox unchanged (304)",
				"account", c.Account().ID, "rateRemaining", rate.Remaining)
			p.update(c, func(v *AccountView) {
				v.Error = ""
				v.Rate = rate
				v.InboxAt = time.Now()
			})
		default:
			fails = 0
			p.logger.Debug("inbox fetched",
				"account", c.Account().ID, "items", len(items), "rateRemaining", rate.Remaining)
			p.update(c, func(v *AccountView) {
				v.Error = ""
				v.Notifications = items
				v.Rate = rate
				v.InboxAt = time.Now()
			})
		}

		wait := time.Duration(p.inboxEvery.Load())
		// Honour the server's pacing hint when it asks for a slower cadence.
		if state.PollInterval > wait {
			wait = state.PollInterval
		}
		if fails > 0 {
			wait = backoff(wait, fails)
		}
		if !p.wait(ctx, wait) {
			return
		}
	}
}

func (p *Poller) workLoop(ctx context.Context, c Client) {
	var fails int
	for {
		if p.paused.Load() {
			if !p.wait(ctx, pausedCheck) {
				return
			}
			continue
		}
		work, rate, err := c.Work(ctx)
		if ctx.Err() != nil {
			return
		}

		if err != nil {
			fails++
			p.update(c, func(v *AccountView) {
				v.Error = err.Error()
				v.Rate = rate
			})
			if errors.Is(err, forge.ErrUnauthorized) {
				p.logger.Error("workload: token rejected, pausing this account",
					"account", c.Account().ID, "err", err)
				return
			}
			p.logger.Warn("workload poll failed", "account", c.Account().ID, "err", err, "fails", fails)
		} else {
			fails = 0
			p.update(c, func(v *AccountView) {
				v.Error = ""
				v.Login = work.Login
				v.AuthoredPRs = work.AuthoredPRs
				v.ReviewRequests = work.ReviewRequests
				v.AssignedIssues = work.AssignedIssues
				v.MergedPRs = work.MergedPRs
				v.Rate = rate
				v.WorkAt = time.Now()
			})
		}

		wait := time.Duration(p.workEvery.Load())
		if fails > 0 {
			wait = backoff(wait, fails)
		}
		if !p.wait(ctx, wait) {
			return
		}
	}
}

// update mutates one account's view and republishes the snapshot.
func (p *Poller) update(c Client, mutate func(*AccountView)) {
	p.mu.Lock()
	defer p.mu.Unlock()

	id := c.Account().ID
	v, ok := p.views[id]
	if !ok {
		v = &AccountView{AccountID: id, Login: c.Account().Login}
		p.views[id] = v
	}
	mutate(v)
	p.publishLocked()
}

func (p *Poller) publish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.publishLocked()
}

// publishLocked deep-copies the views so a snapshot handed to a reader can never
// be mutated underneath it. Callers must hold p.mu.
func (p *Poller) publishLocked() {
	snap := &Snapshot{TakenAt: time.Now()}
	for _, c := range p.clients {
		v, ok := p.views[c.Account().ID]
		if !ok {
			continue
		}
		snap.Accounts = append(snap.Accounts, *v)
	}
	p.snap.Store(snap)
}

// backoff grows the wait on consecutive failures, capped so a recovered network
// is noticed within a quarter hour.
func backoff(base time.Duration, fails int) time.Duration {
	d := base
	for range min(fails, 6) {
		d *= 2
		if d >= backoffMax {
			return backoffMax
		}
	}
	return d
}

// sleep waits for d, reporting false if the context ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
