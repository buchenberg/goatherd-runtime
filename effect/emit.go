package effect

import (
	"context"
	"fmt"
	"time"

	trace "github.com/buchenberg/goatherd-kernel"
)

// FactAppender is the seam Emit needs: somewhere to append a capture record,
// and the owner path it landed on.
//
// It is an interface rather than the concrete *goatherd.Scope for two reasons.
// It keeps this package from importing the runtime root, so effect stays a leaf
// that anything can depend on without a cycle. And it makes Emit testable
// against a recorder instead of a SQLite store. *goatherd.Scope satisfies it
// with the methods it already exports.
type FactAppender interface {
	// AppendFact records one capture fact and returns the receipt.
	AppendFact(ctx context.Context, schemaRef, kindLabel string, payload map[string]any) (trace.AppendReceipt, error)
	// OwnerID is the trace owner path the fact is appended under, and the
	// TraceOwnerID published on the bus so a supervisor can attribute the event.
	OwnerID() string
}

// Emit records each effect as a trace fact and publishes it on the bus for live
// supervision.
//
// The two halves answer different questions, and the kernel's split between
// them is deliberate: the trace store is the durable record for later analysis,
// the bus is transient for real-time observation. A supervisor that misses an
// event because its buffer filled can still read the fact from the store, so
// Emit appends first and publishes only once the record exists — an event must
// never point at a record that was not written.
//
// opID is the operation the effects are attributed to: the run or tool call that
// caused them. It becomes the payload's caused_by and the bus event's sole
// causal parent, which is what makes "why did this file change" answerable. An
// empty opID is allowed and means no known cause; it is then omitted from the
// payload and leaves CausalParents empty rather than recording a blank parent.
//
// A nil bus appends without publishing, which is the right mode for a batch or
// test run with no live supervisor attached.
//
// The appender must be non-nil, and "non-nil" means a real value: because
// FactAppender is an interface, a typed nil (an unassigned *goatherd.Scope
// passed as the parameter) satisfies the nil check below and then panics when
// its method is called. Detecting that would need reflection over every
// possible underlying kind, which is not a trade worth making for a programming
// error — so callers own it, as they do for every interface parameter in Go.
//
// Emission is not atomic: each effect is its own append, so a failure part way
// through leaves the earlier effects recorded and returns the error. That is
// preferable to the alternatives — swallowing the failure would leave a silent
// hole in the trace, and there is no multi-fact append on this seam. The error
// names the path that failed so a caller can tell what was and was not
// recorded.
func Emit(ctx context.Context, appender FactAppender, bus *trace.EffectBus, opID string, effects []ChangeEffect) error {
	if appender == nil {
		return fmt.Errorf("effect: Emit requires a FactAppender")
	}
	if len(effects) == 0 {
		return nil
	}

	ownerID := appender.OwnerID()
	var causalParents []string
	if opID != "" {
		causalParents = []string{opID}
	}

	for _, e := range effects {
		receipt, err := appender.AppendFact(ctx, e.SchemaRef, e.KindLabel, e.Payload(opID))
		if err != nil {
			return fmt.Errorf("effect: append %s (%s): %w", e.Path, e.KindLabel, err)
		}
		// Exactly one fact ID, because this seam appends exactly one draft per
		// effect. The check is an equality and not an emptiness test on purpose:
		// FactAppender is generic, and an implementation that returned several
		// IDs would otherwise have its extra records silently absent from the
		// bus while this code claimed one record was written. Zero IDs is the
		// same class of failure — an event pointing at a record nobody can
		// fetch back — so both fail closed.
		if len(receipt.FactIDs) != 1 {
			return fmt.Errorf("effect: append %s (%s) returned %d fact IDs, want exactly 1; refusing to publish an event that does not correspond to one readable record", e.Path, e.KindLabel, len(receipt.FactIDs))
		}

		if bus == nil {
			continue
		}
		bus.Publish(trace.EffectEvent{
			RecordID:     receipt.FactIDs[0],
			IntentID:     receipt.AppendIntentID,
			TraceOwnerID: ownerID,
			Mode:         trace.Capture,
			SchemaRef:    e.SchemaRef,
			KindLabel:    e.KindLabel,
			// A fresh payload per event, not the one passed to AppendFact.
			// Sharing a map between the durable record and the bus event would
			// let a consumer mutating the event corrupt what the store holds.
			Payload: e.Payload(opID),
			// Copied per event. The bus fans out to subscribers that may run
			// concurrently, and EffectEvent is copied by value — the slice
			// header is copied but the backing array is not. Aliasing one array
			// across every event would let a consumer writing CausalParents[0]
			// in place corrupt the others and race the subscribers reading them.
			CausalParents: append([]string(nil), causalParents...),
			Timestamp:     time.Now(),
		})
	}
	return nil
}
