package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/karamble/omarchy-omagihu/accounts"
	"github.com/karamble/omarchy-omagihu/forge"
)

// statsFresh is how long fetched figures are served from memory. A row
// disclosed twice in an hour costs one request, not two.
const statsFresh = time.Hour

// statsFetcher is the one call the cache makes outward.
type statsFetcher interface {
	RepoStats(ctx context.Context, owner, name string) (forge.RepoStats, error)
}

// statsEntry is one repository as last seen, with the failure if the last
// look failed and the figures before it if there were any.
type statsEntry struct {
	stats   *forge.RepoStats
	err     string
	account string
	at      time.Time
}

// statsResponse is what a disclosed row reads. ExpiresAt is when the figures
// stop being served from memory, so a clock can say how long they last.
type statsResponse struct {
	Repo      string           `json:"repo"`
	Host      string           `json:"host"`
	Account   string           `json:"account,omitempty"`
	Stats     *forge.RepoStats `json:"stats"`
	Error     string           `json:"error,omitempty"`
	FetchedAt time.Time        `json:"fetchedAt,omitzero"`
	ExpiresAt time.Time        `json:"expiresAt,omitzero"`
	// Cached says the figures came from memory, Stale that they are older
	// than the window and could not be refreshed, Paused that nothing was
	// fetched because monitoring is off.
	Cached bool `json:"cached"`
	Stale  bool `json:"stale"`
	Paused bool `json:"paused"`
}

// handleRepoStats serves one repository's statistics: from memory while
// fresh, fetched when stale, and stale with a flag while monitoring is off,
// because the pause means nothing leaves the machine.
func (s *Server) handleRepoStats(w http.ResponseWriter, r *http.Request) {
	host, owner, name, ok := statsTarget(r)
	if !ok {
		writeJSON(w, s.logger, http.StatusBadRequest, map[string]string{
			"error": `query must carry origin=<git remote url> or repo=owner/name`,
		})
		return
	}
	account, ok := s.statsAccount(host, owner)
	if !ok {
		writeJSON(w, s.logger, http.StatusBadRequest, map[string]string{
			"error": "no enabled account for " + host,
		})
		return
	}

	key := host + "/" + owner + "/" + name
	now := s.clock()
	resp := statsResponse{Repo: owner + "/" + name, Host: host}

	s.statsMu.Lock()
	entry, known := s.stats[key]
	s.statsMu.Unlock()

	fresh := known && now.Sub(entry.at) < statsFresh
	switch {
	case fresh:
		resp.Cached = true
	case !s.store.MonitoringEnabled():
		resp.Paused = true
		resp.Stale = known
		if !known {
			resp.Error = "monitoring is off: nothing is fetched until it is switched back on"
		}
	default:
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		stats, err := s.statsClient(account).RepoStats(ctx, owner, name)
		next := statsEntry{account: account.Login, at: now}
		if err != nil {
			// The previous figures outlive a failed refresh, and say so.
			next.err = err.Error()
			if known {
				next.stats = entry.stats
			}
			resp.Stale = known
			s.logger.Warn("repository statistics failed", "repo", key, "err", err)
		} else {
			next.stats = &stats
			s.logger.Debug("repository statistics fetched", "repo", key, "cost", stats.Cost, "remaining", stats.Remaining)
		}
		s.statsMu.Lock()
		s.stats[key] = next
		s.statsMu.Unlock()
		entry, known = next, true
	}

	if known {
		resp.Account = entry.account
		resp.Stats = entry.stats
		resp.Error = entry.err
		resp.FetchedAt = entry.at
		resp.ExpiresAt = entry.at.Add(statsFresh)
	}
	writeJSON(w, s.logger, http.StatusOK, resp)
}

// statsTarget reads which repository is asked about: a git remote, or
// owner/name on a host that defaults to github.com.
func statsTarget(r *http.Request) (host, owner, name string, ok bool) {
	q := r.URL.Query()
	if origin := q.Get("origin"); origin != "" {
		return forge.ParseRemote(origin)
	}
	owner, name, found := strings.Cut(q.Get("repo"), "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", "", false
	}
	host = strings.ToLower(q.Get("host"))
	if host == "" {
		host = "github.com"
	}
	return host, owner, name, true
}

// statsAccount picks the token to ask with: the account whose login or
// organisations own the repository, since a private one is visible only to
// that token, else the first enabled account on the host. No account on the
// host means the repository is not one omagihu can ask about.
func (s *Server) statsAccount(host, owner string) (accounts.Account, bool) {
	var first *accounts.Account
	orgs := make(map[string][]string)
	for _, v := range s.poller.Snapshot().Accounts {
		orgs[v.AccountID] = v.Organizations
	}
	for _, a := range s.store.Enabled() {
		if accountHost(a) != host {
			continue
		}
		if first == nil {
			first = &a
		}
		if strings.EqualFold(a.Login, owner) {
			return a, true
		}
		for _, org := range orgs[a.ID] {
			if strings.EqualFold(org, owner) {
				return a, true
			}
		}
	}
	if first == nil {
		return accounts.Account{}, false
	}
	return *first, true
}

// accountHost is the host an account speaks to, github.com when unset.
func accountHost(a accounts.Account) string {
	if a.Host == "" {
		return "github.com"
	}
	return strings.ToLower(a.Host)
}

// statsClients hands out one forge client per account, built on first use.
type statsClients struct {
	mu      sync.Mutex
	version string
	clients map[string]*forge.Client
}

func (c *statsClients) get(a accounts.Account) statsFetcher {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.clients == nil {
		c.clients = make(map[string]*forge.Client)
	}
	if client, ok := c.clients[a.ID]; ok {
		return client
	}
	client := forge.New(a, c.version)
	c.clients[a.ID] = client
	return client
}
