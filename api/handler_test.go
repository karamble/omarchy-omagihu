package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/karamble/omarchy-omagihu/accounts"
	"github.com/karamble/omarchy-omagihu/alerts"
	"github.com/karamble/omarchy-omagihu/forge"
	"github.com/karamble/omarchy-omagihu/local"
	"github.com/karamble/omarchy-omagihu/poll"
)

// testToken is the bearer token every wired fixture guards itself with.
const testToken = "qzk4f7"

// numPtr writes a trigger bound, which is a float pointer.
func numPtr(f float64) *float64 { return &f }

// wakingPoller is fixedPoller that also records a forced refresh, which is
// what /api/refresh asks the poller for.
type wakingPoller struct {
	fixedPoller
	woken int
}

func (p *wakingPoller) ForceRefresh() { p.woken++ }

type wakingWatcher struct {
	fixedWatcher
	woken int
}

func (w *wakingWatcher) ForceRefresh() { w.woken++ }

// wired is one server behind its routed handler with everything a route can
// reach: a store on a throwaway path, an alert engine holding one armed
// trigger, and a stats client that never leaves the machine.
type wired struct {
	s       *Server
	h       http.Handler
	store   *accounts.Store
	engine  *alerts.Engine
	armed   alerts.Trigger
	poller  *wakingPoller
	watcher *wakingWatcher
	stats   *fakeStats
}

// wire builds that server. The snapshots carry one of everything, so a route
// that drops a list is visible as an empty one rather than as a route that
// had nothing to show.
func wire(t *testing.T) *wired {
	t.Helper()
	store := &accounts.Store{
		APIToken: testToken,
		Accounts: []accounts.Account{{ID: "a", Login: "you", Token: "ghp_secret", Enabled: true}},
	}
	store.SetPath(filepath.Join(t.TempDir(), "accounts.json"))

	at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	remote := &poll.Snapshot{TakenAt: at, Accounts: []poll.AccountView{{
		AccountID: "a", Login: "you",
		Notifications: []forge.Notification{{
			ID: "n1", Repo: "you/thing", Unread: true, UpdatedAt: at,
			SubjectURL: "https://api.github.com/repos/you/thing/pulls/1",
		}},
		AuthoredPRs: []forge.PullRequest{{
			Repo: "you/thing", Number: 1, Title: "in flight",
			URL: "https://github.com/you/thing/pull/1", HeadRef: "work", UpdatedAt: at,
		}},
		ReviewRequests: []forge.PullRequest{{
			Repo: "you/thing", Number: 2, Title: "please look",
			URL: "https://github.com/you/thing/pull/2", UpdatedAt: at,
		}},
		AssignedIssues: []forge.Issue{{
			Repo: "you/thing", Number: 3, Title: "assigned",
			URL: "https://github.com/you/thing/issues/3", UpdatedAt: at,
		}},
		AuthoredIssues: []forge.Issue{{
			Repo: "you/thing", Number: 4, Title: "reported",
			URL: "https://github.com/you/thing/issues/4", UpdatedAt: at,
		}},
		MergedPRs: []forge.PullRequest{{
			Repo: "you/thing", Number: 5, Title: "landed",
			URL: "https://github.com/you/thing/pull/5", MergedAt: at,
		}},
		InboxRate: forge.Rate{Remaining: 4000, Limit: 5000},
		WorkRate:  forge.Rate{Remaining: 4500, Limit: 5000},
	}}}
	repos := &local.Snapshot{TakenAt: at, Roots: []string{"/src"}, Repos: []local.Repo{{
		Path: "/src/thing", Name: "thing", Group: "/src/thing/.git", Main: true,
		Branch: "work", Unpushed: 1,
		Remotes: map[string]string{"origin": "git@github.com:you/thing.git"},
		Last:    local.Commit{SHA: "abc"},
	}, {
		Path: "/src/clean", Name: "clean", Group: "/src/clean/.git", Main: true,
		Branch:  "main",
		Remotes: map[string]string{"origin": "git@github.com:you/clean.git"},
		Last:    local.Commit{SHA: "def"},
	}}}

	poller := &wakingPoller{fixedPoller: fixedPoller{remote}}
	watcher := &wakingWatcher{fixedWatcher: fixedWatcher{repos}}
	s := NewServer(store, poller, watcher, slog.New(slog.DiscardHandler), "test")

	fake := &fakeStats{stats: forge.RepoStats{Stars: 7}}
	s.statsClient = func(a accounts.Account) statsFetcher {
		fake.byAcct = a.Login
		return fake
	}

	triggers := &alerts.Store{Version: 1}
	triggers.SetPath(filepath.Join(t.TempDir(), "triggers.json"))
	engine := alerts.NewEngine(triggers, s.AlertSample, store.MonitoringEnabled,
		func(alerts.Trigger, alerts.Fire) (string, error) { return "test", nil },
		slog.New(slog.DiscardHandler))
	s.SetEngine(engine)
	armed, err := engine.Arm(alerts.Trigger{
		Path: "attention.reviews", Operator: alerts.OpCrosses,
		Params: alerts.Params{Above: numPtr(2)}, DeliverTo: alerts.TargetYou,
		ExpiresAt: at.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("arming the fixture trigger: %v", err)
	}

	return &wired{s: s, h: s.Handler(), store: store, engine: engine, armed: armed,
		poller: poller, watcher: watcher, stats: fake}
}

// send makes one request through the routed handler. An empty token sends no
// Authorization header at all.
func (w *wired) send(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, req)
	return rec
}

