// Command omagihud is the omagihu daemon: it holds the account store, will run
// the pollers and the filesystem watcher, and serves the panel over HTTP.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/karamble/omarchy-omagihu/accounts"
	"github.com/karamble/omarchy-omagihu/alerts"
	"github.com/karamble/omarchy-omagihu/api"
	"github.com/karamble/omarchy-omagihu/forge"
	"github.com/karamble/omarchy-omagihu/local"
	"github.com/karamble/omarchy-omagihu/notify"
	"github.com/karamble/omarchy-omagihu/poll"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "omagihud:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	var (
		configPath = flag.String("config", "", "path to accounts.json (default ~/.config/omagihu/accounts.json)")
		addr       = flag.String("addr", "127.0.0.1:8099", "listen address, loopback only by design")
		logLevel   = flag.String("log-level", "info", "debug, info, warn or error")
		inboxEvery = flag.Duration("inbox-interval", poll.DefaultInboxInterval, "notification poll cadence; GitHub's X-Poll-Interval wins when it asks for slower")
		workEvery  = flag.Duration("work-interval", poll.DefaultWorkInterval, "pull request, review and issue poll cadence")
		roots      = flag.String("roots", "", "comma separated workspace roots to watch; empty reads them from the store")
		maxDepth   = flag.Int("max-depth", local.DefaultMaxDepth, "how many levels below each root to search")
		excludes   = flag.String("excludes", strings.Join(local.DefaultExcludes, ","), "comma separated directory names never descended into")
		refresh    = flag.Duration("refresh", local.DefaultRefresh, "working tree re-inspection cadence")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("omagihud", version)
		return nil
	}

	level, err := parseLevel(*logLevel)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	store, err := accounts.Load(*configPath)
	if err != nil {
		return err
	}
	if store.APIToken == "" {
		return fmt.Errorf("%s has no apiToken: run omagihu-setup", store.Path())
	}

	logger.Info("starting",
		"version", version,
		"addr", *addr,
		"config", store.Path(),
		"accounts", len(store.Accounts),
		"enabled", len(store.Enabled()),
	)
	if len(store.Enabled()) == 0 {
		logger.Warn("no enabled accounts: the remote plane will stay idle until one is added")
	}

	clients := make([]poll.Client, 0, len(store.Enabled()))
	for _, account := range store.Enabled() {
		clients = append(clients, forge.New(account, version))
	}
	poller := poll.New(clients, logger, *inboxEvery, *workEvery)

	// The roots live in the store. The flag stays as an override, for a one-off
	// run against somewhere else, but the daemon is normally started with no
	// arguments at all.
	watchRoots := store.RootsOrDefault(local.DefaultRoots)
	if *roots != "" {
		watchRoots = local.SplitList(*roots)
	}

	watcher := local.NewWatcher(local.Config{
		Roots:    watchRoots,
		MaxDepth: *maxDepth,
		Excludes: local.SplitList(*excludes),
	}, logger, *refresh, local.DefaultRediscover)

	apiServer := api.NewServer(store, poller, watcher, logger, version)
	// Come back in whatever state the switch was left in.
	apiServer.ApplyStored()
	if !store.MonitoringEnabled() {
		logger.Warn("monitoring is off: no outbound requests will be made")
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	triggers, err := alerts.Load("")
	if err != nil {
		return err
	}
	engine := alerts.NewEngine(triggers, apiServer.AlertSample, store.MonitoringEnabled,
		alerts.NewDeliverer(notify.Desktop), logger)
	apiServer.SetEngine(engine)
	logger.Info("alerts loaded", "armed", len(triggers.Triggers), "store", triggers.Path())

	alertsDone := make(chan struct{})
	go func() {
		defer close(alertsDone)
		engine.Run(ctx)
	}()

	notifier := notify.New(poller, watcher, apiServer.NotifyPrefs, apiServer.Facts,
		store.MonitoringEnabled, logger)
	notifyDone := make(chan struct{})
	go func() {
		defer close(notifyDone)
		notifier.Run(ctx)
	}()

	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		poller.Run(ctx)
	}()

	fetchDone := make(chan struct{})
	go func() {
		defer close(fetchDone)
		watcher.RunFetch(ctx)
	}()

	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		if err := watcher.Run(ctx); err != nil {
			logger.Error("local watcher stopped", "err", err)
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("listening on %s: %w", *addr, err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdownCtx)
		<-pollDone
		<-watchDone
		<-fetchDone
		<-notifyDone
		<-alertsDone
		if err != nil {
			return fmt.Errorf("shutting down: %w", err)
		}
		return nil
	}
}

func parseLevel(name string) (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(name)); err != nil {
		return 0, fmt.Errorf("bad log level %q: want debug, info, warn or error", name)
	}
	return l, nil
}
