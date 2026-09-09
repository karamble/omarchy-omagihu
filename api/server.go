// Package api serves the daemon's HTTP surface: the panel reads it, and the
// MCP tool surface will later mount on the same listener. Every route sits
// behind a constant-time bearer check.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/karamble/omarchy-omagihu/accounts"
	"github.com/karamble/omarchy-omagihu/alerts"
	"github.com/karamble/omarchy-omagihu/attention"
	"github.com/karamble/omarchy-omagihu/correlate"
	"github.com/karamble/omarchy-omagihu/forge"
	"github.com/karamble/omarchy-omagihu/local"
	"github.com/karamble/omarchy-omagihu/mcpserver"
	"github.com/karamble/omarchy-omagihu/notify"
	"github.com/karamble/omarchy-omagihu/poll"
)

// Snapshotter is the poller as the API consumes it.
type Snapshotter interface {
	Snapshot() *poll.Snapshot
}

// LocalSnapshotter is the filesystem watcher as the API consumes it.
type LocalSnapshotter interface {
	Snapshot() *local.Snapshot
}

// Pausable is anything the master switch can stop. Both the poller and the
// watcher satisfy it.
type Pausable interface {
	SetPaused(bool)
	Paused() bool
}

// Fetcher is the watcher's background fetch, as the API consumes it.
type Fetcher interface {
	SetFetch(enabled bool, every time.Duration)
}

// Rhythmic is the poller's settable cadence, as the API consumes it.
type Rhythmic interface {
	SetIntervalMinutes(int)
}

// Refreshable is anything that can be told to look again now.
type Refreshable interface {
	Refresh()
}

// Server holds the daemon state the HTTP handlers read.
type Server struct {
	engine   *alerts.Engine
	store    *accounts.Store
	poller   Snapshotter
	watcher  LocalSnapshotter
	pausable []Pausable
	logger   *slog.Logger
	version  string
	started  time.Time
}

// NewServer builds the API over the account store and the poller.
func NewServer(store *accounts.Store, poller Snapshotter, watcher LocalSnapshotter, logger *slog.Logger, version string) *Server {
	s := &Server{
		store:   store,
		poller:  poller,
		watcher: watcher,
		logger:  logger,
		version: version,
		started: time.Now(),
	}
	if p, ok := poller.(Pausable); ok {
		s.pausable = append(s.pausable, p)
	}
	if p, ok := watcher.(Pausable); ok {
		s.pausable = append(s.pausable, p)
	}
	return s
}

// setMonitoring flips the master switch and persists it, so a daemon restart
// comes back asleep if that is how it was left.
func (s *Server) setMonitoring(enabled bool) error {
	for _, p := range s.pausable {
		p.SetPaused(!enabled)
	}
	s.store.SetMonitoring(enabled)
	return s.store.Save()
}

// setInterval changes the polling rhythm and persists it.
func (s *Server) setInterval(minutes int) error {
	if r, ok := s.poller.(Rhythmic); ok {
		r.SetIntervalMinutes(minutes)
	}
	s.store.SetInterval(minutes)
	return s.store.Save()
}

// ApplyStored puts the daemon into the state the store remembers: the master
// switch and the polling rhythm both survive a restart.
func (s *Server) ApplyStored() {
	for _, p := range s.pausable {
		p.SetPaused(!s.store.MonitoringEnabled())
	}
	if r, ok := s.poller.(Rhythmic); ok {
		r.SetIntervalMinutes(s.store.Interval())
	}
	s.applyFetch()
}

// applyFetch pushes the stored fetch settings into the watcher.
func (s *Server) applyFetch() {
	if f, ok := s.watcher.(Fetcher); ok {
		f.SetFetch(s.store.FetchActive(),
			time.Duration(s.store.FetchInterval())*time.Minute)
	}
}

// alertEngine hands the MCP surface the engine, or nothing at all. It is called
// per request rather than once, because the daemon builds the handler before the
// engine exists. Returning the pointer directly when it is nil would wrap that
// nil in a non-nil interface, which reads as present and then panics.
func (s *Server) alertEngine() mcpserver.Alerts {
	if s.engine == nil {
		return nil
	}
	return s.engine
}

