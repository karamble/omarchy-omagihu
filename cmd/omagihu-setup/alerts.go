package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/karamble/omarchy-omagihu/accounts"
	"github.com/karamble/omarchy-omagihu/alerts"
)

// alertFlags are the terms of a watch, as typed. Which of them were actually
// given matters for edit, where an absent flag means "leave it alone" rather
// than "set it to zero", so the parser records what it saw.
type alertFlags struct {
	above     *float64
	below     *float64
	rearm     *float64
	value     string
	hold      string
	field     string
	olderThan string
	where     []string
	expires   string
	reason    string
	deliver   string
	standing  bool
	once      bool
	jsonOut   bool
	dryRun    bool
	install   bool
	uninstall bool
	recipes   bool
	generate  string
	seen      map[string]bool
}

func (a alertFlags) given(name string) bool { return a.seen[name] }

// armedBy is the identity a watch is filed under. Inside a herdr pane that is
// the pane, so an alarm can find its way back to the agent that asked; outside
// one it is the person at the keyboard.
func armedBy() string {
	if pane := strings.TrimSpace(os.Getenv("HERDR_PANE_ID")); pane != "" {
		return pane
	}
	return alerts.TargetYou
}

// ---- the verbs

func showCatalogue(opts options) error {
	body, err := apiGet(opts, "/api/catalogue")
	if err != nil {
		return err
	}
	if opts.alerts.jsonOut {
		_, err := os.Stdout.Write(body)
		return err
	}
	fmt.Print(alerts.CatalogueMarkdown())
	return nil
}

