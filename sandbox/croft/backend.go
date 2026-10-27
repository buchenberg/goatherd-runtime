// Package croft is the containerd sandbox backend: it puts
// github.com/buchenberg/goatherd-croft behind the sandbox package's registry
// seam as backend "containerd", so a container workspace is addressable
// through OpenSandbox exactly like the git backend is.
//
// Croft is a *world* (the substrate axis — a sibling of sandbox/git), not a
// *paddock* (the authority axis, containment.Backend, where landlock and
// winacl live). The container boundary — namespaces, cgroups, an unshared
// network namespace — is what encloses the work, so this backend reports
// ContainContained and never a syscall-tier claim. Do not layer Landlock
// inside the container: a container seccomp profile may block the Landlock
// syscalls outright, and under namespaces it would be redundant.
//
// This is a nested module so the runtime core keeps its "kernel + stdlib"
// dependency footprint: only this module pulls containerd v2 and the OCI
// tree. It is the Go analogue of the Shepherd reference keeping its remote
// sandbox backends in an optional, lazily-imported package.
//
// # Digest stability — a known, recorded gap
//
// sandbox.Sandbox.Capture requires that two captures of identical content at
// an unchanged timeline position produce equal WorkspaceState digests, because
// settlement's select precondition compares digests as content equality.
//
// This backend does NOT satisfy that, and cannot until goatherd-croft ships
// content-derived state identity (croft bead croft-dlb: a no-change capture
// fast path plus an exposed ContentDigest). Croft states are sequence-keyed —
// Capture unconditionally commits a fresh layer and mints a new SnapshotKey,
// even when the active layer is unchanged. Hashing the state's fields does not
// help: the inputs themselves differ on every capture, so any hash of them
// differs too. croft-broker's four-field sha256 is a stable *name* for a given
// state, not content equality, and is deliberately not reproduced here.
//
// The gap is safe today only because settlement never sees a container state:
// yaah gates retained outputs on `sessionSandbox == nil`, so a container
// session produces none. Enabling retained outputs in container mode requires
// croft-dlb first. Per the Capture contract's own instruction, the gap is
// recorded here rather than papered over.
package croft

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	trace "github.com/buchenberg/goatherd-kernel"
	sbx "github.com/buchenberg/goatherd-runtime/sandbox"

	croftlib "github.com/buchenberg/goatherd-croft"
)

// BackendName is the registry key this backend answers to, and the value
// stamped into every trace.WorkspaceState it produces. The kernel's WorkspaceState
// doc names "containerd" as the container backend's identifier, and Apply
// rejects a state carrying a different one.
const BackendName = "containerd"

// Config configures the containerd backend. It is adapter-owned and mirrors
// the croft essentials field-for-field so a host never imports croft types —
// the same reason GitSandboxConfig exists for the git backend.
type Config struct {
	// Address is the containerd socket or endpoint. Empty uses containerd's
	// own default resolution.
	Address string
	// Namespace isolates this backend's containers and snapshots from other
	// containerd users on the same daemon.
	Namespace string
	// Image is the base image the workspace is provisioned from. Required:
	// a container with no rootfs has nothing to run.
	Image string
	// Runtime is the OCI runtime name, for example "io.containerd.runc.v2".
	// Empty uses containerd's default.
	Runtime string
	// Snapshotter selects the containerd snapshotter plugin. Empty uses
	// "overlayfs". Other snapshotters differ in commit semantics, so the
	// default is not assumed silently — it is croft's documented default.
	Snapshotter string
	// InitArgs is the long-lived command keeping the container up between
	// execs. Empty uses croft's `/bin/sh -c "sleep infinity"`, which needs a
	// shell in the image.
	InitArgs []string
	// Workdir is the workspace path inside the container; relative file paths
	// and commands resolve against it. Empty defaults to "/workspace".
	//
	// This governs the workspace root. sbx.SandboxSpec.Workdir is ignored by
	// Create, per that field's contract ("ignored by backends that infer their
	// own") — the container's root is fixed at construction, not per call.
	Workdir string
	// WriteChunkBytes bounds how much file content one WriteFile exec carries
	// before base64 expansion. Empty uses croft's default.
	WriteChunkBytes int
	// MaxOutputBytes caps each of an exec's stdout and stderr in memory,
	// applied per stream. Zero or negative means unbounded. Data transport
	// (ReadFile, diff readers) stays lossless regardless.
	MaxOutputBytes int
	// FIFODir is where the containerd *client* creates the stdio FIFOs for an
	// exec. containerd's default is root-owned, so an unprivileged caller must
	// set this to a directory it can write to.
	FIFODir string
	// Limits bounds the container's CPU, memory and process count. Nil means
	// unbounded. Fixed at construction: decide limits before New.
	Limits *Limits
	// Network is the workspace's network posture. "" or "none" keeps the
	// container's own unshared namespace — loopback only, no egress, DNS fails
	// fast, which is the default and the container lane's egress answer.
	// "host" is the explicit opt-in that shares the daemon host's network.
	// Unknown values are rejected by croft at every generation-starting entry.
	Network string
	// Secrets are host files bind-mounted read-only into the container. A
	// token arrives as a file, never in argv or the process environment.
	Secrets []SecretMount
	// StagingDir is a host directory enabling argv-free writes: when set,
	// WriteFile stages through it and moves the payload into place inside the
	// container, so content appears in no argv. When unset, WriteFile keeps
	// croft's documented base64-in-argv posture.
	StagingDir string
}

