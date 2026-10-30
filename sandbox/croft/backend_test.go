//go:build linux

package croft

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	trace "github.com/buchenberg/goatherd-kernel"
	sbx "github.com/buchenberg/goatherd-runtime/sandbox"

	croftlib "github.com/buchenberg/goatherd-croft"
)

const testImage = "docker.io/library/golang:1.27"

func newTestSandbox(t *testing.T) (*Sandbox, *fakeCroft) {
	t.Helper()
	fake := newFakeCroft(testImage)
	return newWithInner(Config{Image: testImage}, fake), fake
}

func TestNew_RequiresImage(t *testing.T) {
	// A container with no base image has no filesystem, so the mistake is
	// reported at construction naming the field, not as an opaque daemon
	// failure at Create mid-session.
	_, err := New(Config{})
	if err == nil {
		t.Fatal("New with an empty Image must fail")
	}
	if !strings.Contains(err.Error(), "Config.Image") {
		t.Errorf("error must name the missing field, got: %v", err)
	}
}

func TestNew_LowersConfigOntoCroft(t *testing.T) {
	// The adapter Config exists so hosts never import croft types; this pins
	// that the lowering is field-for-field, so adding a field to one side
	// without the other is a test failure rather than a silent drop.
	got := toCroftConfig(Config{
		Address:         "/run/containerd/containerd.sock",
		Namespace:       "yaah",
		Image:           testImage,
		Runtime:         "io.containerd.runc.v2",
		Snapshotter:     "overlayfs",
		InitArgs:        []string{"/bin/sh", "-c", "sleep infinity"},
		Workdir:         "/work",
		WriteChunkBytes: 4096,
		MaxOutputBytes:  1 << 20,
		FIFODir:         "/tmp/fifo",
		Network:         "none",
		StagingDir:      "/tmp/staging",
		Limits:          &Limits{CPUShares: 1024, CPUQuota: 50000, MemoryBytes: 1 << 30, Pids: 256},
		Secrets:         []SecretMount{{Source: "/host/token", Destination: "/run/token"}},
	})

	want := croftlib.Config{
		Address:         "/run/containerd/containerd.sock",
		Namespace:       "yaah",
		Image:           testImage,
		Runtime:         "io.containerd.runc.v2",
		Snapshotter:     "overlayfs",
		InitArgs:        []string{"/bin/sh", "-c", "sleep infinity"},
		Workdir:         "/work",
		WriteChunkBytes: 4096,
		MaxOutputBytes:  1 << 20,
		FIFODir:         "/tmp/fifo",
		Network:         "none",
		StagingDir:      "/tmp/staging",
		Limits:          &croftlib.Limits{CPUShares: 1024, CPUQuota: 50000, MemoryBytes: 1 << 30, Pids: 256},
		Secrets:         []croftlib.SecretMount{{Source: "/host/token", Destination: "/run/token"}},
	}
	if !configEqual(got, want) {
		t.Errorf("Config lowering lost or changed a field:\n got %+v\nwant %+v", got, want)
	}
}

func TestNew_NilLimitsStaysNil(t *testing.T) {
	// Nil Limits means unbounded in croft; substituting a zero-valued struct
	// would be equivalent in effect but would hide the caller's intent, and a
	// non-nil pointer to zero fields reads as "explicitly configured".
	got := toCroftConfig(Config{Image: testImage})
	if got.Limits != nil {
		t.Errorf("nil Limits must stay nil, got %+v", got.Limits)
	}
	if got.Secrets != nil {
		t.Errorf("empty Secrets must stay nil, got %+v", got.Secrets)
	}
}

func TestSandbox_Backend(t *testing.T) {
	s, _ := newTestSandbox(t)
	if got := s.Backend(); got != "containerd" {
		t.Errorf("Backend() = %q, want %q", got, "containerd")
	}
	if BackendName != "containerd" {
		t.Errorf("BackendName = %q, want %q — the kernel names containerd as the container backend identifier", BackendName, "containerd")
	}
}

func TestSandbox_Capabilities(t *testing.T) {
	// Capabilities must be honest: a container provisions real resources,
	// executes, does file IO, diffs, and is isolated from the host tree —
	// but it enforces nothing at the syscall level, so Containment is
	// contained and never full. Claiming the syscall tier here is exactly the
	// dishonesty the sandbox package forbids.
	s, _ := newTestSandbox(t)
	got := s.Capabilities()

	want := sbx.SandboxCapabilities{
		Lifecycle:   true,
		Exec:        true,
		FileIO:      true,
		Diff:        true,
		Isolated:    true,
		Containment: trace.ContainContained,
	}
	if got != want {
		t.Errorf("Capabilities() = %+v, want %+v", got, want)
	}
	if got.Containment == trace.ContainFull {
		t.Error("a container boundary must never claim ContainFull: a kernel exploit escapes it")
	}
}