// Handler returns the routed, authenticated handler tree.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/accounts", s.handleAccounts)
	mux.HandleFunc("GET /api/inbox", s.handleInbox)
	mux.HandleFunc("GET /api/work", s.handleWork)
	mux.HandleFunc("GET /api/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /api/repos", s.handleRepos)
	mux.HandleFunc("GET /api/facts", s.handleFacts)
	mux.HandleFunc("GET /api/alerts", s.handleAlerts)
	mux.HandleFunc("GET /api/catalogue", s.handleCatalogue)
	mux.HandleFunc("GET /api/agents", s.handleAgents)
	mux.HandleFunc("POST /api/alerts", s.handleArm)
	mux.HandleFunc("PATCH /api/alerts/{id}", s.handleEdit)
	mux.HandleFunc("DELETE /api/alerts/{id}", s.handleDisarm)
	mux.HandleFunc("GET /api/dashboard", s.handleDashboard)
	mux.HandleFunc("POST /api/monitoring", s.handleMonitoring)
	mux.HandleFunc("POST /api/mcp", s.handleMCPToggle)
	mux.HandleFunc("POST /api/notify", s.handleNotify)
	mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	mux.HandleFunc("POST /api/fetch", s.handleFetchToggle)
	mux.HandleFunc("POST /api/token/recycle", s.handleRecycleToken)

	// The MCP endpoint is checked per request rather than mounted once, so the
	// switch takes effect immediately instead of at the next daemon restart.
	mcpHandler := mcpserver.Handler(mcpserver.Source{
		Remote:     s.poller,
		Local:      s.watcher,
		Monitoring: s.store.MonitoringEnabled,
		Facts:      s.Facts,
		Alerts:     s.alertEngine,
	}, s.version)
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.store.MCPActive() {
			writeJSON(w, s.logger, http.StatusNotFound, map[string]string{
				"error": "the mcp endpoint is disabled in omagihu settings",
			})
			return
		}
		mcpHandler.ServeHTTP(w, r)
	}))
	return s.withRequestLog(s.withAuth(mux))
}

