# Implementation Plan: Execution-Bound Reversible Runs (Go)

> **Scope.** This is **Plan 2** of the containment/execution work. Plan 1
> (`01-landlock-containment.md`) delivered the Linux containment component —
> the paddock, its contract, the authority lowering, and confined `Exec`. What Plan 1
> deliberately fenced out is *how a confined execution is run*: Python Shepherd
> wraps execution in a **reversible transaction** (isolated fork → capability →
> recorded operation → merge/discard). This plan brings that wrap to Go.
>
> **Key finding motivating this plan:** the Go runtime **already implements the
> reversibility substrate** (`ScopeManager.ForkIsolated`, `Capture/Apply`,
> `Seal`, `Settle`, merge gate, recovery). This plan is a *composition* layer that
> joins those primitives to the Plan-1 confined `Exec` — not new substrate.
>
> **Status:** all four phases are implemented in
> [`run/reversible.go`](../../run/reversible.go) and
> [`fact.go`](../../fact.go) — 20 passing tests. Merge propagates
> physically via `Seal` + `SettleSelect`/`SettleApply`. `authority_merge`
> (Phase 4) is the `DecisionProvider` gate (Plan 3 Phase 4).
>
> **Historical note:** this plan referenced `CommitMerge`; that helper was
> removed in `shepherd-runtime-go-s3u` because it only applied to shared-sandbox
> children. Review now records the explicit
> `RecordMergeProposal` / `RecordMergeDecision` pair.

## 1. Objective & Context

Plan 1 shipped:
- `containment.Backend` + `landlock` (confined `Exec`, fail-closed probe, denial
  classification, authority evidence digest).
- `confinement` (may= / grants → `ConfinementSpec`, placement, `PermissionPlan`).
- `GitSandbox.Exec` with `WithExec` (paddocked/advisory) and `ExecMay`/`ExecGrants`.

What is missing for functional parity with Python's execution boundary: a
**reversible-by-default execution wrap**. In Python, an execution-bound command is
run inside `_run_execution_bound` (`_vcscore_runtime.py:997-1121`):

1. Fork an **isolated** child scope (a carrier that cannot isolate refuses).
2. Construct the `ExecutionCapability` **only after the fork succeeded** — the
   capability *carries the proof*.
3. Dispatch the driver's `prepare_bound` inside a **recorded operation**.
4. On failure: **discard** (never auto-merge a half-run delta). On success:
   **merge** (the default), **seal**, or **authority_merge**.
5. A loud, recorded opt-out (`non_reversible_run`) runs on the ground with
   `isolation="ground"`.

The Go runtime has all the pieces but does not join them, and `GitSandbox.Exec`
itself neither forks nor records.

## 2. What "Parity" Means Here (the reference surface)

| Python | File | Role |
|---|---|---|
| `_run_execution_bound` (the PD2 wrap) | `_vcscore_runtime.py:997-1121` | fork→capability→dispatch→merge/discard |
| `ExecutionCapability` / `launch_confined` | `_execution_capability.py:162-195` | the per-run handle + the only real-exec verb |
| `ExecutionBoundDriver` (`prepare_bound`, `execution_commands`) | `_execution_capability.py:198-224` | opt-in execution authority |
| `verify_execution_negotiation` | `_execution_capability.py:227-273` | fail-closed under skew |
| `CommandExecutionOptions(non_reversible_run, success_disposition, …)` | `_command_envelope.py:78` | the opt-out + disposition |
| `runtime_operation_boundary` | `_vcscore_runtime.py` | recorded-operation bracket |
| `merge_with_authority` | `_vcscore_runtime.py` | the authority-merge disposition |
| `NON_REVERSIBLE_RUN_FLAG` | `_execution_capability.py:50` | the greppable opt-out key |

Go substrate this composes with (already implemented):

| Primitive | API |
|---|---|
| isolated fork | `(*ScopeManager).ForkIsolated(parentID, ownerID string, snapshot any, sb sandbox.Sandbox) (*Scope, error)` |
| shared fork | `(*ScopeManager).Fork(parentID, ownerID string, snapshot any) (*Scope, error)` |
| capture/restore | `(*Scope).CaptureWorkspace/ApplyWorkspace(ctx) (trace.WorkspaceState, error)` |
| seal | `(*ScopeManager).Seal(ctx, scopeID string) (*RetainedOutput, error)` |
| settle | `(*ScopeManager).Settle(ctx, outputID string, action SettlementAction) error` — `SettleSelect`/`SettleApply`/`SettleRelease`/`SettleDiscard` |
| review-gated merge | `(*ScopeManager).ProposeMerge` + `RecordMergeProposal` / `RecordMergeDecision` (+ `SupervisorDeniedError`) |
| discard | `(*ScopeManager).Discard(childID string) error` |
| confined exec | `containment.LaunchConfined`, `containment.Capability`, `git.WithExec` |

