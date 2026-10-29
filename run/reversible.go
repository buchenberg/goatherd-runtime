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
// `SettleApply` (three-way). A reviewer or authority decision runs against the
// child via `ProposeMerge`; the proposal is recorded before review, and the
// decision is recorded when the outcome is known (after settlement for an
// approval, before discard for a denial) via `RecordMergeProposal` /
// `RecordMergeDecision`. Shared-sandbox children are integrated with
// Scope.Merge by their own host; this package does not use the removed
// CommitMerge gate.
package run

import (
	"context"
	"errors"
	"fmt"

	goatherd "github.com/buchenberg/goatherd-runtime"
	"github.com/buchenberg/goatherd-runtime/sandbox"
	"github.com/buchenberg/goatherd-runtime/sandbox/confinement"
	"github.com/buchenberg/goatherd-runtime/sandbox/containment"
)

// Disposition is what a successful reversible run does with its isolated work.
// ErrReviewDecisionUnrecorded reports that the run's work integrated but its
// approval decision could not be appended. The work is on the parent; the
// failure is a provenance gap, not a failed integration.
var ErrReviewDecisionUnrecorded = errors.New("run: work integrated but review decision was not recorded")

type Disposition string

const (
	// DispositionMerge integrates the run's changes into the parent. It is the
	// default and, for an isolated run, is a physical propagation: Seal then
	// SettleSelect, falling back to SettleApply when the parent has moved.
	DispositionMerge Disposition = "merge"
	// DispositionSeal freezes the run's changes into a RetainedOutput and leaves
	// integration to the caller (fork-and-choose).
	DispositionSeal Disposition = "seal"
	// DispositionAuthorityMerge is the authority-aware merge: the run's sealed
	// changes are judged by Options.Decision before integration, and a denial
	// discards them.
	DispositionAuthorityMerge Disposition = "authority_merge"
)

// DecisionProvider judges a run's proposed changes before integration,
// mirroring merge_with_authority's data-only decision provider. Approve by
// returning nil; deny by returning an error (the run is discarded).
type DecisionProvider func(ctx context.Context, proposal goatherd.MergeProposal) error

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
	Placement containment.Placement
	// Confinement is the caller's authority lowering; nil asks the backend for
	// its default. Plan 1's confinement lowerers produce it. Mutually exclusive
	// with Plan.
	Confinement *confinement.ConfinementSpec
	// Plan is the compiled permission plan (confinement.Install) driving this
	// run: it supplies the confinement spec (the jail monitor's) and the plan
	// digest / descriptor recorded as authority evidence. Mutually exclusive
	// with Confinement.
	Plan *confinement.PermissionPlan
	// Reviewer, when set, gates a merge disposition: a denial discards the run
	// and returns *goatherd.SupervisorDeniedError.
	Reviewer goatherd.MergeReviewer
	// Decision, required for DispositionAuthorityMerge, judges the run's proposed
	// changes before integration; a denial discards the run.
	Decision DecisionProvider
	// Backend is the paddock backend the capability carries (e.g.
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

// RunContext is what a Handler receives: the run's scope, the execution
// capability that proves authority, and the resolved confinement spec. The
// handler runs confined commands through RunContext.Exec (capability-first,
// paddock-or-refuse) or the scope's sandbox (sandbox-first), and never needs to
// know about forks or settlement.
type RunContext struct {
	Scope       *goatherd.Scope
	Capability  containment.Capability
	Confinement confinement.ConfinementSpec
}

// Exec runs a command confined by the capability: resolve → profile → probe →
// launch. It is the handler's first-class execution path and refuses without a
// paddock backend (ErrPaddockNotEstablished) rather than running unconfined — the
// structural guarantee that real execution implies authority.
func (rc *RunContext) Exec(req containment.LaunchRequest) (containment.LaunchResult, error) {
	if req.WorkingRoot == "" {
		req.WorkingRoot = rc.Capability.WorkingRoot
	}
	return containment.LaunchConfined(rc.Capability.Backend, rc.Confinement, req)
}