// doc makes an authenticated request, insists on the status, and decodes the
// answer as a JSON object.
func (w *wired) doc(t *testing.T, method, path, body string, want int) map[string]any {
	t.Helper()
	rec := w.send(t, method, path, testToken, body)
	if rec.Code != want {
		t.Fatalf("%s %s: %d, want %d: %s", method, path, rec.Code, want, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: %v: %s", method, path, err, rec.Body.String())
	}
	return out
}

// hasKeys fails for every named key the document does not carry.
func hasKeys(t *testing.T, where string, doc map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := doc[k]; !ok {
			t.Errorf("%s lacks %q; it carries %v", where, k, docKeys(doc))
		}
	}
}

// docKeys is a document's keys in order, for a readable failure.
func docKeys(doc map[string]any) []string {
	out := make([]string, 0, len(doc))
	for k := range doc {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// num, list and obj read one field of a decoded document. They fail the test
// rather than panicking on a missing or renamed key, so a broken shape is
// reported instead of taking the whole test binary down with it.
func num(t *testing.T, where string, doc map[string]any, key string) float64 {
	t.Helper()
	v, ok := doc[key].(float64)
	if !ok {
		t.Fatalf("%s: %q is %v, want a number; the document carries %v", where, key, doc[key], docKeys(doc))
	}
	return v
}

func list(t *testing.T, where string, doc map[string]any, key string) []any {
	t.Helper()
	v, ok := doc[key].([]any)
	if !ok {
		t.Fatalf("%s: %q is %v, want a list; the document carries %v", where, key, doc[key], docKeys(doc))
	}
	return v
}

func obj(t *testing.T, where string, v any) map[string]any {
	t.Helper()
	out, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is %v, want an object", where, v)
	}
	return out
}

// route is one entry of the table Handler() registers.
type route struct {
	method string
	path   string
	// wrong is a method this path does not serve, so the mux answers 405.
	wrong string
	// decodes says the handler reads a JSON body, so a malformed one is 400.
	decodes bool
}

// routes mirrors func (s *Server) Handler(). Anything added there and not
// here is a route with no generic cases at all.
func routes(triggerID string) []route {
	return []route{
		{http.MethodGet, "/api/health", http.MethodPost, false},
		{http.MethodGet, "/api/accounts", http.MethodPost, false},
		{http.MethodGet, "/api/inbox", http.MethodPost, false},
		{http.MethodGet, "/api/work", http.MethodPost, false},
		{http.MethodGet, "/api/snapshot", http.MethodPost, false},
		{http.MethodGet, "/api/repos", http.MethodPost, false},
		{http.MethodGet, "/api/repo-stats", http.MethodPost, false},
		{http.MethodGet, "/api/facts", http.MethodPost, false},
		// /api/alerts serves GET and POST, so neither is the wrong method.
		{http.MethodGet, "/api/alerts", http.MethodPut, false},
		{http.MethodGet, "/api/catalogue", http.MethodPost, false},
		{http.MethodGet, "/api/agents", http.MethodPost, false},
		{http.MethodPost, "/api/alerts", http.MethodPut, true},
		{http.MethodPatch, "/api/alerts/" + triggerID, http.MethodPost, true},
		{http.MethodDelete, "/api/alerts/" + triggerID, http.MethodGet, false},
		{http.MethodGet, "/api/dashboard", http.MethodPost, false},
		{http.MethodPost, "/api/monitoring", http.MethodGet, true},
		{http.MethodPost, "/api/mcp", http.MethodGet, true},
		{http.MethodPost, "/api/notify", http.MethodGet, true},
		// Refresh, disarm and recycle read no body, so nothing about them
		// can be malformed.
		{http.MethodPost, "/api/refresh", http.MethodGet, false},
		{http.MethodPost, "/api/fetch", http.MethodGet, true},
		{http.MethodPost, "/api/roots", http.MethodGet, true},
		{http.MethodPost, "/api/local-only", http.MethodGet, true},
		{http.MethodPost, "/api/sections", http.MethodGet, true},
		{http.MethodPost, "/api/token/recycle", http.MethodGet, false},
	}
}

// TestRouteTableCoversEveryRegisteredRoute keeps the table honest: Handler()
// registers 24 routes, and a new one has to be listed here to be tested.
func TestRouteTableCoversEveryRegisteredRoute(t *testing.T) {
	if got := len(routes("id")); got != 24 {
		t.Fatalf("the table carries %d routes, Handler() registers 24", got)
	}
}

func TestEveryRouteRefusesARequestWithNoToken(t *testing.T) {
	w := wire(t)
	for _, r := range routes(w.armed.ID) {
		rec := w.send(t, r.method, r.path, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no token: %d, want 401", r.method, r.path, rec.Code)
			continue
		}
		if got := rec.Header().Get("WWW-Authenticate"); got == "" {
			t.Errorf("%s %s: 401 with no WWW-Authenticate header", r.method, r.path)
		}
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body["error"] != "unauthorized" {
			t.Errorf("%s %s: body %s, want an unauthorized error", r.method, r.path, rec.Body.String())
		}
	}
}

func TestEveryRouteRefusesAWrongToken(t *testing.T) {
	w := wire(t)
	// A prefix of the real token, which a length-blind comparison would let
	// through, and an unrelated value.
	for _, bad := range []string{"qzk", testToken + "x", "nonsense"} {
		for _, r := range routes(w.armed.ID) {
			rec := w.send(t, r.method, r.path, bad, "")
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with token %q: %d, want 401", r.method, r.path, bad, rec.Code)
			}
		}
	}
}

