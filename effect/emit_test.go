package effect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	trace "github.com/buchenberg/goatherd-kernel"
)

// The assertion that *goatherd.Scope satisfies FactAppender lives in
// scope_test.go, in the external test package. It cannot live here: importing
// the runtime root from an in-package test forms a cycle the moment the root
// imports effect, which is the integration this package exists to support.

type recordedFact struct {
	schemaRef string
	kindLabel string
	payload   map[string]any
}

// fakeAppender records what Emit appends, and can fail on demand.
type fakeAppender struct {
	ownerID string
	facts   []recordedFact

	// errOn is the 1-based index of the append that should fail, 0 for none.
	errOn int
	// factIDs overrides the IDs each receipt carries. Nil means the normal
	// single ID; an empty non-nil slice means none, which is how the "exactly
	// one" invariant gets probed from both sides.
	factIDs []string

	seq int
}

func (f *fakeAppender) AppendFact(_ context.Context, schemaRef, kindLabel string, payload map[string]any) (trace.AppendReceipt, error) {
	f.seq++
	f.facts = append(f.facts, recordedFact{schemaRef: schemaRef, kindLabel: kindLabel, payload: payload})
	if f.errOn != 0 && f.seq == f.errOn {
		return trace.AppendReceipt{}, errStore
	}
	receipt := trace.AppendReceipt{AppendIntentID: fmt.Sprintf("intent:%d", f.seq)}
	if f.factIDs != nil {
		receipt.FactIDs = f.factIDs
	} else {
		receipt.FactIDs = []string{fmt.Sprintf("fact:%d", f.seq)}
	}
	return receipt, nil
}

func (f *fakeAppender) OwnerID() string { return f.ownerID }

func sampleEffects() []ChangeEffect {
	return []ChangeEffect{
		{SchemaRef: SchemaFileCreated, KindLabel: LabelFileCreated, Path: "new.txt", Mode: 0o644, Size: 5},
		{SchemaRef: SchemaFilePatched, KindLabel: LabelFilePatched, Path: "old.txt", Mode: 0o600, Size: 9},
		{SchemaRef: SchemaFileDeleted, KindLabel: LabelFileDeleted, Path: "gone.txt"},
	}
}

func drain(ch <-chan trace.EffectEvent, n int) []trace.EffectEvent {
	out := make([]trace.EffectEvent, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, <-ch)
	}
	return out
}

func TestEmit_AppendsAndPublishes(t *testing.T) {
	app := &fakeAppender{ownerID: "sub:agent-1"}
	bus := trace.NewEffectBus(16)
	events := bus.Subscribe("test")

	if err := Emit(context.Background(), app, bus, "run:42", sampleEffects()); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if len(app.facts) != 3 {
		t.Fatalf("appended %d facts, want 3", len(app.facts))
	}
	for i, want := range sampleEffects() {
		if app.facts[i].schemaRef != want.SchemaRef || app.facts[i].kindLabel != want.KindLabel {
			t.Errorf("fact %d = (%q, %q), want (%q, %q)", i,
				app.facts[i].schemaRef, app.facts[i].kindLabel, want.SchemaRef, want.KindLabel)
		}
		if got := app.facts[i].payload["caused_by"]; got != "run:42" {
			t.Errorf("fact %d caused_by = %v, want run:42", i, got)
		}
	}

	got := drain(events, 3)
	for i, ev := range got {
		wantID := fmt.Sprintf("fact:%d", i+1)
		if ev.RecordID != wantID {
			t.Errorf("event %d RecordID = %q, want %q — an event must point at the record that was written", i, ev.RecordID, wantID)
		}
		if ev.IntentID != fmt.Sprintf("intent:%d", i+1) {
			t.Errorf("event %d IntentID = %q", i, ev.IntentID)
		}
		if ev.TraceOwnerID != "sub:agent-1" {
			t.Errorf("event %d TraceOwnerID = %q, want the appender's owner path", i, ev.TraceOwnerID)
		}
		if ev.Mode != trace.Capture {
			t.Errorf("event %d Mode = %v, want Capture — these are observations, not declarations", i, ev.Mode)
		}
		if len(ev.CausalParents) != 1 || ev.CausalParents[0] != "run:42" {
			t.Errorf("event %d CausalParents = %v, want [run:42]", i, ev.CausalParents)
		}
		if ev.Timestamp.IsZero() {
			t.Errorf("event %d has no timestamp", i)
		}
		if ev.SchemaRef != app.facts[i].schemaRef || ev.KindLabel != app.facts[i].kindLabel {
			t.Errorf("event %d disagrees with the record it was published for", i)
		}
	}
}

