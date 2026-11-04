package materialize

// Receipt wire-format pins (d7c review): the ledger's receipt_json is
// cross-language contract surface (Python's _receipt_to_json and the Go
// orchestrator must agree byte-for-byte), so the exact canonical bytes are
// pinned here — struct-equality tests alone would pass through a key-spelling
// or null-vs-[] drift.

import (
	"encoding/json"
	"reflect"
	"testing"

	substrate "github.com/buchenberg/goatherd-runtime/substrate"
)

func goldenReceipt() MaterializationReceipt {
	return MaterializationReceipt{
		Outcome:           substrate.MaterializationSuccess,
		SubstrateRef:      "test.golden.v1",
		TargetRecordIDs:   []string{"decl:1", "decl:2"},
		ProducedRecordIDs: []string{"cap:1"},
		FailureReason:     "",
		// json.Number anchors, the shape read paths produce (UseNumber) and
		// the shape Python's json.loads preserves.
		WorldSideAnchors: []map[string]any{{"call": json.Number("1")}},
	}
}

const goldenReceiptJSON = `{"failure_reason":"","outcome":"success","produced_record_ids":["cap:1"],"substrate_ref":"test.golden.v1","target_record_ids":["decl:1","decl:2"],"world_side_anchors":[{"call":1}]}`

func TestMaterializationReceiptJSONBytesPinned(t *testing.T) {
	got, err := receiptJSON(goldenReceipt())
	if err != nil {
		t.Fatalf("receiptJSON: %v", err)
	}
	if got != goldenReceiptJSON {
		t.Errorf("receipt bytes drifted:\n got  %s\n want %s", got, goldenReceiptJSON)
	}
}

func TestMaterializationReceiptRoundTrip(t *testing.T) {
	payload, err := receiptJSON(goldenReceipt())
	if err != nil {
		t.Fatalf("receiptJSON: %v", err)
	}
	back, err := receiptFromJSON(payload)
	if err != nil {
		t.Fatalf("receiptFromJSON: %v", err)
	}
	// The empty-slice normalization makes the decoded form equal the fresh
	// one under reflect.DeepEqual (anchors [] vs nil are normalized away).
	if !reflect.DeepEqual(back, goldenReceipt()) {
		t.Errorf("replay receipt = %+v, want %+v", back, goldenReceipt())
	}
}

func TestMaterializationReceiptDecodeRejectsDrift(t *testing.T) {
	for name, payload := range map[string]string{
		"empty outcome":   `{"outcome":"","substrate_ref":"x"}`,
		"unknown outcome": `{"outcome":"partially_applied","substrate_ref":"x"}`,
		"unknown field":   `{"outcome":"success","substrate_ref":"x","extra":1}`,
		"not json":        `not json at all`,
	} {
		if _, err := receiptFromJSON(payload); err == nil {
			t.Errorf("%s: receiptFromJSON accepted %q", name, payload)
		}
	}
}