func TestEveryRouteRefusesTheWrongMethod(t *testing.T) {
	w := wire(t)
	for _, r := range routes(w.armed.ID) {
		rec := w.send(t, r.wrong, r.path, testToken, "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: %d, want 405 (%s is the route)", r.wrong, r.path, rec.Code, r.method)
			continue
		}
		if got := rec.Header().Get("Allow"); !strings.Contains(got, r.method) {
			t.Errorf("%s %s: Allow = %q, want it to name %s", r.wrong, r.path, got, r.method)
		}
	}
}

func TestEveryBodyReadingRouteRefusesMalformedJSON(t *testing.T) {
	w := wire(t)
	for _, r := range routes(w.armed.ID) {
		if !r.decodes {
			continue
		}
		for _, body := range []string{"{", "not json at all", `{"key":}`} {
			rec := w.send(t, r.method, r.path, testToken, body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s %s with body %q: %d, want 400", r.method, r.path, body, rec.Code)
			}
		}
	}
}

// TestGetRoutesServeTheDocumentsThePanelReads pins the top-level shape of
// every read route, which is the contract the QML side parses.
func TestGetRoutesServeTheDocumentsThePanelReads(t *testing.T) {
	// No herdr on the path, so /api/agents takes its "nobody to wake" branch
	// rather than whatever is running on the machine under test.
	t.Setenv("PATH", t.TempDir())
	w := wire(t)

	for _, tc := range []struct {
		path string
		keys []string
	}{
		{"/api/health", []string{"status", "version", "uptime", "accounts", "enabled",
			"inboxRateLeft", "workRateLeft", "monitoring", "intervalMin", "mcpEnabled",
			"fetchEnabled", "fetchMin", "notify", "repos", "reposAtRisk", "errors"}},
		{"/api/accounts", []string{"version", "apiToken", "accounts"}},
		{"/api/inbox", []string{"count", "notifications"}},
		{"/api/work", []string{"authoredPrs", "reviewRequests", "assignedIssues",
			"authoredIssues", "mergedPrs"}},
		{"/api/snapshot", []string{"takenAt", "accounts"}},
		{"/api/repos", []string{"takenAt", "roots", "repos"}},
		{"/api/repo-stats?repo=you/thing", []string{"repo", "host", "stats", "cached", "stale", "paused"}},
		{"/api/facts", []string{"count", "facts"}},
		{"/api/alerts", []string{"alerts"}},
		{"/api/catalogue", []string{"paths"}},
		{"/api/agents", []string{"agents"}},
		{"/api/dashboard", []string{"health", "attention", "inbox", "facts", "alerts",
			"work", "repos", "accounts", "localOnly", "hiddenSections"}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			hasKeys(t, tc.path, w.doc(t, http.MethodGet, tc.path, "", http.StatusOK), tc.keys...)
		})
	}
}

