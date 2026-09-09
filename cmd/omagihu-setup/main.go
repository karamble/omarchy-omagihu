// Command omagihu-setup creates and edits the account store. It seeds the first
// identity from the gh CLI, adds further ones from a token read on stdin, and
// prints the snippet that registers the daemon's MCP endpoint with Claude.
//
// Tokens are never accepted as command line arguments: argv is visible to every
// process on the machine.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/karamble/omarchy-omagihu/accounts"
	"github.com/karamble/omarchy-omagihu/local"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "omagihu-setup:", err)
		os.Exit(1)
	}
}

func usage() string {
	return strings.TrimSpace(`
usage: omagihu-setup <command> [flags]

  install         seed an account if needed and choose which folders to watch
  roots [DIRS]    change the directories that are watched, and restart
  uninstall       --purge deletes the stored config and tokens
  status          show whether the daemon is answering, and what it watches
  init            create the store and seed the first account from the gh CLI
  add             add an account, reading its token from stdin
  list            show configured accounts with secrets redacted
  mcp             print the ~/.claude.json entry for the daemon's MCP endpoint

alerts:
  catalogue       everything that can be watched
  alerts          the watches currently armed, and where each one stands
  arm PATH OP     arm a watch; see the catalogue for paths and operators
  edit ID         change a watch's terms, keeping its id and its owner
  disarm ID       remove a watch

flags:
  --config PATH   store location (default ~/.config/omagihu/accounts.json)
  --host HOST     forge host for add (default github.com)
  --login NAME    account login for add, otherwise resolved from the token
  --addr HOSTPORT daemon address (default 127.0.0.1:8099)
  --purge         with uninstall, also delete ~/.config/omagihu
  --roots DIRS    comma separated directories to watch; asked for if omitted

alert flags:
  --expires SPEC  required to arm: 4d, 12h, 2026-10-01 or an RFC 3339 time
  --reason TEXT   the one piece of context the alarm carries
  --deliver WHO   herdr agent or pane, "repo" for whoever is in the checkout,
                  or "you" for the desktop; defaults to your pane
  --above X       bound for crosses and count
  --below X       bound for crosses and count
  --rearm R       how far past the bound the value must return before it rings again
  --value V       the value becomes waits for
  --hold SPAN     how long becomes must be away before it can ring again
  --older-than S  the age ages measures against
  --field NAME    which timestamp ages measures
  --where F=V     filter a list, repeatable; F~=V matches a substring
  --standing      ring every time until expiry, instead of once
  --once          ring once, which is the default
  --dry-run       validate and print, storing nothing
  --json          machine readable output
`)
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage())
	}
	cmd, rest := args[0], args[1:]
	opts, positional, err := parseFlags(rest)
	if err != nil {
		return err
	}

	switch cmd {
	case "install":
		return install(opts)
	case "roots":
		return setRoots(opts, positional)
	case "uninstall":
		return uninstall(opts)
	case "status":
		return status(opts)
	case "init":
		return initStore(opts)
	case "add":
		return addAccount(opts)
	case "list":
		return listAccounts(opts)
	case "mcp":
		return printMCP(opts)
	case "catalogue":
		return showCatalogue(opts)
	case "alerts":
		return listAlerts(opts)
	case "arm":
		return armAlert(opts, positional)
	case "edit":
		return editAlert(opts, positional)
	case "disarm":
		return disarmAlert(opts, positional)
	case "help", "-h", "--help":
		fmt.Println(usage())
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage())
	}
}

type options struct {
	config string
	host   string
	login  string
	addr   string
	purge  bool
	roots  string
	alerts alertFlags
}

// install is the whole first-run path: an account, a unit, a running daemon.
func install(opts options) error {
	store, err := load(opts.config)
	if err != nil {
		return err
	}
	if len(store.Accounts) == 0 {
		fmt.Println("no account configured, seeding from the gh CLI")
		if err := initStore(opts); err != nil {
			return err
		}
	} else {
		fmt.Printf("using %d configured account(s)\n", len(store.Accounts))
	}

	roots := opts.roots
	if roots == "" {
		roots = askRoots()
	}
	if err := saveRoots(opts.config, roots); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("omagihu is set up. The shell runs the daemon while the plugin is")
	fmt.Println("enabled, so there is no service to install and nothing of ours")
	fmt.Println("outside this folder and ~/.config/omagihu.")
	fmt.Println("  omarchy plugin enable karamble.omagihu     if it is not already")
	fmt.Println("  omagihu-setup status                       check on it")
	fmt.Println("  omagihu-setup mcp                          register the MCP endpoint")
	return nil
}