## 3. Scope & Explicit Non-Goals

**In scope:** the reversible run wrap, the execution-bound handler seam, the
recorded-operation boundary + evidence persistence, the dispositions
(merge default, seal, non-reversible opt-out), and the test matrix.

**Non-goals (deferred to later plans):**
- **`authority_merge`** is implemented as a `DecisionProvider` gate (Plan 3
  Phase 4); the reference's richer carrier-candidate cohort lane is a follow-up.
- Egress enforcement, Seatbelt, and the container tier (unchanged non-goals from
  Plan 1 §3).
- Host wiring (`yaah`) / sub-agent supervision integration.

## 4. Parity Map (completion checklist)

| # | Python symbol | Go target | Test | Done |
|---|---|---|---|---|
| 1 | `owner.fork(..., ForkHints(isolated=True))` | `run.Reversible` → `ForkIsolated` | `TestReversible_MergeDefault` (asserts isolation) | ✅ |
| 2 | non-isolated branch (`non_reversible_run`) | `Options.NonReversible` → run on parent, `isolation="ground"` | `TestReversible_NonReversibleOptOut` | ✅ |
| 3 | capability minted after fork | `RunContext.Capability` constructed post-fork | `TestReversible_MergeDefault` (capability isolation observed in handler) | ✅ |
| 4 | `require real-execution authority` | `containment.RequireExecutionAuthority` | `TestReversible_RefusesWithoutBackend` | ✅ |
| 5 | `prepare_bound` dispatch | `Handler func(ctx, *RunContext) (Result, error)` | `TestReversible_HandlerRunsConfined` | ✅ |
| 6 | discard on failure | `mgr.Discard(runScope.ID())` on handler error | `TestReversible_DiscardOnFailure` | ✅ |
| 7 | `owner.merge(run_scope, scope)` (default disposition) | isolated: `mgr.Seal` + `SettleSelect` (fallback `SettleApply`) | `TestReversible_MergeDefault` | ✅ |
| 8 | `success_disposition == "seal"` | `Options.Disposition = DispositionSeal` → `mgr.Seal` | `TestReversible_SealDisposition` | ✅ |
| 9 | `runtime_operation_boundary` | `run` records `shepherd.run.executed.v1` via `(*Scope).AppendFact` | `TestReversible_RecordsRunOperation` | ✅ |
| 10 | authority digest recorded | evidence (`confinement.Evidence`) persisted in the run record | `TestReversible_RecordsEvidence` | ✅ |
| 11 | `verify_execution_negotiation` under skew | `run` refuses without a capability backend | `TestReversible_RefusesWithoutBackend` | ✅ (shape) |
| 12 | `success_disposition == "authority_merge"` | `DispositionAuthorityMerge` + `DecisionProvider` | `TestReversible_AuthorityMerge`, `TestReversible_AuthorityMergeDenied` | ✅ |

## 5. Architecture

### 5.1 Where it lives

A new package `github.com/buchenberg/goatherd-runtime/run`, importing the root
`shepherd` package (Scope/ScopeManager), `sandbox`, `sandbox/containment`,
`sandbox/confinement`. The root hub stays lean; `run` is the orchestration seam
Python puts in `_vcscore_runtime`.

### 5.2 The wrap, mapped

```
run.Reversible(ctx, mgr, parent, opts, handler)
  1. resolve placement + backend availability  ← fail fast, BEFORE forking (no orphan)
  2. if opts.NonReversible:
         runScope = parent; isolation = "ground"
     else:
         sb := opts.NewSandbox()               // e.g. git.NewWorktreeSandbox(repo, path, WithExec(landlock, placement))
         sb.Create(ctx, spec)
         runScope = mgr.ForkIsolated(parent.ID(), ownerID, nil, sb)   // ← isolation proof
         isolation = "isolated"
  3. cap := containment.Capability{Backend: backend, WorkingRoot: workDir, Isolation: isolation}
     containment.RequireExecutionAuthority(&cap)          // fail-closed under skew
  4. open recorded run operation (trace append bracket)
  5. result, err := handler(ctx, &RunContext{Scope: runScope, Capability: cap})
     record evidence + outcome on the operation
  6. close operation
  7. on error:  if !NonReversible { mgr.Discard(runScope.ID()) }; return err
     on ok:     if NonReversible { return }
                switch opts.Disposition:
                  Merge (default): mgr.Seal + SettleSelect (fallback SettleApply)
                  Seal:            out := mgr.Seal(ctx, runScope.ID())
                  AuthorityMerge:  refuse — Plan 3
```