// Limits bounds a container's resources, mapped onto the OCI runtime spec's
// Linux resources (cgroup v2 on a modern daemon). Zero-valued fields are left
// unset, so partial limits compose with the image and runtime defaults.
type Limits struct {
	// CPUShares is the relative CPU weight against other containers.
	CPUShares uint64
	// CPUQuota caps CPU bandwidth: at most CPUQuota microseconds of CPU time
	// per scheduling period (typically 100ms, so 50000 is half a CPU).
	CPUQuota int64
	// MemoryBytes is the cgroup v2 memory.max limit. Swap is accounted
	// separately and left at the runtime default.
	MemoryBytes int64
	// Pids caps the process count in the container's cgroup — the fork-bomb
	// fairness bound.
	Pids int64
}

// SecretMount binds a host file read-only into the container.
type SecretMount struct {
	// Source is an absolute path on the daemon host.
	Source string
	// Destination is the absolute in-container path.
	Destination string
}

// Sandbox is a container workspace behind the runtime's Sandbox seam. It owns
// no filesystem state of its own: every operation delegates to the croft
// sandbox it wraps, converting between the runtime's vocabulary and croft's.
//
// The wrapped value is the croftlib.Sandbox *interface*, not its concrete
// type, so tests can drive the whole adapter against an in-memory fake with no
// containerd daemon and no root.
type Sandbox struct {
	inner croftlib.Sandbox
	cfg   Config
}

var _ sbx.Sandbox = (*Sandbox)(nil)

// New returns an unprovisioned containerd backend. Create remains a separate,
// explicit lifecycle step, as the registry contract requires.
//
// Config.Image is required and is validated here rather than at Create, so a
// missing image is a startup error naming the field instead of a daemon
// failure mid-session.
func New(cfg Config) (*Sandbox, error) {
	if cfg.Image == "" {
		return nil, errors.New("croft sandbox: Config.Image is required: a container workspace has no filesystem without a base image")
	}
	return &Sandbox{inner: croftlib.New(toCroftConfig(cfg)), cfg: cfg}, nil
}

// newWithInner binds an adapter to an already-built croft sandbox. It is the
// test seam: a fake croftlib.Sandbox exercises every conversion in this
// package without a daemon.
func newWithInner(cfg Config, inner croftlib.Sandbox) *Sandbox {
	return &Sandbox{inner: inner, cfg: cfg}
}

