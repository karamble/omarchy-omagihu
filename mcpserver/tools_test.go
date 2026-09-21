package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/karamble/omarchy-omagihu/alerts"
	"github.com/karamble/omarchy-omagihu/correlate"
	"github.com/karamble/omarchy-omagihu/forge"
	"github.com/karamble/omarchy-omagihu/local"
	"github.com/karamble/omarchy-omagihu/poll"
)

// The fixed values every tool is driven over, so an assertion below names a
// value that could only have come from here.
var sampledAt = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

// fixedRemote is two accounts that both saw pull request 2 and both list pull
// request 1, so the merge has something to deduplicate.
var fixedRemote = &poll.Snapshot{
	TakenAt: sampledAt,
	Accounts: []poll.AccountView{
		{
			AccountID: "acct-one",
			Login:     "owner",
			Notifications: []forge.Notification{{
				AccountID:  "acct-one",
				ID:         "n1",
				Repo:       "o/r",
				Type:       "PullRequest",
				Title:      "tighten the poller",
				Reason:     "review_requested",
				Unread:     true,
				UpdatedAt:  sampledAt,
				SubjectURL: "https://api.github.com/repos/o/r/pulls/2",
				WebURL:     "https://github.com/o/r/pull/2",
			}},
			AuthoredPRs: []forge.PullRequest{{
				AccountID: "acct-one", Repo: "o/r", Number: 1,
				Title: "rework the watcher", URL: "https://github.com/o/r/pull/1",
				Author: "owner", HeadRef: "watcher", BaseRef: "main",
				HeadSHA: "aaaa111", ReviewDecision: "CHANGES_REQUESTED",
				ChecksState: "FAILURE", UpdatedAt: sampledAt,
			}},
			ReviewRequests: []forge.PullRequest{{
				AccountID: "acct-one", Repo: "o/r", Number: 2,
				Title: "tighten the poller", URL: "https://github.com/o/r/pull/2",
				Author: "somebody", HeadRef: "poller", BaseRef: "main",
				HeadSHA: "bbbb222", ReviewDecision: "REVIEW_REQUIRED",
				ChecksState: "SUCCESS", UpdatedAt: sampledAt, Incoming: true,
			}},
			AssignedIssues: []forge.Issue{{
				AccountID: "acct-one", Repo: "o/r", Number: 3,
				Title: "crash on an empty root", URL: "https://github.com/o/r/issues/3",
				Author: "reporter", Labels: []forge.Label{{Name: "bug", Color: "d73a4a"}},
				UpdatedAt: sampledAt, Incoming: true,
			}},
			AuthoredIssues: []forge.Issue{{
				AccountID: "acct-one", Repo: "o/other", Number: 4,
				Title: "listing request", URL: "https://github.com/o/other/issues/4",
				Author: "owner", Labels: []forge.Label{{Name: "submission", Color: "0e8a16"}},
				UpdatedAt: sampledAt,
			}},
		},
		{
			AccountID: "acct-two",
			Login:     "alt",
			Notifications: []forge.Notification{{
				AccountID: "acct-two", ID: "n2", Repo: "o/r", Type: "PullRequest",
				Title: "tighten the poller", Reason: "mention", Unread: true,
				UpdatedAt:  sampledAt,
				SubjectURL: "https://api.github.com/repos/o/r/pulls/2",
				WebURL:     "https://github.com/o/r/pull/2",
			}},
			AuthoredPRs: []forge.PullRequest{{
				AccountID: "acct-two", Repo: "o/r", Number: 1,
				Title: "rework the watcher", URL: "https://github.com/o/r/pull/1",
			}},
		},
	},
}

// riskyRepo holds work that would be lost: unpushed commits and a rebase left
// half finished.
var riskyRepo = local.Repo{
	Path: "/checkout/r", Name: "r", Branch: "watcher",
	Upstream: "origin/watcher", Group: "/checkout/r/.git", Main: true,
	Ahead: 2, Behind: 1,
	Staged: 1, Modified: 2, Untracked: 1, Stashes: 1,
	Unpushed: 2, Operation: local.OpRebase,
	Remotes: map[string]string{"origin": "git@github.com:o/r.git"},
	Last: local.Commit{
		SHA: "aaaa111", Subject: "rework the watcher", Author: "owner", At: sampledAt,
	},
	ObservedAt: sampledAt,
}

// calmRepo is dirty and nothing more, which is the normal state of a machine in
// use: omagihu_risk must leave it out.
var calmRepo = local.Repo{
	Path: "/checkout/other", Name: "other", Branch: "main",
	Upstream: "origin/main", Group: "/checkout/other/.git", Main: true,
	Modified: 3,
	Remotes:  map[string]string{"origin": "git@github.com:o/other.git"},
	Last: local.Commit{
		SHA: "cccc333", Subject: "bump the version", Author: "owner", At: sampledAt,
	},
	ObservedAt: sampledAt,
}

var fixedFact = correlate.Fact{
	Kind: correlate.KindMissingWork, Severity: correlate.Urgent,
	Repo: "o/r", Path: "/checkout/r", Branch: "watcher",
	Summary: "2 commits are not on the pull request",
	Detail:  "pull request 1 is missing work that exists only here",
	URL:     "https://github.com/o/r/pull/1", Number: 1,
}

// fakeRemote and fakeLocal are the two snapshot planes reduced to a fixed read.
type fakeRemote struct{ snap *poll.Snapshot }

func (f fakeRemote) Snapshot() *poll.Snapshot { return f.snap }

type fakeLocal struct{ snap *local.Snapshot }

func (f fakeLocal) Snapshot() *local.Snapshot { return f.snap }

// fakeAlerts is the trigger engine reduced to what the tools call: a list that
// arm, edit and disarm work on in memory. armed records what Arm was handed, so
// a test can check what the request was turned into before it was stored.
type fakeAlerts struct {
	triggers []alerts.Trigger
	armed    []alerts.Trigger
}

func (f *fakeAlerts) List() []alerts.Trigger { return f.triggers }

func (f *fakeAlerts) Arm(t alerts.Trigger) (alerts.Trigger, error) {
	f.armed = append(f.armed, t)
	t.ID = fmt.Sprintf("t-%d", len(f.triggers)+1)
	t.Rev = 1
	t.State = alerts.State{Ready: true, Primed: true}
	f.triggers = append(f.triggers, t)
	return t, nil
}

