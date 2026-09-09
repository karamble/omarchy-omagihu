package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/karamble/omarchy-omagihu/alerts"
)

// Alerts is the trigger engine as this package consumes it. It is the only
// thing the MCP surface can write to, and it writes nothing but omagihu's own
// trigger store: arming a watch never touches GitHub, a repository, or anything
// else on the machine.
type Alerts interface {
	List() []alerts.Trigger
	Arm(t alerts.Trigger) (alerts.Trigger, error)
	Edit(id string, apply func(*alerts.Trigger)) (alerts.Trigger, error)
	Disarm(id string) (bool, error)
}

// errNoAlerts is what every alert tool answers with when the daemon came up
// without an engine, rather than panicking on a nil interface.
var errNoAlerts = errors.New("alerts are not running on this daemon")

// alertRow is a trigger plus the one word describing where it stands.
type alertRow struct {
	alerts.Trigger
	Status alerts.Status `json:"status"`
}

type catalogueOut struct {
	Status status        `json:"status"`
	Count  int           `json:"count"`
	Paths  []alerts.Leaf `json:"paths"`
}

type alertsOut struct {
	Status status     `json:"status"`
	Count  int        `json:"count"`
	Alerts []alertRow `json:"alerts"`
}

type agentsOut struct {
	Status status         `json:"status"`
	Count  int            `json:"count"`
	Agents []alerts.Agent `json:"agents"`
	Note   string         `json:"note,omitempty"`
}

type triggerOut struct {
	Status  status         `json:"status"`
	Trigger alerts.Trigger `json:"trigger"`
}

type disarmOut struct {
	Status  status `json:"status"`
	Removed bool   `json:"removed"`
	ID      string `json:"id"`
}

// armIn mirrors the command line's flags field for field, so the two surfaces
// cannot drift into meaning different things.
type armIn struct {
	Path      string   `json:"path" jsonschema:"a path from omagihu_catalogue, such as work.reviewRequests or facts"`
	Operator  string   `json:"operator" jsonschema:"appears, disappears, count, crosses, becomes or ages; the catalogue says which a path takes"`
	Expires   string   `json:"expires" jsonschema:"required: how long the watch stands, such as 4d, 12h or 90m, a date like 2026-10-01, or an RFC 3339 time"`
	Above     *float64 `json:"above,omitempty" jsonschema:"bound for crosses and count; give one of above or below, never both"`
	Below     *float64 `json:"below,omitempty" jsonschema:"bound for crosses and count; give one of above or below, never both"`
	Rearm     *float64 `json:"rearm,omitempty" jsonschema:"how far back past the bound the value must come before it can ring again"`
	Value     string   `json:"value,omitempty" jsonschema:"the value becomes waits for, such as urgent"`
	Hold      string   `json:"hold,omitempty" jsonschema:"how long becomes must be away before it can ring again, default 5m"`
	OlderThan string   `json:"olderThan,omitempty" jsonschema:"the age ages measures against, such as 7d"`
	Field     string   `json:"field,omitempty" jsonschema:"which timestamp ages measures when a path carries more than one"`
	Where     []string `json:"where,omitempty" jsonschema:"filters that must all hold, each field=value for an exact match or field~=text for a substring"`
	Reason    string   `json:"reason,omitempty" jsonschema:"why this matters; it is the only context the alarm carries, so write it for whoever is woken"`
	DeliverTo string   `json:"deliverTo,omitempty" jsonschema:"a herdr agent or pane id, repo for whoever is working in the checkout concerned, or you for a desktop notification; default you"`
	Standing  bool     `json:"standing,omitempty" jsonschema:"ring every time until it expires; the default is to ring once and disarm"`
	ArmedBy   string   `json:"armedBy,omitempty" jsonschema:"who owns this watch, such as your herdr pane id; default agent"`
}