type healthResponse struct {
	Status       string       `json:"status"`
	Version      string       `json:"version"`
	Uptime       string       `json:"uptime"`
	Accounts     int          `json:"accounts"`
	Enabled      int          `json:"enabled"`
	PolledAt     time.Time    `json:"polledAt,omitzero"`
	RateLeft     int          `json:"rateLeft"`
	Monitoring   bool         `json:"monitoring"`
	IntervalMin  int          `json:"intervalMin"`
	MCPEnabled   bool         `json:"mcpEnabled"`
	FetchEnabled bool         `json:"fetchEnabled"`
	FetchMin     int          `json:"fetchMin"`
	Notify       notify.Prefs `json:"notify"`
	Repos        int          `json:"repos"`
	ReposRisk    int          `json:"reposAtRisk"`
	ScannedAt    time.Time    `json:"scannedAt,omitzero"`
	LastError    string       `json:"lastError,omitempty"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.logger, http.StatusOK, s.health(s.poller.Snapshot(), s.watcher.Snapshot()))
}

func (s *Server) health(snap *poll.Snapshot, repos *local.Snapshot) healthResponse {
	resp := healthResponse{
		Status:       "ok",
		Version:      s.version,
		Uptime:       time.Since(s.started).Round(time.Second).String(),
		Accounts:     len(s.store.Accounts),
		Enabled:      len(s.store.Enabled()),
		PolledAt:     snap.TakenAt,
		Monitoring:   s.store.MonitoringEnabled(),
		IntervalMin:  s.store.Interval(),
		MCPEnabled:   s.store.MCPActive(),
		FetchEnabled: s.store.FetchActive(),
		FetchMin:     s.store.FetchInterval(),
		Notify:       s.NotifyPrefs(),
	}
	for _, a := range snap.Accounts {
		resp.RateLeft += a.Rate.Remaining
		if a.Error != "" && resp.LastError == "" {
			resp.LastError = a.Error
		}
	}

	resp.Repos = len(repos.Repos)
	resp.ScannedAt = repos.TakenAt
	for _, r := range repos.Repos {
		if r.AtRisk() {
			resp.ReposRisk++
		}
	}
	return resp
}

// handleAccounts returns the configured identities with every secret stripped.
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.logger, http.StatusOK, s.store.Redacted())
}

type inboxResponse struct {
	Count         int                  `json:"count"`
	Notifications []forge.Notification `json:"notifications"`
}

// handleInbox merges every account's notifications, newest first, deduplicated
// by subject so a repository visible to two identities appears once.
func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	merged := s.mergedInbox(s.poller.Snapshot())
	writeJSON(w, s.logger, http.StatusOK, inboxResponse{
		Count:         len(merged),
		Notifications: merged,
	})
}

func (s *Server) mergedInbox(snap *poll.Snapshot) []forge.Notification {
	var merged []forge.Notification
	seen := make(map[string]struct{})
	for _, a := range snap.Accounts {
		for _, n := range a.Notifications {
			key := n.SubjectURL
			if key == "" {
				key = a.AccountID + "/" + n.ID
			}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, n)
		}
	}
	slices.SortFunc(merged, func(a, b forge.Notification) int {
		return b.UpdatedAt.Compare(a.UpdatedAt)
	})
	return merged
}

type workResponse struct {
	AuthoredPRs    []forge.PullRequest `json:"authoredPrs"`
	ReviewRequests []forge.PullRequest `json:"reviewRequests"`
	AssignedIssues []forge.Issue       `json:"assignedIssues"`
	AuthoredIssues []forge.Issue       `json:"authoredIssues"`
	MergedPRs      []forge.PullRequest `json:"mergedPrs"`
}

// handleWork merges the attention lists across accounts, deduplicated by URL.
func (s *Server) handleWork(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.logger, http.StatusOK, s.mergedWork(s.poller.Snapshot()))
}

func (s *Server) mergedWork(snap *poll.Snapshot) workResponse {
	resp := workResponse{
		AuthoredPRs:    []forge.PullRequest{},
		ReviewRequests: []forge.PullRequest{},
		AssignedIssues: []forge.Issue{},
		AuthoredIssues: []forge.Issue{},
		MergedPRs:      []forge.PullRequest{},
	}
	merged := make(map[string]struct{})
	authored := make(map[string]struct{})
	reviewing := make(map[string]struct{})
	assigned := make(map[string]struct{})
	opened := make(map[string]struct{})

	for _, a := range snap.Accounts {
		for _, pr := range a.AuthoredPRs {
			if _, dup := authored[pr.URL]; dup {
				continue
			}
			authored[pr.URL] = struct{}{}
			resp.AuthoredPRs = append(resp.AuthoredPRs, pr)
		}
		for _, pr := range a.ReviewRequests {
			if _, dup := reviewing[pr.URL]; dup {
				continue
			}
			reviewing[pr.URL] = struct{}{}
			resp.ReviewRequests = append(resp.ReviewRequests, pr)
		}
		for _, is := range a.AssignedIssues {
			if _, dup := assigned[is.URL]; dup {
				continue
			}
			assigned[is.URL] = struct{}{}
			resp.AssignedIssues = append(resp.AssignedIssues, is)
		}
		for _, is := range a.AuthoredIssues {
			if _, dup := opened[is.URL]; dup {
				continue
			}
			opened[is.URL] = struct{}{}
			resp.AuthoredIssues = append(resp.AuthoredIssues, is)
		}
		for _, pr := range a.MergedPRs {
			if _, dup := merged[pr.URL]; dup {
				continue
			}
			merged[pr.URL] = struct{}{}
			resp.MergedPRs = append(resp.MergedPRs, pr)
		}
	}

	byUpdated := func(a, b forge.PullRequest) int { return b.UpdatedAt.Compare(a.UpdatedAt) }
	slices.SortFunc(resp.AuthoredPRs, byUpdated)
	slices.SortFunc(resp.ReviewRequests, byUpdated)
	byIssueUpdated := func(a, b forge.Issue) int { return b.UpdatedAt.Compare(a.UpdatedAt) }
	slices.SortFunc(resp.AssignedIssues, byIssueUpdated)
	slices.SortFunc(resp.AuthoredIssues, byIssueUpdated)
	return resp
}

// dashboardResponse is everything the panel needs in one round trip, so the
// QML side makes a single call and parses one document.
type dashboardResponse struct {
	Health    healthResponse       `json:"health"`
	Attention attention.State      `json:"attention"`
	Inbox     []forge.Notification `json:"inbox"`
	Facts     []correlate.Fact     `json:"facts"`
	Alerts    []alertRow           `json:"alerts"`
	Work      workResponse         `json:"work"`
	Repos     []local.Repo         `json:"repos"`
	Accounts  []poll.AccountView   `json:"accounts"`
}

// handleDashboard serves the landing view: what is waiting, what is in flight,
// what is at risk, resolved into one bar signal.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	remote := s.poller.Snapshot()
	repos := s.watcher.Snapshot()

	work := s.mergedWork(remote)
	inbox := s.mergedInbox(remote)
	facts := correlate.Correlate(remote, repos)

	resp := dashboardResponse{
		Health: s.health(remote, repos),
		Attention: attention.Resolve(attention.Input{
			Reviews:  work.ReviewRequests,
			Authored: work.AuthoredPRs,
			Unread:   inbox,
			Repos:    repos.Repos,
			Facts:    facts,
		}),
		Inbox:    inbox,
		Facts:    facts,
		Alerts:   s.alertRows(),
		Work:     work,
		Repos:    repos.Repos,
		Accounts: remote.Accounts,
	}
	writeJSON(w, s.logger, http.StatusOK, resp)
}

// handleMonitoring is the master switch. Off means the daemon stops polling
// GitHub and stops inspecting repositories: nothing leaves the machine, and no
// git process is spawned. The last snapshot is kept so the panel still has
// something to show.
func (s *Server) handleMonitoring(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled     *bool `json:"enabled"`
		IntervalMin *int  `json:"intervalMin"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil ||
		(body.Enabled == nil && body.IntervalMin == nil) {
		writeJSON(w, s.logger, http.StatusBadRequest, map[string]string{
			"error": `body must carry "enabled" and/or "intervalMin"`,
		})
		return
	}

	// A rhythm implies waking: choosing "every 5 minutes" cannot leave the
	// daemon asleep.
	if body.IntervalMin != nil {
		if *body.IntervalMin <= 0 {
			writeJSON(w, s.logger, http.StatusBadRequest, map[string]string{
				"error": "intervalMin must be positive; use enabled:false to stop polling",
			})
			return
		}
		if err := s.setInterval(*body.IntervalMin); err != nil {
			s.logger.Error("persisting interval", "err", err)
			writeJSON(w, s.logger, http.StatusInternalServerError, map[string]string{
				"error": "could not persist the setting",
			})
			return
		}
		if body.Enabled == nil {
			wake := true
			body.Enabled = &wake
		}
	}

	if err := s.setMonitoring(*body.Enabled); err != nil {
		s.logger.Error("persisting monitoring state", "err", err)
		writeJSON(w, s.logger, http.StatusInternalServerError, map[string]string{
			"error": "could not persist the setting",
		})
		return
	}
	s.logger.Info("monitoring switched", "enabled", *body.Enabled, "intervalMin", s.store.Interval())
	writeJSON(w, s.logger, http.StatusOK, map[string]any{
		"monitoring":  *body.Enabled,
		"intervalMin": s.store.Interval(),
	})
}