func (f *fakeAlerts) Edit(id string, apply func(*alerts.Trigger)) (alerts.Trigger, error) {
	for i := range f.triggers {
		if f.triggers[i].ID != id {
			continue
		}
		apply(&f.triggers[i])
		f.triggers[i].Rev++
		return f.triggers[i], nil
	}
	return alerts.Trigger{}, fmt.Errorf("no trigger %q", id)
}

func (f *fakeAlerts) Disarm(id string) (bool, error) {
	for i, t := range f.triggers {
		if t.ID == id {
			f.triggers = slices.Delete(f.triggers, i, i+1)
			return true, nil
		}
	}
	return false, nil
}

// armedTrigger is a watch that has taken a sample and can still fire.
func armedTrigger() alerts.Trigger {
	return alerts.Trigger{
		ID: "t-armed", Rev: 3, Path: "work.reviewRequests", Operator: alerts.OpCount,
		Params:    alerts.Params{Above: ptr(2), Rearm: ptr(1)},
		Where:     []alerts.Where{{Field: "repo", Op: "=", Value: "o/r"}},
		DeliverTo: alerts.TargetYou, Standing: true,
		ExpiresAt: sampledAt.Add(48 * time.Hour),
		Reason:    "the queue is backing up",
		ArmedBy:   "pane-7", ArmedAt: sampledAt,
		State: alerts.State{Ready: true, Primed: true, LastValue: float64(1)},
	}
}

// firedTrigger has already rung, so the row's status has to say so rather than
// repeating whatever the first row said.
func firedTrigger() alerts.Trigger {
	return alerts.Trigger{
		ID: "t-fired", Rev: 1, Path: "inbox", Operator: alerts.OpAppears,
		DeliverTo: alerts.TargetYou,
		ExpiresAt: sampledAt.Add(72 * time.Hour),
		ArmedBy:   "agent", ArmedAt: sampledAt,
		State: alerts.State{
			Primed: true, FireCount: 1, FiredAt: sampledAt, Delivered: "you",
		},
	}
}

func ptr(f float64) *float64 { return &f }

// stub builds a Source over the fixed values above, which is all the tools
// read. A nil engine is a daemon that came up without alerts.
func stub(engine Alerts) Source {
	src := Source{
		Remote:     fakeRemote{fixedRemote},
		Local:      fakeLocal{&local.Snapshot{TakenAt: sampledAt, Repos: []local.Repo{riskyRepo, calmRepo}}},
		Monitoring: func() bool { return true },
		Facts:      func() []correlate.Fact { return []correlate.Fact{fixedFact} },
	}
	if engine != nil {
		src.Alerts = func() Alerts { return engine }
	}
	return src
}

// serve connects the real server to a client over the SDK's in-memory
// transport, so a test reads exactly what an agent would read: the registered
// tools, over the protocol, rather than a copy of what they should return.
func serve(t *testing.T, src Source) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := newServer(src, "test").Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("connect server: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// invoke calls one tool and hands back the raw result, error and all.
func invoke(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	params := &mcp.CallToolParams{Name: name}
	if args != nil {
		params.Arguments = args
	}
	res, err := s.CallTool(context.Background(), params)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

// call runs one tool and hands back its payload as keys on the wire rather than
// as a Go struct, which is what makes these assertions bite: a field that stops
// being emitted is an absent key here, not a zero value.
func call(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	res := invoke(t, s, name, args)
	if res.IsError {
		t.Fatalf("%s refused: %s", name, message(res))
	}
	payload, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("%s returned no structured payload (%T)", name, res.StructuredContent)
	}
	return payload
}

// message is the text a refusal carries.
func message(res *mcp.CallToolResult) string {
	var out []string
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			out = append(out, text.Text)
		}
	}
	return strings.Join(out, "\n")
}

// field walks the payload and fails naming the key that is not there, which is
// the whole point: a field a caller depends on cannot vanish quietly.
func field(t *testing.T, payload map[string]any, path ...string) any {
	t.Helper()
	var cur any = payload
	for i, key := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%s is not an object, so %q cannot be read", strings.Join(path[:i], "."), key)
		}
		v, ok := obj[key]
		if !ok {
			t.Fatalf("missing field %q", strings.Join(path[:i+1], "."))
		}
		cur = v
	}
	return cur
}

func str(t *testing.T, payload map[string]any, path ...string) string {
	t.Helper()
	v, ok := field(t, payload, path...).(string)
	if !ok {
		t.Fatalf("field %q is not text", strings.Join(path, "."))
	}
	return v
}

func num(t *testing.T, payload map[string]any, path ...string) float64 {
	t.Helper()
	v, ok := field(t, payload, path...).(float64)
	if !ok {
		t.Fatalf("field %q is not a number", strings.Join(path, "."))
	}
	return v
}

func boolean(t *testing.T, payload map[string]any, path ...string) bool {
	t.Helper()
	v, ok := field(t, payload, path...).(bool)
	if !ok {
		t.Fatalf("field %q is not a bool", strings.Join(path, "."))
	}
	return v
}

// object reads a field that carries a nested object.
func object(t *testing.T, payload map[string]any, path ...string) map[string]any {
	t.Helper()
	v, ok := field(t, payload, path...).(map[string]any)
	if !ok {
		t.Fatalf("field %q is not an object", strings.Join(path, "."))
	}
	return v
}

func list(t *testing.T, payload map[string]any, path ...string) []any {
	t.Helper()
	v, ok := field(t, payload, path...).([]any)
	if !ok {
		t.Fatalf("field %q is not a list", strings.Join(path, "."))
	}
	return v
}

// row reads one entry of a list as an object.
func row(t *testing.T, payload map[string]any, i int, path ...string) map[string]any {
	t.Helper()
	entries := list(t, payload, path...)
	if i >= len(entries) {
		t.Fatalf("%s has %d entries, wanted entry %d", strings.Join(path, "."), len(entries), i)
	}
	obj, ok := entries[i].(map[string]any)
	if !ok {
		t.Fatalf("%s[%d] is not an object", strings.Join(path, "."), i)
	}
	return obj
}

// wantKeys fails naming every field the object does not carry. It reports
// rather than fatals, so one run lists everything that has gone.
func wantKeys(t *testing.T, where string, obj map[string]any, want ...string) {
	t.Helper()
	for _, key := range want {
		if _, ok := obj[key]; !ok {
			t.Errorf("%s is missing field %q", where, key)
		}
	}
}