func TestEmit_NoCauseOmitsTheField(t *testing.T) {
	// An empty opID means no known cause. It must be omitted rather than
	// recorded as an empty string, so a consumer can distinguish the two — and
	// an empty causal parent would be a dangling edge in the graph.
	app := &fakeAppender{ownerID: "sub:x"}
	bus := trace.NewEffectBus(8)
	events := bus.Subscribe("t")

	if err := Emit(context.Background(), app, bus, "", sampleEffects()[:1]); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if _, ok := app.facts[0].payload["caused_by"]; ok {
		t.Errorf("caused_by must be absent with no opID, got %#v", app.facts[0].payload["caused_by"])
	}
	ev := drain(events, 1)[0]
	if len(ev.CausalParents) != 0 {
		t.Errorf("CausalParents = %v, want none", ev.CausalParents)
	}
}

func TestEmit_NilBusAppendsOnly(t *testing.T) {
	// A batch run or a test with no live supervisor still needs the durable
	// record; publishing is optional, appending is not.
	app := &fakeAppender{ownerID: "sub:x"}
	if err := Emit(context.Background(), app, nil, "run:1", sampleEffects()); err != nil {
		t.Fatalf("Emit with a nil bus: %v", err)
	}
	if len(app.facts) != 3 {
		t.Errorf("appended %d facts, want 3", len(app.facts))
	}
}

func TestEmit_EmptyEffectsIsANoOp(t *testing.T) {
	app := &fakeAppender{ownerID: "sub:x"}
	bus := trace.NewEffectBus(4)
	events := bus.Subscribe("t")

	if err := Emit(context.Background(), app, bus, "run:1", nil); err != nil {
		t.Fatalf("Emit with no effects: %v", err)
	}
	if len(app.facts) != 0 {
		t.Errorf("appended %d facts, want 0", len(app.facts))
	}
	select {
	case ev := <-events:
		t.Errorf("published an event for no effects: %+v", ev)
	default:
	}
}

func TestEmit_NilAppenderIsAnError(t *testing.T) {
	if err := Emit(context.Background(), nil, nil, "run:1", sampleEffects()); err == nil {
		t.Fatal("Emit with no appender must fail")
	}
}

func TestEmit_AppendFailureStopsAndNamesThePath(t *testing.T) {
	// A failure part way through leaves earlier effects recorded, which is
	// honest as long as the error says what happened. Continuing would risk
	// publishing events for records that were never written.
	app := &fakeAppender{ownerID: "sub:x", errOn: 2}
	bus := trace.NewEffectBus(8)
	events := bus.Subscribe("t")

	err := Emit(context.Background(), app, bus, "run:1", sampleEffects())
	if err == nil {
		t.Fatal("Emit must propagate an append failure")
	}
	if !strings.Contains(err.Error(), "old.txt") {
		t.Errorf("the error must name the path that failed, got: %v", err)
	}
	if !errors.Is(err, errStore) {
		t.Errorf("the underlying cause must survive wrapping: %v", err)
	}
	if len(app.facts) != 2 {
		t.Errorf("appended %d facts, want 2 (the failed one is recorded as attempted)", len(app.facts))
	}
	// Only the first effect may have been published — the second's append
	// failed, so no event may point at a record that does not exist.
	published := 0
	for {
		select {
		case <-events:
			published++
			continue
		default:
		}
		break
	}
	if published != 1 {
		t.Errorf("published %d events, want exactly 1 — an event must never point at an unwritten record", published)
	}
}

