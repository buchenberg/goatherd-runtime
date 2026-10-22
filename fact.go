package goatherd

import (
	"context"
	"fmt"

	trace "github.com/buchenberg/goatherd-kernel"
)

// AppendFact appends a capture record on this scope's owner path and returns the
// append receipt. It is the runtime's general "record something on this scope"
// seam — the run wrap uses it to persist its execution record (authority
// evidence and outcome).
//
// schemaRef is free-form (the kernel validates only that it is non-empty), so a
// caller can introduce a runtime-level record without a kernel ABI change.
func (s *Scope) AppendFact(ctx context.Context, schemaRef, kindLabel string, payload map[string]any) (trace.AppendReceipt, error) {
	if schemaRef == "" {
		return trace.AppendReceipt{}, fmt.Errorf("AppendFact: schema ref is required")
	}
	if kindLabel == "" {
		return trace.AppendReceipt{}, fmt.Errorf("AppendFact: kind label is required")
	}
	return s.store.Append(ctx, trace.TrustedAppendContext, trace.AppendBatch{
		AppendIntentID: fmt.Sprintf("%s:fact:%d", s.ownerID, nextCheckpointSeq.Add(1)),
		Groups: []trace.AppendGroup{{
			TraceOwnerID: s.ownerID,
			FactDrafts: []trace.RecordDraft{{
				Mode:      trace.Capture,
				SchemaRef: schemaRef,
				KindLabel: kindLabel,
				Payload:   trace.NonNilMap(payload),
			}},
		}},
	})
}