// wantAwake checks the status block a caller uses to tell current data from the
// last thing seen before the daemon was put to sleep.
func wantAwake(t *testing.T, payload map[string]any) {
	t.Helper()
	if !boolean(t, payload, "status", "monitoring") {
		t.Error("status says the daemon is asleep, the stub is monitoring")
	}
	if note, ok := object(t, payload, "status")["note"]; ok {
		t.Errorf("a monitoring daemon carried a note: %v", note)
	}
}

// fakeHerdr puts a herdr on PATH that answers with a fixed agent list, so the
// tool's populated path runs without a real multiplexer.
func fakeHerdr(t *testing.T, reply string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat <<'JSON'\n" + reply + "\nJSON\n"
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake herdr: %v", err)
	}
	// Prepended, not replacing: the script needs the usual tools, and a real
	// herdr on this machine must not win.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// noHerdr empties PATH, which is a machine with no herdr installed.
func noHerdr(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// toolCall is one tool with an argument set it accepts.
type toolCall struct {
	name string
	args map[string]any
}

// everyTool is all twelve, ordered so the edit still finds the trigger that the
// disarm then takes away. Both of the tests that sweep the whole surface read
// this list, so a new tool is added in one place.
func everyTool(id string) []toolCall {
	return []toolCall{
		{name: "omagihu_inbox"},
		{name: "omagihu_work"},
		{name: "omagihu_repos"},
		{name: "omagihu_risk"},
		{name: "omagihu_facts"},
		{name: "omagihu_attention"},
		{name: "omagihu_catalogue"},
		{name: "omagihu_alerts"},
		{name: "omagihu_agents"},
		{name: "omagihu_arm", args: map[string]any{
			"path": "inbox", "operator": "appears", "expires": "4d"}},
		{name: "omagihu_edit", args: map[string]any{
			"id": id, "reason": "still worth watching"}},
		{name: "omagihu_disarm", args: map[string]any{"id": id}},
	}
}

func TestInboxToolCarriesEveryFieldAnAgentReads(t *testing.T) {
	out := call(t, serve(t, stub(nil)), "omagihu_inbox", nil)

	wantKeys(t, "omagihu_inbox", out, "status", "count", "notifications")
	wantAwake(t, out)

	// Both accounts saw pull request 2, and one notification is what a caller
	// should be told about it.
	if got := num(t, out, "count"); got != 1 {
		t.Fatalf("count is %v, want 1: the two accounts share a subject", got)
	}
	if got := len(list(t, out, "notifications")); got != 1 {
		t.Fatalf("got %d notifications, want 1", got)
	}

	first := row(t, out, 0, "notifications")
	wantKeys(t, "notifications[0]", first,
		"accountId", "id", "repo", "type", "title", "reason", "unread",
		"updatedAt", "subjectUrl", "webUrl")

	if got := first["id"]; got != "n1" {
		t.Errorf("id is %v, want n1: the first account's copy is the one kept", got)
	}
	if got := first["repo"]; got != "o/r" {
		t.Errorf("repo is %v, want o/r", got)
	}
	if got := first["reason"]; got != "review_requested" {
		t.Errorf("reason is %v, want review_requested", got)
	}
	if got := first["unread"]; got != true {
		t.Errorf("unread is %v, want true", got)
	}
	if got := first["subjectUrl"]; got != "https://api.github.com/repos/o/r/pulls/2" {
		t.Errorf("subjectUrl is %v", got)
	}
	if got := first["webUrl"]; got != "https://github.com/o/r/pull/2" {
		t.Errorf("webUrl is %v", got)
	}
}

func TestWorkToolCarriesEveryFieldAnAgentReads(t *testing.T) {
	out := call(t, serve(t, stub(nil)), "omagihu_work", nil)

	wantKeys(t, "omagihu_work", out,
		"status", "authoredPrs", "reviewRequests", "assignedIssues", "authoredIssues")
	wantAwake(t, out)

	// Both accounts list pull request 1, which is one pull request.
	if got := len(list(t, out, "authoredPrs")); got != 1 {
		t.Fatalf("got %d authored pull requests, want 1", got)
	}

	authored := row(t, out, 0, "authoredPrs")
	wantKeys(t, "authoredPrs[0]", authored,
		"accountId", "repo", "number", "title", "url", "author", "headRef",
		"baseRef", "headSha", "isDraft", "reviewDecision", "checksState", "updatedAt")

	if got := authored["number"]; got != float64(1) {
		t.Errorf("number is %v, want 1", got)
	}
	if got := authored["headSha"]; got != "aaaa111" {
		t.Errorf("headSha is %v, want aaaa111: it is what makes a local comparison exact", got)
	}
	// The tool promises the rollup and the decision, which is what tells an
	// agent a pull request of yours is stuck rather than merely open.
	if got := authored["checksState"]; got != "FAILURE" {
		t.Errorf("checksState is %v, want FAILURE", got)
	}
	if got := authored["reviewDecision"]; got != "CHANGES_REQUESTED" {
		t.Errorf("reviewDecision is %v, want CHANGES_REQUESTED", got)
	}

	review := row(t, out, 0, "reviewRequests")
	wantKeys(t, "reviewRequests[0]", review,
		"accountId", "repo", "number", "title", "url", "author", "headRef",
		"baseRef", "headSha", "reviewDecision", "checksState", "updatedAt", "incoming")
	if got := review["number"]; got != float64(2) {
		t.Errorf("reviewRequests[0].number is %v, want 2", got)
	}
	// Incoming separates a pull request somebody opened on a repository you own
	// from one whose review was actually requested.
	if got := review["incoming"]; got != true {
		t.Errorf("reviewRequests[0].incoming is %v, want true", got)
	}

	assigned := row(t, out, 0, "assignedIssues")
	wantKeys(t, "assignedIssues[0]", assigned,
		"accountId", "repo", "number", "title", "url", "author", "labels",
		"updatedAt", "incoming")
	label := row(t, assigned, 0, "labels")
	wantKeys(t, "assignedIssues[0].labels[0]", label, "name", "color")
	if got := label["name"]; got != "bug" {
		t.Errorf("label name is %v, want bug", got)
	}
	if got := label["color"]; got != "d73a4a" {
		t.Errorf("label color is %v, want d73a4a", got)
	}

	opened := row(t, out, 0, "authoredIssues")
	wantKeys(t, "authoredIssues[0]", opened,
		"accountId", "repo", "number", "title", "url", "author", "labels", "updatedAt")
	if got := opened["number"]; got != float64(4) {
		t.Errorf("authoredIssues[0].number is %v, want 4", got)
	}
}

func TestReposToolCarriesEveryFieldAnAgentReads(t *testing.T) {
	out := call(t, serve(t, stub(nil)), "omagihu_repos", nil)

	wantKeys(t, "omagihu_repos", out, "status", "count", "repos")
	wantAwake(t, out)

	if got := num(t, out, "count"); got != 2 {
		t.Fatalf("count is %v, want 2", got)
	}

	risky := row(t, out, 0, "repos")
	wantKeys(t, "repos[0]", risky,
		"path", "name", "branch", "upstream", "group", "main", "ahead", "behind",
		"staged", "modified", "deleted", "untracked", "conflicted", "stashes",
		"unpushed", "operation", "remotes", "last", "observedAt", "atRisk", "dirty")

	if got := risky["path"]; got != "/checkout/r" {
		t.Errorf("path is %v, want /checkout/r", got)
	}
	if got := risky["branch"]; got != "watcher" {
		t.Errorf("branch is %v, want watcher", got)
	}
	if got := risky["unpushed"]; got != float64(2) {
		t.Errorf("unpushed is %v, want 2", got)
	}
	if got := risky["operation"]; got != "rebase" {
		t.Errorf("operation is %v, want rebase", got)
	}
	// The tool promises the daemon's own classification, so a reader never has
	// to recompute it and drift.
	if got := risky["atRisk"]; got != true {
		t.Errorf("atRisk is %v, want true", got)
	}
	if got := risky["dirty"]; got != true {
		t.Errorf("dirty is %v, want true", got)
	}
	wantKeys(t, "repos[0].last", object(t, risky, "last"), "sha", "subject", "author", "at")
	if got := str(t, risky, "last", "sha"); got != "aaaa111" {
		t.Errorf("last.sha is %v, want aaaa111", got)
	}
	if got := str(t, risky, "remotes", "origin"); got != "git@github.com:o/r.git" {
		t.Errorf("remotes.origin is %v", got)
	}

	// Dirt on its own is not risk, and the two flags have to say so separately.
	calm := row(t, out, 1, "repos")
	if calm["atRisk"] != false || calm["dirty"] != true {
		t.Errorf("repos[1] atRisk=%v dirty=%v, want false and true", calm["atRisk"], calm["dirty"])
	}
}

func TestRiskToolReturnsOnlyTheCheckoutsHoldingWorkThatWouldBeLost(t *testing.T) {
	out := call(t, serve(t, stub(nil)), "omagihu_risk", nil)

	wantKeys(t, "omagihu_risk", out, "status", "count", "repos")
	wantAwake(t, out)

	if got := num(t, out, "count"); got != 1 {
		t.Fatalf("count is %v, want 1: only one checkout holds work at risk", got)
	}
	risky := row(t, out, 0, "repos")
	wantKeys(t, "repos[0]", risky,
		"path", "name", "branch", "unpushed", "operation", "atRisk", "dirty")
	if got := risky["path"]; got != "/checkout/r" {
		t.Errorf("path is %v, want /checkout/r", got)
	}
	// The dirty checkout carries three modified files and nothing at risk, so
	// naming it here would be the tool crying wolf.
	for i := range list(t, out, "repos") {
		if got := row(t, out, i, "repos")["path"]; got == "/checkout/other" {
			t.Error("a checkout that is only dirty was reported as at risk")
		}
	}
}

func TestFactsToolCarriesEveryFieldAnAgentReads(t *testing.T) {
	out := call(t, serve(t, stub(nil)), "omagihu_facts", nil)

	wantKeys(t, "omagihu_facts", out, "status", "count", "facts")
	wantAwake(t, out)

	if got := num(t, out, "count"); got != 1 {
		t.Fatalf("count is %v, want 1", got)
	}
	fact := row(t, out, 0, "facts")
	wantKeys(t, "facts[0]", fact,
		"kind", "severity", "repo", "path", "branch", "summary", "detail", "url", "number")

	if got := fact["kind"]; got != "missing-work" {
		t.Errorf("kind is %v, want missing-work", got)
	}
	if got := fact["severity"]; got != "urgent" {
		t.Errorf("severity is %v, want urgent", got)
	}
	// A fact names both planes: the repository on GitHub and the checkout here.
	if got := fact["repo"]; got != "o/r" {
		t.Errorf("repo is %v, want o/r", got)
	}
	if got := fact["path"]; got != "/checkout/r" {
		t.Errorf("path is %v, want /checkout/r", got)
	}
	if got := fact["branch"]; got != "watcher" {
		t.Errorf("branch is %v, want watcher", got)
	}
	if got := fact["number"]; got != float64(1) {
		t.Errorf("number is %v, want 1", got)
	}
}

func TestAttentionToolCarriesEveryFieldAnAgentReads(t *testing.T) {
	out := call(t, serve(t, stub(nil)), "omagihu_attention", nil)

	wantKeys(t, "omagihu_attention", out, "status", "attention")
	wantAwake(t, out)

	state := object(t, out, "attention")
	wantKeys(t, "attention", state,
		"tier", "level", "count", "summary", "reviews", "brokenPrs", "reconcile",
		"factsTotal", "unread", "reposAtRisk", "unpushedTotal", "interrupted")

	// A review request outranks everything else in the fixture, and the whole
	// breakdown rides along with the winning tier.
	if got := state["level"]; got != "urgent" {
		t.Errorf("level is %v, want urgent", got)
	}
	if got := state["summary"]; got != "1 review waiting on you" {
		t.Errorf("summary is %v", got)
	}
	for _, want := range []struct {
		field string
		count float64
	}{
		{"count", 1},
		{"reviews", 1},
		{"brokenPrs", 1},
		{"reconcile", 1},
		{"factsTotal", 1},
		{"unread", 1},
		{"reposAtRisk", 1},
		{"unpushedTotal", 2},
		{"interrupted", 1},
	} {
		if got := num(t, state, want.field); got != want.count {
			t.Errorf("attention.%s is %v, want %v", want.field, got, want.count)
		}
	}
}

func TestCatalogueToolCarriesEveryFieldAnAgentReads(t *testing.T) {
	out := call(t, serve(t, stub(nil)), "omagihu_catalogue", nil)

	wantKeys(t, "omagihu_catalogue", out, "status", "count", "paths")
	wantAwake(t, out)

	want := len(alerts.Catalogue())
	if got := num(t, out, "count"); int(got) != want {
		t.Fatalf("count is %v, want %d", got, want)
	}
	if got := len(list(t, out, "paths")); got != want {
		t.Fatalf("got %d paths, want %d", got, want)
	}

	// A list leaf carries everything an agent needs to arm without guessing:
	// the operators it takes, what a filter can narrow it by, what makes two
	// entries the same entry, and the timestamps ages can measure.
	var reviews map[string]any
	for _, entry := range list(t, out, "paths") {
		leaf, ok := entry.(map[string]any)
		if !ok {
			t.Fatal("the catalogue carries an entry that is not a leaf")
		}
		wantKeys(t, fmt.Sprintf("paths[%v]", leaf["path"]), leaf, "path", "kind", "operators", "describes")
		if leaf["path"] == "work.reviewRequests" {
			reviews = leaf
		}
	}
	if reviews == nil {
		t.Fatal("the catalogue does not carry work.reviewRequests")
	}
	wantKeys(t, "work.reviewRequests", reviews, "fields", "identity", "timeFields")
	if got := reviews["kind"]; got != "list" {
		t.Errorf("work.reviewRequests kind is %v, want list", got)
	}
	for _, op := range []string{"appears", "disappears", "count", "ages"} {
		if !slices.Contains(texts(list(t, reviews, "operators")), op) {
			t.Errorf("work.reviewRequests does not offer %q", op)
		}
	}
	if !slices.Contains(texts(list(t, reviews, "identity")), "number") {
		t.Error("work.reviewRequests identity does not include number")
	}
	if !slices.Contains(texts(list(t, reviews, "timeFields")), "updatedAt") {
		t.Error("work.reviewRequests cannot be aged on updatedAt")
	}
}

// texts flattens a list of text values, so a membership check reads plainly.
func texts(entries []any) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, fmt.Sprint(e))
	}
	return out
}

