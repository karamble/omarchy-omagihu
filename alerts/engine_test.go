package alerts

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/karamble/omarchy-omagihu/attention"
	"github.com/karamble/omarchy-omagihu/forge"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// engine builds an engine over a throwaway store with a settable sample.
func engine(t *testing.T, sample *Snapshot, awake *bool) (*Engine, *[]Fire) {
	t.Helper()
	store := &Store{Version: 1}
	store.SetPath(filepath.Join(t.TempDir(), "triggers.json"))

	var delivered []Fire
	e := NewEngine(store, func() Snapshot { return *sample }, func() bool { return *awake },
		func(tr Trigger, f Fire) (string, error) {
			delivered = append(delivered, f)
			return "test", nil
		}, quiet())
	return e, &delivered
}

func TestEngineArmValidatesAgainstTheCatalogue(t *testing.T) {
	snap := Snapshot{}
	awake := true
	e, _ := engine(t, &snap, &awake)

	if _, err := e.Arm(Trigger{Path: "no.such.path", Operator: OpCrosses,
		Params: Params{Above: ptr(1)}, DeliverTo: "you",
		ExpiresAt: time.Now().Add(time.Hour)}); err == nil {
		t.Error("armed a trigger on a path that does not exist")
	}

	if _, err := e.Arm(Trigger{Path: "attention.unread", Operator: OpAppears,
		DeliverTo: "you", ExpiresAt: time.Now().Add(time.Hour)}); err == nil {
		t.Error("armed appears on a number")
	}

	if _, err := e.Arm(Trigger{Path: "attention.unread", Operator: OpCrosses,
		DeliverTo: "you", ExpiresAt: time.Now().Add(time.Hour)}); err == nil {
		t.Error("armed crosses with no bound")
	}

	if _, err := e.Arm(Trigger{Path: "attention.unread", Operator: OpCrosses,
		Params: Params{Above: ptr(5)}, DeliverTo: "you"}); err == nil {
		t.Error("armed a trigger with no expiry")
	}
}

func TestEngineRefusesDuplicates(t *testing.T) {
	snap := Snapshot{}
	awake := true
	e, _ := engine(t, &snap, &awake)

	tr := Trigger{Path: "attention.unread", Operator: OpCrosses,
		Params: Params{Above: ptr(5)}, DeliverTo: "you",
		ExpiresAt: time.Now().Add(time.Hour)}

	if _, err := e.Arm(tr); err != nil {
		t.Fatalf("first arm failed: %v", err)
	}
	if _, err := e.Arm(tr); err == nil {
		t.Error("armed a second identical trigger, doubling the noise")
	}
}

func TestEnginePassDelivers(t *testing.T) {
	snap := Snapshot{}
	awake := true
	e, delivered := engine(t, &snap, &awake)

	if _, err := e.Arm(Trigger{Path: "work.reviewRequests", Operator: OpAppears,
		DeliverTo: "you", ExpiresAt: time.Now().Add(time.Hour),
		Reason: "somebody is blocked"}); err != nil {
		t.Fatalf("arm: %v", err)
	}

	e.Pass(time.Now()) // primes
	if len(*delivered) != 0 {
		t.Fatalf("delivered on the priming pass: %+v", *delivered)
	}

	snap = Snapshot{Reviews: []forge.PullRequest{{Repo: "o/r", Number: 1}}}
	e.Pass(time.Now())
	if len(*delivered) != 1 {
		t.Fatalf("delivered %d alarms, want 1", len(*delivered))
	}
	if (*delivered)[0].Reason != "somebody is blocked" {
		t.Errorf("alarm carried reason %q", (*delivered)[0].Reason)
	}

	got := e.List()
	if len(got) != 1 || got[0].State.Delivered != "test" {
		t.Errorf("trigger did not record its delivery: %+v", got)
	}
}

// TestEngineAsleepDeliversNothing: the master switch silences alarms too.
func TestEngineAsleepDeliversNothing(t *testing.T) {
	snap := Snapshot{}
	awake := true
	e, delivered := engine(t, &snap, &awake)

	e.Arm(Trigger{Path: "attention.level", Operator: OpBecomes,
		Params: Params{Value: "urgent"}, DeliverTo: "you",
		ExpiresAt: time.Now().Add(time.Hour)})
	e.Pass(time.Now())

	awake = false
	snap = Snapshot{Attention: attention.State{Level: "urgent"}}
	e.Pass(time.Now())

	if len(*delivered) != 0 {
		t.Errorf("delivered %d alarms while asleep, want 0", len(*delivered))
	}
}

func TestEngineDisarm(t *testing.T) {
	snap := Snapshot{}
	awake := true
	e, _ := engine(t, &snap, &awake)

	armed, err := e.Arm(Trigger{Path: "attention.unread", Operator: OpCrosses,
		Params: Params{Above: ptr(5)}, DeliverTo: "you",
		ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("arm: %v", err)
	}

	ok, err := e.Disarm(armed.ID)
	if err != nil || !ok {
		t.Fatalf("disarm = %v, %v", ok, err)
	}
	if len(e.List()) != 0 {
		t.Error("trigger survived disarming")
	}
	if ok, _ := e.Disarm("t-nothing"); ok {
		t.Error("disarming an unknown id reported success")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "triggers.json")

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load of a missing file should be an empty store: %v", err)
	}
	s.Add(Trigger{ID: "t-1", Path: "inbox", Operator: OpAppears,
		DeliverTo: "w7:p1", ExpiresAt: time.Now().Add(time.Hour)})
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	again, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(again.Triggers) != 1 || again.Triggers[0].DeliverTo != "w7:p1" {
		t.Errorf("round trip lost the trigger: %+v", again.Triggers)
	}
}