// toCroftConfig lowers the adapter's Config onto croft's. The mapping is
// field-for-field; the adapter type exists so hosts never import croft, not to
// reshape the configuration.
func toCroftConfig(c Config) croftlib.Config {
	out := croftlib.Config{
		Address:         c.Address,
		Namespace:       c.Namespace,
		Image:           c.Image,
		Runtime:         c.Runtime,
		Snapshotter:     c.Snapshotter,
		InitArgs:        c.InitArgs,
		Workdir:         c.Workdir,
		WriteChunkBytes: c.WriteChunkBytes,
		MaxOutputBytes:  c.MaxOutputBytes,
		FIFODir:         c.FIFODir,
		Network:         c.Network,
		StagingDir:      c.StagingDir,
	}
	if c.Limits != nil {
		out.Limits = &croftlib.Limits{
			CPUShares:   c.Limits.CPUShares,
			CPUQuota:    c.Limits.CPUQuota,
			MemoryBytes: c.Limits.MemoryBytes,
			Pids:        c.Limits.Pids,
		}
	}
	if len(c.Secrets) > 0 {
		out.Secrets = make([]croftlib.SecretMount, len(c.Secrets))
		for i, s := range c.Secrets {
			out.Secrets[i] = croftlib.SecretMount{Source: s.Source, Destination: s.Destination}
		}
	}
	return out
}

// Backend returns "containerd", the registry key and the state provenance
// marker.
func (s *Sandbox) Backend() string { return BackendName }

// Capabilities reports the container backend's support matrix.
//
// Every capability is true: a container provisions and destroys real
// resources, executes, does file IO, and diffs. Isolated is true because the
// workspace is a snapshot layer, not the host tree.
//
// Containment is ContainContained — the container boundary. It is deliberately
// not ContainFull: a kernel exploit escapes a container, and this backend
// enforces nothing at the syscall level. Claiming the syscall tier here would
// be exactly the dishonesty the sandbox package doc forbids.
func (s *Sandbox) Capabilities() sbx.SandboxCapabilities {
	return sbx.SandboxCapabilities{
		Lifecycle:   true,
		Exec:        true,
		FileIO:      true,
		Diff:        true,
		Isolated:    true,
		Containment: trace.ContainContained,
	}
}

// Create provisions the container workspace: resolves the image rootfs,
// prepares the base snapshot and starts the task.
//
// spec.Timeout bounds the operation, which matters because a first run may
// pull an image. spec.Workdir and spec.Env are ignored: the container's root
// and environment are fixed by Config at construction, and this backend
// infers its own workspace path.
func (s *Sandbox) Create(ctx context.Context, spec sbx.SandboxSpec) error {
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	return s.inner.Create(ctx)
}

// Destroy releases the container and its active layer. It never touches
// captured states: those stay pinned by lease and remain applyable, which is
// how a state outlives the sandbox that produced it. Use ReleaseState to drop
// a state.
func (s *Sandbox) Destroy(ctx context.Context) error {
	return s.inner.Destroy(ctx)
}

// Capture commits the active layer, pins it under a lease and continues on a
// fresh layer, returning the state as a trace.WorkspaceState.
//
// See the package doc: the returned digest is NOT content-stable across
// captures of unchanged content, because croft's state identity is
// sequence-derived. Callers must not compare two captures as content equality
// until croft-dlb lands.
func (s *Sandbox) Capture(ctx context.Context) (trace.WorkspaceState, error) {
	cs, err := s.inner.Capture(ctx)
	if err != nil {
		return trace.WorkspaceState{}, fmt.Errorf("croft sandbox: capture: %w", err)
	}
	return toTraceState(cs), nil
}

// Apply rebinds the workspace to a previously captured state by preparing a
// new active snapshot from the committed key.
//
// Single-use per state, as the Sandbox contract anticipates for a
// snapshot-backed backend: Apply provisions fresh resources and rebinds this
// sandbox's identity, so it is not a reusable reset the way the git backend's
// is. Layers created after the applied state survive as states; reclaim them
// with ReleaseState.
//
// A state from another backend is rejected, matching the git backend's
// provenance check.
func (s *Sandbox) Apply(ctx context.Context, ws trace.WorkspaceState) error {
	cs, err := fromTraceState(ws)
	if err != nil {
		return err
	}
	if err := s.inner.Apply(ctx, cs); err != nil {
		return fmt.Errorf("croft sandbox: apply: %w", err)
	}
	return nil
}