func TestAlertsToolCarriesEveryFieldAnAgentReads(t *testing.T) {
	engine := &fakeAlerts{triggers: []alerts.Trigger{armedTrigger(), firedTrigger()}}
	out := call(t, serve(t, stub(engine)), "omagihu_alerts", nil)

	wantKeys(t, "omagihu_alerts", out, "status", "count", "alerts")
	wantAwake(t, out)

	if got := num(t, out, "count"); got != 2 {
		t.Fatalf("count is %v, want 2", got)
	}

	armed := row(t, out, 0, "alerts")
	wantKeys(t, "alerts[0]", armed,
		"id", "rev", "path", "operator", "params", "where", "deliverTo",
		"standing", "expiresAt", "reason", "armedBy", "armedAt", "state", "status")

	if got := armed["id"]; got != "t-armed" {
		t.Errorf("id is %v, want t-armed", got)
	}
	if got := armed["path"]; got != "work.reviewRequests" {
		t.Errorf("path is %v, want work.reviewRequests", got)
	}
	if got := armed["operator"]; got != "count" {
		t.Errorf("operator is %v, want count", got)
	}
	if got := num(t, armed, "params", "above"); got != 2 {
		t.Errorf("params.above is %v, want 2", got)
	}
	// armedBy is what tells an agent whose watch it is looking at, and the tool
	// tells it never to disarm somebody else's.
	if got := armed["armedBy"]; got != "pane-7" {
		t.Errorf("armedBy is %v, want pane-7", got)
	}
	if got := armed["reason"]; got != "the queue is backing up" {
		t.Errorf("reason is %v", got)
	}
	where := row(t, armed, 0, "where")
	wantKeys(t, "alerts[0].where[0]", where, "field", "op", "value")
	wantKeys(t, "alerts[0].state", object(t, armed, "state"), "ready", "primed")

	// The status is computed per row, not copied: one watch can still fire and
	// the other has already rung.
	if got := armed["status"]; got != "armed" {
		t.Errorf("alerts[0].status is %v, want armed", got)
	}
	fired := row(t, out, 1, "alerts")
	if got := fired["status"]; got != "fired" {
		t.Errorf("alerts[1].status is %v, want fired", got)
	}
}

