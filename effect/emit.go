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
		// One draft per append, so one fact ID. An empty list means the store
		// accepted the append and reported nothing written, which would leave a
		// bus event pointing at a record nobody can fetch — fail instead.
		if len(receipt.FactIDs) == 0 {
			return fmt.Errorf("effect: append %s (%s) returned no fact ID; refusing to publish an event for a record that cannot be read back", e.Path, e.KindLabel)
		}

		if bus == nil {
			continue
		}
		bus.Publish(trace.EffectEvent{
			RecordID:      receipt.FactIDs[0],
			IntentID:      receipt.AppendIntentID,
			TraceOwnerID:  ownerID,
			Mode:          trace.Capture,
			SchemaRef:     e.SchemaRef,
			KindLabel:     e.KindLabel,
			Payload:       e.Payload(opID),
			CausalParents: causalParents,
			Timestamp:     time.Now(),
		})
	}
	return nil
}
