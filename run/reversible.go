// Package run is the execution-bound reversible-run wrap: it composes the
// runtime's scope and settlement primitives around an execution so a run is
// reversible by default — an isolated fork whose work is merged on success and
// discarded on failure.
//
// It is the Go port of Python Shepherd's `_run_execution_bound` (PD2,
// _vcscore_runtime.py): fork isolated → mint the execution capability only after
// the fork succeeded → dispatch → merge (default) / seal / discard. The heavy
// machinery already lives in the runtime (Scope, ScopeManager, Seal, Settle, the
// merge gate); this package is the composition.
//
// Merge semantics: an isolated run (the default) propagates physically through
// settlement, not Scope.Merge — `Seal` then `SettleSelect` (fast-forward) or
// `SettleApply` (three-way). `ScopeManager.CommitMerge` is for children that
// share the parent's sandbox and refuses isolated ones; the reviewer gate here
// runs against the sealed-eligible child via `ProposeMerge`.
package run

import (
	"context"
	"errors"
	"fmt"

	shepherd "github.com/buchenberg/shepherd-runtime-go"
	"github.com/buchenberg/shepherd-runtime-go/sandbox"
	"github.com/buchenberg/shepherd-runtime-go/sandbox/confinement"
	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
)

// Disposition is what a successful reversible run does with its isolated work.
type Disposition string

const (
	// DispositionMerge integrates the run's changes into the parent. It is the
	// default and, for an isolated run, is a physical propagation: Seal then
	// SettleSelect, falling back to SettleApply when the parent has moved.
	DispositionMerge Disposition = "merge"
	// DispositionSeal freezes the run's changes into a RetainedOutput and leaves
	// integration to the caller (fork-and-choose).
	DispositionSeal Disposition = "seal"
	// DispositionAuthorityMerge is the authority-aware merge. It is defined for
	// callers to compile but refuses until the permission-plan compiler lands.
	DispositionAuthorityMerge Disposition = "authority_merge"
)

// ErrAuthorityMergeUnsupported is returned for DispositionAuthorityMerge: the
// disposition needs the permission-plan digests (effective_match_digest,
// permission_plan_digest, descriptor) the compiler will provide.
var ErrAuthorityMergeUnsupported = errors.New("run: authority_merge requires the permission-plan compiler (not yet landed)")

// Options configures one reversible run. The zero value plus OwnerID runs a
// merge-disposition, auto-placement, isolated run with a Permissive default
// confinement.
type Options struct {
	// OwnerID names the run's child scope. Required.
	OwnerID string
	// NonReversible runs against the parent scope with isolation "ground",
	// recorded as such — the loud opt-out. It cannot be combined with a
	// non-merge disposition.
	NonReversible bool
	// Disposition defaults to DispositionMerge.
	Disposition Disposition
	// Placement defaults to PlacementAuto.
	Placement confinement.Placement
	// Confinement is the caller's authority lowering; nil asks the backend for
	// its default. Plan 1's confinement lowerers produce it.
	Confinement *containment.ConfinementSpec
	// MatchDigest is the authority-surface digest folded into evidence.
	MatchDigest string
	// Reviewer, when set, gates a merge disposition: a denial discards the run
	// and returns *shepherd.SupervisorDeniedError.
	Reviewer shepherd.MergeReviewer
	// Backend is the jail backend the capability carries (e.g.
	// landlock.Detect()). It may be nil on an advisory host.
	Backend containment.Backend
	// NewSandbox constructs the run's isolated sandbox. Required for a
	// reversible run; Reversible calls Create on it. It is not used for a
	// NonReversible run.
	NewSandbox func(runID string) (sandbox.Sandbox, error)
	// Snapshot is the opaque caller snapshot recorded at fork (typically
	// conversation history).
	Snapshot any
	// MaxDiffLines bounds a reviewer proposal's diff; <= 0 is unbounded.
	MaxDiffLines int
}

// RunContext is what a Handler receives: the run's scope and the execution
// capability that proves authority. The handler runs confined commands through
// the scope's sandbox or the capability, and never needs to know about forks or
// settlement.
type RunContext struct {
	Scope      *shepherd.Scope
	Capability containment.Capability
}

// Result is a handler's outcome: its value plus the authority-evidence digest
// (the jail monitor's permission-plan digest, when a jailed run recorded one).
type Result struct {
	Value    map[string]any
	Evidence string
}

// Handler runs the run's work in RunContext. It is the structural analogue of a
// driver's prepare_bound: a handler receives execution authority per call and
// never stores it.
type Handler func(ctx context.Context, rc *RunContext) (Result, error)

// Outcome is the wrap's result.
type Outcome struct {
	// Scope is the run scope (the child, or the parent for a NonReversible run).
	Scope *shepherd.Scope
	// Output is the sealed output for a seal disposition or a merge that sealed
	// on the way (nil otherwise).
	Output *shepherd.RetainedOutput
	// Result is the handler's outcome.
	Result Result
	// Isolation is "isolated" or "ground".
	Isolation string
	// Disposition is the disposition that ran.
	Disposition Disposition
}