func TestEmit_RequiresExactlyOneFactID(t *testing.T) {
	// Emit appends one draft per effect, so a conforming appender returns one
	// fact ID. Both deviations fail closed rather than publishing an event that
	// does not correspond to one readable record:
	//
	//   - zero IDs means the store accepted the append and reported nothing
	//     written, so the event would point at a record nobody can fetch;
	//   - several means the extras would be silently absent from the bus while
	//     the code claimed a single record was written.
	//
	// FactAppender is a generic seam, so an implementation could legitimately
	// return more than one — which is exactly why the count is checked for
	// equality rather than merely non-emptiness.
	cases := []struct {
		name string
		ids  []string
	}{
		{"none", []string{}},
		{"several", []string{"fact:a", "fact:b", "fact:c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &fakeAppender{ownerID: "sub:x", factIDs: tc.ids}
			bus := trace.NewEffectBus(8)
			events := bus.Subscribe("t")

			err := Emit(context.Background(), app, bus, "run:1", sampleEffects()[:1])
			if err == nil {
				t.Fatalf("a receipt with %d fact IDs must be an error", len(tc.ids))
			}
			if !strings.Contains(err.Error(), "want exactly 1") {
				t.Errorf("the error must state the invariant that broke, got: %v", err)
			}
			if !strings.Contains(err.Error(), "new.txt") {
				t.Errorf("the error must name the path, got: %v", err)
			}
			select {
			case ev := <-events:
				t.Errorf("published an event despite the bad receipt: %+v", ev)
			default:
			}
		})
	}
}

func TestEmit_AppendsBeforePublishing(t *testing.T) {
	// The ordering is the invariant: a supervisor may fetch the record the
	// moment it sees the event, so the record must already exist.
	bus := trace.NewEffectBus(8)
	events := bus.Subscribe("t")
	app := &orderingAppender{events: events}

	if err := Emit(context.Background(), app, bus, "run:1", sampleEffects()[:1]); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if app.publishBeforeAppend {
		t.Error("the bus event was published before the fact was appended — a supervisor could fetch a record that does not exist yet")
	}
	ev := drain(events, 1)[0]
	if ev.RecordID != app.lastRecordedID {
		t.Errorf("event RecordID %q does not match the appended record %q", ev.RecordID, app.lastRecordedID)
	}
}

// orderingAppender observes the invariant directly rather than inferring it: at
// append time it checks the bus, and finding an event already waiting means Emit
// published before the record was written.
type orderingAppender struct {
	events              <-chan trace.EffectEvent
	publishBeforeAppend bool
	lastRecordedID      string
}

func (o *orderingAppender) AppendFact(_ context.Context, _, _ string, _ map[string]any) (trace.AppendReceipt, error) {
	select {
	case <-o.events:
		o.publishBeforeAppend = true
	default:
	}
	o.lastRecordedID = "fact:1"
	return trace.AppendReceipt{AppendIntentID: "intent:1", FactIDs: []string{"fact:1"}}, nil
}

func (o *orderingAppender) OwnerID() string { return "sub:x" }

func TestEmit_EventsDoNotShareCausalParents(t *testing.T) {
	// The bus hands each subscriber a copy of EffectEvent, but a copy of a
	// struct containing a slice copies the header, not the backing array. If
	// every event in one Emit shared a single array, a consumer writing
	// CausalParents[0] in place would corrupt the other events — and race any
	// subscriber reading them concurrently, since the bus fans out.
	app := &fakeAppender{ownerID: "sub:x"}
	bus := trace.NewEffectBus(8)
	events := bus.Subscribe("t")

	if err := Emit(context.Background(), app, bus, "run:1", sampleEffects()); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	got := drain(events, 3)
	for i := range got {
		if len(got[i].CausalParents) != 1 || got[i].CausalParents[0] != "run:1" {
			t.Fatalf("event %d CausalParents = %v, want [run:1]", i, got[i].CausalParents)
		}
	}

	// The exact write an aliasing implementation would let leak.
	got[0].CausalParents[0] = "corrupted"
	for i := 1; i < len(got); i++ {
		if got[i].CausalParents[0] != "run:1" {
			t.Errorf("event %d CausalParents became %q after mutating event 0 in place — the events share one backing array", i, got[i].CausalParents[0])
		}
	}
}

var errStore = errors.New("store unavailable")