func TestAgentsToolCarriesEveryFieldAnAgentReads(t *testing.T) {
	t.Run("herdr answers", func(t *testing.T) {
		fakeHerdr(t, `{"result":{"agents":[{"pane_id":"pane-7","agent":"claude",`+
			`"agent_status":"idle","cwd":"/checkout/r",`+
			`"terminal_title_stripped":"omagihu","workspace_id":"ws-1"}]}}`)

		out := call(t, serve(t, stub(nil)), "omagihu_agents", nil)

		wantKeys(t, "omagihu_agents", out, "status", "count", "agents")
		wantAwake(t, out)

		if got := num(t, out, "count"); got != 1 {
			t.Fatalf("count is %v, want 1", got)
		}
		agent := row(t, out, 0, "agents")
		wantKeys(t, "agents[0]", agent,
			"pane_id", "agent", "agent_status", "cwd", "terminal_title_stripped", "workspace_id")

		// deliverTo is filled from pane_id, so that field above all must survive.
		if got := agent["pane_id"]; got != "pane-7" {
			t.Errorf("pane_id is %v, want pane-7", got)
		}
		if got := agent["agent_status"]; got != "idle" {
			t.Errorf("agent_status is %v, want idle", got)
		}
		if got := agent["cwd"]; got != "/checkout/r" {
			t.Errorf("cwd is %v, want /checkout/r", got)
		}
	})

	// No herdr is a normal state, not a failure: the tool says why the list is
	// empty instead of refusing.
	t.Run("no herdr", func(t *testing.T) {
		noHerdr(t)

		out := call(t, serve(t, stub(nil)), "omagihu_agents", nil)

		wantKeys(t, "omagihu_agents", out, "status", "count", "agents", "note")
		if got := num(t, out, "count"); got != 0 {
			t.Errorf("count is %v, want 0", got)
		}
		if got := len(list(t, out, "agents")); got != 0 {
			t.Errorf("got %d agents with no herdr, want 0", got)
		}
		if str(t, out, "note") == "" {
			t.Error("no herdr and no note saying why the list is empty")
		}
	})
}

