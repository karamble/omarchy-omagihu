package accounts

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoadMissingIsNotConfigured(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	if _, err := Load(path); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Load(missing) error = %v, want ErrNotConfigured", err)
	}
}

func TestLoadRejectsWidePermissions(t *testing.T) {
	tests := []struct {
		name    string
		mode    os.FileMode
		wantErr bool
	}{
		{"owner only", 0o600, false},
		{"group readable", 0o640, true},
		{"world readable", 0o604, true},
		{"world writable", 0o622, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "accounts.json")
			if err := os.WriteFile(path, []byte(`{"version":1,"apiToken":"t"}`), tt.mode); err != nil {
				t.Fatalf("writing fixture: %v", err)
			}
			// WriteFile is subject to umask, so force the mode we are testing.
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatalf("chmod: %v", err)
			}

			_, err := Load(path)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "want 0600") {
					t.Fatalf("Load(mode %04o) error = %v, want a permissions complaint", tt.mode, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load(mode %04o) error = %v, want nil", tt.mode, err)
			}
		})
	}
}

func TestSaveMintsTokenAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")

	store := &Store{}
	store.SetPath(path)
	store.Upsert(Account{ID: "a@github.com", Login: "a", Host: "github.com", Token: "secret", Enabled: true})
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if store.APIToken == "" {
		t.Fatal("Save did not mint an APIToken")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("saved mode = %04o, want 0600", perm)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.APIToken != store.APIToken {
		t.Errorf("APIToken = %q, want %q", loaded.APIToken, store.APIToken)
	}
	got, ok := loaded.Find("a@github.com")
	if !ok {
		t.Fatal("Find did not return the saved account")
	}
	if got.Token != "secret" {
		t.Errorf("Token = %q, want %q", got.Token, "secret")
	}
}

func TestUpsertReplacesSameID(t *testing.T) {
	var store Store
	store.Upsert(Account{ID: "a", Login: "old", Enabled: false})
	store.Upsert(Account{ID: "a", Login: "new", Enabled: true})
	store.Upsert(Account{ID: "b", Login: "other", Enabled: false})

	if len(store.Accounts) != 2 {
		t.Fatalf("len(Accounts) = %d, want 2", len(store.Accounts))
	}
	got, _ := store.Find("a")
	if got.Login != "new" {
		t.Errorf("Login = %q, want %q", got.Login, "new")
	}
	if enabled := store.Enabled(); len(enabled) != 1 || enabled[0].ID != "a" {
		t.Errorf("Enabled() = %+v, want just account a", enabled)
	}
}

func TestRedactedHidesEverySecret(t *testing.T) {
	store := &Store{
		Version:  1,
		APIToken: "omagihu_supersecret",
		Accounts: []Account{{ID: "a", Login: "a", Token: "gho_realtoken", Enabled: true}},
	}

	got := store.Redacted()
	if got.APIToken != "[redacted]" {
		t.Errorf("APIToken = %q, want [redacted]", got.APIToken)
	}
	if got.Accounts[0].Token != "[redacted]" {
		t.Errorf("account token = %q, want [redacted]", got.Accounts[0].Token)
	}
	// The original must be untouched: the daemon still needs the real values.
	if store.APIToken != "omagihu_supersecret" || store.Accounts[0].Token != "gho_realtoken" {
		t.Error("Redacted mutated the source store")
	}
}

func TestAPIBaseAndGraphQLURL(t *testing.T) {
	tests := []struct {
		host    string
		api     string
		graphql string
	}{
		{"", "https://api.github.com", "https://api.github.com/graphql"},
		{"github.com", "https://api.github.com", "https://api.github.com/graphql"},
		{"ghe.example.org", "https://ghe.example.org/api/v3", "https://ghe.example.org/api/graphql"},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			a := Account{Host: tt.host}
			if got := a.APIBase(); got != tt.api {
				t.Errorf("APIBase() = %q, want %q", got, tt.api)
			}
			if got := a.GraphQLURL(); got != tt.graphql {
				t.Errorf("GraphQLURL() = %q, want %q", got, tt.graphql)
			}
		})
	}
}

func TestNewAPITokenIsUniqueAndPrefixed(t *testing.T) {
	a, err := NewAPIToken()
	if err != nil {
		t.Fatalf("NewAPIToken: %v", err)
	}
	b, err := NewAPIToken()
	if err != nil {
		t.Fatalf("NewAPIToken: %v", err)
	}
	if a == b {
		t.Error("two calls returned the same token")
	}
	if !strings.HasPrefix(a, "omagihu_") {
		t.Errorf("token %q lacks the omagihu_ prefix", a)
	}
}

// TestStoreConcurrentAccess exercises reads and mutations concurrently so the
// race detector can prove the store's internal lock protects its fields when
// the API mutates it (monitoring, notify, roots, token) while the auth
// middleware, health handler and notifier read it. The store is exercised on a
// temporary path so Save can run for real.
func TestStoreConcurrentAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	st := &Store{}
	st.SetPath(path)
	st.Upsert(Account{ID: "a@github.com", Login: "a", Token: "tok", Enabled: true})
	if err := st.Save(); err != nil {
		t.Fatalf("initial Save: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers: mutate configuration the way the API handlers do.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			switch i % 4 {
			case 0:
				st.SetMonitoring(i%2 == 0)
			case 1:
				st.SetInterval(1 + i%10)
			case 2:
				st.SetMCP(i%2 == 0)
			default:
				st.SetNotify("inbox", i%2 == 0)
			}
			if err := st.Save(); err != nil {
				return
			}
		}
	}()

	// Readers: read configuration and the token the way the auth middleware,
	// health handler and poller do.
	for _, rd := range []struct {
		name string
		fn   func()
	}{
		{"token", func() { _ = st.Token() }},
		{"monitoring", func() { _ = st.MonitoringEnabled() }},
		{"interval", func() { _ = st.Interval() }},
		{"mcp", func() { _ = st.MCPActive() }},
		{"fetch", func() { _ = st.FetchActive(); _ = st.FetchInterval() }},
		{"notify", func() { _ = st.NotifyOrDefault("inbox", true) }},
		{"count", func() { _ = st.AccountCount() }},
		{"enabled", func() { _ = st.Enabled() }},
		{"redacted", func() { _ = st.Redacted() }},
	} {
		wg.Add(1)
		go func(fn func()) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				fn()
			}
		}(rd.fn)
	}

	// Run briefly, then signal and wait for everything to settle.
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestStoreConcurrentFromCold is the case TestStoreConcurrentAccess cannot
// reach. That test calls SetPath, Upsert and Save on one goroutine first,
// which is enough to settle any lazily-created lock before the workers start.
// The daemon does no such thing: Load assigns s.path directly, so the store it
// hands to the API, the notifier and the alerts engine has never been touched
// when they all reach it at once.
func TestStoreConcurrentFromCold(t *testing.T) {
	st := &Store{}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				st.SetMonitoring(n%4 == 0)
				st.SetInterval(n + 1)
				return
			}
			_ = st.MonitoringEnabled()
			_ = st.Interval()
			_ = st.Token()
			_ = st.AccountCount()
			_ = st.Redacted()
		}(i)
	}
	wg.Wait()
}