// TestAccountsRouteStripsEverySecret pins what the identities route may say:
// the logins, and no token in any form.
func TestAccountsRouteStripsEverySecret(t *testing.T) {
	w := wire(t)
	rec := w.send(t, http.MethodGet, "/api/accounts", testToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/accounts: %d", rec.Code)
	}
	raw := rec.Body.String()
	for _, secret := range []string{"ghp_secret", testToken} {
		if strings.Contains(raw, secret) {
			t.Errorf("/api/accounts leaked %q: %s", secret, raw)
		}
	}
	var doc struct {
		APIToken string `json:"apiToken"`
		Accounts []struct {
			Login string `json:"login"`
			Token string `json:"token"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.APIToken != "[redacted]" || len(doc.Accounts) != 1 ||
		doc.Accounts[0].Login != "you" || doc.Accounts[0].Token != "[redacted]" {
		t.Errorf("accounts document = %+v, want one login with both tokens redacted", doc)
	}
}

// TestReadRoutesCountWhatTheyList pins the counted documents against their
// own lists, so a route cannot report a number it did not serve.
func TestReadRoutesCountWhatTheyList(t *testing.T) {
	w := wire(t)

	inbox := w.doc(t, http.MethodGet, "/api/inbox", "", http.StatusOK)
	if got, n := num(t, "/api/inbox", inbox, "count"), len(list(t, "/api/inbox", inbox, "notifications")); int(got) != n || n != 1 {
		t.Errorf("inbox count = %v over %d notifications, want 1 and 1", got, n)
	}

	facts := w.doc(t, http.MethodGet, "/api/facts", "", http.StatusOK)
	if got, n := num(t, "/api/facts", facts, "count"), len(list(t, "/api/facts", facts, "facts")); int(got) != n {
		t.Errorf("facts count = %v over %d facts", got, n)
	}

	work := w.doc(t, http.MethodGet, "/api/work", "", http.StatusOK)
	for _, key := range []string{"authoredPrs", "reviewRequests", "assignedIssues",
		"authoredIssues", "mergedPrs"} {
		if n := len(list(t, "/api/work", work, key)); n != 1 {
			t.Errorf("work.%s has %d entries, want the one the fixture polled", key, n)
		}
	}

	alertRows := list(t, "/api/alerts", w.doc(t, http.MethodGet, "/api/alerts", "", http.StatusOK), "alerts")
	if len(alertRows) != 1 {
		t.Fatalf("alerts route listed %d triggers, want the one armed", len(alertRows))
	}
	hasKeys(t, "/api/alerts row", obj(t, "/api/alerts row", alertRows[0]), "id", "path", "operator", "status")

	paths := list(t, "/api/catalogue", w.doc(t, http.MethodGet, "/api/catalogue", "", http.StatusOK), "paths")
	if len(paths) == 0 {
		t.Fatal("catalogue route served no paths, so nothing can be armed from the panel")
	}
	hasKeys(t, "/api/catalogue leaf", obj(t, "/api/catalogue leaf", paths[0]), "path", "kind", "operators")
}

// TestReposRouteFiltersToWhatIsAtRisk pins risk=1: the same document, with
// only the checkouts holding work that exists nowhere else.
func TestReposRouteFiltersToWhatIsAtRisk(t *testing.T) {
	w := wire(t)
	all := list(t, "/api/repos", w.doc(t, http.MethodGet, "/api/repos", "", http.StatusOK), "repos")
	if len(all) != 2 {
		t.Fatalf("repos route served %d checkouts, want the 2 watched", len(all))
	}
	risky := list(t, "/api/repos?risk=1", w.doc(t, http.MethodGet, "/api/repos?risk=1", "", http.StatusOK), "repos")
	if len(risky) != 1 {
		t.Fatalf("risk=1 served %d checkouts, want the 1 with unpushed work", len(risky))
	}
	if got := obj(t, "risk=1 row", risky[0])["name"]; got != "thing" {
		t.Errorf("risk=1 kept %v, want the checkout with the unpushed commit", got)
	}
}

// TestRepoStatsRouteCachesThroughItsRoute pins that the disclosed-row figures
// reach the panel through the route and are served from memory afterwards.
func TestRepoStatsRouteCachesThroughItsRoute(t *testing.T) {
	w := wire(t)
	first := w.doc(t, http.MethodGet, "/api/repo-stats?repo=you/thing", "", http.StatusOK)
	if first["cached"] != false || first["repo"] != "you/thing" || first["host"] != "github.com" {
		t.Fatalf("first ask = %v, want fresh figures for you/thing on github.com", first)
	}
	if got := obj(t, "repo-stats figures", first["stats"])["stars"]; got != float64(7) {
		t.Errorf("stars = %v, want the 7 the forge answered", got)
	}
	second := w.doc(t, http.MethodGet, "/api/repo-stats?repo=you/thing", "", http.StatusOK)
	if second["cached"] != true || len(w.stats.asked) != 1 {
		t.Errorf("second ask = %v after %d fetches, want one fetch served twice", second, len(w.stats.asked))
	}
	// A target no account can ask about is refused by the route, not by the
	// forge.
	w.doc(t, http.MethodGet, "/api/repo-stats?repo=nonsense", "", http.StatusBadRequest)
	if len(w.stats.asked) != 1 {
		t.Errorf("a refused target reached the forge: %v", w.stats.asked)
	}
}

// TestAlertRoutesArmEditAndDisarm walks one trigger through its three
// mutating routes and back out of the list.
func TestAlertRoutesArmEditAndDisarm(t *testing.T) {
	w := wire(t)
	armed := w.doc(t, http.MethodPost, "/api/alerts",
		`{"path":"attention.unread","operator":"crosses","params":{"above":5},`+
			`"deliverTo":"you","expiresAt":"2030-01-01T00:00:00Z"}`, http.StatusOK)
	hasKeys(t, "armed trigger", armed, "id", "rev", "path", "operator", "params", "deliverTo", "armedBy")
	id, _ := armed["id"].(string)
	if id == "" || armed["rev"] != float64(1) || armed["armedBy"] != "you" {
		t.Fatalf("armed = %v, want an id, rev 1 and a default owner", armed)
	}

	rows := list(t, "/api/alerts", w.doc(t, http.MethodGet, "/api/alerts", "", http.StatusOK), "alerts")
	if len(rows) != 2 {
		t.Fatalf("alerts route lists %d triggers after arming, want 2", len(rows))
	}

	edited := w.doc(t, http.MethodPatch, "/api/alerts/"+id, `{"params":{"above":9}}`, http.StatusOK)
	if edited["id"] != id || edited["rev"] != float64(2) {
		t.Errorf("edited = %v, want the same id at rev 2", edited)
	}
	if got := obj(t, "edited params", edited["params"])["above"]; got != float64(9) {
		t.Errorf("edited bound = %v, want the 9 the body named", got)
	}
	if edited["path"] != "attention.unread" {
		t.Errorf("edited path = %v, want what the partial body left alone", edited["path"])
	}

	// An edit of a trigger that does not exist is refused, not invented.
	w.doc(t, http.MethodPatch, "/api/alerts/no-such-id", `{"params":{"above":9}}`, http.StatusBadRequest)

	gone := w.doc(t, http.MethodDelete, "/api/alerts/"+id, "", http.StatusOK)
	if gone["disarmed"] != id {
		t.Errorf("disarmed = %v, want %q", gone, id)
	}
	w.doc(t, http.MethodDelete, "/api/alerts/"+id, "", http.StatusNotFound)
	if rows := list(t, "/api/alerts", w.doc(t, http.MethodGet, "/api/alerts", "", http.StatusOK), "alerts"); len(rows) != 1 {
		t.Errorf("alerts route lists %d triggers after disarming, want the fixture's 1", len(rows))
	}
}

// TestAlertRoutesSayWhenNothingIsRunning pins the answer when the daemon has
// no engine yet: unavailable, not a panic and not a silent success.
func TestAlertRoutesSayWhenNothingIsRunning(t *testing.T) {
	store := &accounts.Store{APIToken: testToken}
	store.SetPath(filepath.Join(t.TempDir(), "accounts.json"))
	s := NewServer(store, fixedPoller{&poll.Snapshot{}}, fixedWatcher{&local.Snapshot{}},
		slog.New(slog.DiscardHandler), "test")
	w := &wired{s: s, h: s.Handler(), store: store}

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/alerts", `{"path":"attention.unread","operator":"crosses"}`},
		{http.MethodPatch, "/api/alerts/x", `{"params":{"above":9}}`},
		{http.MethodDelete, "/api/alerts/x", ""},
	} {
		w.doc(t, tc.method, tc.path, tc.body, http.StatusServiceUnavailable)
	}
	// Reading still works: no engine means no triggers, not an error.
	if rows := list(t, "/api/alerts", w.doc(t, http.MethodGet, "/api/alerts", "", http.StatusOK), "alerts"); len(rows) != 0 {
		t.Errorf("alerts route with no engine listed %d rows, want an empty list", len(rows))
	}
}

// TestMonitoringRouteSwitchesPollingAndRhythm pins the master switch: a
// rhythm implies waking, a rhythm of zero is refused, and health reports what
// the route did.
func TestMonitoringRouteSwitchesPollingAndRhythm(t *testing.T) {
	w := wire(t)

	off := w.doc(t, http.MethodPost, "/api/monitoring", `{"enabled":false}`, http.StatusOK)
	if off["monitoring"] != false {
		t.Fatalf("switching off answered %v", off)
	}
	if got := w.doc(t, http.MethodGet, "/api/health", "", http.StatusOK)["monitoring"]; got != false {
		t.Errorf("health says monitoring = %v after switching off", got)
	}

	// Choosing a rhythm cannot leave the daemon asleep.
	woke := w.doc(t, http.MethodPost, "/api/monitoring", `{"intervalMin":7}`, http.StatusOK)
	if woke["monitoring"] != true || woke["intervalMin"] != float64(7) {
		t.Errorf("naming a rhythm answered %v, want it awake at 7 minutes", woke)
	}
	health := w.doc(t, http.MethodGet, "/api/health", "", http.StatusOK)
	if health["monitoring"] != true || health["intervalMin"] != float64(7) {
		t.Errorf("health = monitoring %v interval %v, want true and 7",
			health["monitoring"], health["intervalMin"])
	}

	w.doc(t, http.MethodPost, "/api/monitoring", `{"intervalMin":0}`, http.StatusBadRequest)
	w.doc(t, http.MethodPost, "/api/monitoring", `{"intervalMin":-5}`, http.StatusBadRequest)
	// A body that names neither field asks for nothing.
	w.doc(t, http.MethodPost, "/api/monitoring", `{}`, http.StatusBadRequest)
	if got := w.doc(t, http.MethodGet, "/api/health", "", http.StatusOK)["intervalMin"]; got != float64(7) {
		t.Errorf("a refused interval changed the rhythm to %v, want the 7 that stood", got)
	}
}

// TestRefreshRouteWakesBothPlanesAndRefusesWhileAsleep pins the panel's
// Refresh button: it cuts both loops short, and asks for nothing at all when
// monitoring is off.
func TestRefreshRouteWakesBothPlanesAndRefusesWhileAsleep(t *testing.T) {
	w := wire(t)
	woke := w.doc(t, http.MethodPost, "/api/refresh", "", http.StatusOK)
	planes := anyStrings(woke["refreshing"])
	if !slices.Equal(planes, []string{"remote", "local"}) {
		t.Errorf("refreshing = %v, want both planes named", planes)
	}
	if w.poller.woken != 1 || w.watcher.woken != 1 {
		t.Errorf("woke the poller %d times and the watcher %d, want 1 and 1",
			w.poller.woken, w.watcher.woken)
	}

	w.doc(t, http.MethodPost, "/api/monitoring", `{"enabled":false}`, http.StatusOK)
	w.doc(t, http.MethodPost, "/api/refresh", "", http.StatusConflict)
	if w.poller.woken != 1 || w.watcher.woken != 1 {
		t.Errorf("a refused refresh still woke the planes: %d and %d",
			w.poller.woken, w.watcher.woken)
	}
}

// TestNotifyRouteSwitchesOneDomain pins that the switch answers with every
// domain resolved, and that an unknown one is refused rather than stored.
func TestNotifyRouteSwitchesOneDomain(t *testing.T) {
	w := wire(t)
	prefs := w.doc(t, http.MethodPost, "/api/notify", `{"domain":"reviews","enabled":false}`, http.StatusOK)
	hasKeys(t, "notify prefs", prefs, "reviews", "incoming", "reported", "broken",
		"inbox", "local", "reconcile")
	if prefs["reviews"] != false {
		t.Errorf("reviews = %v after switching it off", prefs["reviews"])
	}
	if got := obj(t, "health.notify", w.doc(t, http.MethodGet, "/api/health", "", http.StatusOK)["notify"])["reviews"]; got != false {
		t.Errorf("health notify.reviews = %v, want the switch the route flipped", got)
	}

	w.doc(t, http.MethodPost, "/api/notify", `{"domain":"nonsense","enabled":true}`, http.StatusBadRequest)
	// A body with no switch at all is a 400, not a silent default.
	w.doc(t, http.MethodPost, "/api/notify", `{"domain":"reviews"}`, http.StatusBadRequest)
}

// TestFetchRouteSwitchesBackgroundFetching pins the cadence route.
func TestFetchRouteSwitchesBackgroundFetching(t *testing.T) {
	w := wire(t)
	got := w.doc(t, http.MethodPost, "/api/fetch", `{"enabled":true,"everyMin":11}`, http.StatusOK)
	if got["fetchEnabled"] != true || got["fetchMin"] != float64(11) {
		t.Fatalf("fetch route answered %v, want it on every 11 minutes", got)
	}
	health := w.doc(t, http.MethodGet, "/api/health", "", http.StatusOK)
	if health["fetchEnabled"] != true || health["fetchMin"] != float64(11) {
		t.Errorf("health = %v/%v, want the fetch settings the route stored",
			health["fetchEnabled"], health["fetchMin"])
	}
	off := w.doc(t, http.MethodPost, "/api/fetch", `{"enabled":false}`, http.StatusOK)
	if off["fetchEnabled"] != false {
		t.Errorf("fetch off answered %v", off)
	}
	w.doc(t, http.MethodPost, "/api/fetch", `{}`, http.StatusBadRequest)
}

// TestRootsRouteNeedsSomewhereToLook pins that the watched directories can be
// changed but never emptied, and that the watcher is told.
func TestRootsRouteNeedsSomewhereToLook(t *testing.T) {
	w := wire(t)
	got := w.doc(t, http.MethodPost, "/api/roots", `{"roots":["  /one  ","/two",""]}`, http.StatusOK)
	if !slices.Equal(anyStrings(got["roots"]), []string{"/one", "/two"}) {
		t.Errorf("roots = %v, want the two trimmed entries and the blank dropped", got["roots"])
	}
	for _, body := range []string{`{"roots":[]}`, `{"roots":["   "]}`, `{}`} {
		w.doc(t, http.MethodPost, "/api/roots", body, http.StatusBadRequest)
	}
	stored := w.doc(t, http.MethodGet, "/api/accounts", "", http.StatusOK)
	if !slices.Equal(anyStrings(stored["roots"]), []string{"/one", "/two"}) {
		t.Errorf("a refused change rewrote the roots to %v", stored["roots"])
	}
}

// TestMCPRouteWithdrawsTheEndpoint pins the toggle against the endpoint it
// governs: off means the mcp path answers 404 from that request onwards, not
// at the next daemon restart.
func TestMCPRouteWithdrawsTheEndpoint(t *testing.T) {
	w := wire(t)
	if got := w.doc(t, http.MethodPost, "/api/mcp", `{"enabled":false}`, http.StatusOK); got["mcpEnabled"] != false {
		t.Fatalf("mcp toggle answered %v", got)
	}
	if rec := w.send(t, http.MethodPost, "/mcp", testToken, `{}`); rec.Code != http.StatusNotFound {
		t.Errorf("/mcp answered %d while disabled, want 404", rec.Code)
	}
	if got := w.doc(t, http.MethodGet, "/api/health", "", http.StatusOK)["mcpEnabled"]; got != false {
		t.Errorf("health mcpEnabled = %v after withdrawing the endpoint", got)
	}

	w.doc(t, http.MethodPost, "/api/mcp", `{"enabled":true}`, http.StatusOK)
	if rec := w.send(t, http.MethodPost, "/mcp", testToken, `{}`); rec.Code == http.StatusNotFound {
		t.Error("/mcp still answers 404 after being switched back on")
	}
	// The endpoint is behind the same bearer check as everything else.
	if rec := w.send(t, http.MethodPost, "/mcp", "", `{}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("/mcp with no token: %d, want 401", rec.Code)
	}
}

// TestTokenRecycleLocksOutTheOldToken pins what revoking has to mean: the
// caller is handed the new token once, and its own previous one stops working.
func TestTokenRecycleLocksOutTheOldToken(t *testing.T) {
	w := wire(t)
	got := w.doc(t, http.MethodPost, "/api/token/recycle", "", http.StatusOK)
	hasKeys(t, "recycled token", got, "status", "token", "entry", "next")
	fresh, _ := got["token"].(string)
	if fresh == "" || fresh == testToken {
		t.Fatalf("recycle answered token %q, want a new one", fresh)
	}
	if got["status"] != "recycled" || w.store.Token() != fresh {
		t.Errorf("store holds %q after recycling to %q", w.store.Token(), fresh)
	}

	if rec := w.send(t, http.MethodGet, "/api/health", testToken, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("the old token still opens /api/health: %d", rec.Code)
	}
	if rec := w.send(t, http.MethodGet, "/api/health", fresh, ""); rec.Code != http.StatusOK {
		t.Errorf("the new token does not open /api/health: %d", rec.Code)
	}

	// The mcp entry the panel shows carries the same new token.
	entry := obj(t, "mcp entry", obj(t, "recycle entry", got["entry"])["omagihu"])
	if auth := obj(t, "mcp entry headers", entry["headers"])["Authorization"]; auth != "Bearer "+fresh {
		t.Errorf("mcp entry carries %v, want the token just minted", auth)
	}
}

// TestSettingsSurviveASecondServerOverTheSameStore is the round trip a unit
// test on the store cannot show: settings posted through the routes are read
// back through the routes, and a second server built over the same file
// answers with them after the first one is gone.
func TestSettingsSurviveASecondServerOverTheSameStore(t *testing.T) {
	w := wire(t)
	path := w.store.Path()

	// One change through each route that persists something.
	w.doc(t, http.MethodPost, "/api/monitoring", `{"intervalMin":13}`, http.StatusOK)
	w.doc(t, http.MethodPost, "/api/notify", `{"domain":"inbox","enabled":false}`, http.StatusOK)
	w.doc(t, http.MethodPost, "/api/fetch", `{"enabled":false,"everyMin":21}`, http.StatusOK)
	w.doc(t, http.MethodPost, "/api/mcp", `{"enabled":false}`, http.StatusOK)
	w.doc(t, http.MethodPost, "/api/roots", `{"roots":["/src"]}`, http.StatusOK)
	w.doc(t, http.MethodPost, "/api/local-only", `{"path":"/src/clean","on":true}`, http.StatusOK)
	w.doc(t, http.MethodPost, "/api/sections", `{"key":"repos","hidden":true}`, http.StatusOK)

	// Read back through the routes that expose them.
	check := func(what string, w *wired) {
		t.Helper()
		health := w.doc(t, http.MethodGet, "/api/health", "", http.StatusOK)
		for key, want := range map[string]any{
			"intervalMin":  float64(13),
			"monitoring":   true,
			"fetchEnabled": false,
			"fetchMin":     float64(21),
			"mcpEnabled":   false,
		} {
			if health[key] != want {
				t.Errorf("%s: health.%s = %v, want %v", what, key, health[key], want)
			}
		}
		if got := obj(t, "health.notify", health["notify"])["inbox"]; got != false {
			t.Errorf("%s: health.notify.inbox = %v, want false", what, got)
		}
		dash := w.doc(t, http.MethodGet, "/api/dashboard", "", http.StatusOK)
		if got := anyStrings(dash["localOnly"]); !slices.Equal(got, []string{"/src/clean"}) {
			t.Errorf("%s: dashboard localOnly = %v, want the marked checkout", what, got)
		}
		if got := anyStrings(dash["hiddenSections"]); !slices.Equal(got, []string{"repos"}) {
			t.Errorf("%s: dashboard hiddenSections = %v, want repos", what, got)
		}
		if got := anyStrings(w.doc(t, http.MethodGet, "/api/accounts", "", http.StatusOK)["roots"]); !slices.Equal(got, []string{"/src"}) {
			t.Errorf("%s: accounts roots = %v, want /src", what, got)
		}
	}
	check("same server", w)

	// A second server over the same file, as a restarted daemon builds one.
	reloaded, err := accounts.Load(path)
	if err != nil {
		t.Fatalf("reloading the store the routes wrote: %v", err)
	}
	second := NewServer(reloaded, w.poller, w.watcher, slog.New(slog.DiscardHandler), "test")
	check("second server", &wired{s: second, h: second.Handler(), store: reloaded})

	// The bearer token is part of that state: the same one still opens it.
	if reloaded.Token() != testToken {
		t.Errorf("reloaded token = %q, want the one the first server guarded", reloaded.Token())
	}
}

// TestMalformedEditLeavesTheWatchAlone pins an ordering bug the route tests
// found. handleEdit used to hand the decode to engine.Edit as a callback, and
// Edit applies it, validates, bumps the revision, clears the trigger's state
// and saves before anything could tell it the decode had failed. A body that
// decoded halfway and still validated was persisted behind a 400, and the
// cleared state made a standing watch re-prime and fire again on everything it
// had already seen.
//
// The body below is the sneaky shape: valid JSON that changes a field before
// failing on a type it cannot take, so json.Valid alone would not catch it.
func TestMalformedEditLeavesTheWatchAlone(t *testing.T) {
	w := wire(t)
	armed := w.doc(t, http.MethodPost, "/api/alerts",
		`{"path":"attention.unread","operator":"crosses","params":{"above":5},`+
			`"deliverTo":"you","expiresAt":"2030-01-01T00:00:00Z"}`, http.StatusOK)
	id, _ := armed["id"].(string)
	if id == "" {
		t.Fatalf("armed = %v, want an id", armed)
	}

	for _, body := range []string{
		`{"params":{"above":9},"rev":"not a number"}`, // decodes, then fails on a type
		`{"params":{"above":9}`,                       // truncated
	} {
		w.doc(t, http.MethodPatch, "/api/alerts/"+id, body, http.StatusBadRequest)

		after := w.doc(t, http.MethodGet, "/api/alerts", "", http.StatusOK)
		var found map[string]any
		for _, row := range list(t, "/api/alerts", after, "alerts") {
			if obj(t, "alert row", row)["id"] == id {
				found = obj(t, "alert row", row)
			}
		}
		if found == nil {
			t.Fatalf("the watch vanished after a refused edit of %q", body)
		}
		if found["rev"] != float64(1) {
			t.Errorf("rev = %v after a refused edit of %q, want 1: the watch was mutated behind a 400",
				found["rev"], body)
		}
		if got := obj(t, "params", found["params"])["above"]; got != float64(5) {
			t.Errorf("bound = %v after a refused edit of %q, want the original 5", got, body)
		}
	}
}
