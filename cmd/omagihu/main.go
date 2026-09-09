// Command omagihu is the panel's helper. QML cannot speak HTTP comfortably, so
// the bar widget runs this and reads one JSON document from stdout, the same
// shape Demarchy uses for its helper.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/karamble/omarchy-omagihu/accounts"
	"github.com/karamble/omarchy-omagihu/api"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		// The panel parses stdout, so failures go to stderr and are reported
		// through the exit code.
		fmt.Fprintln(os.Stderr, "omagihu:", err)
		os.Exit(1)
	}
}

func usage() string {
	return strings.TrimSpace(`
usage: omagihu <command> [flags]

  dashboard    everything the panel renders, as one JSON document
  repos        the local repositories only
  inbox        unread notifications only
  health       daemon status
  sleep        stop all polling: nothing leaves this machine
  wake         resume polling
  every <min>  set the polling rhythm in minutes and resume
  refresh      poll now, instead of waiting out the current interval
  alerts       list armed watches
  catalogue    what can be watched
  agents       herdr agents available to wake
  arm <json>       arm a watch from a trigger document
  edit <id> <json> change an armed watch, keeping its id and its owner
  disarm <id>      remove an armed watch
  mcp on|off   serve or withdraw the MCP endpoint
  fetch on|off [min]  background fetch of remote refs, and its cadence
  recycle      mint a new bearer token, locking out every current client
  notify <domain> on|off   reviews, broken, inbox or local
  clip token|entry         copy the bearer token, or the whole ~/.claude.json
                           entry, to the clipboard
  open <url>   open a url in the browser
  version      print the version

flags:
  --config PATH    account store (default ~/.config/omagihu/accounts.json)
  --addr HOSTPORT  daemon address (default 127.0.0.1:8099)
  --demo           with dashboard, emit a fabricated document and exit; for
                   building the panel and for the README screenshot, never
                   used by the widget
`)
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage())
	}
	cmd, rest := args[0], args[1:]

	config, addr, demo, positional, err := parseFlags(rest)
	if err != nil {
		return err
	}

	switch cmd {
	case "version":
		fmt.Println("omagihu", version)
		return nil
	case "open":
		if len(positional) == 0 {
			return errors.New("open needs a url")
		}
		return openURL(positional[0])
	case "every":
		if len(positional) == 0 {
			return errors.New("every needs a number of minutes")
		}
		minutes, err := strconv.Atoi(positional[0])
		if err != nil || minutes <= 0 {
			return fmt.Errorf("every needs a positive number of minutes, got %q", positional[0])
		}
		body, err := postMonitoring(config, addr, fmt.Sprintf(`{"intervalMin":%d}`, minutes))
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(body)
		return err
	case "refresh":
		body, err := post(config, addr, "/api/refresh", `{}`)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(body)
		return err
	case "fetch":
		if len(positional) == 0 || (positional[0] != "on" && positional[0] != "off") {
			return errors.New("fetch needs on or off")
		}
		payload := fmt.Sprintf(`{"enabled":%v}`, positional[0] == "on")
		if len(positional) > 1 {
			minutes, err := strconv.Atoi(positional[1])
			if err != nil || minutes <= 0 {
				return fmt.Errorf("fetch cadence must be a positive number of minutes, got %q", positional[1])
			}
			payload = fmt.Sprintf(`{"enabled":%v,"everyMin":%d}`, positional[0] == "on", minutes)
		}
		body, err := post(config, addr, "/api/fetch", payload)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(body)
		return err
	case "mcp":
		if len(positional) == 0 || (positional[0] != "on" && positional[0] != "off") {
			return errors.New("mcp needs on or off")
		}
		body, err := post(config, addr, "/api/mcp",
			fmt.Sprintf(`{"enabled":%v}`, positional[0] == "on"))
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(body)
		return err
	case "notify":
		if len(positional) < 2 || (positional[1] != "on" && positional[1] != "off") {
			return errors.New("notify needs a domain and on or off")
		}
		body, err := post(config, addr, "/api/notify",
			fmt.Sprintf(`{"domain":%q,"enabled":%v}`, positional[0], positional[1] == "on"))
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(body)
		return err
	case "clip":
		what := "entry"
		if len(positional) > 0 {
			what = positional[0]
		}
		return clip(config, addr, what)
	case "recycle":
		body, err := post(config, addr, "/api/token/recycle", `{}`)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(body)
		return err
	case "sleep", "wake":
		body, err := setMonitoring(config, addr, cmd == "wake")
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(body)
		return err
	case "edit":
		if len(positional) < 2 {
			return errors.New("edit needs a trigger id and a document")
		}
		body, err := send(config, addr, http.MethodPatch, "/api/alerts/"+positional[0], positional[1])
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(body)
		return err
	case "disarm":
		if len(positional) == 0 {
			return errors.New("disarm needs a trigger id")
		}
		body, err := send(config, addr, http.MethodDelete, "/api/alerts/"+positional[0], "")
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(body)
		return err
	case "arm":
		if len(positional) == 0 {
			return errors.New("arm needs a trigger document")
		}
		body, err := post(config, addr, "/api/alerts", positional[0])
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(body)
		return err
	case "dashboard", "repos", "inbox", "health", "alerts", "catalogue", "agents", "facts":
		// The demo document never reaches the daemon: it exists so the panel can
		// be drawn without an account, a network or a single real repository.
		if demo && cmd == "dashboard" {
			raw, err := json.MarshalIndent(api.Demo(), "", "  ")
			if err != nil {
				return fmt.Errorf("encoding the demo document: %w", err)
			}
			fmt.Println(string(raw))
			return nil
		}
		body, err := fetch(config, addr, "/api/"+cmd)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(body)
		return err
	case "help", "-h", "--help":
		fmt.Println(usage())
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage())
	}
}