// handleFetchToggle switches background fetching and its cadence.
func (s *Server) handleFetchToggle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled  *bool `json:"enabled"`
		EveryMin *int  `json:"everyMin"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil ||
		(body.Enabled == nil && body.EveryMin == nil) {
		writeJSON(w, s.logger, http.StatusBadRequest, map[string]string{
			"error": `body must carry "enabled" and/or "everyMin"`,
		})
		return
	}

	enabled := s.store.FetchActive()
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	minutes := 0
	if body.EveryMin != nil {
		minutes = *body.EveryMin
	}

	s.store.SetFetch(enabled, minutes)
	if err := s.store.Save(); err != nil {
		s.logger.Error("persisting fetch state", "err", err)
		writeJSON(w, s.logger, http.StatusInternalServerError, map[string]string{
			"error": "could not persist the setting",
		})
		return
	}
	s.applyFetch()
	s.logger.Info("background fetch switched",
		"enabled", s.store.FetchActive(), "everyMin", s.store.FetchInterval())
	writeJSON(w, s.logger, http.StatusOK, map[string]any{
		"fetchEnabled": s.store.FetchActive(),
		"fetchMin":     s.store.FetchInterval(),
	})
}

// facts joins the two planes. It is cheap: both snapshots are already in
// memory, so this is a walk over what has been polled, not a fetch.
func (s *Server) facts() []correlate.Fact {
	return correlate.Correlate(s.poller.Snapshot(), s.watcher.Snapshot())
}

// SetEngine hands the API the alert engine once it exists.
func (s *Server) SetEngine(e *alerts.Engine) { s.engine = e }

// AlertSample builds what a trigger is evaluated against, from the same state
// the panel renders, so an alarm and a person can never disagree.
func (s *Server) AlertSample() alerts.Snapshot {
	remote := s.poller.Snapshot()
	repos := s.watcher.Snapshot()
	work := s.mergedWork(remote)
	health := s.health(remote, repos)

	return alerts.Snapshot{
		Attention: attention.Resolve(attention.Input{
			Reviews:  work.ReviewRequests,
			Authored: work.AuthoredPRs,
			Unread:   s.mergedInbox(remote),
			Repos:    repos.Repos,
			Facts:    s.facts(),
		}),
		Health: alerts.Health{
			Repos:      health.Repos,
			RateLeft:   health.RateLeft,
			Monitoring: health.Monitoring,
			LastError:  health.LastError,
		},
		Inbox:    s.mergedInbox(remote),
		Reviews:  work.ReviewRequests,
		Authored: work.AuthoredPRs,
		Merged:   work.MergedPRs,
		Issues:   work.AssignedIssues,
		Facts:    s.facts(),
		Repos:    repos.Repos,
		TakenAt:  repos.TakenAt,
	}
}

// alertRow is a trigger plus the one word that describes where it stands.
type alertRow struct {
	alerts.Trigger
	Status alerts.Status `json:"status"`
}

// alertRows lists every trigger with its current status.
func (s *Server) alertRows() []alertRow {
	if s.engine == nil {
		return []alertRow{}
	}
	now := time.Now()
	list := s.engine.List()
	out := make([]alertRow, 0, len(list))
	for _, t := range list {
		out = append(out, alertRow{Trigger: t, Status: t.Status(now)})
	}
	return out
}

// handleAlerts lists every trigger with its current status.
func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.logger, http.StatusOK, map[string]any{"alerts": s.alertRows()})
}

// handleCatalogue serves what can be watched, which the panel's path picker and
// the omagihu_catalogue tool both read.
func (s *Server) handleCatalogue(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.logger, http.StatusOK, map[string]any{"paths": alerts.Catalogue()})
}

// handleAgents lists live herdr agents, for the delivery picker.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := alerts.Agents(r.Context())
	if err != nil {
		// No herdr is a normal state, not a failure: there is simply nobody to
		// wake but the person at the keyboard.
		writeJSON(w, s.logger, http.StatusOK, map[string]any{"agents": []any{}, "note": err.Error()})
		return
	}
	writeJSON(w, s.logger, http.StatusOK, map[string]any{"agents": agents})
}

// handleArm stores a new trigger.
func (s *Server) handleArm(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeJSON(w, s.logger, http.StatusServiceUnavailable, map[string]string{"error": "alerts are not running"})
		return
	}
	var t alerts.Trigger
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&t); err != nil {
		writeJSON(w, s.logger, http.StatusBadRequest, map[string]string{"error": "unreadable trigger"})
		return
	}
	if t.ArmedBy == "" {
		t.ArmedBy = alerts.TargetYou
	}
	armed, err := s.engine.Arm(t)
	if err != nil {
		writeJSON(w, s.logger, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, s.logger, http.StatusOK, armed)
}

// handleDisarm removes a trigger.
// handleEdit changes a trigger's terms. The body is a partial trigger: whatever
// it names is replaced, whatever it leaves out is kept. Decoding onto a copy of
// the stored trigger is what gives that merge, so there is no second notion of
// which fields are patchable.
func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeJSON(w, s.logger, http.StatusServiceUnavailable, map[string]string{"error": "alerts are not running"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, s.logger, http.StatusBadRequest, map[string]string{"error": "unreadable body"})
		return
	}

	var decodeErr error
	edited, err := s.engine.Edit(r.PathValue("id"), func(t *alerts.Trigger) {
		decodeErr = json.Unmarshal(body, t)
	})
	if decodeErr != nil {
		writeJSON(w, s.logger, http.StatusBadRequest, map[string]string{"error": "unreadable trigger"})
		return
	}
	if err != nil {
		writeJSON(w, s.logger, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, s.logger, http.StatusOK, edited)
}

func (s *Server) handleDisarm(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeJSON(w, s.logger, http.StatusServiceUnavailable, map[string]string{"error": "alerts are not running"})
		return
	}
	ok, err := s.engine.Disarm(r.PathValue("id"))
	if err != nil {
		writeJSON(w, s.logger, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, s.logger, http.StatusNotFound, map[string]string{"error": "no such trigger"})
		return
	}
	writeJSON(w, s.logger, http.StatusOK, map[string]string{"disarmed": r.PathValue("id")})
}

// Facts is the exported form the notifier and the MCP surface read.
func (s *Server) Facts() []correlate.Fact { return s.facts() }

// handleFacts serves the correlations on their own, for agents and scripts.
func (s *Server) handleFacts(w http.ResponseWriter, r *http.Request) {
	facts := s.facts()
	if facts == nil {
		facts = []correlate.Fact{}
	}
	writeJSON(w, s.logger, http.StatusOK, map[string]any{
		"count": len(facts),
		"facts": facts,
	})
}

// handleRefresh cuts short whatever the loops are waiting on so the next poll
// happens now. Without it a long rhythm means a long wait even after the
// network comes back, and the panel's Refresh button could only re-read a
// stale cache.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.store.MonitoringEnabled() {
		writeJSON(w, s.logger, http.StatusConflict, map[string]string{
			"error": "monitoring is off; nothing will be fetched",
		})
		return
	}

	var woke []string
	if p, ok := s.poller.(Refreshable); ok {
		p.Refresh()
		woke = append(woke, "remote")
	}
	if wch, ok := s.watcher.(Refreshable); ok {
		wch.Refresh()
		woke = append(woke, "local")
	}
	s.logger.Info("refresh requested", "planes", woke)
	writeJSON(w, s.logger, http.StatusOK, map[string]any{"refreshing": woke})
}

// NotifyPrefs resolves the stored per-domain switches against the defaults, so
// the daemon and the panel always agree on what is currently on.
func (s *Server) NotifyPrefs() notify.Prefs {
	d := notify.Defaults()
	return notify.Prefs{
		Reviews:   s.store.NotifyOrDefault("reviews", d.Reviews),
		Broken:    s.store.NotifyOrDefault("broken", d.Broken),
		Inbox:     s.store.NotifyOrDefault("inbox", d.Inbox),
		Local:     s.store.NotifyOrDefault("local", d.Local),
		Reconcile: s.store.NotifyOrDefault("reconcile", d.Reconcile),
	}
}

// handleNotify switches one notification domain.
func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Domain  string `json:"domain"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.Enabled == nil {
		writeJSON(w, s.logger, http.StatusBadRequest, map[string]string{
			"error": `body must be {"domain":"reviews|broken|inbox|local","enabled":true|false}`,
		})
		return
	}
	if _, ok := notify.ParseDomain(body.Domain); !ok {
		writeJSON(w, s.logger, http.StatusBadRequest, map[string]string{
			"error": "unknown domain " + body.Domain,
		})
		return
	}

	s.store.SetNotify(body.Domain, *body.Enabled)
	if err := s.store.Save(); err != nil {
		s.logger.Error("persisting notify state", "err", err)
		writeJSON(w, s.logger, http.StatusInternalServerError, map[string]string{
			"error": "could not persist the setting",
		})
		return
	}
	s.logger.Info("notifications switched", "domain", body.Domain, "enabled", *body.Enabled)
	writeJSON(w, s.logger, http.StatusOK, s.NotifyPrefs())
}

