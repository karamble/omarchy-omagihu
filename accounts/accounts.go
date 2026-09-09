// Package accounts stores the GitHub identities omagihu watches and the bearer
// token that guards the daemon's own API. The file holds credentials, so it is
// written 0600 and refused if the mode is wider.
package accounts

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ErrNotConfigured reports that no store exists yet. The daemon turns this into
// an instruction to run omagihu-setup rather than a stack trace.
var ErrNotConfigured = errors.New("no account store: run omagihu-setup")

// Account is one GitHub identity. Token is a personal access token or a token
// lifted from gh; it never appears in argv and is redacted from logs.
type Account struct {
	ID      string `json:"id"`
	Login   string `json:"login"`
	Host    string `json:"host"`
	Token   string `json:"token"`
	Enabled bool   `json:"enabled"`
}

// APIBase is the REST root for the account's host, covering GitHub Enterprise
// as well as github.com.
func (a Account) APIBase() string {
	if a.Host == "" || a.Host == "github.com" {
		return "https://api.github.com"
	}
	return "https://" + a.Host + "/api/v3"
}

// GraphQLURL is the GraphQL endpoint for the account's host.
func (a Account) GraphQLURL() string {
	if a.Host == "" || a.Host == "github.com" {
		return "https://api.github.com/graphql"
	}
	return "https://" + a.Host + "/api/graphql"
}

// Store is the on-disk configuration. The zero value is usable: Save writes it
// with a freshly generated APIToken if one is missing.
type Store struct {
	Version  int       `json:"version"`
	APIToken string    `json:"apiToken"`
	Accounts []Account `json:"accounts"`

	// Monitoring is the master switch. Nil means never set, which reads as on.
	// A pointer rather than a bool so an absent field cannot silently mean off.
	Monitoring *bool `json:"monitoring,omitempty"`

	// IntervalMin is the polling rhythm in minutes. Nil means the default.
	IntervalMin *int `json:"intervalMin,omitempty"`

	// Notify holds the per-domain desktop notification switches. Nil members
	// fall back to the package defaults, so a new domain added later starts at
	// its intended setting rather than silently off.
	Notify *NotifyPrefs `json:"notify,omitempty"`

	// FetchEnabled keeps remote-tracking refs current in the background. Nil
	// reads as on: without it "unpushed" and "behind" slowly drift into
	// fiction.
	FetchEnabled *bool `json:"fetchEnabled,omitempty"`

	// FetchMin is the background fetch cadence in minutes. Nil is the default.
	FetchMin *int `json:"fetchMin,omitempty"`

	// MCPEnabled exposes the Model Context Protocol endpoint. Nil reads as on:
	// it sits behind the same bearer token as the rest of the API, so it adds
	// no surface beyond what is already there.
	MCPEnabled *bool `json:"mcpEnabled,omitempty"`

	// Roots are the directories searched for checkouts. A plain slice, because
	// empty says "never set" clearly enough here. They used to live in the
	// systemd unit's ExecStart line, which meant configuration was kept in a
	// command line and could only be read back by parsing a service file.
	Roots []string `json:"roots,omitempty"`

	path string
}

// MonitoringEnabled reports whether the daemon may talk to anything at all.
// When it is false omagihu makes no outbound request of any kind.
func (s *Store) MonitoringEnabled() bool {
	return s.Monitoring == nil || *s.Monitoring
}

// SetMonitoring records the master switch.
func (s *Store) SetMonitoring(enabled bool) {
	s.Monitoring = &enabled
}

// NotifyPrefs mirrors notify.Prefs on disk, with pointers so "never set" is
// distinguishable from "set to false".
type NotifyPrefs struct {
	Reviews   *bool `json:"reviews,omitempty"`
	Broken    *bool `json:"broken,omitempty"`
	Inbox     *bool `json:"inbox,omitempty"`
	Local     *bool `json:"local,omitempty"`
	Reconcile *bool `json:"reconcile,omitempty"`
}

// NotifyOrDefault resolves one domain against the supplied fallback.
func (s *Store) NotifyOrDefault(domain string, fallback bool) bool {
	if s.Notify == nil {
		return fallback
	}
	var v *bool
	switch domain {
	case "reviews":
		v = s.Notify.Reviews
	case "broken":
		v = s.Notify.Broken
	case "inbox":
		v = s.Notify.Inbox
	case "local":
		v = s.Notify.Local
	case "reconcile":
		v = s.Notify.Reconcile
	}
	if v == nil {
		return fallback
	}
	return *v
}

// SetNotify records one domain switch.
func (s *Store) SetNotify(domain string, enabled bool) {
	if s.Notify == nil {
		s.Notify = &NotifyPrefs{}
	}
	switch domain {
	case "reviews":
		s.Notify.Reviews = &enabled
	case "broken":
		s.Notify.Broken = &enabled
	case "inbox":
		s.Notify.Inbox = &enabled
	case "local":
		s.Notify.Local = &enabled
	case "reconcile":
		s.Notify.Reconcile = &enabled
	}
}

// DefaultFetchMin is how often a fresh install refreshes remote refs.
const DefaultFetchMin = 30

// FetchActive reports whether background fetching is on.
func (s *Store) FetchActive() bool {
	return s.FetchEnabled == nil || *s.FetchEnabled
}