func TestArmToolReturnsTheTriggerItStored(t *testing.T) {
	engine := &fakeAlerts{}
	out := call(t, serve(t, stub(engine)), "omagihu_arm", map[string]any{
		"path":     "work.reviewRequests",
		"operator": "count",
		"expires":  "4d",
		"above":    2,
		"reason":   "the queue is backing up",
		"where":    []string{"repo=o/r"},
	})

	wantKeys(t, "omagihu_arm", out, "status", "trigger")
	wantAwake(t, out)

	trigger := object(t, out, "trigger")
	wantKeys(t, "trigger", trigger,
		"id", "rev", "path", "operator", "params", "where", "deliverTo",
		"expiresAt", "reason", "armedBy", "armedAt", "state")

	if got := trigger["id"]; got != "t-1" {
		t.Errorf("id is %v, want the id the store minted", got)
	}
	if got := trigger["path"]; got != "work.reviewRequests" {
		t.Errorf("path is %v, want work.reviewRequests", got)
	}
	if got := trigger["operator"]; got != "count" {
		t.Errorf("operator is %v, want count", got)
	}
	if got := num(t, trigger, "params", "above"); got != 2 {
		t.Errorf("params.above is %v, want 2", got)
	}
	// A request that names neither owner nor target gets the defaults, which is
	// what keeps an agent's watch identifiable and deliverable.
	if got := trigger["armedBy"]; got != "agent" {
		t.Errorf("armedBy is %v, want agent", got)
	}
	if got := trigger["deliverTo"]; got != alerts.TargetYou {
		t.Errorf("deliverTo is %v, want %v", got, alerts.TargetYou)
	}
	where := row(t, trigger, 0, "where")
	if where["field"] != "repo" || where["op"] != "=" || where["value"] != "o/r" {
		t.Errorf("where[0] is %v, want repo=o/r parsed into three parts", where)
	}

	if len(engine.armed) != 1 {
		t.Fatalf("the engine was handed %d triggers, want 1", len(engine.armed))
	}
	// An expiry is required and it is a time, not the text that was sent.
	if !engine.armed[0].ExpiresAt.After(time.Now().Add(3 * 24 * time.Hour)) {
		t.Errorf("4d became %v, which is not four days out", engine.armed[0].ExpiresAt)
	}
}

// A request the tool cannot turn into a watch must be refused before the store
// sees it, and the refusal has to name what was wrong with it.
func TestArmToolRefusesARequestItCannotTurnIntoAWatch(t *testing.T) {
	bad := []struct {
		name string
		args map[string]any
		says string
	}{
		{
			name: "no expiry",
			args: map[string]any{"path": "inbox", "operator": "appears", "expires": ""},
			says: "expiry",
		},
		{
			name: "an expiry that is not a time",
			args: map[string]any{"path": "inbox", "operator": "appears", "expires": "whenever"},
			says: "whenever",
		},
		{
			name: "a filter that is not field=value",
			args: map[string]any{"path": "inbox", "operator": "appears", "expires": "4d",
				"where": []string{"repo"}},
			says: "repo",
		},
	}

	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			engine := &fakeAlerts{}
			res := invoke(t, serve(t, stub(engine)), "omagihu_arm", tc.args)
			if !res.IsError {
				t.Fatal("the request was armed rather than refused")
			}
			if got := message(res); !strings.Contains(got, tc.says) {
				t.Errorf("the refusal is %q, which does not name %q", got, tc.says)
			}
			if len(engine.armed) != 0 {
				t.Error("a request that could not be read still reached the store")
			}
		})
	}
}