func listAlerts(opts options) error {
	body, err := apiGet(opts, "/api/alerts")
	if err != nil {
		return err
	}
	if opts.alerts.jsonOut {
		_, err := os.Stdout.Write(body)
		return err
	}

	var reply struct {
		Alerts []struct {
			alerts.Trigger
			Status alerts.Status `json:"status"`
		} `json:"alerts"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return fmt.Errorf("reading alerts: %w", err)
	}
	if len(reply.Alerts) == 0 {
		fmt.Println("nothing is being waited on")
		return nil
	}
	for _, a := range reply.Alerts {
		fmt.Printf("%s  %-15s %s\n", a.ID, a.Status, conditionLine(a.Trigger))
		fmt.Printf("%s  %s\n", strings.Repeat(" ", len(a.ID)), detailLine(a.Trigger))
	}
	return nil
}

func armAlert(opts options, positional []string) error {
	if len(positional) < 2 {
		return errors.New("arm needs a path and an operator: omagihu-setup arm <path> <operator>")
	}
	t := alerts.Trigger{
		Path:      positional[0],
		Operator:  alerts.Operator(positional[1]),
		ArmedBy:   armedBy(),
		DeliverTo: opts.alerts.deliver,
	}
	if t.DeliverTo == "" {
		t.DeliverTo = armedBy()
	}
	if err := applyFlags(&t, opts.alerts, time.Now()); err != nil {
		return err
	}
	// Validate here as well as in the daemon, so --dry-run is a real check and
	// not just an echo of what was typed.
	if err := alerts.Validate(t); err != nil {
		return err
	}
	if opts.alerts.dryRun {
		return printTrigger(opts, t)
	}

	warnUnwatched(opts)
	body, err := apiSend(opts, http.MethodPost, "/api/alerts", t)
	if err != nil {
		return err
	}
	return report(opts, body, "armed")
}

func editAlert(opts options, positional []string) error {
	if len(positional) == 0 {
		return errors.New("edit needs a trigger id")
	}
	id := positional[0]

	// The patch carries only what was given. Everything else is whatever the
	// daemon already holds, which is what makes an edit an edit.
	patch := map[string]any{}
	params := map[string]any{}
	f := opts.alerts

	if f.given("--above") {
		params["above"] = f.above
		params["below"] = nil
	}
	if f.given("--below") {
		params["below"] = f.below
		params["above"] = nil
	}
	if f.given("--rearm") {
		params["rearm"] = f.rearm
	}
	if f.given("--value") {
		params["value"] = f.value
	}
	if f.given("--hold") {
		params["hold"] = f.hold
	}
	if f.given("--field") {
		params["field"] = f.field
	}
	if f.given("--older-than") {
		params["olderThan"] = f.olderThan
	}
	if len(params) > 0 {
		patch["params"] = params
	}

	if f.given("--where") {
		where, err := parseWhere(f.where)
		if err != nil {
			return err
		}
		patch["where"] = where
	}
	if f.given("--expires") {
		when, err := alerts.ParseExpiry(f.expires, time.Now())
		if err != nil {
			return err
		}
		patch["expiresAt"] = when
	}
	if f.given("--reason") {
		patch["reason"] = f.reason
	}
	if f.given("--deliver") {
		patch["deliverTo"] = f.deliver
	}
	if f.given("--standing") {
		patch["standing"] = true
	}
	if f.given("--once") {
		patch["standing"] = false
	}
	if len(patch) == 0 {
		return errors.New("edit needs something to change")
	}

	body, err := apiSend(opts, http.MethodPatch, "/api/alerts/"+id, patch)
	if err != nil {
		return err
	}
	return report(opts, body, "edited")
}

func disarmAlert(opts options, positional []string) error {
	if len(positional) == 0 {
		return errors.New("disarm needs a trigger id")
	}
	body, err := apiSend(opts, http.MethodDelete, "/api/alerts/"+positional[0], nil)
	if err != nil {
		return err
	}
	if opts.alerts.jsonOut {
		_, err := os.Stdout.Write(body)
		return err
	}
	fmt.Println("disarmed", positional[0])
	return nil
}

// applyFlags turns the typed terms into a trigger, leaving the catalogue to
// decide whether they make sense together.
func applyFlags(t *alerts.Trigger, f alertFlags, now time.Time) error {
	t.Params = alerts.Params{
		Above:     f.above,
		Below:     f.below,
		Rearm:     f.rearm,
		Value:     f.value,
		Hold:      f.hold,
		Field:     f.field,
		OlderThan: f.olderThan,
	}

	where, err := parseWhere(f.where)
	if err != nil {
		return err
	}
	t.Where = where

	when, err := alerts.ParseExpiry(f.expires, now)
	if err != nil {
		return err
	}
	t.ExpiresAt = when
	t.Reason = f.reason
	t.Standing = f.standing && !f.once
	t.ArmedAt = now
	return nil
}

// parseWhere reads the repeatable filter, which is field=value for an exact
// match or field~=text for a case-insensitive substring.
func parseWhere(raw []string) ([]alerts.Where, error) {
	out := make([]alerts.Where, 0, len(raw))
	for _, w := range raw {
		if field, value, ok := strings.Cut(w, "~="); ok {
			out = append(out, alerts.Where{Field: strings.TrimSpace(field), Op: "~=", Value: value})
			continue
		}
		field, value, ok := strings.Cut(w, "=")
		if !ok {
			return nil, fmt.Errorf("bad --where %q: use field=value or field~=text", w)
		}
		out = append(out, alerts.Where{Field: strings.TrimSpace(field), Op: "=", Value: value})
	}
	return out, nil
}

// warnUnwatched says so when a watch is being stored with nothing running to
// sample it. The trigger is still armed: this is a warning, not a refusal.
func warnUnwatched(opts options) {
	body, err := apiGet(opts, "/api/health")
	if err != nil {
		return
	}
	var health struct {
		Monitoring bool `json:"monitoring"`
	}
	if json.Unmarshal(body, &health) == nil && !health.Monitoring {
		fmt.Fprintln(os.Stderr,
			"omagihu-setup: monitoring is off, so nothing is being sampled and this cannot fire until it is switched back on")
	}
}

func report(opts options, body []byte, what string) error {
	if opts.alerts.jsonOut {
		_, err := os.Stdout.Write(body)
		return err
	}
	var t alerts.Trigger
	if err := json.Unmarshal(body, &t); err != nil {
		_, err := os.Stdout.Write(body)
		return err
	}
	fmt.Printf("%s %s: %s\n", what, t.ID, conditionLine(t))
	fmt.Printf("%s %s\n", strings.Repeat(" ", len(what)+len(t.ID)), detailLine(t))
	return nil
}

func printTrigger(opts options, t alerts.Trigger) error {
	if opts.alerts.jsonOut {
		raw, err := json.MarshalIndent(t, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(raw))
		return nil
	}
	// A dry run carries no id, because nothing was created.
	fmt.Printf("would arm: %s\n          %s\n", conditionLine(t), detailLine(t))
	return nil
}

// conditionLine reads a trigger back as the sentence that armed it.
func conditionLine(t alerts.Trigger) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", t.Path, t.Operator)

	switch t.Operator {
	case alerts.OpCrosses, alerts.OpCount:
		if t.Params.Above != nil {
			fmt.Fprintf(&b, " above %s", trimFloat(*t.Params.Above))
		} else if t.Params.Below != nil {
			fmt.Fprintf(&b, " below %s", trimFloat(*t.Params.Below))
		}
	case alerts.OpBecomes:
		fmt.Fprintf(&b, " %q", t.Params.Value)
	case alerts.OpAges:
		fmt.Fprintf(&b, " past %s", t.Params.OlderThan)
		if t.Params.Field != "" {
			fmt.Fprintf(&b, " on %s", t.Params.Field)
		}
	}

	if len(t.Where) > 0 {
		parts := make([]string, 0, len(t.Where))
		for _, w := range t.Where {
			parts = append(parts, w.Field+w.Op+w.Value)
		}
		fmt.Fprintf(&b, " where %s", strings.Join(parts, " and "))
	}
	return b.String()
}

func detailLine(t alerts.Trigger) string {
	kind := "once"
	if t.Standing {
		kind = "standing"
	}
	bits := []string{
		"wakes " + t.DeliverTo,
		kind,
		"expires " + t.ExpiresAt.Local().Format("2006-01-02 15:04"),
		"armed by " + t.ArmedBy,
	}
	if t.Reason != "" {
		bits = append(bits, strconv.Quote(t.Reason))
	}
	return strings.Join(bits, " · ")
}

func trimFloat(v float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.4f", v), "0"), ".")
}

// ---- talking to the daemon

func apiGet(opts options, path string) ([]byte, error) {
	return apiSend(opts, http.MethodGet, path, nil)
}

// apiSend is the one authenticated call every alert verb goes through. The
// token comes from the 0600 store, so the daemon and the command line share one
// secret and no other process sees it.
func apiSend(opts options, method, path string, payload any) ([]byte, error) {
	store, err := accounts.Load(opts.config)
	if err != nil {
		return nil, err
	}

	var reader io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encoding request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, "http://"+opts.addr+path, reader)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+store.APIToken)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the omagihu daemon is not answering on %s: %w", opts.addr, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
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