### 5.3 Deviations from Python (deliberate, stricter)

1. **Pre-flight before fork.** Python forks first and may fail later (leaving a
   scope to discard). Go resolves placement + backend availability *before*
   `ForkIsolated`, so a paddock-less host never allocates a worktree.
2. **Explicit sandbox factory.** Python's carrier forks implicitly; Go's
   `ForkIsolated` takes the sandbox, so the wrap needs `Options.NewSandbox`.
3. **Shared vs isolated is explicit.** A `NonReversible` run reuses the parent's
   sandbox (no fork); a reversible run always uses `ForkIsolated` (matching
   Python's "no isolation knob to forget").

## 6. Phase 1 — The reversible run wrap (`run` package)

```go
package run

type Disposition string

const (
    DispositionMerge          Disposition = "merge"           // default
    DispositionSeal           Disposition = "seal"
    DispositionAuthorityMerge Disposition = "authority_merge"
)

type Options struct {
    OwnerID       string
    NonReversible bool                          // loud opt-out: run on the parent, isolation="ground"
    Disposition   Disposition                   // default DispositionMerge
    Placement     confinement.Placement         // default PlacementAuto
    Confinement   *containment.ConfinementSpec  // nil ⇒ PermissiveFor(workdir)
    MatchDigest   string                        // authority-surface digest for evidence
    Reviewer      goatherd.MergeReviewer        // merge disposition
    NewSandbox    func(runID string) (sandbox.Sandbox, error) // required for reversible runs
    Snapshot      any
    MaxDiffLines  int
}

type RunContext struct {
    Scope      *goatherd.Scope
    Capability containment.Capability
}

type Result struct {
    Value    map[string]any
    Evidence string // permission-plan digest, when a paddocked run recorded one
}

type Outcome struct {
    Scope       *goatherd.Scope
    Output      *goatherd.RetainedOutput // DispositionSeal
    Result      Result
    Isolation   string                   // "isolated" | "ground"
    Disposition Disposition
}

func Reversible(ctx context.Context, mgr *goatherd.ScopeManager, parent *goatherd.Scope,
    opts Options, handler Handler) (Outcome, error)

type Handler func(ctx context.Context, rc *RunContext) (Result, error)
```

The handler runs confined commands through `rc.Capability.LaunchConfined(spec, req)`
or `rc.Scope.Sandbox().Exec(ctx, req)`; it never needs to know about forks or
settlement — that is the wrap's job.

## 7. Phase 2 — The execution-bound handler seam (implemented)

Go has no driver protocol; the analogue is a **handler** that structurally
receives execution authority through `RunContext`, mirroring `prepare_bound`
receiving the `ExecutionCapability` per call (never stored). As built:

- `RunContext` carries the scope, the `Capability`, and the resolved
  `ConfinementSpec`; `RunContext.Exec` is the handler's first-class confined
  execution path (capability-first, paddock-or-refuse via `LaunchConfined`).
- `containment.RequireExecutionAuthority` (and its method form
  `Capability.RequireAuthority`) is the version-skew guard: no paddock backend for a
  paddock placement ⇒ refuse before dispatch.
- `run.VerifyHandler(ctx, probe, h)` is the negotiation conformance probe,
  mirroring `verify_execution_negotiation`: a handler handed a backend-less
  capability must refuse with `ErrExecutionAuthorityRequired` /
  `ErrPaddockNotEstablished`; a handler that returns nil (ran without authority) or
  refuses with an unrelated error fails conformance. Verified by
  `TestVerifyHandler_RefusesWithoutCapability`.
- A "pure-data" path never receives a `RunContext` — the structural analogue of a
  driver that doesn't implement `prepare_bound`. Driver *binding* remains out of
  scope (host integration).

## 8. Phase 3 — Recorded run operation + evidence persistence (implemented)

Python brackets dispatch in `runtime_operation_boundary`, yielding a durable
operation id the run records. Go's recording is `store.Append(ctx,
trace.TrustedAppendContext, trace.AppendBatch{...})` returning an
`AppendReceipt`. As built:

1. **The append seam.** [`(*Scope).AppendFact`](../../fact.go) appends a capture
   record on the scope's owner path — the runtime's general "record on this
   scope" seam.
2. **The run record.** `run` appends one `shepherd.run.executed.v1`
   (`run.SchemaRunExecuted`) record per run, capturing `owner`, `parent_scope`,
   `isolation`, `disposition`, `authority_evidence` (the `confinement.Evidence`
   digest), `exit_code`, and `denied`. The wrap records it right after the
   handler returns, before any discard/disposition, so a failed run is still
   recorded. Verified by `TestReversible_RecordsRunOperation` and
   `TestReversible_RecordsEvidence`.
3. **Evidence persistence** — the digest Plan 1 produced on `ExecResult` is now
   written into the trace, closing Plan 1 §16.4's "surfaced but not persisted".
4. **Schema discipline.** `shepherd.run.executed.v1` is a **runtime-level** record
   — the kernel validates only that a schema ref is non-empty, so this needs no
   kernel ABI change.

## 9. Phase 4 — Dispositions + the non-reversible opt-out

- **Merge (default):** for an isolated run this is **physical propagation via
  settlement** — `mgr.Seal(ctx, runScope.ID())` then
  `mgr.Settle(ctx, out.ID(), SettleSelect)`, falling back to `SettleApply` when
  the parent moved. The review gate runs via `ProposeMerge` before sealing and
  records the explicit proposal/decision pair; a denial discards the run and
  returns `*SupervisorDeniedError`.
- **Seal:** `out := mgr.Seal(ctx, runScope.ID())`; the caller settles later
  (`SettleSelect`/`Apply`), enabling fork-and-choose (`Outcome.Output`).
- **`authority_merge`:** the `DecisionProvider` gate (Plan 3 Phase 4) — the run's
  proposed changes are decided before integration; a denial discards the run and
  returns `*SupervisorDeniedError`. A missing `Decision` is a validation error.
- **`non_reversible_run`:** run on the parent scope with `isolation="ground"`,
  recorded so the defaulted-Permissive/loud-opt-out population stays countable
  (the same discipline as `MayResolution.Source`). Refuse if combined with a
  non-merge disposition (mirrors `_command_envelope.py:126`).

## 10. Phase 5 — Test & conformance matrix

Mirrors the Python PD2 behaviours; runs against the real runtime (+ Landlock where
a paddock is exercised). Build the harness on a git worktree sandbox.

| Done | Test | Action | Expected |
|---|---|---|---|
| ✅ | `TestReversible_MergeDefault` | reversible run on a repo | child scope, `Isolation=="isolated"`, parent's tree untouched, then merged |
| ✅ | `TestReversible_NonReversibleOptOut` | `NonReversible: true` | runs on parent scope, `Isolation=="ground"`, write lands directly |
| ✅ | (capability-after-fork) | inspect `RunContext.Capability` in the handler | capability carries `Isolation=="isolated"` (asserted in `MergeDefault`) |
| ✅ | `TestReversible_RefusesWithoutBackend` | paddock placement, nil backend | `ErrPaddockNotEstablished`; **no scope allocated** |
| ✅ | `TestReversible_HandlerRunsConfined` | handler runs a paddocked command | paddocked run; `Result.Evidence` non-empty |
| ✅ | `TestReversible_DiscardOnFailure` | handler returns error | `mgr.Discard`; parent unchanged |
| ✅ | `TestReversible_MergeDeniedByReviewer` | rejecting `MergeReviewer` | `*SupervisorDeniedError`; parent untouched |
| ✅ | `TestReversible_SealDisposition` | `DispositionSeal` | `Outcome.Output` sealed; caller `Settle(SettleSelect)` integrates it |
| ✅ | `TestReversible_RecordsRunOperation` | any run | one `shepherd.run.executed.v1` record carries isolation/disposition/exit |
| ✅ | `TestReversible_RecordsEvidence` | paddocked run | the record carries the authority-evidence digest |
| ✅ | `TestReversible_AuthorityMerge`/`_Denied` | `DispositionAuthorityMerge` + `DecisionProvider` | approve→settle; deny→discard + `*SupervisorDeniedError` |
| ✅ | `TestReversible_NonReversibleWithSealRefused` | `NonReversible` + `Seal` | validation error (mirrors `_command_envelope.py:126`) |
| ✅ | `TestVerifyHandler_RefusesWithoutCapability` | handler handed a backend-less capability | refuses; a nil return or wrong error fails conformance |
| 🚧 | `TestReversible_Recovers` | run, restart, `RecoverScopes` | run scope + evidence recoverable from the trace |

## 11. Parity Comparison: Reversible Execution

| Capability / Attribute | Python Shepherd | Go (this plan) |
|---|---|---|
| **Reversible-by-default execution** | `_run_execution_bound` PD2 | **Parity**: `run.Reversible` |
| **Isolated fork as the default** | `ForkHints(isolated=True)`, no knob | **Parity**: `ForkIsolated`, no knob |
| **Capability minted after fork (proof)** | `ExecutionCapability` post-fork | **Parity**: `RunContext.Capability` post-fork |
| **Discard on failure** | `owner.discard(run_scope)` | **Parity**: `mgr.Discard` |
| **Merge on success** | `owner.merge` / `prepare_merge` diff | **Parity**: isolated propagation via `Seal` + `SettleSelect`/`SettleApply` (physical, digest-stable); review gate via `ProposeMerge` |
| **Seal disposition** | `success_disposition="seal"` | **Parity**: `DispositionSeal` + `Settle*` |
| **authority_merge disposition** | `merge_with_authority` | **Parity (simplified)**: `DecisionProvider` gate; cohort lane is a follow-up |
| **Loud non-reversible opt-out** | `non_reversible_run` | **Parity** (to build): `Options.NonReversible` + recorded |
| **Recorded operation bracket** | `runtime_operation_boundary` | **Parity**: `shepherd.run.executed.v1` run record (`(*Scope).AppendFact`) |
| **Authority evidence persisted** | `permission_plan_digest` in the run | **Parity**: the digest is written into the run record |
| **Execution-bound driver protocol** | `ExecutionBoundDriver`/`prepare_bound` | **Adapted**: `run.Handler` + `RunContext`; driver binding out of scope |
| **Recovery across restart** | trace-projected registry | **Parity/stronger**: `RecoverScopes` |
| **Review/supervision gate** | `merge:proposed` events | **Parity/stronger**: `Supervisor.ReviewMerge`, `MergeReviewer` |

## 12. Sequencing, Dependencies, Risks

**Order:** Phase 1 (wrap) → Phase 2 (handler seam) → Phase 4 (dispositions, merge
first) → Phase 3 (recorded operation + evidence) → Phase 5 (tests throughout).

**Dependencies:** Plan 1 (done); the runtime's Scope/Settlement/Merge-gate
(done); a git worktree sandbox with `WithExec(landlock, paddock)` for paddocked runs.

**Risks / mitigations:**
- *Orphan worktrees on early failure* → pre-flight placement/availability before
  `ForkIsolated` (§5.3 #1); `DestroyScopeSandbox` on every error path.
- *Scope leak if discard fails* → discard is best-effort but logged; the run
  records the failure.
- *Half-run deltas auto-merging* → default is discard-on-failure, never
  auto-merge (mirrors PD2's comment).
- *New record shape touching the frozen kernel ABI* → prefer an existing record
  kind; a new one follows the schema discipline (§8.3).
- *Heuristic denial (Plan 1 §16.4)* → treat a denied run as failed for
  disposition (discard), unless the caller opts into keeping failed output.

## 13. Decisions (resolved by the implementation)

1. **Package** — ✅ **`run`** (`github.com/buchenberg/goatherd-runtime/run`).
   Keeps the root hub lean; the wrap is orchestration.
2. **Recorded-operation shape** — ✅ **runtime-level record, no kernel change**:
   `shepherd.run.executed.v1` appended via `(*Scope).AppendFact` (schema refs are
   validated only for non-emptiness).
3. **`authority_merge`** — ✅ **implemented** (Plan 3 Phase 4): the
   `DecisionProvider` gate over the run's proposed changes.
4. **Evidence carriage** — ✅ **both**: on `ExecResult` (Plan 1, the seam) and in
   the run record (Plan 2, the durable evidence).
5. **Handler granularity** — ✅ **multi-command handler**: a handler may issue
   several confined calls; the wrap owns the transaction, not the command.