func TestEditToolKeepsWhateverTheRequestDidNotName(t *testing.T) {
	engine := &fakeAlerts{triggers: []alerts.Trigger{armedTrigger()}}
	out := call(t, serve(t, stub(engine)), "omagihu_edit", map[string]any{
		"id":     "t-armed",
		"above":  5,
		"reason": "the queue is worse than it was",
	})

	wantKeys(t, "omagihu_edit", out, "status", "trigger")
	wantAwake(t, out)

	trigger := object(t, out, "trigger")
	wantKeys(t, "trigger", trigger,
		"id", "rev", "path", "operator", "params", "where", "deliverTo",
		"standing", "expiresAt", "reason", "armedBy", "armedAt", "state")

	if got := num(t, trigger, "params", "above"); got != 5 {
		t.Errorf("params.above is %v, want the new bound of 5", got)
	}
	if got := trigger["reason"]; got != "the queue is worse than it was" {
		t.Errorf("reason is %v, want the new one", got)
	}
	// The id and the owner survive an edit, which is the difference between
	// editing a watch and arming a second one.
	if got := trigger["id"]; got != "t-armed" {
		t.Errorf("id is %v, want t-armed", got)
	}
	if got := trigger["armedBy"]; got != "pane-7" {
		t.Errorf("armedBy is %v, want pane-7", got)
	}
	// Nothing the request left out moved.
	if got := trigger["path"]; got != "work.reviewRequests" {
		t.Errorf("path is %v, want the one it already had", got)
	}
	if got := trigger["operator"]; got != "count" {
		t.Errorf("operator is %v, want the one it already had", got)
	}
	if got := trigger["standing"]; got != true {
		t.Errorf("standing is %v, want the one it already had", got)
	}
	if got := row(t, trigger, 0, "where")["value"]; got != "o/r" {
		t.Errorf("where[0].value is %v, want the filter it already had", got)
	}

	// The other half of the contract: everything the request does name moves.
	// The fake store takes what it is handed, as the real one does before it
	// validates, so this is the fold itself and nothing else.
	t.Run("and moves every field it does name", func(t *testing.T) {
		becomes := alerts.Trigger{
			ID: "t-becomes", Path: "attention.level", Operator: alerts.OpBecomes,
			Params:    alerts.Params{Value: "warn", Hold: "5m"},
			DeliverTo: alerts.TargetYou, ExpiresAt: sampledAt.Add(time.Hour),
			ArmedBy: "agent", ArmedAt: sampledAt,
		}
		ages := alerts.Trigger{
			ID: "t-ages", Path: "work.reviewRequests", Operator: alerts.OpAges,
			Params:    alerts.Params{OlderThan: "7d", Field: "updatedAt"},
			DeliverTo: alerts.TargetYou, ExpiresAt: sampledAt.Add(time.Hour),
			ArmedBy: "agent", ArmedAt: sampledAt,
		}
		engine := &fakeAlerts{triggers: []alerts.Trigger{armedTrigger(), becomes, ages}}
		session := serve(t, stub(engine))

		moved := object(t, call(t, session, "omagihu_edit", map[string]any{
			"id":        "t-armed",
			"path":      "work.authoredPrs",
			"operator":  "crosses",
			"below":     3,
			"rearm":     1,
			"where":     []string{"checksState~=fail"},
			"deliverTo": "pane-9",
			"standing":  false,
			"expires":   "12h",
		}), "trigger")

		if got := moved["path"]; got != "work.authoredPrs" {
			t.Errorf("path is %v, want work.authoredPrs", got)
		}
		if got := moved["operator"]; got != "crosses" {
			t.Errorf("operator is %v, want crosses", got)
		}
		// A bound is one-sided, so naming below clears the above it replaces
		// rather than leaving a watch that asks for both.
		if got := num(t, moved, "params", "below"); got != 3 {
			t.Errorf("params.below is %v, want 3", got)
		}
		if got, ok := object(t, moved, "params")["above"]; ok {
			t.Errorf("params.above survived a new below as %v", got)
		}
		if got := num(t, moved, "params", "rearm"); got != 1 {
			t.Errorf("params.rearm is %v, want 1", got)
		}
		if got := row(t, moved, 0, "where"); got["op"] != "~=" || got["value"] != "fail" {
			t.Errorf("where[0] is %v, want the substring filter that replaced it", got)
		}
		if got := moved["deliverTo"]; got != "pane-9" {
			t.Errorf("deliverTo is %v, want pane-9", got)
		}
		// Standing is dropped from the wire when it is false, which is the
		// difference between ringing every time and ringing once.
		if got, ok := moved["standing"]; ok && got != false {
			t.Errorf("standing is %v, want a watch that rings once", got)
		}
		if got := str(t, moved, "expiresAt"); got == armedTrigger().ExpiresAt.Format(time.RFC3339) {
			t.Error("expiresAt did not move")
		}

		text := object(t, call(t, session, "omagihu_edit", map[string]any{
			"id": "t-becomes", "value": "urgent", "hold": "30m",
		}), "trigger")
		if got := str(t, text, "params", "value"); got != "urgent" {
			t.Errorf("params.value is %v, want urgent", got)
		}
		if got := str(t, text, "params", "hold"); got != "30m" {
			t.Errorf("params.hold is %v, want 30m", got)
		}

		aged := object(t, call(t, session, "omagihu_edit", map[string]any{
			"id": "t-ages", "olderThan": "2d", "field": "mergedAt",
		}), "trigger")
		if got := str(t, aged, "params", "olderThan"); got != "2d" {
			t.Errorf("params.olderThan is %v, want 2d", got)
		}
		if got := str(t, aged, "params", "field"); got != "mergedAt" {
			t.Errorf("params.field is %v, want mergedAt", got)
		}
	})

	// An edit with no id has nothing to fold onto and must say so.
	t.Run("and refuses an edit with no id", func(t *testing.T) {
		res := invoke(t, serve(t, stub(&fakeAlerts{})), "omagihu_edit",
			map[string]any{"id": "", "reason": "nowhere to put this"})
		if !res.IsError {
			t.Fatal("an edit naming no trigger was accepted")
		}
		if got := message(res); !strings.Contains(got, "id") {
			t.Errorf("the refusal is %q and does not mention the missing id", got)
		}
	})
}

func TestDisarmToolReportsWhatItRemoved(t *testing.T) {
	engine := &fakeAlerts{triggers: []alerts.Trigger{armedTrigger()}}
	session := serve(t, stub(engine))

	out := call(t, session, "omagihu_disarm", map[string]any{"id": "t-armed"})

	wantKeys(t, "omagihu_disarm", out, "status", "removed", "id")
	wantAwake(t, out)

	if got := boolean(t, out, "removed"); !got {
		t.Error("removed is false having removed the trigger")
	}
	// The id comes back so a caller can tell which of several disarms answered.
	if got := str(t, out, "id"); got != "t-armed" {
		t.Errorf("id is %v, want t-armed", got)
	}
	if len(engine.triggers) != 0 {
		t.Errorf("the store still holds %d triggers", len(engine.triggers))
	}

	// A disarm that found nothing must say so rather than answering removed
	// false, which reads as though the watch is still armed.
	res := invoke(t, session, "omagihu_disarm", map[string]any{"id": "t-gone"})
	if !res.IsError {
		t.Fatal("disarming an id the store does not have was reported as success")
	}
	if !strings.Contains(message(res), "t-gone") {
		t.Errorf("the refusal does not name the id: %s", message(res))
	}
}

// Every tool carries the status block, so an agent reading a sleeping daemon is
// told the data is the last thing seen and not what is true now.
func TestEveryToolSaysSoWhenTheDaemonIsAsleep(t *testing.T) {
	noHerdr(t)

	engine := &fakeAlerts{triggers: []alerts.Trigger{
		{ID: "t-seed", Path: "inbox", Operator: alerts.OpAppears,
			DeliverTo: alerts.TargetYou, ExpiresAt: sampledAt.Add(time.Hour),
			ArmedBy: "agent", ArmedAt: sampledAt},
	}}
	src := stub(engine)
	src.Monitoring = func() bool { return false }
	session := serve(t, src)

	for _, tool := range everyTool("t-seed") {
		t.Run(tool.name, func(t *testing.T) {
			out := call(t, session, tool.name, tool.args)

			if _, ok := out["status"]; !ok {
				t.Fatalf("%s answered without a status block", tool.name)
			}
			if boolean(t, out, "status", "monitoring") {
				t.Errorf("%s says the daemon is monitoring, it is asleep", tool.name)
			}
			if note := str(t, out, "status", "note"); !strings.Contains(note, "asleep") {
				t.Errorf("%s carries no note saying the daemon is asleep: %q", tool.name, note)
			}
		})
	}
}

