// Package notify turns transitions into desktop notifications. A daemon that
// watches and never speaks is only a slower way of looking, but one that speaks
// about everything gets muted, so each domain is switched separately and the
// quiet ones are off by default.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"time"

	"github.com/karamble/omarchy-omagihu/attention"
	"github.com/karamble/omarchy-omagihu/correlate"
	"github.com/karamble/omarchy-omagihu/local"
	"github.com/karamble/omarchy-omagihu/poll"
)

// checkEvery is how often the in-memory state is diffed. This costs no network
// and no git, so it can be brisk: the polls themselves set the real pace.
const checkEvery = 20 * time.Second

// Domain names one class of event a person may or may not want to hear about.
type Domain string

const (
	// DomainReviews: somebody is blocked waiting on you.
	DomainReviews Domain = "reviews"
	// DomainBroken: your own pull request failed or was sent back.
	DomainBroken Domain = "broken"
	// DomainInbox: any unread notification.
	DomainInbox Domain = "inbox"
	// DomainLocal: an interrupted git operation on this machine.
	DomainLocal Domain = "local"
	// DomainReconcile: the two planes have drifted apart.
	DomainReconcile Domain = "reconcile"
)

// Prefs says which domains may speak. The defaults let the two tiers that mean
// "somebody is waiting" through, and keep the rest quiet.
type Prefs struct {
	Reviews   bool `json:"reviews"`
	Broken    bool `json:"broken"`
	Inbox     bool `json:"inbox"`
	Local     bool `json:"local"`
	Reconcile bool `json:"reconcile"`
}

// Defaults returns the out-of-the-box preferences.
func Defaults() Prefs {
	return Prefs{Reviews: true, Broken: true, Inbox: false, Local: false, Reconcile: false}
}

// Enabled reports whether one domain may speak.
func (p Prefs) Enabled(d Domain) bool {
	switch d {
	case DomainReviews:
		return p.Reviews
	case DomainBroken:
		return p.Broken
	case DomainInbox:
		return p.Inbox
	case DomainLocal:
		return p.Local
	case DomainReconcile:
		return p.Reconcile
	}
	return false
}

// Set flips one domain and returns the updated preferences.
func (p Prefs) Set(d Domain, on bool) Prefs {
	switch d {
	case DomainReviews:
		p.Reviews = on
	case DomainBroken:
		p.Broken = on
	case DomainInbox:
		p.Inbox = on
	case DomainLocal:
		p.Local = on
	case DomainReconcile:
		p.Reconcile = on
	}
	return p
}

// ParseDomain maps a name to a Domain.
func ParseDomain(name string) (Domain, bool) {
	switch Domain(name) {
	case DomainReviews, DomainBroken, DomainInbox, DomainLocal, DomainReconcile:
		return Domain(name), true
	}
	return "", false
}

// Remote is the poller as this package consumes it.
type Remote interface{ Snapshot() *poll.Snapshot }

// Local is the filesystem watcher as this package consumes it.
type Local interface{ Snapshot() *local.Snapshot }

// Notifier diffs successive snapshots and speaks about what is new.
type Notifier struct {
	remote Remote
	local  Local
	prefs  func() Prefs
	facts  func() []correlate.Fact
	awake  func() bool
	logger *slog.Logger
	send   func(urgency, title, body string) error

	// seen holds the keys already announced, so a standing failure is reported
	// once rather than every twenty seconds.
	seen map[string]struct{}
	// primed guards the first pass: the state at startup is history, not news.
	primed bool
}

// New builds a notifier over the two snapshot sources.
func New(remote Remote, lcl Local, prefs func() Prefs, facts func() []correlate.Fact, awake func() bool, logger *slog.Logger) *Notifier {
	return &Notifier{
		remote: remote,
		local:  lcl,
		prefs:  prefs,
		facts:  facts,
		awake:  awake,
		logger: logger,
		send:   sendDesktop,
		seen:   make(map[string]struct{}),
	}
}

// Run diffs on a timer until ctx is done.
func (n *Notifier) Run(ctx context.Context) {
	t := time.NewTicker(checkEvery)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.check()
		}
	}
}