func parseFlags(args []string) (config, addr string, demo bool, positional []string, err error) {
	addr = "127.0.0.1:8099"
	for i := 0; i < len(args); i++ {
		key, value, hasInline := strings.Cut(args[i], "=")
		if !strings.HasPrefix(key, "--") {
			positional = append(positional, args[i])
			continue
		}
		// A boolean flag takes no value, so it is settled before anything tries
		// to consume the next token.
		if key == "--demo" {
			demo = true
			continue
		}
		if !hasInline {
			if i+1 >= len(args) {
				return "", "", false, nil, fmt.Errorf("%s needs a value", key)
			}
			i++
			value = args[i]
		}
		switch key {
		case "--config":
			config = value
		case "--addr":
			addr = value
		default:
			return "", "", false, nil, fmt.Errorf("unknown flag %q", key)
		}
	}
	return config, addr, demo, positional, nil
}

// fetch calls the daemon with the shared bearer token.
func fetch(config, addr, path string) ([]byte, error) {
	store, err := accounts.Load(config)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+store.APIToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the omagihu daemon is not answering on %s: %w", addr, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s", path, resp.Status)
	}
	return body, nil
}

// setMonitoring flips the daemon's master switch.
func setMonitoring(config, addr string, enabled bool) ([]byte, error) {
	payload := `{"enabled":false}`
	if enabled {
		payload = `{"enabled":true}`
	}
	return postMonitoring(config, addr, payload)
}

// postMonitoring sends one control message to the daemon.
func postMonitoring(config, addr, payload string) ([]byte, error) {
	return post(config, addr, "/api/monitoring", payload)
}

// post sends one authenticated control message.
func post(config, addr, path, payload string) ([]byte, error) {
	return send(config, addr, http.MethodPost, path, payload)
}

// send is the authenticated request every write verb goes through.
func send(config, addr, method, path, payload string) ([]byte, error) {
	store, err := accounts.Load(config)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method,
		"http://"+addr+path, strings.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+store.APIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the omagihu daemon is not answering on %s: %w", addr, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The daemon explains a refused trigger in the body, and that sentence is
		// the whole point of the reply, so it is carried out rather than dropped.
		var reply struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &reply) == nil && reply.Error != "" {
			return nil, errors.New(reply.Error)
		}
		return nil, fmt.Errorf("%s returned %s", path, resp.Status)
	}
	return body, nil
}

// clip puts the token, or the whole MCP entry, on the clipboard. The value is
// read from the 0600 store and piped to wl-copy on stdin, so it never appears
// in argv where any process on the machine could read it.
func clip(config, addr, what string) error {
	store, err := accounts.Load(config)
	if err != nil {
		return err
	}

	var payload string
	switch what {
	case "token":
		payload = store.APIToken
	case "entry":
		entry := map[string]any{
			"omagihu": map[string]any{
				"type":    "http",
				"url":     "http://" + addr + "/mcp",
				"headers": map[string]string{"Authorization": "Bearer " + store.APIToken},
			},
		}
		raw, err := json.MarshalIndent(entry, "", "  ")
		if err != nil {
			return fmt.Errorf("encoding entry: %w", err)
		}
		payload = string(raw)
	default:
		return fmt.Errorf("clip takes token or entry, got %q", what)
	}

	cmd := exec.Command("wl-copy")
	cmd.Stdin = strings.NewReader(payload)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("wl-copy: %w", err)
	}
	fmt.Printf("{\"copied\":%q}\n", what)
	return nil
}

// openURL hands a link to the desktop. This is the whole of the write surface:
// omagihu opens things, it never changes them.
func openURL(url string) error {
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return fmt.Errorf("refusing to open %q: only http and https urls", url)
	}
	if err := exec.Command("xdg-open", url).Start(); err != nil {
		return fmt.Errorf("opening %s: %w", url, err)
	}
	return nil
}
