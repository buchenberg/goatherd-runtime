//go:build linux

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

// createCeiling bounds a Create whose spec carries no Timeout.
//
// It is more generous than execCeiling because Create is the verb that may pull
// an image, and a pull is network-bound in a way no command execution is: a
// multi-hundred-megabyte workspace image on a cold cache is minutes of work
// that has nothing to do with the daemon being wedged. The ceiling exists to
// stop an unreachable registry or a hung daemon from blocking a caller that
// passed context.Background() — the Sandbox contract's requirement — not to
// second-guess a slow but progressing pull. A var so tests can shrink it.
var createCeiling = 30 * time.Minute

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
// spec.Timeout bounds the operation. Zero means this backend's default, which
// is createCeiling — the Sandbox contract asks a backend to impose its own
// ceiling so a caller passing context.Background() cannot hang indefinitely,
// and Create is the verb most likely to: a first run may pull an image, so a
// slow or unreachable registry would otherwise block forever. An explicit
// Timeout above the ceiling is honored as the caller's contract, not clamped.
//
// spec.Workdir and spec.Env are ignored: the container's root and environment
// are fixed by Config at construction, and this backend infers its own
// workspace path.
func (s *Sandbox) Create(ctx context.Context, spec sbx.SandboxSpec) error {
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = createCeiling
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

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