func TestSandbox_CreateDestroy(t *testing.T) {
	s, fake := newTestSandbox(t)
	ctx := context.Background()

	if err := s.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !fake.created {
		t.Error("Create did not reach croft")
	}
	if err := s.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !fake.gone {
		t.Error("Destroy did not reach croft")
	}
	if got := strings.Join(fake.calls, ","); got != "create,destroy" {
		t.Errorf("calls = %s, want create,destroy", got)
	}
}

func TestSandbox_CreateHonorsSpecTimeout(t *testing.T) {
	// A first run may pull an image, so spec.Timeout must actually bound the
	// call rather than be ignored.
	old := createCeiling
	createCeiling = time.Hour
	t.Cleanup(func() { createCeiling = old })

	s, fake := newTestSandbox(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	start := time.Now()
	if err := s.Create(ctx, sbx.SandboxSpec{Timeout: 30 * time.Second}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if fake.lastCreateDL.IsZero() {
		t.Fatal("Create must bound the context when spec.Timeout is set")
	}
	if got := fake.lastCreateDL.Sub(start); got < 29*time.Second || got > 31*time.Second {
		t.Errorf("deadline was %v out, want the caller's 30s (not the %v ceiling)", got, createCeiling)
	}
}

func TestSandbox_CreateBoundedByDefaultCeiling(t *testing.T) {
	// The Sandbox contract: spec.Timeout of zero means "the backend default",
	// and a backend must impose one so a caller passing context.Background()
	// cannot hang indefinitely. Create is the verb most exposed to that — a
	// first run may pull an image from an unreachable registry.
	old := createCeiling
	createCeiling = 200 * time.Millisecond
	t.Cleanup(func() { createCeiling = old })

	s, fake := newTestSandbox(t)
	start := time.Now()
	if err := s.Create(context.Background(), sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if fake.lastCreateDL.IsZero() {
		t.Fatal("Create must bound the context even when spec.Timeout is zero")
	}
	if got := fake.lastCreateDL.Sub(start); got < 100*time.Millisecond || got > 400*time.Millisecond {
		t.Errorf("deadline was %v out, want roughly the %v ceiling", got, createCeiling)
	}
}

func TestSandbox_CreateIgnoresSpecWorkdir(t *testing.T) {
	// Documented behaviour, asserted so a future change is deliberate: the
	// container's root is fixed by Config at construction. SandboxSpec.Workdir
	// is "ignored by backends that infer their own".
	s, _ := newTestSandbox(t)
	if err := s.Create(context.Background(), sbx.SandboxSpec{Workdir: "/elsewhere"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.cfg.Workdir != "" {
		t.Errorf("Config.Workdir must be unchanged by Create, got %q", s.cfg.Workdir)
	}
}

func TestSandbox_CreatePropagatesError(t *testing.T) {
	s, fake := newTestSandbox(t)
	fake.createErr = errors.New("daemon unreachable")
	if err := s.Create(context.Background(), sbx.SandboxSpec{}); err == nil {
		t.Fatal("Create must propagate a croft failure")
	}
}

func TestSandbox_CaptureStateMapping(t *testing.T) {
	// The kernel type was designed for this shape: Backend for provenance,
	// Revision for the timeline position (its doc names "a snapshot key for a
	// snapshot-backed backend"), Data for the rest of the lineage.
	s, _ := newTestSandbox(t)
	ctx := context.Background()
	if err := s.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ws, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if ws.Backend != BackendName {
		t.Errorf("Backend = %q, want %q", ws.Backend, BackendName)
	}
	if ws.Revision != "croft-seq-1" {
		t.Errorf("Revision = %q, want the snapshot key croft-seq-1", ws.Revision)
	}
	if got := ws.Data["parent_key"]; got != "croft-base" {
		t.Errorf("Data[parent_key] = %v, want croft-base", got)
	}
	if got := ws.Data["image"]; got != testImage {
		t.Errorf("Data[image] = %v, want %v", got, testImage)
	}
	if _, ok := ws.Data["base_key"]; !ok {
		t.Error("Data must carry base_key even when empty, so the merge base round-trips")
	}
}

func TestSandbox_CaptureDigestIsDeterministic(t *testing.T) {
	// Given one state, the digest must be stable and map-order independent —
	// the kernel's CanonicalDigest sorts keys at every nesting level. This is
	// the property the adapter relies on instead of hand-rolling a hash.
	s, _ := newTestSandbox(t)
	ctx := context.Background()
	if err := s.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ws, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	first, err := ws.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := ws.Digest()
		if err != nil {
			t.Fatalf("Digest: %v", err)
		}
		if again != first {
			t.Fatalf("digest is not stable across calls: %q then %q", first, again)
		}
	}
	// A re-ordered Data map must digest identically.
	shuffledData := make(map[string]any, len(ws.Data))
	for _, k := range []string{"parent_key", "image", "base_key"} {
		shuffledData[k] = ws.Data[k]
	}
	shuffled := trace.WorkspaceState{
		Backend:  ws.Backend,
		Revision: ws.Revision,
		Data:     shuffledData,
	}
	got, err := shuffled.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if got != first {
		t.Errorf("digest must not depend on map iteration order: %q vs %q", got, first)
	}
}

// TestSandbox_CaptureDoesNotCarryContentDigest pins the deliberate omission:
// the adapter does not force croft's per-capture full-tree diff on, because no
// consumer reads the field and the kernel digest includes Revision anyway.
func TestSandbox_CaptureDoesNotCarryContentDigest(t *testing.T) {
	s, _ := newTestSandbox(t)
	ctx := context.Background()
	if err := s.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ws, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, ok := ws.Data["content_digest"]; ok {
		t.Fatalf("state Data carries content_digest = %v, want the adapter not to map it", ws.Data["content_digest"])
	}
	back, err := fromTraceState(ws)
	if err != nil {
		t.Fatalf("fromTraceState: %v", err)
	}
	if back.ContentDigest != "" {
		t.Fatalf("ContentDigest = %q, want empty", back.ContentDigest)
	}
}

// TestSandbox_CaptureUnchangedIsStable pins the adapter end of croft-dlb: with
// a fake that models the no-change fast path, two captures of unchanged
// content digest equally, and a write breaks the equality.
func TestSandbox_CaptureUnchangedIsStable(t *testing.T) {
	s, _ := newTestSandbox(t)
	ctx := context.Background()
	if err := s.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	first, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("first Capture: %v", err)
	}
	second, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("second Capture: %v", err)
	}
	d1, err := first.Digest()
	if err != nil {
		t.Fatalf("Digest first: %v", err)
	}
	d2, err := second.Digest()
	if err != nil {
		t.Fatalf("Digest second: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("unchanged content must digest equally: %s vs %s", d1, d2)
	}

	if err := s.WriteFile(ctx, "/workspace/changed.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	third, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("third Capture: %v", err)
	}
	d3, err := third.Digest()
	if err != nil {
		t.Fatalf("Digest third: %v", err)
	}
	if d3 == d1 {
		t.Fatal("a changed tree must not digest like the unchanged one")
	}
}

func TestSandbox_CapturePropagatesError(t *testing.T) {
	s, fake := newTestSandbox(t)
	fake.captureErr = errors.New("commit failed")
	if _, err := s.Capture(context.Background()); err == nil {
		t.Fatal("Capture must propagate a croft failure")
	}
}

func TestSandbox_ApplyRoundTripsState(t *testing.T) {
	// A captured state must survive the lift into trace.WorkspaceState and the
	// lowering back, or Apply would reach the daemon with the wrong key.
	s, fake := newTestSandbox(t)
	ctx := context.Background()
	if err := s.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.WriteFile(ctx, "a.txt", []byte("one"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ws, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// Mutate, then roll back through the seam.
	if err := s.WriteFile(ctx, "a.txt", []byte("two"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := s.Apply(ctx, ws); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if fake.applied.SnapshotKey != ws.Revision {
		t.Errorf("Apply reached croft with key %q, want %q", fake.applied.SnapshotKey, ws.Revision)
	}
	if fake.applied.Image != testImage {
		t.Errorf("Apply lost the image lineage: %q", fake.applied.Image)
	}
	got, err := s.ReadFile(ctx, "a.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "one" {
		t.Errorf("after Apply the file is %q, want %q", got, "one")
	}
}

func TestSandbox_ApplyRejectsForeignBackend(t *testing.T) {
	// Mirrors the git backend's provenance check: a container Apply against a
	// git state would otherwise reach the daemon with an empty snapshot key and
	// fail somewhere opaque.
	s, _ := newTestSandbox(t)
	err := s.Apply(context.Background(), trace.WorkspaceState{Backend: "git", Revision: "abc123"})
	if err == nil {
		t.Fatal("Apply must reject a state from another backend")
	}
	if !strings.Contains(err.Error(), `"git"`) {
		t.Errorf("error must name the offending backend, got: %v", err)
	}
}

func TestSandbox_ApplyRejectsMissingRevision(t *testing.T) {
	s, _ := newTestSandbox(t)
	err := s.Apply(context.Background(), trace.WorkspaceState{Backend: BackendName})
	if err == nil {
		t.Fatal("Apply must reject a state with no snapshot key")
	}
	if !strings.Contains(err.Error(), "snapshot key") {
		t.Errorf("error must name the missing field, got: %v", err)
	}
}

func TestSandbox_ApplyToleratesMissingDataKeys(t *testing.T) {
	// A state recorded by an older adapter build, or one whose Data was
	// stripped in transit, must still lower: BaseKey is legitimately empty on a
	// fresh lineage and croft is what rejects an unmergeable state, with a
	// message explaining why.
	s, fake := newTestSandbox(t)
	ctx := context.Background()
	if err := s.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ws, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	bare := trace.WorkspaceState{Backend: BackendName, Revision: ws.Revision}
	if err := s.Apply(ctx, bare); err != nil {
		t.Fatalf("Apply with nil Data: %v", err)
	}
	if fake.applied.Image != "" {
		t.Errorf("a state with no Data must lower to an empty image, got %q", fake.applied.Image)
	}
}

func TestSandbox_FileIO(t *testing.T) {
	s, _ := newTestSandbox(t)
	ctx := context.Background()

	if err := s.WriteFile(ctx, "dir/f.txt", []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := s.ReadFile(ctx, "dir/f.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("ReadFile = %q, want %q", got, "hello")
	}

	_, err = s.ReadFile(ctx, "missing.txt")
	if err == nil {
		t.Fatal("ReadFile of a missing path must fail")
	}
}

func TestSandbox_ReleaseState(t *testing.T) {
	// ReleaseState is the discard verb. It sits outside sbx.Sandbox, found by
	// type assertion, so settlement's discard path can drop a rejected
	// proposal's lease instead of pinning the layer forever.
	s, fake := newTestSandbox(t)
	ctx := context.Background()
	if err := s.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ws, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	var releaser interface {
		ReleaseState(context.Context, trace.WorkspaceState) error
	} = s
	if err := releaser.ReleaseState(ctx, ws); err != nil {
		t.Fatalf("ReleaseState: %v", err)
	}
	if len(fake.releasedStates) != 1 || fake.releasedStates[0].SnapshotKey != ws.Revision {
		t.Errorf("ReleaseState reached croft with %+v, want key %q", fake.releasedStates, ws.Revision)
	}
	// A released state is no longer applyable.
	if err := s.Apply(ctx, ws); err == nil {
		t.Error("Apply of a released state must fail")
	}
}

func TestSandbox_ApplyDeltaPropagatesConflict(t *testing.T) {
	// Settlement's apply verb must fail closed on a conflict rather than
	// synthesize a merge, so croft's sentinel has to survive the adapter's
	// error wrapping for errors.Is at the call site.
	s, fake := newTestSandbox(t)
	ctx := context.Background()
	if err := s.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ws, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	// Give the state a merge base so croft's own precondition is satisfied and
	// the conflict is what is under test.
	fake.mu.Lock()
	fake.states[ws.Revision] = map[string][]byte{"a.txt": []byte("merged")}
	fake.mu.Unlock()
	ws.Data["base_key"] = "croft-base"

	fake.deltaErr = croftlib.ErrDeltaConflict
	err = s.ApplyDelta(ctx, ws)
	if err == nil {
		t.Fatal("ApplyDelta must propagate a conflict")
	}
	if !errors.Is(err, croftlib.ErrDeltaConflict) {
		t.Errorf("errors.Is(err, croft.ErrDeltaConflict) = false; wrapping lost the sentinel: %v", err)
	}
}

func TestSandbox_ApplyDeltaWithoutBaseIsRejected(t *testing.T) {
	// A state captured from a sandbox that never Applied has no merge base, so
	// there is nothing to three-way against.
	s, _ := newTestSandbox(t)
	ctx := context.Background()
	if err := s.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ws, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := s.ApplyDelta(ctx, ws); err == nil {
		t.Fatal("ApplyDelta of a state with no merge base must fail")
	}
}

func TestSandbox_ApplyDeltaRejectsForeignBackend(t *testing.T) {
	s, _ := newTestSandbox(t)
	err := s.ApplyDelta(context.Background(), trace.WorkspaceState{Backend: "git", Revision: "abc"})
	if err == nil {
		t.Fatal("ApplyDelta must reject a state from another backend")
	}
}

func TestSandbox_ChangesExposedForEffectExtraction(t *testing.T) {
	// Changes is not part of sbx.Sandbox; plan 05's effect.FromChanges finds it
	// by type assertion, the same convention as DeltaApplier.
	s, fake := newTestSandbox(t)
	ctx := context.Background()
	if err := s.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ws, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	fake.mu.Lock()
	fake.changes = []croftlib.Change{
		{Path: "a.txt", Kind: croftlib.ChangeAdd, Size: 3},
		{Path: "b.txt", Kind: croftlib.ChangeModify, Size: 4},
		{Path: "c.txt", Kind: croftlib.ChangeDelete},
	}
	fake.mu.Unlock()

	var changer interface {
		Changes(context.Context, trace.WorkspaceState) ([]croftlib.Change, error)
	} = s
	got, err := changer.Changes(ctx, ws)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Changes returned %d entries, want 3", len(got))
	}
	if got[2].Kind != croftlib.ChangeDelete {
		t.Errorf("a delete must stay classified as ChangeDelete (containerd resolves whiteouts), got %v", got[2].Kind)
	}
}

func TestSandbox_ChangesRejectsForeignBackend(t *testing.T) {
	s, _ := newTestSandbox(t)
	_, err := s.Changes(context.Background(), trace.WorkspaceState{Backend: "git", Revision: "abc"})
	if err == nil {
		t.Fatal("Changes must reject a state from another backend")
	}
}

func TestSandbox_ImplementsContracts(t *testing.T) {
	// Compile-time proof the adapter satisfies the seam it registers into, and
	// the optional capability settlement's apply verb looks for.
	var _ sbx.Sandbox = (*Sandbox)(nil)
	var _ sbx.DeltaApplier = (*Sandbox)(nil)

	s, _ := newTestSandbox(t)
	if _, ok := any(s).(sbx.DeltaApplier); !ok {
		t.Error("the adapter must be discoverable as a DeltaApplier by type assertion")
	}
	// Addressed must NOT be implemented: croft's identity is not a path, and
	// the interface doc forbids it for such a backend (recovery would persist a
	// handle that cannot survive path validation).
	if _, ok := any(s).(sbx.Addressed); ok {
		t.Error("the adapter must not implement Addressed until croft identity is a path (croft-jqp)")
	}
	// DeltaBaseAnnotator is redundant here: croft already carries BaseKey on
	// every state and pins layers under a lease.
	if _, ok := any(s).(sbx.DeltaBaseAnnotator); ok {
		t.Error("the adapter must not implement DeltaBaseAnnotator; croft already records BaseKey and pins via lease")
	}
}

// configEqual compares two croft configs, which carry slices and pointers and
// so are not comparable with ==.
func configEqual(a, b croftlib.Config) bool { return reflect.DeepEqual(a, b) }

// TestFromTraceState_IgnoresLegacyContentDigest pins upgrade compatibility at
// the state boundary: a state persisted by the previous adapter still carries
// content_digest, and it must remain applyable. The key is ignored; only a host
// that compares its Digest across the version boundary needs a migration (see
// the envelope schema history note in state.go).
func TestFromTraceState_IgnoresLegacyContentDigest(t *testing.T) {
	legacy := trace.WorkspaceState{
		Backend:  BackendName,
		Revision: "snap-1",
		Data: map[string]any{
			"parent_key":     "snap-0",
			"base_key":       "snap-0",
			"image":          testImage,
			"content_digest": "sha256:legacy",
		},
	}
	got, err := fromTraceState(legacy)
	if err != nil {
		t.Fatalf("fromTraceState: %v", err)
	}
	if got.SnapshotKey != "snap-1" || got.ParentKey != "snap-0" || got.BaseKey != "snap-0" || got.Image != testImage {
		t.Errorf("legacy state lost a mapped field: %+v", got)
	}
	if got.ContentDigest != "" {
		t.Errorf("ContentDigest = %q, want the legacy value to be ignored", got.ContentDigest)
	}
}
