package supervisor_test

// Lifecycle regression tests for the supervisor (shepherd-runtime-go-l8t):
// Start must report activation failure instead of silently no-op'ing, and
// Close must join the loop, close Interventions in every case, and stay
// race-free against a concurrent Start.

import (
	"testing"
	"time"

	trace "github.com/buchenberg/goatherd-kernel"
	goatherd "github.com/buchenberg/goatherd-runtime"
	"github.com/buchenberg/goatherd-runtime/internal/rttest"
	"github.com/buchenberg/goatherd-runtime/supervisor"
)

// Start without a bus must return an activation error — not silently
// no-op — and Close must still close Interventions so a consumer ranging
// over the channel does not block forever.
func TestSupervisor_StartWithoutBus_ReturnsErrorAndCloseClosesChannel(t *testing.T) {
	mgr := goatherd.NewScopeManager(rttest.NewMemStore(t))
	sup := supervisor.NewSupervisor(mgr, nil)

	if err := sup.Start("no-bus"); err == nil {
		t.Fatal("expected Start to return an error for a nil bus")
	}
	sup.Close()

	select {
	case iv, ok := <-sup.Interventions():
		if ok {
			t.Fatalf("expected closed channel, got intervention %v", iv)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Interventions not closed 2s after Close on an unactivated supervisor")
	}
}

// Close after a real Start: the loop is joined (no post-Close sends) and
// Interventions closes; buffered interventions remain receivable.
func TestSupervisor_Close_JoinsAndKeepsBufferedReceivable(t *testing.T) {
	store := rttest.NewMemStore(t)
	bus := trace.NewEffectBus(64)
	defer bus.Close()
	mgr := goatherd.NewScopeManager(store)

	scope, err := mgr.Create("sub:close-drain", nil)
	if err != nil {
		t.Fatalf("create scope: %v", err)
	}

	sup := supervisor.NewSupervisor(mgr, bus)
	// The rule blocks inside evaluate until Close has been called, so the
	// intervention send is guaranteed to happen after close(s.done) — the
	// join in Close must cover it.
	processed := make(chan struct{})
	release := make(chan struct{})
	sup.AddRule(supervisor.SupervisionRule{
		Name:  "blocking_rule",
		Match: func(e trace.EffectEvent) bool { return e.KindLabel == "bash" },
		Action: func(e trace.EffectEvent) *supervisor.Intervention {
			close(processed)
			<-release
			return &supervisor.Intervention{
				Type:    supervisor.InterventionDeny,
				ScopeID: "scope:" + e.TraceOwnerID,
			}
		},
	})
	if err := sup.Start("close-drain"); err != nil {
		t.Fatalf("start: %v", err)
	}

	bus.Publish(trace.EffectEvent{
		RecordID:     "sha256:close-drain",
		TraceOwnerID: scope.OwnerID(),
		KindLabel:    "bash",
	})
	<-processed

	closeDone := make(chan struct{})
	go func() { defer close(closeDone); sup.Close() }()

	// Close must be joining the loop (not returned while evaluate is stuck).
	select {
	case <-closeDone:
		t.Fatal("Close returned while a rule was still evaluating in-flight — no join")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	<-closeDone

	// The closed channel still yields the buffered intervention.
	select {
	case iv, ok := <-sup.Interventions():
		if !ok {
			t.Fatal("buffered intervention lost across Close")
		}
		if iv.Type != supervisor.InterventionDeny {
			t.Fatalf("unexpected intervention: %+v", iv)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Interventions never yielded the buffered intervention")
	}
	if iv, ok := <-sup.Interventions(); ok {
		t.Fatalf("expected channel closed after drain, got %v", iv)
	}
}

// Close's read of subscriberID must be synchronized against Start's write.
// Run under -race: any unsynchronized access anywhere in the pair fails
// the run. Exercises the Close-during-Start interleaving repeatedly.
func TestSupervisor_CloseDuringStart_IsRaceFree(t *testing.T) {
	store := rttest.NewMemStore(t)
	bus := trace.NewEffectBus(64)
	defer bus.Close()
	mgr := goatherd.NewScopeManager(store)

	for i := 0; i < 100; i++ {
		sup := supervisor.NewSupervisor(mgr, bus)
		started := make(chan struct{})
		go func() {
			defer close(started)
			_ = sup.Start("race-sub")
		}()
		sup.Close()
		<-started
	}
}
