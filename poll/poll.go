// Package poll runs one scheduler goroutine per account and keeps the most
// recent view of every account in memory. Readers take a snapshot; nothing
// blocks on the network.
package poll

import (
	"context"
	"errors"
	"log/slog"
	"strings"
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
	AccountID string `json:"accountId"`
	Login     string `json:"login"`
	// Organizations the account belongs to, kept from the last answer that
	// delivered them, so a cycle that loses the list does not read as
	// leaving every organisation.
	Organizations  []string             `json:"organizations,omitempty"`
	Notifications  []forge.Notification `json:"notifications"`
	AuthoredPRs    []forge.PullRequest  `json:"authoredPrs"`
	ReviewRequests []forge.PullRequest  `json:"reviewRequests"`
	AssignedIssues []forge.Issue        `json:"assignedIssues"`
	AuthoredIssues []forge.Issue        `json:"authoredIssues"`
	MergedPRs      []forge.PullRequest  `json:"mergedPrs"`
	// REST and GraphQL are separate budgets at GitHub, so each loop reports
	// its own.
	InboxRate forge.Rate `json:"inboxRate"`
	WorkRate  forge.Rate `json:"workRate"`
	InboxAt   time.Time  `json:"inboxAt,omitzero"`
	WorkAt    time.Time  `json:"workAt,omitzero"`
	// Each loop writes and clears only its own error, so a healthy poll on
	// one plane never hides a failure on the other.
	InboxError string `json:"inboxError,omitempty"`
	WorkError  string `json:"workError,omitempty"`
	// WorkPartial names what the last workload answer left out. The poll
	// succeeded and the lists it delivered are current; only the ones named
	// here are stale.
	WorkPartial string `json:"workPartial,omitempty"`
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

	snap              atomic.Pointer[Snapshot]
	paused            atomic.Bool
	refreshGeneration atomic.Uint64 // incremented by ForceRefresh(); each loop tracks its own seen generation

	// wakeMu guards wakeTick and the broadcast below, so a decision to wake and
	// the wait itself are atomic: a Wake() or ForceRefresh() can never be lost
	// because a loop happened not to be waiting at that instant. (The old
	// close-and-recreate channel leak got goroutines stuck between a fetch and
	// a wait, missing the force until the next natural cycle.)
	wakeMu   sync.Mutex
	wakeTick uint64
	wakeCond *sync.Cond
}

// Wake cuts short every sleeping loop so the next poll happens now.
// A rhythm of an hour used to mean an hour, even after the network came back.
// This does NOT force an unconditional GitHub fetch; it merely wakes the loops.
// Use ForceRefresh() for user-initiated refreshes that must bypass conditional cache.
func (p *Poller) Wake() {
	p.wakeMu.Lock()
	p.wakeTick++
	p.wakeCond.Broadcast()
	p.wakeMu.Unlock()
}

// ForceRefresh wakes all loops AND forces the next inbox fetch for each
// account to be unconditional (no If-Modified-Since), bypassing any stale
// 304 cache. This is for user-initiated refreshes via the panel/API.
func (p *Poller) ForceRefresh() {
	p.refreshGeneration.Add(1)
	p.Wake()
}

// wait sleeps for d, returning false only when the context ends. A wake
// broadcast returns early, so the caller polls immediately. It is
// generation-agnostic (used by the workLoop and paused checks), so a
// ForceRefresh generation bump must not turn it into a busy loop.
func (p *Poller) wait(ctx context.Context, d time.Duration) bool {
	_, ok := p.waitForced(ctx, d, p.refreshGeneration.Load())
	return ok
}

// waitForced sleeps for d, waking early on a Wake() or a ForceRefresh()
// generation change (so the inbox loop can clear its conditional state and
// fetch unconditionally). It returns the latest refreshGeneration so the loop
// records what it has seen.
func (p *Poller) waitForced(ctx context.Context, d time.Duration, seenGen uint64) (uint64, bool) {
	timer := time.NewTimer(d)
	defer timer.Stop()

	// timedOut is closed when the deadline (or context) fires; it is checked by
	// the parked goroutine so a timeout looks like a wake to the Cond.
	timedOut := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-timer.C:
		case <-ctx.Done():
		case <-stop:
			return
		}
		close(timedOut)
		// Wake the parking goroutine; if it is mid-loop and not parked yet, the
		// broadcast is harmless because waitForced re-checks its gates below.
		p.wakeMu.Lock()
		p.wakeCond.Broadcast()
		p.wakeMu.Unlock()
	}()

	p.wakeMu.Lock()
	lastTick := p.wakeTick
	for {
		cur := p.refreshGeneration.Load()
		if cur != seenGen {
			p.wakeMu.Unlock()
			return cur, true
		}
		if ctx.Err() != nil {
			p.wakeMu.Unlock()
			return cur, false
		}
		select {
		case <-timedOut:
			p.wakeMu.Unlock()
			return cur, true
		default:
		}
		if p.wakeTick != lastTick {
			p.wakeMu.Unlock()
			return cur, true
		}
		p.wakeCond.Wait() // releases wakeMu while parked, reacquires on wake
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
	// Use Wake() (not ForceRefresh()) so conditional requests are preserved.
	p.Wake()
}