// editIn is armIn with an id, and with nothing required but that id: whatever
// is left out keeps the value the trigger already has.
type editIn struct {
	ID        string   `json:"id" jsonschema:"the trigger id, from omagihu_alerts"`
	Path      string   `json:"path,omitempty"`
	Operator  string   `json:"operator,omitempty"`
	Expires   string   `json:"expires,omitempty" jsonschema:"a new expiry; left out, the current one stands"`
	Above     *float64 `json:"above,omitempty"`
	Below     *float64 `json:"below,omitempty"`
	Rearm     *float64 `json:"rearm,omitempty"`
	Value     string   `json:"value,omitempty"`
	Hold      string   `json:"hold,omitempty"`
	OlderThan string   `json:"olderThan,omitempty"`
	Field     string   `json:"field,omitempty"`
	Where     []string `json:"where,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	DeliverTo string   `json:"deliverTo,omitempty"`
	Standing  *bool    `json:"standing,omitempty"`
}

type disarmIn struct {
	ID string `json:"id" jsonschema:"the trigger id, from omagihu_alerts"`
}

// registerAlerts adds the watching surface: three tools that read, and three
// that change omagihu's own trigger store and nothing else.
func registerAlerts(s *mcp.Server, src Source) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "omagihu_catalogue",
		Description: "Everything a watch can be armed on: each path, what kind of value it " +
			"is, the operators it takes, the fields a filter can narrow it by, and which " +
			"timestamps ages can measure. Read this before arming.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, catalogueOut, error) {
		paths := alerts.Catalogue()
		return nil, catalogueOut{Status: src.status(), Count: len(paths), Paths: paths}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "omagihu_alerts",
		Description: "Watches currently armed, with where each one stands: armed, rearming, " +
			"fired, expired, no-sample or delivery-failed. Read this before arming so you do " +
			"not duplicate a watch, and never disarm or edit one whose armedBy is not you.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, alertsOut, error) {
		engine, ok := src.alerts()
		if !ok {
			return nil, alertsOut{}, errNoAlerts
		}
		now := time.Now()
		list := engine.List()
		out := make([]alertRow, 0, len(list))
		for _, t := range list {
			out = append(out, alertRow{Trigger: t, Status: t.Status(now)})
		}
		return nil, alertsOut{Status: src.status(), Count: len(out), Alerts: out}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "omagihu_agents",
		Description: "herdr agents that an alarm can be delivered to, for the deliverTo " +
			"field. No herdr is a normal state, not a failure: it means there is nobody to " +
			"wake but the person at the keyboard.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, agentsOut, error) {
		found, err := alerts.Agents(ctx)
		if err != nil {
			return nil, agentsOut{Status: src.status(), Agents: []alerts.Agent{}, Note: err.Error()}, nil
		}
		return nil, agentsOut{Status: src.status(), Count: len(found), Agents: found}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "omagihu_arm",
		Description: "Arm a watch: ring when this becomes true, then stop. The first sample " +
			"never fires, because a condition already true is history rather than an event; " +
			"nothing fires while monitoring is off; and an expiry is required, because " +
			"nothing stays armed for ever. The alarm carries the condition and your reason " +
			"and no values, so read omagihu yourself when woken. Writes only to omagihu's " +
			"own trigger store: it never acts on GitHub or on a repository.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in armIn) (*mcp.CallToolResult, triggerOut, error) {
		engine, ok := src.alerts()
		if !ok {
			return nil, triggerOut{}, errNoAlerts
		}
		t, err := in.trigger()
		if err != nil {
			return nil, triggerOut{}, err
		}
		armed, err := engine.Arm(t)
		if err != nil {
			return nil, triggerOut{}, err
		}
		return nil, triggerOut{Status: src.status(), Trigger: armed}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "omagihu_edit",
		Description: "Change an armed watch's terms, keeping its id and its owner. Whatever " +
			"is left out keeps the value it has. Editing clears what the watch had learned, " +
			"so the next sample teaches it again exactly as arming did. Move a bound or " +
			"extend an expiry with this rather than arming a second watch.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in editIn) (*mcp.CallToolResult, triggerOut, error) {
		engine, ok := src.alerts()
		if !ok {
			return nil, triggerOut{}, errNoAlerts
		}
		if in.ID == "" {
			return nil, triggerOut{}, errors.New("edit needs a trigger id")
		}
		var applyErr error
		edited, err := engine.Edit(in.ID, func(t *alerts.Trigger) { applyErr = in.apply(t) })
		if applyErr != nil {
			return nil, triggerOut{}, applyErr
		}
		if err != nil {
			return nil, triggerOut{}, err
		}
		return nil, triggerOut{Status: src.status(), Trigger: edited}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "omagihu_disarm",
		Description: "Remove an armed watch. Disarm a standing watch once it has done its " +
			"job. Never disarm one whose armedBy is not you: say who owns it instead.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in disarmIn) (*mcp.CallToolResult, disarmOut, error) {
		engine, ok := src.alerts()
		if !ok {
			return nil, disarmOut{}, errNoAlerts
		}
		if in.ID == "" {
			return nil, disarmOut{}, errors.New("disarm needs a trigger id")
		}
		removed, err := engine.Disarm(in.ID)
		if err != nil {
			return nil, disarmOut{}, err
		}
		if !removed {
			return nil, disarmOut{}, fmt.Errorf("no trigger %q", in.ID)
		}
		return nil, disarmOut{Status: src.status(), Removed: true, ID: in.ID}, nil
	})
}

// trigger turns the arming request into a trigger. The catalogue has the last
// word on whether it makes sense: Engine.Arm validates against it.
func (in armIn) trigger() (alerts.Trigger, error) {
	where, err := alerts.ParseWhereAll(in.Where)
	if err != nil {
		return alerts.Trigger{}, err
	}
	expires, err := alerts.ParseExpiry(in.Expires, time.Now())
	if err != nil {
		return alerts.Trigger{}, err
	}

	armedBy := in.ArmedBy
	if armedBy == "" {
		armedBy = "agent"
	}
	deliverTo := in.DeliverTo
	if deliverTo == "" {
		deliverTo = alerts.TargetYou
	}

	return alerts.Trigger{
		Path:     in.Path,
		Operator: alerts.Operator(in.Operator),
		Params: alerts.Params{
			Above: in.Above, Below: in.Below, Rearm: in.Rearm,
			Value: in.Value, Hold: in.Hold,
			Field: in.Field, OlderThan: in.OlderThan,
		},
		Where:     where,
		DeliverTo: deliverTo,
		Standing:  in.Standing,
		ExpiresAt: expires,
		Reason:    in.Reason,
		ArmedBy:   armedBy,
		ArmedAt:   time.Now(),
	}, nil
}

// apply folds an edit onto the stored trigger, leaving alone whatever the
// request did not name.
func (in editIn) apply(t *alerts.Trigger) error {
	if in.Path != "" {
		t.Path = in.Path
	}
	if in.Operator != "" {
		t.Operator = alerts.Operator(in.Operator)
	}
	// A bound is one-sided, so naming one clears the other rather than leaving
	// a trigger that asks for both.
	if in.Above != nil {
		t.Params.Above, t.Params.Below = in.Above, nil
	}
	if in.Below != nil {
		t.Params.Below, t.Params.Above = in.Below, nil
	}
	if in.Rearm != nil {
		t.Params.Rearm = in.Rearm
	}
	if in.Value != "" {
		t.Params.Value = in.Value
	}
	if in.Hold != "" {
		t.Params.Hold = in.Hold
	}
	if in.OlderThan != "" {
		t.Params.OlderThan = in.OlderThan
	}
	if in.Field != "" {
		t.Params.Field = in.Field
	}
	if in.Where != nil {
		where, err := alerts.ParseWhereAll(in.Where)
		if err != nil {
			return err
		}
		t.Where = where
	}
	if in.Reason != "" {
		t.Reason = in.Reason
	}
	if in.DeliverTo != "" {
		t.DeliverTo = in.DeliverTo
	}
	if in.Standing != nil {
		t.Standing = *in.Standing
	}
	if in.Expires != "" {
		expires, err := alerts.ParseExpiry(in.Expires, time.Now())
		if err != nil {
			return err
		}
		t.ExpiresAt = expires
	}
	return nil
}