// saveRoots records where checkouts are looked for, and tells a running daemon
// so the change takes hold without anything being restarted.
func saveRoots(config, roots string) error {
	store, err := load(config)
	if err != nil {
		return err
	}
	list := local.SplitList(roots)
	if len(list) == 0 {
		return errors.New("at least one directory is needed, or there is nothing to watch")
	}
	store.SetRoots(list)
	if err := store.Save(); err != nil {
		return err
	}
	fmt.Printf("watching %s\n", strings.Join(list, ", "))
	return nil
}

// defaultRoots is the answer offered when nobody says otherwise.
func defaultRoots() string { return strings.Join(local.DefaultRoots, ",") }

// askRoots offers the defaults and takes an answer, when there is somebody
// there to ask. Watching the wrong directories is the one first-run mistake
// that leaves the panel looking broken with no clue why, so it is worth a
// question rather than a hand-edited unit file later.
func askRoots() string {
	stat, err := os.Stdin.Stat()
	if err != nil || stat.Mode()&os.ModeCharDevice == 0 {
		return defaultRoots()
	}

	fmt.Println()
	fmt.Println("Which directories hold your git checkouts?")
	fmt.Println("Separate several with commas. ~ is expanded.")
	fmt.Printf("  [%s]: ", defaultRoots())

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return defaultRoots()
	}
	if answer := strings.TrimSpace(line); answer != "" {
		return answer
	}
	return defaultRoots()
}

// setRoots changes the watched directories on an existing install.
func setRoots(opts options, positional []string) error {
	roots := opts.roots
	if roots == "" && len(positional) > 0 {
		roots = strings.Join(positional, ",")
	}
	if roots == "" {
		roots = askRoots()
	}

	if err := saveRoots(opts.config, roots); err != nil {
		return err
	}
	// A running daemon is told directly, so the new roots take effect on the
	// next pass. When nothing is running the stored value is picked up at start.
	if _, err := apiSend(opts, http.MethodPost, "/api/roots",
		map[string]any{"roots": local.SplitList(roots)}); err != nil {
		fmt.Println("saved; the daemon is not running, so it will pick these up when it starts")
		return nil
	}
	fmt.Println("the daemon is now watching the new roots")
	return nil
}

// uninstall reverses install. omarchy plugin remove deletes the plugin folder
// but knows nothing about the stored tokens, so this is how they are removed.
func uninstall(opts options) error {
	// There is no service to take down: the shell starts the daemon while the
	// plugin is enabled and stops it when it is not, so removing or disabling
	// the plugin is the whole of that. What is left is configuration.
	if !opts.purge {
		fmt.Println("nothing to stop: the daemon runs only while the plugin is enabled.")
		fmt.Println("configuration kept; pass --purge to delete it too")
		return nil
	}

	{
		store, err := load(opts.config)
		if err != nil {
			return err
		}
		// Remove the files omagihu owns, never the directory they happen to sit
		// in. --config can point anywhere, and deleting a parent directory on
		// the strength of that is how a teardown eats something it should not.
		// Each file is named rather than globbed, for the same reason.
		file := store.Path()
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", file, err)
		}
		fmt.Printf("removed %s including every stored token\n", file)

		// The watches are ours too, and leaving them behind would have a fresh
		// install inherit the last one's alarms.
		triggers := filepath.Join(filepath.Dir(file), "triggers.json")
		if err := os.Remove(triggers); err == nil {
			fmt.Printf("removed %s\n", triggers)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", triggers, err)
		}

		// Only tidy the enclosing directory when it is ours and now empty.
		dir := filepath.Dir(file)
		if filepath.Base(dir) == "omagihu" {
			if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
				if err := os.Remove(dir); err == nil {
					fmt.Printf("removed empty %s\n", dir)
				}
			}
		}
	}
	fmt.Println("now remove the plugin itself: omarchy plugin remove karamble.omagihu")
	return nil
}