// Result is a handler's outcome: its value, the authority-evidence digest (the
// paddock monitor's permission-plan digest, when a confined run recorded one), and
// the run's primary execution status.
type Result struct {
	Value    map[string]any
	Evidence string
	ExitCode int
	Denied   bool
}

// SchemaRunExecuted is the runtime record the wrap appends for each run — the
// durable trail of isolation, disposition, authority evidence, and outcome.
const SchemaRunExecuted = "goatherd.run.executed.v1"

// Handler runs the run's work in RunContext. It is the structural analogue of a
// driver's prepare_bound: a handler receives execution authority per call and
// never stores it.
type Handler func(ctx context.Context, rc *RunContext) (Result, error)

// Outcome is the wrap's result.
type Outcome struct {
	// Scope is the run scope (the child, or the parent for a NonReversible run).
	Scope *goatherd.Scope
	// Output is the sealed output for a seal disposition or a merge that sealed
	// on the way (nil otherwise).
	Output *goatherd.RetainedOutput
	// Result is the handler's outcome.
	Result Result
	// Isolation is "isolated" or "ground".
	Isolation string
	// Disposition is the disposition that ran.
	Disposition Disposition
}

// Reversible runs handler as a reversible transaction.
//
//  1. Resolve placement and backend availability before forking, so a paddock that
//     cannot be established never allocates a scope.
//  2. A reversible run creates an isolated child scope (ForkIsolated); a
//     NonReversible run uses the parent scope.
//  3. The capability is minted only after the fork succeeded, and a confined run
//     is guarded by RequireExecutionAuthority.
//  4. The handler runs; on error the child is discarded, never half-merged.
//  5. On success the disposition integrates (merge), seals, or refuses.
func Reversible(ctx context.Context, mgr *goatherd.ScopeManager, parent *goatherd.Scope, opts Options, handler Handler) (Outcome, error) {
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
	if disposition == DispositionAuthorityMerge && opts.Decision == nil {
		return Outcome{}, errors.New("run: disposition authority_merge requires Options.Decision")
	}
	if opts.NonReversible && disposition != DispositionMerge {
		return Outcome{}, fmt.Errorf("run: a non-reversible run cannot use disposition %q (mirrors non_reversible_run validation)", disposition)
	}
	switch disposition {
	case DispositionMerge, DispositionSeal, DispositionAuthorityMerge:
	default:
		return Outcome{}, fmt.Errorf("run: unknown disposition %q", disposition)
	}
	if opts.Plan != nil && opts.Confinement != nil {
		return Outcome{}, errors.New("run: Options.Plan is mutually exclusive with Confinement")
	}

	// Resolve a plan's jail spec before allocating anything: a plan with no jail
	// clause (carrier-only) refuses without leaving an orphan scope.
	var planSpec *confinement.ConfinementSpec
	if opts.Plan != nil {
		s, err := opts.Plan.Confinement()
		if err != nil {
			return Outcome{}, fmt.Errorf("run: Options.Plan: %w", err)
		}
		planSpec = &s
	}

	placement := opts.Placement
	if placement == "" {
		placement = containment.PlacementAuto
	}

	// Resolve the paddock BEFORE allocating anything: a paddock placement on a
	// paddock-less host refuses without leaving an orphan scope behind.
	available := false
	if opts.Backend != nil {
		if ok, _ := opts.Backend.Available(); ok {
			available = true
		}
	}
	confined, err := containment.ResolvePlacement(placement, available)
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
			_ = sb.Destroy(ctx)
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
	if confined {
		if err := containment.RequireExecutionAuthority(&capability); err != nil {
			if !opts.NonReversible {
				_ = mgr.Discard(runScope.ID())
			}
			return Outcome{}, err
		}
	}

	spec := defaultSpec(opts, capability.WorkingRoot)
	if planSpec != nil {
		spec = *planSpec
	}
	result, runErr := handler(ctx, &RunContext{Scope: runScope, Capability: capability, Confinement: spec})
	recordErr := recordRun(ctx, runScope, parent, isolation, disposition, result, opts.Plan)
	if runErr != nil {
		if !opts.NonReversible {
			_ = mgr.Discard(runScope.ID())
		}
		return Outcome{Scope: runScope, Isolation: isolation, Disposition: disposition}, runErr
	}
	if recordErr != nil {
		if !opts.NonReversible {
			_ = mgr.Discard(runScope.ID())
		}
		return Outcome{Scope: runScope, Result: result, Isolation: isolation, Disposition: disposition}, recordErr
	}

	// A syscall denial is a discarded, Failed run — never integrate a partial or
	// refused result (Plan 2 §9.4).
	if !opts.NonReversible && result.Denied {
		_ = mgr.Discard(runScope.ID())
		return Outcome{Scope: runScope, Result: result, Isolation: isolation, Disposition: disposition},
			fmt.Errorf("%w: the run was refused at the syscall", containment.ErrSandboxDenied)
	}

	if opts.NonReversible {
		return Outcome{Scope: runScope, Result: result, Isolation: isolation, Disposition: disposition}, nil
	}

	// Gate: a merge may be reviewed; an authority merge is decided. Both deny by
	// discarding the run, never integrating. When a reviewer runs, the proposal
	// and its decision are recorded as a provenance pair independent of the
	// physical settlement (RecordMergeProposal / RecordMergeDecision).
	var reviewProposal *goatherd.MergeProposal
	var reviewFactID string
	recordProposal := func() error {
		proposal, err := mgr.ProposeMerge(ctx, runScope.ID(), opts.MaxDiffLines)
		if err != nil {
			return fmt.Errorf("run: propose merge: %w", err)
		}
		factID, err := mgr.RecordMergeProposal(ctx, proposal)
		if err != nil {
			return fmt.Errorf("run: record merge proposal: %w", err)
		}
		reviewProposal, reviewFactID = proposal, factID
		return nil
	}
	recordDecision := func(approved bool, reason, action string) error {
		if reviewProposal == nil {
			return nil
		}
		_, err := mgr.RecordMergeDecision(ctx, reviewProposal, approved, reason, action, reviewFactID)
		if err != nil {
			return fmt.Errorf("run: record merge decision: %w", err)
		}
		return nil
	}
	reject := func(reason string) (Outcome, error) {
		recErr := recordDecision(false, reason, "discarded")
		_ = mgr.Discard(runScope.ID())
		var changes []goatherd.ProposedChange
		if reviewProposal != nil {
			changes = reviewProposal.Changes
		}
		denyErr := &goatherd.SupervisorDeniedError{Reason: reason, Changes: changes}
		if recErr != nil {
			return Outcome{Scope: runScope, Result: result, Isolation: isolation, Disposition: disposition}, errors.Join(denyErr, recErr)
		}
		return Outcome{Scope: runScope, Result: result, Isolation: isolation, Disposition: disposition}, denyErr
	}

	if disposition == DispositionMerge && opts.Reviewer != nil {
		if err := recordProposal(); err != nil {
			_ = mgr.Discard(runScope.ID())
			return Outcome{Scope: runScope, Result: result, Isolation: isolation, Disposition: disposition}, err
		}
		if deny := opts.Reviewer(*reviewProposal); deny != nil {
			return reject(deny.Error())
		}
	}
	if disposition == DispositionAuthorityMerge {
		if err := recordProposal(); err != nil {
			_ = mgr.Discard(runScope.ID())
			return Outcome{Scope: runScope, Result: result, Isolation: isolation, Disposition: disposition}, err
		}
		if deny := opts.Decision(ctx, *reviewProposal); deny != nil {
			return reject(deny.Error())
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
	action := goatherd.SettleSelect
	if err := mgr.Settle(ctx, sealed.ID(), action); err != nil {
		action = goatherd.SettleApply
		if aerr := mgr.Settle(ctx, sealed.ID(), action); aerr != nil {
			return out, fmt.Errorf("run: settle: select failed (%v) and apply failed: %w", err, aerr)
		}
	}
	if err := recordDecision(true, "", string(action)); err != nil {
		return out, fmt.Errorf("%w: %v", ErrReviewDecisionUnrecorded, err)
	}
	return out, nil
}

// defaultSpec resolves the confinement a handler's RunContext carries: the
// caller's lowering, or a Permissive default over the run's working root.
func defaultSpec(opts Options, workDir string) confinement.ConfinementSpec {
	if opts.Confinement != nil {
		return *opts.Confinement
	}
	return confinement.PermissiveFor(workDir)
}

// VerifyHandler is the negotiation conformance probe for a host-supplied
// handler, mirroring verify_execution_negotiation. A handler is execution-bound
// by receiving a RunContext; conformance means it refuses real execution when
// handed a capability with no backend, rather than succeeding unconfined.
//
// probe is a RunContext the caller supplies (typically a throwaway scope
// carrying a backend-less capability); it is passed to the handler. A handler
// that attempts execution through the capability refuses with
// ErrExecutionAuthorityRequired or ErrPaddockNotEstablished — VerifyHandler
// requires exactly that. A handler that returns nil ran without authority and
// fails. This is a deliberate, opt-in probe for handlers that should be
// execution-bound; a pure-data path never receives a RunContext at all.
func VerifyHandler(ctx context.Context, probe *RunContext, h Handler) error {
	if h == nil {
		return errors.New("run: VerifyHandler: nil handler")
	}
	if probe == nil {
		return errors.New("run: VerifyHandler: nil probe context")
	}
	if err := containment.RequireExecutionAuthority(&probe.Capability); err == nil {
		return errors.New("run: VerifyHandler: the probe capability must have no backend")
	}
	_, err := h(ctx, probe)
	if err == nil {
		return errors.New("run: VerifyHandler: handler ran real execution without authority (no refusal)")
	}
	if !errors.Is(err, containment.ErrExecutionAuthorityRequired) && !errors.Is(err, containment.ErrPaddockNotEstablished) {
		return fmt.Errorf("run: VerifyHandler: handler refused with %v, want ErrExecutionAuthorityRequired or ErrPaddockNotEstablished", err)
	}
	return nil
}

// workDirOf returns the sandbox's durable address when it is path-addressed
// (a git worktree or repository), else "".
func workDirOf(s *goatherd.Scope) string {
	sb := s.Sandbox()
	if sb == nil {
		return ""
	}
	if ad, ok := sb.(sandbox.Addressed); ok {
		return ad.Address()
	}
	return ""
}

// recordRun persists the run's execution record on the run scope's owner path:
// isolation, disposition, the authority-evidence digest, and the outcome. It is
// the durable half of the authority trail — Plan 1 surfaced the digest on the
// ExecResult; this writes it into the trace.
func recordRun(ctx context.Context, runScope, parent *goatherd.Scope, isolation string, disposition Disposition, result Result, plan *confinement.PermissionPlan) error {
	payload := map[string]any{
		"owner":              runScope.OwnerID(),
		"parent_scope":       parent.ID(),
		"isolation":          isolation,
		"disposition":        string(disposition),
		"authority_evidence": result.Evidence,
		"exit_code":          result.ExitCode,
		"denied":             result.Denied,
	}
	if plan != nil {
		payload["permission_plan_digest"] = plan.Digest()
		payload["permission_plan_descriptor"] = plan.Descriptor()
	}
	if _, err := runScope.AppendFact(ctx, SchemaRunExecuted, "run:executed", payload); err != nil {
		return fmt.Errorf("run: record execution: %w", err)
	}
	return nil
}
