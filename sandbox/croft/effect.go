//go:build linux

package croft

import (
	"context"
	"fmt"

	trace "github.com/buchenberg/goatherd-kernel"
	"github.com/buchenberg/goatherd-runtime/effect"

	croftlib "github.com/buchenberg/goatherd-croft"
)

// EffectChanges reports the differences between ws and the current workspace in
// the runtime's backend-neutral effect vocabulary, ready to hand to
// effect.FromChanges and then effect.Emit.
//
// This is the bridge that lets a host emit typed workspace effects from a
// container session without importing goatherd-croft. The adapter already
// exposes Changes, but that returns croft's own type — so a host calling it
// would have to depend on croft directly, which defeats the reason this adapter
// owns a Config type of its own, and on Windows would not compile at all
// (goatherd-croft v0.8.0 does not build there). Callers detect this method by
// type assertion against EffectChanger, the same convention the runtime uses
// for DeltaApplier, so a host on any platform can probe for it without this
// package's Linux-only implementation being present.
//
// The mapping is deliberately lossless with respect to what croft reports:
// every change is carried across, directories included, and effect.FromChanges
// is what decides to omit directories. Splitting it that way keeps this
// function a faithful translation and leaves effect policy in the effect
// package, so a future caller that wants directory effects does not have to
// change this seam to get them.
//
// One fidelity detail survives here but not in a recorded payload: croft marks a
// deleted symlink by setting the ModeSymlink type bit on Change.Mode while
// leaving the permission bits zero, which is how a consumer tells a deleted
// symlink from a deleted regular file. effect.Change.Mode carries that bit
// intact, but effect's payload renders modes as Unix permission octal, which
// has no type bits — so the distinction is available to a caller holding
// []effect.Change and is not visible in the trace record. Nothing today needs
// it; if something later does, the payload needs a type field rather than this
// mapping changing.
func (s *Sandbox) EffectChanges(ctx context.Context, ws trace.WorkspaceState) ([]effect.Change, error) {
	changes, err := s.Changes(ctx, ws)
	if err != nil {
		return nil, err
	}
	return toEffectChanges(changes)
}

// toEffectChanges translates croft's classified change set into the runtime's
// neutral vocabulary.
//
// Paths pass through untouched: croft already reports them workspace-relative
// and slash-separated, which is what effect.Change documents.
//
// Ordering is inherited, not enforced, and that distinction is worth stating
// because a caller depends on it. Croft's Change doc says "Changes are sorted by
// Path"; this function copies that order verbatim, so anyone correlating effects
// against a diff is relying on croft's contract rather than on a guarantee made
// here. Re-sorting would duplicate a guarantee croft already makes and would
// mask the contract change that actually matters, so the dependency is recorded
// instead of papered over.
func toEffectChanges(changes []croftlib.Change) ([]effect.Change, error) {
	out := make([]effect.Change, 0, len(changes))
	for _, c := range changes {
		kind, err := toEffectKind(c.Kind)
		if err != nil {
			return nil, fmt.Errorf("croft sandbox: path %s: %w", c.Path, err)
		}
		out = append(out, effect.Change{
			Path:  c.Path,
			Kind:  kind,
			IsDir: c.IsDir,
			Mode:  c.Mode,
			Size:  c.Size,
		})
	}
	return out, nil
}

// toEffectKind maps croft's three change kinds onto the effect vocabulary.
//
// The default is an error, not a skip. An unrecognised kind means croft grew a
// classification this adapter has not been taught about — a rename, say — and
// silently dropping it would leave a workspace mutation unrecorded with nothing
// saying so, which is the exact failure mode typed effects exist to prevent.
// Failing here makes adding a kind a deliberate act that includes choosing how
// it maps, and it surfaces at the adapter rather than as a quiet hole in a
// trace somebody reads weeks later.
func toEffectKind(k croftlib.ChangeKind) (effect.Kind, error) {
	switch k {
	case croftlib.ChangeAdd:
		return effect.KindCreated, nil
	case croftlib.ChangeModify:
		return effect.KindPatched, nil
	case croftlib.ChangeDelete:
		return effect.KindDeleted, nil
	}
	return "", fmt.Errorf("unknown croft change kind %d — the effect vocabulary has no mapping for it", int(k))
}