func status(opts options) error {
	// There is no unit to inspect. The daemon runs while the plugin is enabled,
	// so the honest question is whether it is answering.
	if body, err := apiGet(opts, "/api/health"); err == nil {
		var health struct {
			Version    string `json:"version"`
			Accounts   int    `json:"accounts"`
			Repos      int    `json:"repos"`
			Monitoring bool   `json:"monitoring"`
		}
		if json.Unmarshal(body, &health) == nil {
			fmt.Printf("daemon:   answering on %s, version %s\n", opts.addr, health.Version)
			fmt.Printf("watching: %d repositories, monitoring %v\n", health.Repos, health.Monitoring)
		}
	} else {
		fmt.Printf("daemon:   not answering on %s\n", opts.addr)
		fmt.Println("          the shell starts it while the plugin is enabled;")
		fmt.Println("          check omarchy plugin list, and that bin/ is built")
	}

	store, err := accounts.Load(opts.config)
	if err != nil {
		fmt.Println("accounts:", err)
		return nil
	}
	fmt.Printf("accounts: %d configured, monitoring %v\n",
		len(store.Accounts), store.MonitoringEnabled())
	fmt.Printf("roots:    %s\n", strings.Join(store.RootsOrDefault(local.DefaultRoots), ", "))
	fmt.Printf("config:   %s\n", store.Path())
	return nil
}

func parseFlags(args []string) (options, []string, error) {
	opts := options{host: "github.com", addr: "127.0.0.1:8099"}
	opts.alerts.seen = map[string]bool{}
	var positional []string

	for i := 0; i < len(args); i++ {
		key, value, hasInline := strings.Cut(args[i], "=")

		// Anything that is not a flag is an argument to the command, such as
		// the directories given to "roots".
		if !strings.HasPrefix(key, "--") {
			positional = append(positional, args[i])
			continue
		}

		// Boolean flags take no value, so they are settled before anything
		// tries to consume the next token.
		switch key {
		case "--purge":
			opts.purge = true
			continue
		case "--standing":
			opts.alerts.standing, opts.alerts.seen[key] = true, true
			continue
		case "--once":
			opts.alerts.once, opts.alerts.seen[key] = true, true
			continue
		case "--dry-run":
			opts.alerts.dryRun = true
			continue
		case "--json":
			opts.alerts.jsonOut = true
			continue
		}
		if !hasInline {
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("%s needs a value", key)
			}
			i++
			value = args[i]
		}
		opts.alerts.seen[key] = true
		switch key {
		case "--config":
			opts.config = value
		case "--host":
			opts.host = value
		case "--login":
			opts.login = value
		case "--addr":
			opts.addr = value
		case "--roots":
			opts.roots = value
		case "--expires":
			opts.alerts.expires = value
		case "--reason":
			opts.alerts.reason = value
		case "--deliver":
			opts.alerts.deliver = value
		case "--value":
			opts.alerts.value = value
		case "--hold":
			opts.alerts.hold = value
		case "--field":
			opts.alerts.field = value
		case "--older-than":
			opts.alerts.olderThan = value
		case "--where":
			opts.alerts.where = append(opts.alerts.where, value)
		case "--above", "--below", "--rearm":
			n, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return opts, nil, fmt.Errorf("%s needs a number, got %q", key, value)
			}
			switch key {
			case "--above":
				opts.alerts.above = &n
			case "--below":
				opts.alerts.below = &n
			case "--rearm":
				opts.alerts.rearm = &n
			}
		default:
			return opts, nil, fmt.Errorf("unknown flag %q\n\n%s", key, usage())
		}
	}
	return opts, positional, nil
}

// load opens an existing store, or returns an empty one ready to be saved.
func load(path string) (*accounts.Store, error) {
	s, err := accounts.Load(path)
	if errors.Is(err, accounts.ErrNotConfigured) {
		fresh := &accounts.Store{Version: 1}
		if path != "" {
			fresh.SetPath(path)
		}
		return fresh, nil
	}
	return s, err
}