// handleMCPToggle serves or withdraws the MCP endpoint.
func (s *Server) handleMCPToggle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.Enabled == nil {
		writeJSON(w, s.logger, http.StatusBadRequest, map[string]string{
			"error": `body must be {"enabled": true|false}`,
		})
		return
	}
	s.store.SetMCP(*body.Enabled)
	if err := s.store.Save(); err != nil {
		s.logger.Error("persisting mcp state", "err", err)
		writeJSON(w, s.logger, http.StatusInternalServerError, map[string]string{
			"error": "could not persist the setting",
		})
		return
	}
	s.logger.Info("mcp endpoint switched", "enabled", *body.Enabled)
	writeJSON(w, s.logger, http.StatusOK, map[string]bool{"mcpEnabled": *body.Enabled})
}

// handleRecycleToken mints a new bearer token. Every existing client, this one
// included, is locked out until it is given the new value, which is exactly
// what revoking a leaked token has to mean.
func (s *Server) handleRecycleToken(w http.ResponseWriter, r *http.Request) {
	token, err := s.store.RecycleAPIToken()
	if err != nil {
		s.logger.Error("recycling token", "err", err)
		writeJSON(w, s.logger, http.StatusInternalServerError, map[string]string{
			"error": "could not mint a new token",
		})
		return
	}
	if err := s.store.Save(); err != nil {
		s.logger.Error("persisting new token", "err", err)
		writeJSON(w, s.logger, http.StatusInternalServerError, map[string]string{
			"error": "could not persist the new token",
		})
		return
	}
	s.logger.Warn("api token recycled: existing clients must be re-pointed")

	// The new token comes back so the panel can show it once, at the moment it
	// is created, which is the only moment it is needed. The caller already
	// proved it holds the previous token to reach this route at all.
	writeJSON(w, s.logger, http.StatusOK, map[string]any{
		"status": "recycled",
		"token":  token,
		"entry": map[string]any{
			"omagihu": map[string]any{
				"type":    "http",
				"url":     "http://" + r.Host + "/mcp",
				"headers": map[string]string{"Authorization": "Bearer " + token},
			},
		},
		"next": "paste entry into the mcpServers object in ~/.claude.json",
	})
}

// handleRepos exposes the local plane: every watched checkout, at-risk first.
func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	snap := s.watcher.Snapshot()
	if r.URL.Query().Get("risk") == "1" {
		filtered := &local.Snapshot{TakenAt: snap.TakenAt, Roots: snap.Roots}
		for _, repo := range snap.Repos {
			if repo.AtRisk() {
				filtered.Repos = append(filtered.Repos, repo)
			}
		}
		snap = filtered
	}
	writeJSON(w, s.logger, http.StatusOK, snap)
}

// handleSnapshot exposes the raw per-account view, which is what the panel uses
// to attribute a row to an identity.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.logger, http.StatusOK, s.poller.Snapshot())
}

// withAuth rejects anything without the store's bearer token. The comparison is
// constant time so a wrong token leaks nothing through timing.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the token per request: recycling it must lock out old clients
		// immediately, not at the next daemon restart.
		want := []byte(s.store.APIToken)
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="omagihu"`)
			writeJSON(w, s.logger, http.StatusUnauthorized, map[string]string{
				"error": "unauthorized",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.logger.Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"dur", time.Since(start),
		)
	})
}

func writeJSON(w http.ResponseWriter, logger *slog.Logger, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		logger.Error("encoding response", "err", err)
	}
}
