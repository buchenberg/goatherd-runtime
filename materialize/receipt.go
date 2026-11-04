package materialize

// The materialize receipt (d7c): the kernel materialize transition receipt
// returned to callers and stored in the kernel's completed-intent ledger.
// It and the outcome vocabulary it borrows (substrate.MaterializationOutcome)
// moved here from the kernel trace package, where they existed only for the
// orchestrator's sake; the kernel now keeps no materialization vocabulary of
// its own and exposes the ledger only as opaque get/put over receipt_json
// bytes.

import (
	"encoding/json"
	"fmt"
	"strings"

	trace "github.com/buchenberg/goatherd-kernel"
	substrate "github.com/buchenberg/goatherd-runtime/substrate"
)

// isValidOutcome reports whether an outcome is one of the three vocabulary
// values. The dispatch rejects anything else before an append or ledger
// write, so the ledger never stores an unknown outcome.
func isValidOutcome(o substrate.MaterializationOutcome) bool {
	switch o {
	case substrate.MaterializationSuccess, substrate.MaterializationCleanFailure, substrate.MaterializationSplitState:
		return true
	}
	return false
}

// isCleanFailure reports whether an outcome is the clean_failure value.
func isCleanFailure(o substrate.MaterializationOutcome) bool {
	return o == substrate.MaterializationCleanFailure
}

// OpMaterialize is the reserved operation kind the materialize orchestrator
// requires (plan 03): Materialize accepts an OperationContext and rejects any
// context whose Operation is not this kind. It moved here from the kernel's
// trace package (d7c) — the append/read entry points derive their own
// operation kinds, so this one is only presentable through Materialize.
const OpMaterialize = trace.OperationKind("materialize")

// MaterializationReceipt is the materialize transition receipt handed back to
// callers and stored in the kernel's completed-intent ledger. Its field set
// mirrors Python's MaterializationReceipt exactly, because the ledger's
// receipt_json is serialized from it and must stay cross-readable with the
// Python reference.
type MaterializationReceipt struct {
	Outcome           substrate.MaterializationOutcome
	SubstrateRef      string
	TargetRecordIDs   []string
	ProducedRecordIDs []string
	FailureReason     string
	WorldSideAnchors  []map[string]any
}

// receiptJSON serializes a receipt for the ledger the way Python's
// _receipt_to_json does: sorted keys, compact separators, the ASCII flavour.
func receiptJSON(receipt MaterializationReceipt) (string, error) {
	body, err := trace.CanonicalJSONASCIIBytes(map[string]any{
		"outcome":             string(receipt.Outcome),
		"substrate_ref":       receipt.SubstrateRef,
		"target_record_ids":   trace.NonNilStrings(receipt.TargetRecordIDs),
		"produced_record_ids": trace.NonNilStrings(receipt.ProducedRecordIDs),
		"failure_reason":      receipt.FailureReason,
		"world_side_anchors":  nonNilAnchors(receipt.WorldSideAnchors),
	})
	if err != nil {
		return "", fmt.Errorf("materialization receipt encode: %w", err)
	}
	return string(body), nil
}

// receiptFromJSON decodes a stored ledger receipt.
func receiptFromJSON(payload string) (MaterializationReceipt, error) {
	var stored struct {
		Outcome           string           `json:"outcome"`
		SubstrateRef      string           `json:"substrate_ref"`
		TargetRecordIDs   []string         `json:"target_record_ids"`
		ProducedRecordIDs []string         `json:"produced_record_ids"`
		FailureReason     string           `json:"failure_reason"`
		WorldSideAnchors  []map[string]any `json:"world_side_anchors"`
	}
	// UseNumber, matching retained-body reads: without it a numeric anchor
	// value would decode as float64 while a fresh receipt (and Python's
	// json.loads) holds the exact number, so the replay comparison in the
	// orchestrator would silently stop holding.
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&stored); err != nil {
		return MaterializationReceipt{}, fmt.Errorf("materialization receipt decode: %w", err)
	}
	// Replay is the idempotency commit point: a stored receipt whose outcome
	// drifted out of the vocabulary (or was never written) must fail the
	// replay loudly instead of surfacing zero values to the caller.
	if !isValidOutcome(substrate.MaterializationOutcome(stored.Outcome)) {
		return MaterializationReceipt{}, fmt.Errorf("materialization receipt decode: unknown outcome %q", stored.Outcome)
	}
	return MaterializationReceipt{
		Outcome:           substrate.MaterializationOutcome(stored.Outcome),
		SubstrateRef:      stored.SubstrateRef,
		TargetRecordIDs:   trace.NonNilStrings(stored.TargetRecordIDs),
		ProducedRecordIDs: trace.NonNilStrings(stored.ProducedRecordIDs),
		FailureReason:     stored.FailureReason,
		WorldSideAnchors:  nonNilAnchors(stored.WorldSideAnchors),
	}, nil
}

// nonNilAnchors returns an empty (non-nil) slice for nil input, so a receipt
// without anchors serializes as [] rather than null, matching Python's tuple.
func nonNilAnchors(anchors []map[string]any) []map[string]any {
	if anchors == nil {
		return []map[string]any{}
	}
	return anchors
}