// FetchInterval reports the background fetch cadence in minutes.
func (s *Store) FetchInterval() int {
	if s.FetchMin == nil || *s.FetchMin <= 0 {
		return DefaultFetchMin
	}
	return *s.FetchMin
}

// SetFetch records the background fetch switch and cadence. A non-positive
// number of minutes leaves the cadence alone.
func (s *Store) SetFetch(enabled bool, minutes int) {
	s.FetchEnabled = &enabled
	if minutes > 0 {
		s.FetchMin = &minutes
	}
}

// MCPActive reports whether the MCP endpoint should be served.
func (s *Store) MCPActive() bool {
	return s.MCPEnabled == nil || *s.MCPEnabled
}

// SetMCP records whether the MCP endpoint is served.
func (s *Store) SetMCP(enabled bool) {
	s.MCPEnabled = &enabled
}

// RootsOrDefault is where to look for checkouts, falling back to the defaults
// when nothing has been chosen yet.
func (s *Store) RootsOrDefault(fallback []string) []string {
	if len(s.Roots) == 0 {
		return fallback
	}
	return slices.Clone(s.Roots)
}

// SetRoots records the directories to watch.
func (s *Store) SetRoots(roots []string) {
	s.Roots = slices.Clone(roots)
}

// RecycleAPIToken mints a fresh bearer token, invalidating every client that
// holds the old one. That is the point: it is how a leaked token is revoked.
func (s *Store) RecycleAPIToken() (string, error) {
	tok, err := NewAPIToken()
	if err != nil {
		return "", err
	}
	s.APIToken = tok
	return tok, nil
}

// DefaultIntervalMin is the rhythm a fresh install polls at.
const DefaultIntervalMin = 5

// Interval reports the chosen polling rhythm in minutes.
func (s *Store) Interval() int {
	if s.IntervalMin == nil || *s.IntervalMin <= 0 {
		return DefaultIntervalMin
	}
	return *s.IntervalMin
}

// SetInterval records the polling rhythm in minutes.
func (s *Store) SetInterval(minutes int) {
	if minutes > 0 {
		s.IntervalMin = &minutes
	}
}

// DefaultPath honours XDG_CONFIG_HOME and falls back to ~/.config/omagihu.
func DefaultPath() string {
	if dir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); dir != "" {
		return filepath.Join(dir, "omagihu", "accounts.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "omagihu", "accounts.json")
	}
	return filepath.Join(home, ".config", "omagihu", "accounts.json")
}

// Load reads the store at path. A missing file yields ErrNotConfigured so the
// caller can tell the difference between "not set up" and "broken".
func Load(path string) (*Store, error) {
	if path == "" {
		path = DefaultPath()
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", path, ErrNotConfigured)
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%s is mode %04o, want 0600: it holds tokens", path, perm)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var s Store
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	s.path = path
	return &s, nil
}

// Save writes the store atomically at 0600, minting an APIToken if absent.
func (s *Store) Save() error {
	if s.path == "" {
		s.path = DefaultPath()
	}
	if s.Version == 0 {
		s.Version = 1
	}
	if s.APIToken == "" {
		tok, err := NewAPIToken()
		if err != nil {
			return err
		}
		s.APIToken = tok
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding store: %w", err)
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(dir, ".accounts-*.json")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("renaming into %s: %w", s.path, err)
	}
	return nil
}

// Path reports where the store was loaded from or will be written.
func (s *Store) Path() string {
	if s.path == "" {
		return DefaultPath()
	}
	return s.path
}

// SetPath overrides the location, for tests and for --config.
func (s *Store) SetPath(path string) { s.path = path }

// Enabled returns the accounts the pollers should run for.
func (s *Store) Enabled() []Account {
	var out []Account
	for _, a := range s.Accounts {
		if a.Enabled {
			out = append(out, a)
		}
	}
	return out
}

// Find returns the account with the given id.
func (s *Store) Find(id string) (Account, bool) {
	i := slices.IndexFunc(s.Accounts, func(a Account) bool { return a.ID == id })
	if i < 0 {
		return Account{}, false
	}
	return s.Accounts[i], true
}

// Upsert adds an account or replaces the one sharing its id.
func (s *Store) Upsert(a Account) {
	if i := slices.IndexFunc(s.Accounts, func(x Account) bool { return x.ID == a.ID }); i >= 0 {
		s.Accounts[i] = a
		return
	}
	s.Accounts = append(s.Accounts, a)
}

// Redacted copies the store with every secret replaced, for logs and for the
// accounts endpoint. Tokens must never leave the process.
func (s *Store) Redacted() Store {
	out := Store{
		Version:      s.Version,
		APIToken:     redact(s.APIToken),
		Monitoring:   s.Monitoring,
		IntervalMin:  s.IntervalMin,
		MCPEnabled:   s.MCPEnabled,
		Notify:       s.Notify,
		FetchEnabled: s.FetchEnabled,
		FetchMin:     s.FetchMin,
	}
	for _, a := range s.Accounts {
		a.Token = redact(a.Token)
		out.Accounts = append(out.Accounts, a)
	}
	return out
}

func redact(secret string) string {
	if secret == "" {
		return ""
	}
	return "[redacted]"
}

// NewAPIToken mints the bearer token the panel and MCP clients present.
func NewAPIToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating api token: %w", err)
	}
	return "omagihu_" + base64.RawURLEncoding.EncodeToString(buf), nil
}