// Paused reports the current state of the switch.
func (p *Poller) Paused() bool { return p.paused.Load() }

// New builds a poller over already constructed clients.
func New(clients []Client, logger *slog.Logger, inboxEvery, workEvery time.Duration) *Poller {
	p := &Poller{
		clients: clients,
		logger:  logger,
		views:   make(map[string]*AccountView, len(clients)),
	}
	p.wakeCond = sync.NewCond(&p.wakeMu)
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
	// Use Wake() (not ForceRefresh()) so conditional requests are preserved.
	p.Wake()
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
		state   forge.InboxState
		fails   int
		seenGen uint64
	)
	for {
		if p.paused.Load() {
			if !p.wait(ctx, pausedCheck) {
				return
			}
			continue
		}
		// If a forced refresh occurred since the generation we last acted on,
		// clear LastModified to force an unconditional fetch and bypass any
		// stale 304 cache. seenGen is advanced only after the fetch completes,
		// so the clear is applied even when the force landed between a fetch
		// and this loop's wait.
		currentGen := p.refreshGeneration.Load()
		forced := currentGen != seenGen
		if forced {
			state.LastModified = ""
			p.logger.Debug("inbox: forced unconditional fetch", "generation", currentGen, "account", c.Account().ID)
		}
		items, next, unchanged, rate, err := c.Inbox(ctx, state)
		if ctx.Err() != nil {
			return
		}
		state = next
		if forced {
			seenGen = currentGen
		}

		switch {
		case err != nil:
			fails++
			p.update(c, func(v *AccountView) {
				v.InboxError = err.Error()
				v.InboxRate = rate
			})
			// Only a rejected token stops the loop. Everything else, a spent
			// budget, a 502, a resource the token cannot see, backs off.
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
				v.InboxError = ""
				v.InboxRate = rate
				v.InboxAt = time.Now()
			})
		default:
			fails = 0
			p.logger.Debug("inbox fetched",
				"account", c.Account().ID, "items", len(items), "rateRemaining", rate.Remaining)
			p.update(c, func(v *AccountView) {
				v.InboxError = ""
				v.Notifications = items
				v.InboxRate = rate
				v.InboxAt = time.Now()
			})
		}

		wait := time.Duration(p.inboxEvery.Load())
		// Honour the server's pacing hint when it asks for a slower cadence.
		if state.PollInterval > wait {
			wait = state.PollInterval
		}
		if fails > 0 {
			wait = retryWait(wait, fails, err, time.Now())
		}
		// Force-aware wait: returns immediately if a ForceRefresh() bumped the
		// generation while this loop slept or was between calls, so no account
		// misses an unconditional refresh. seenGen is only advanced after a fetch
		// above; leaving it here means the top of the loop performs the clear.
		if _, ok := p.waitForced(ctx, wait, seenGen); !ok {
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
				v.WorkError = err.Error()
				v.WorkRate = rate
			})
			if errors.Is(err, forge.ErrUnauthorized) {
				p.logger.Error("workload: token rejected, pausing this account",
					"account", c.Account().ID, "err", err)
				return
			}
			p.logger.Warn("workload poll failed", "account", c.Account().ID, "err", err, "fails", fails)
		} else {
			fails = 0
			// A partial answer is a success with holes: the lists GitHub did
			// not deliver keep their previous values, and the view says why.
			if len(work.Warnings) > 0 {
				p.logger.Warn("workload answered partially",
					"account", c.Account().ID, "errors", work.Warnings, "unresolved", work.Unresolved)
			}
			p.update(c, func(v *AccountView) {
				v.WorkError = ""
				v.WorkPartial = strings.Join(work.Warnings, "; ")
				if work.Resolved("login") {
					v.Login = work.Login
				}
				if work.Resolved("organizations") {
					v.Organizations = work.Organizations
				}
				if work.Resolved("authoredPrs") {
					v.AuthoredPRs = work.AuthoredPRs
				}
				if work.Resolved("reviewRequests") {
					v.ReviewRequests = work.ReviewRequests
				}
				if work.Resolved("assignedIssues") {
					v.AssignedIssues = work.AssignedIssues
				}
				if work.Resolved("authoredIssues") {
					v.AuthoredIssues = work.AuthoredIssues
				}
				if work.Resolved("mergedPrs") {
					v.MergedPRs = work.MergedPRs
				}
				v.WorkRate = rate
				v.WorkAt = time.Now()
			})
		}

		wait := time.Duration(p.workEvery.Load())
		if fails > 0 {
			wait = retryWait(wait, fails, err, time.Now())
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

// retryWait is how long a loop sleeps after a failure: until the time GitHub
// named, capped by backoffMax so a misreported reset cannot park a loop for
// hours, or the doubling backoff when it named none.
func retryWait(base time.Duration, fails int, err error, now time.Time) time.Duration {
	var apiErr *forge.APIError
	if errors.As(err, &apiErr) && apiErr.RetryAt.After(now) {
		return min(apiErr.RetryAt.Sub(now), backoffMax)
	}
	return backoff(base, fails)
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