// check compares the current state against what has already been announced.
func (n *Notifier) check() {
	// Asleep means asleep: the switch stops the daemon speaking as well as
	// polling, otherwise "nothing leaves this machine" would be a half truth.
	if n.awake != nil && !n.awake() {
		return
	}

	prefs := n.prefs()
	fresh := make(map[string]struct{})
	type event struct {
		urgency string
		title   string
		body    string
	}
	var events []event

	remote := n.remote.Snapshot()
	for _, acct := range remote.Accounts {
		for _, pr := range acct.ReviewRequests {
			key := "review:" + pr.URL
			fresh[key] = struct{}{}
			if n.isNew(key) && prefs.Enabled(DomainReviews) {
				events = append(events, event{"critical",
					"Review requested",
					fmt.Sprintf("%s #%d\n%s", pr.Repo, pr.Number, pr.Title)})
			}
		}

		for _, pr := range acct.AuthoredPRs {
			if !attention.Broken(pr) {
				continue
			}
			// The state is part of the key: a PR that fails, is fixed, then
			// fails again is news the second time too.
			key := "broken:" + pr.URL + ":" + pr.ChecksState + ":" + pr.ReviewDecision
			fresh[key] = struct{}{}
			if n.isNew(key) && prefs.Enabled(DomainBroken) {
				why := "checks failing"
				if pr.ReviewDecision == "CHANGES_REQUESTED" {
					why = "changes requested"
				}
				events = append(events, event{"critical",
					"Your pull request needs you",
					fmt.Sprintf("%s #%d, %s\n%s", pr.Repo, pr.Number, why, pr.Title)})
			}
		}

		for _, note := range acct.Notifications {
			key := "inbox:" + note.ID
			fresh[key] = struct{}{}
			if n.isNew(key) && prefs.Enabled(DomainInbox) {
				events = append(events, event{"normal",
					note.Repo,
					note.Title})
			}
		}
	}

	if n.facts != nil {
		for _, f := range n.facts() {
			// The kind and the head commit are both in the key, so the same
			// drift is announced once but a fresh one is news again.
			key := "fact:" + string(f.Kind) + ":" + f.Path + ":" + f.Branch + ":" + f.URL
			fresh[key] = struct{}{}
			if n.isNew(key) && prefs.Enabled(DomainReconcile) {
				urgency := "normal"
				if f.Urgent() {
					urgency = "critical"
				}
				events = append(events, event{urgency, "Needs reconciling", f.Summary})
			}
		}
	}

	for _, repo := range n.local.Snapshot().Repos {
		if repo.Operation == local.OpNone {
			continue
		}
		key := "local:" + repo.Path + ":" + string(repo.Operation)
		fresh[key] = struct{}{}
		if n.isNew(key) && prefs.Enabled(DomainLocal) {
			events = append(events, event{"normal",
				"Unfinished " + string(repo.Operation),
				fmt.Sprintf("%s is mid-%s on %s", repo.Name, repo.Operation, repo.Branch)})
		}
	}

	// The first pass only learns the world; announcing it would mean a burst of
	// notifications every time the daemon restarts.
	if !n.primed {
		n.seen = fresh
		n.primed = true
		n.logger.Debug("notifier primed", "known", len(fresh))
		return
	}
	n.seen = fresh

	for _, e := range events {
		if err := n.send(e.urgency, e.title, e.body); err != nil {
			n.logger.Warn("desktop notification failed", "err", err)
		}
	}
	if len(events) > 0 {
		n.logger.Info("notified", "count", len(events))
	}
}

func (n *Notifier) isNew(key string) bool {
	_, known := n.seen[key]
	return !known
}

// Desktop is the exported desktop sender, so alerts raise notifications through
// the same call site rather than opening a second one.
func Desktop(urgency, title, body string) error { return sendDesktop(urgency, title, body) }

// sendDesktop hands one notification to the desktop. A missing notify-send is
// reported once by the caller and never fatal.
func sendDesktop(urgency, title, body string) error {
	cmd := exec.Command("notify-send",
		"--app-name=omagihu",
		"--urgency="+urgency,
		"--expire-time="+strconv.Itoa(10000),
		title, body)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("notify-send: %w", err)
	}
	return nil
}