// ReadFile reads a file inside the container workspace.
func (s *Sandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	b, err := s.inner.ReadFile(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("croft sandbox: read %s: %w", path, err)
	}
	return b, nil
}

// WriteFile writes a file inside the container workspace. When
// Config.StagingDir is set the payload never appears in argv; otherwise it
// travels base64 in exec arguments, which is croft's documented exposure.
func (s *Sandbox) WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error {
	if err := s.inner.WriteFile(ctx, path, data, perm); err != nil {
		return fmt.Errorf("croft sandbox: write %s: %w", path, err)
	}
	return nil
}

// Changes reports the typed differences between ws and the current workspace.
//
// It is not part of sbx.Sandbox: callers detect it by type assertion, the same
// convention as DeltaApplier. It exists so the runtime's effect extraction
// (plan 05, effect.FromChanges) can consume croft's already-classified change
// set — containerd resolves adds, modifies, deletes and whiteouts itself, so
// nothing needs to walk the overlay by hand.
func (s *Sandbox) Changes(ctx context.Context, ws trace.WorkspaceState) ([]croftlib.Change, error) {
	cs, err := fromTraceState(ws)
	if err != nil {
		return nil, err
	}
	changes, err := s.inner.Changes(ctx, cs)
	if err != nil {
		return nil, fmt.Errorf("croft sandbox: changes: %w", err)
	}
	return changes, nil
}

// ReleaseState drops the lease pinning a captured state's snapshot, letting
// the daemon's GC reclaim the layer. Idempotent.
//
// It is the discard verb, and like Changes it sits outside sbx.Sandbox for
// callers to find by type assertion: the runtime's interface has no
// state-release method, but settlement's discard path needs one so a rejected
// proposal does not pin a layer forever.
func (s *Sandbox) ReleaseState(ctx context.Context, ws trace.WorkspaceState) error {
	cs, err := fromTraceState(ws)
	if err != nil {
		return err
	}
	if err := s.inner.ReleaseState(ctx, cs); err != nil {
		return fmt.Errorf("croft sandbox: release state: %w", err)
	}
	return nil
}

// ApplyDelta merges ws's changes onto the current workspace at the tree level,
// without resetting it — settlement's apply verb, for a parent that has moved
// past the state's base.
//
// The merge is tree-level, not line-level: two divergent edits to the same
// file conflict even when they touch different lines. A conflict returns
// croft's ErrDeltaConflict unwrapped in the message chain and leaves the
// workspace untouched, so settlement fails closed rather than synthesizing a
// merge. Callers can test errors.Is(err, croftlib.ErrDeltaConflict).
//
// A state with an empty base — captured from a sandbox that never Applied —
// has no merge base and croft rejects it.
func (s *Sandbox) ApplyDelta(ctx context.Context, ws trace.WorkspaceState) error {
	cs, err := fromTraceState(ws)
	if err != nil {
		return err
	}
	if err := s.inner.ApplyDelta(ctx, cs); err != nil {
		return fmt.Errorf("croft sandbox: apply delta: %w", err)
	}
	return nil
}

var _ sbx.DeltaApplier = (*Sandbox)(nil)

// init self-registers the backend, so importing this module makes
// OpenSandbox("containerd", ...) resolve — the sandbox/git pattern. A host
// that never imports it cannot select it, which keeps the dependency tree
// honest.
func init() {
	sbx.RegisterSandbox(BackendName, func(cfg any) (sbx.Sandbox, error) {
		c, ok := cfg.(Config)
		if !ok {
			if cfg == nil {
				return nil, fmt.Errorf("croft sandbox: OpenSandbox requires a croft.Config; nil is not one — a backend cannot be chosen by omission")
			}
			return nil, fmt.Errorf("croft sandbox: OpenSandbox configuration is croft.Config, got %T", cfg)
		}
		return New(c)
	})
}

// deadlineCtx bounds an operation by timeout when one is given, and is a
// pass-through otherwise. Shared by Create and Exec so both apply the same
// rule.
func deadlineCtx(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}