// Reversible runs handler as a reversible transaction.
//
//  1. Resolve placement and backend availability before forking, so a jail that
//     cannot be established never allocates a scope.
//  2. A reversible run creates an isolated child scope (ForkIsolated); a
//     NonReversible run uses the parent scope.
//  3. The capability is minted only after the fork succeeded, and a jailed run
//     is guarded by RequireExecutionAuthority.
//  4. The handler runs; on error the child is discarded, never half-merged.
//  5. On success the disposition integrates (merge), seals, or refuses.
func Reversible(ctx context.Context, mgr *shepherd.ScopeManager, parent *shepherd.Scope, opts Options, handler Handler) (Outcome, error) {
	if mgr == nil {
		return Outcome{}, errors.New("run: nil scope manager")
	}
	if parent == nil {
		return Outcome{}, errors.New("run: nil parent scope")
	}
	if handler == nil {
		return Outcome{}, errors.New("run: nil handler")
	}
	if opts.OwnerID == "" {
		return Outcome{}, errors.New("run: OwnerID is required")
	}

	disposition := opts.Disposition
	if disposition == "" {
		disposition = DispositionMerge
	}
	if disposition == DispositionAuthorityMerge {
		return Outcome{}, ErrAuthorityMergeUnsupported
	}
	if opts.NonReversible && disposition != DispositionMerge {
		return Outcome{}, fmt.Errorf("run: a non-reversible run cannot use disposition %q (mirrors non_reversible_run validation)", disposition)
	}

	placement := opts.Placement
	if placement == "" {
		placement = confinement.PlacementAuto
	}

	// Resolve the jail BEFORE allocating anything: a jail placement on a
	// jail-less host refuses without leaving an orphan scope behind.
	available := false
	if opts.Backend != nil {
		if ok, _ := opts.Backend.Available(); ok {
			available = true
		}
	}
	jailed, err := confinement.ResolvePlacement(placement, available)
	if err != nil {
		return Outcome{}, err
	}

	isolation := "ground"
	runScope := parent
	if !opts.NonReversible {
		if opts.NewSandbox == nil {
			return Outcome{}, errors.New("run: Options.NewSandbox is required for a reversible run")
		}
		sb, err := opts.NewSandbox(opts.OwnerID)
		if err != nil {
			return Outcome{}, fmt.Errorf("run: new sandbox: %w", err)
		}
		if err := sb.Create(ctx, sandbox.SandboxSpec{}); err != nil {
			return Outcome{}, fmt.Errorf("run: create sandbox: %w", err)
		}
		child, err := mgr.ForkIsolated(parent.ID(), opts.OwnerID, opts.Snapshot, sb)
		if err != nil {
			_ = sb.Destroy(ctx)
			return Outcome{}, fmt.Errorf("run: fork isolated: %w", err)
		}
		runScope = child
		isolation = "isolated"
	}

	capability := containment.Capability{
		Backend:     opts.Backend,
		WorkingRoot: workDirOf(runScope),
		Isolation:   isolation,
	}
	if jailed {
		if err := containment.RequireExecutionAuthority(&capability); err != nil {
			if !opts.NonReversible {
				_ = mgr.Discard(runScope.ID())
			}
			return Outcome{}, err
		}
	}

	result, runErr := handler(ctx, &RunContext{Scope: runScope, Capability: capability})
	if runErr != nil {
		if !opts.NonReversible {
			_ = mgr.Discard(runScope.ID())
		}
		return Outcome{Scope: runScope, Isolation: isolation, Disposition: disposition}, runErr
	}

	if opts.NonReversible {
		return Outcome{Scope: runScope, Result: result, Isolation: isolation, Disposition: disposition}, nil
	}

	// Reviewer gate (merge only): deny ⇒ discard, never integrate.
	if disposition == DispositionMerge && opts.Reviewer != nil {
		proposal, err := mgr.ProposeMerge(ctx, runScope.ID(), opts.MaxDiffLines)
		if err != nil {
			_ = mgr.Discard(runScope.ID())
			return Outcome{Scope: runScope, Result: result, Isolation: isolation, Disposition: disposition},
				fmt.Errorf("run: propose merge: %w", err)
		}
		if deny := opts.Reviewer(*proposal); deny != nil {
			_ = mgr.Discard(runScope.ID())
			return Outcome{Scope: runScope, Result: result, Isolation: isolation, Disposition: disposition},
				&shepherd.SupervisorDeniedError{Reason: deny.Error(), Changes: proposal.Changes}
		}
	}

	out := Outcome{Scope: runScope, Result: result, Isolation: isolation, Disposition: disposition}

	sealed, err := mgr.Seal(ctx, runScope.ID())
	if err != nil {
		_ = mgr.Discard(runScope.ID())
		return out, fmt.Errorf("run: seal: %w", err)
	}
	out.Output = sealed
	if disposition == DispositionSeal {
		return out, nil
	}

	// Merge: fast-forward, or three-way when the parent moved since the fork.
	if err := mgr.Settle(ctx, sealed.ID(), shepherd.SettleSelect); err != nil {
		if aerr := mgr.Settle(ctx, sealed.ID(), shepherd.SettleApply); aerr != nil {
			return out, fmt.Errorf("run: settle: select failed (%v) and apply failed: %w", err, aerr)
		}
	}
	return out, nil
}

// workDirOf returns the sandbox's durable address when it is path-addressed
// (a git worktree or repository), else "".
func workDirOf(s *shepherd.Scope) string {
	sb := s.Sandbox()
	if sb == nil {
		return ""
	}
	if ad, ok := sb.(sandbox.Addressed); ok {
		return ad.Address()
	}
	return ""
}