func initStore(opts options) error {
	store, err := load(opts.config)
	if err != nil {
		return err
	}
	if len(store.Accounts) > 0 {
		return fmt.Errorf("%s already has %d account(s): use add", store.Path(), len(store.Accounts))
	}

	token, err := ghToken(opts.host)
	if err != nil {
		return fmt.Errorf("seeding from gh: %w (run `gh auth login`, or use omagihu-setup add)", err)
	}
	login, err := resolveLogin(opts.login, token, opts.host)
	if err != nil {
		return err
	}

	store.Upsert(accounts.Account{
		ID:      login + "@" + opts.host,
		Login:   login,
		Host:    opts.host,
		Token:   token,
		Enabled: true,
	})
	if err := store.Save(); err != nil {
		return err
	}

	fmt.Printf("wrote %s\n", store.Path())
	fmt.Printf("seeded account %s from the gh CLI\n", login)
	fmt.Printf("\nstart the daemon:\n  omagihud --addr %s\n", opts.addr)
	fmt.Printf("\nregister its MCP endpoint:\n  omagihu-setup mcp\n")
	return nil
}

func addAccount(opts options) error {
	store, err := load(opts.config)
	if err != nil {
		return err
	}
	token, err := readTokenStdin()
	if err != nil {
		return err
	}
	login, err := resolveLogin(opts.login, token, opts.host)
	if err != nil {
		return err
	}

	id := login + "@" + opts.host
	if _, exists := store.Find(id); exists {
		fmt.Printf("replacing existing account %s\n", id)
	}
	store.Upsert(accounts.Account{
		ID:      id,
		Login:   login,
		Host:    opts.host,
		Token:   token,
		Enabled: true,
	})
	if err := store.Save(); err != nil {
		return err
	}
	fmt.Printf("added %s to %s\n", id, store.Path())
	return nil
}

func listAccounts(opts options) error {
	store, err := accounts.Load(opts.config)
	if err != nil {
		return err
	}
	if len(store.Accounts) == 0 {
		fmt.Println("no accounts configured")
		return nil
	}
	for _, a := range store.Accounts {
		state := "disabled"
		if a.Enabled {
			state = "enabled"
		}
		fmt.Printf("%-32s %-16s %s\n", a.ID, a.Host, state)
	}
	return nil
}

func printMCP(opts options) error {
	store, err := accounts.Load(opts.config)
	if err != nil {
		return err
	}
	entry := map[string]any{
		"type": "http",
		"url":  "http://" + opts.addr,
		"headers": map[string]string{
			"Authorization": "Bearer " + store.APIToken,
		},
	}
	raw, err := json.MarshalIndent(map[string]any{"omagihu": entry}, "  ", "  ")
	if err != nil {
		return fmt.Errorf("encoding mcp entry: %w", err)
	}
	fmt.Println("add to the mcpServers object in ~/.claude.json:")
	fmt.Println()
	fmt.Println("  " + string(raw))
	return nil
}

// ghToken lifts the active token out of the gh CLI so the common case needs no
// manual PAT creation.
func ghToken(host string) (string, error) {
	cmd := exec.Command("gh", "auth", "token", "--hostname", host)
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return "", fmt.Errorf("gh auth token: %s", strings.TrimSpace(string(exit.Stderr)))
		}
		return "", fmt.Errorf("running gh auth token: %w", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", errors.New("gh returned an empty token")
	}
	return token, nil
}

// readTokenStdin takes the token off stdin so it never lands in argv or shell
// history.
func readTokenStdin() (string, error) {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return "", fmt.Errorf("inspecting stdin: %w", err)
	}
	if stat.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(os.Stderr, "paste the personal access token, then press enter: ")
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("reading token from stdin: %w", err)
	}
	token := strings.TrimSpace(line)
	if token == "" {
		return "", errors.New("empty token")
	}
	return token, nil
}

// resolveLogin trusts an explicit --login, otherwise asks the forge who the
// token belongs to, which also proves the token works. It calls the API
// directly so adding a pasted token does not require the gh CLI.
func resolveLogin(explicit, token, host string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	base := accounts.Account{Host: host}.APIBase()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/user", nil)
	if err != nil {
		return "", fmt.Errorf("building request to %s: %w", base, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("asking %s who this token belongs to: %w", base, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s/user returned %s: check the token, or pass --login", base, resp.Status)
	}
	var body struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("decoding %s/user: %w", base, err)
	}
	if body.Login == "" {
		return "", fmt.Errorf("%s/user returned no login: pass --login", base)
	}
	return body.Login, nil
}
