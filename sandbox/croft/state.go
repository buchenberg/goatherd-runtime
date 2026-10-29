//go:build linux

package croft

import (
	"fmt"

	trace "github.com/buchenberg/goatherd-kernel"

	croftlib "github.com/buchenberg/goatherd-croft"
)

// State field keys inside trace.WorkspaceState.Data.
//
// The kernel type carries a backend-agnostic shape — Backend, Revision and an
// opaque Data map — and its doc names both halves of this mapping: Backend is
// "git", "containerd", ... and Revision is "the git HEAD SHA for the git
// backend, a snapshot key for a snapshot-backed backend". Croft's four-field
// state fits that shape without loss: SnapshotKey is the timeline position,
// and the remaining three are lineage the merge and the image check need.
const (
	// keyParentKey is the snapshot this state's layer was prepared from — the
	// layer lineage.
	keyParentKey = "parent_key"
	// keyBaseKey is the snapshot key of the last state the capturing sandbox
	// had Applied: the merge base ApplyDelta diffs against. Empty means the
	// sandbox never Applied anything, so the state has no merge base and
	// ApplyDelta refuses it.
	keyBaseKey = "base_key"
	// keyImage is the image the workspace was provisioned from.
	keyImage = "image"
	// keyContentDigest is the content-addressed digest of the workspace tree
	// (croft-dlb). It is stable across captures of identical content, so a
	// host can compare content even when the timeline moved and the snapshot
	// key changed.
	keyContentDigest = "content_digest"
)

// toTraceState lifts a croft state into the kernel's workspace state.
//
// Revision carries the snapshot key, so a state's identity survives a round
// trip through the trace exactly as a git state's HEAD does. Digest comes from
// the kernel's own CanonicalDigest, which sorts keys at every nesting level —
// the adapter must not hand-roll a hash. Since croft-dlb the state is
// digest-stable for unchanged content: Capture returns the previous state
// verbatim when nothing changed, so Revision and the lineage fields are equal
// too. ContentDigest rides in Data for hosts that want content equality across
// a moved timeline.
func toTraceState(cs croftlib.WorkspaceState) trace.WorkspaceState {
	return trace.WorkspaceState{
		Backend:  BackendName,
		Revision: cs.SnapshotKey,
		Data: map[string]any{
			keyParentKey:     cs.ParentKey,
			keyBaseKey:       cs.BaseKey,
			keyImage:         cs.Image,
			keyContentDigest: cs.ContentDigest,
		},
	}
}

// fromTraceState lowers a kernel workspace state back to croft's, rejecting a
// state this backend did not produce.
//
// The provenance check mirrors the git backend's (`ws.Backend != "git"`): a
// container Apply against a git state would otherwise reach the daemon with an
// empty snapshot key and fail somewhere opaque. Rejecting at the seam names
// the actual mistake.
//
// Revision is required for the same reason — it is the snapshot key, and an
// Apply without one has nothing to resume from.
func fromTraceState(ws trace.WorkspaceState) (croftlib.WorkspaceState, error) {
	if ws.Backend != BackendName {
		return croftlib.WorkspaceState{}, fmt.Errorf("croft sandbox: cannot use state from backend %q", ws.Backend)
	}
	if ws.Revision == "" {
		return croftlib.WorkspaceState{}, fmt.Errorf("croft sandbox: workspace state is missing a snapshot key")
	}
	return croftlib.WorkspaceState{
		SnapshotKey:   ws.Revision,
		ParentKey:     stringFromData(ws.Data, keyParentKey),
		BaseKey:       stringFromData(ws.Data, keyBaseKey),
		Image:         stringFromData(ws.Data, keyImage),
		ContentDigest: stringFromData(ws.Data, keyContentDigest),
	}, nil
}

// stringFromData reads a string field out of a state's Data map, tolerating a
// nil map and a missing or non-string value as the empty string.
//
// Empty is meaningful rather than an error here: BaseKey is legitimately empty
// on a fresh lineage, and croft is the component that rejects an unmergeable
// state — with a message that explains why. A state arriving from an older
// adapter build with fewer keys degrades the same way instead of failing to
// load at all.
func stringFromData(data map[string]any, key string) string {
	if data == nil {
		return ""
	}
	v, ok := data[key].(string)
	if !ok {
		return ""
	}
	return v
}