// A daemon that came up without an alert engine has nothing to write to. The
// tools that need one must refuse and say why, not panic on a nil interface.
func TestAlertToolsRefuseCleanlyWithoutAnEngine(t *testing.T) {
	session := serve(t, stub(nil))

	tools := []toolCall{
		{name: "omagihu_alerts"},
		{name: "omagihu_arm", args: map[string]any{
			"path": "inbox", "operator": "appears", "expires": "4d"}},
		{name: "omagihu_edit", args: map[string]any{"id": "t-armed", "reason": "no engine"}},
		{name: "omagihu_disarm", args: map[string]any{"id": "t-armed"}},
	}

	for _, tool := range tools {
		t.Run(tool.name, func(t *testing.T) {
			res := invoke(t, session, tool.name, tool.args)
			if !res.IsError {
				t.Fatalf("%s answered as though an engine were wired", tool.name)
			}
			if got := message(res); !strings.Contains(got, errNoAlerts.Error()) {
				t.Errorf("%s refused with %q, want %q", tool.name, got, errNoAlerts)
			}
		})
	}

	// The reading tools do not need an engine and keep working without one.
	for _, name := range []string{"omagihu_inbox", "omagihu_catalogue"} {
		if res := invoke(t, session, name, nil); res.IsError {
			t.Errorf("%s refused without an engine: %s", name, message(res))
		}
	}
}

// advertised reads the output schema each tool publishes over tools/list.
func advertised(t *testing.T, s *mcp.ClientSession) map[string]map[string]any {
	t.Helper()
	listed, err := s.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	out := make(map[string]map[string]any, len(listed.Tools))
	for _, tool := range listed.Tools {
		schema, ok := tool.OutputSchema.(map[string]any)
		if !ok {
			t.Fatalf("%s advertises no output schema (%T)", tool.Name, tool.OutputSchema)
		}
		out[tool.Name] = schema
	}
	return out
}

// undeclared reports every key the answer carries that the schema does not
// declare. The SDK builds that schema by reflecting over the Go type and then
// validates the marshalled answer against it, so an undeclared key is not
// untidiness: the call dies before a caller sees any of it.
func undeclared(t *testing.T, where string, payload any, schema map[string]any) {
	t.Helper()
	if _, ok := schema["$ref"]; ok {
		t.Fatalf("%s: the schema is a $ref, which this check cannot follow", where)
	}

	switch got := payload.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		extra := schema["additionalProperties"]
		loose, openEnded := extra.(map[string]any)
		for _, key := range slices.Sorted(maps.Keys(got)) {
			at := where + "." + key
			if sub, ok := props[key]; ok {
				// A declared field whose schema is the bare true accepts
				// anything, as an any does, and there is nothing to walk.
				if deeper, ok := sub.(map[string]any); ok {
					undeclared(t, at, got[key], deeper)
				}
				continue
			}
			// A map or an any is open by design and declares no properties.
			if openEnded {
				undeclared(t, at, got[key], loose)
				continue
			}
			if extra == false {
				t.Errorf("%s reaches the wire and %s does not declare it", at, where)
			}
		}
	case []any:
		items, ok := schema["items"].(map[string]any)
		if !ok {
			return
		}
		for i, entry := range got {
			undeclared(t, fmt.Sprintf("%s[%d]", where, i), entry, items)
		}
	}
}

// Every tool has to advertise every field it actually emits. This is the shape
// of the bug that killed omagihu_repos and omagihu_risk: Repo.MarshalJSON put
// atRisk and dirty on the wire, no struct field declared them, and the SDK's
// own output validation refused the answer. Holding it for all twelve means a
// field added the same way anywhere is caught here rather than in production.
func TestEveryToolAdvertisesTheFieldsItEmits(t *testing.T) {
	noHerdr(t)

	session := serve(t, stub(&fakeAlerts{triggers: []alerts.Trigger{armedTrigger()}}))
	schemas := advertised(t, session)

	for _, tool := range everyTool("t-armed") {
		t.Run(tool.name, func(t *testing.T) {
			schema, ok := schemas[tool.name]
			if !ok {
				t.Fatalf("%s is registered but not advertised", tool.name)
			}
			undeclared(t, tool.name, call(t, session, tool.name, tool.args), schema)
		})
	}
}

// A fixture cannot emit a field it leaves empty, so the wire form of local.Repo
// is checked on its own: every key a fully populated repo marshals to must be
// declared by the schema omagihu_repos advertises. The repo is filled by
// reflection rather than by hand, so a field added to it is covered here
// without this test being touched.
func TestRepoWireFormIsFullyDeclaredByTheReposSchema(t *testing.T) {
	session := serve(t, stub(nil))

	schema := advertised(t, session)["omagihu_repos"]
	repos, ok := schema["properties"].(map[string]any)["repos"].(map[string]any)
	if !ok {
		t.Fatal("the repos tool advertises no repos property")
	}
	items, ok := repos["items"].(map[string]any)
	if !ok {
		t.Fatal("the repos property declares no entry shape")
	}
	declared, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatal("a repo entry declares no properties at all")
	}

	var full local.Repo
	fill(t, reflect.ValueOf(&full).Elem())

	marshalled, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal a repo: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(marshalled, &wire); err != nil {
		t.Fatalf("read back a marshalled repo: %v", err)
	}

	// The classification the marshaller adds is the pair that was missing, so
	// name it rather than leaving it to the sweep below.
	for _, key := range []string{"atRisk", "dirty"} {
		if _, ok := wire[key]; !ok {
			t.Errorf("a marshalled repo no longer carries %q", key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(wire)) {
		if _, ok := declared[key]; !ok {
			t.Errorf("a repo marshals %q, which the advertised schema does not declare: "+
				"omagihu_repos and omagihu_risk refuse every call while that is true", key)
		}
	}
}

// fill sets every field of v to a non-zero value, so a marshalled struct
// carries every key it can rather than only the ones a fixture happens to set.
func fill(t *testing.T, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fill(t, p.Elem())
		v.Set(p)
	case reflect.Slice:
		entries := reflect.MakeSlice(v.Type(), 1, 1)
		fill(t, entries.Index(0))
		v.Set(entries)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		fill(t, key)
		value := reflect.New(v.Type().Elem()).Elem()
		fill(t, value)
		m.SetMapIndex(key, value)
		v.Set(m)
	case reflect.Struct:
		// A time is a struct with nothing exported to fill.
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(sampledAt))
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fill(t, v.Field(i))
			}
		}
	default:
		t.Fatalf("fill does not handle a %s", v.Kind())
	}
}
