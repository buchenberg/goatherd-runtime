# Implementation Plan: Execution-Bound Reversible Runs (Go)

> **Scope.** This is **Plan 2** of the containment/execution work. Plan 1
> (`01-landlock-containment.md`) delivered the Linux containment component —
> the jail, its contract, the authority lowering, and confined `Exec`. What Plan 1
> deliberately fenced out is *how a confined execution is run*: Python Shepherd
> wraps execution in a **reversible transaction** (isolated fork → capability →
> recorded operation → merge/discard). This plan brings that wrap to Go.
>
> **Key finding motivating this plan:** the Go runtime **already implements the
> reversibility substrate** (`ScopeManager.ForkIsolated`, `Capture/Apply`,
> `Seal`, `Settle`, merge gate, recovery). This plan is a *composition* layer that
> joins those primitives to the Plan-1 confined `Exec` — not new substrate.
>
> **Status:** Phase 1 (the reversible-run wrap) is implemented in
> [`run/reversible.go`](../../run/reversible.go), with 9 passing tests. Phases 2
> (handler seam), 3 (recorded operation + evidence persistence), and 4
> (`authority_merge`) remain. Merge propagates physically via `Seal` +
> `SettleSelect`/`SettleApply` — `CommitMerge` refuses isolated children (§9).

## 1. Objective & Context

Plan 1 shipped:
- `containment.Backend` + `landlock` (confined `Exec`, fail-closed probe, denial
  classification, authority evidence digest).
- `confinement` (may= / grants → `ConfinementSpec`, placement, `PermissionPlan`).
- `GitSandbox.Exec` with `WithExec` (jailed/advisory) and `ExecMay`/`ExecGrants`.

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
| review-gated merge | `(*ScopeManager).CommitMerge(ctx, childID string, review MergeReviewer) error` (+ `ProposeMerge`, `SupervisorDeniedError`) |
| discard | `(*ScopeManager).Discard(childID string) error` |
| confined exec | `containment.LaunchConfined`, `containment.Capability`, `git.WithExec` |

## 3. Scope & Explicit Non-Goals

**In scope:** the reversible run wrap, the execution-bound handler seam, the
recorded-operation boundary + evidence persistence, the dispositions
(merge default, seal, non-reversible opt-out), and the test matrix.

**Non-goals (deferred to later plans):**
- **`authority_merge`** depends on the permission-plan **compiler** (jail +
  carrier assignment, `effective_match_digest`, `permission_plan_descriptor`) —
  that is Plan 3. Phase 4 defines its signature and refuses it loudly until Plan 3.
- Egress enforcement, Seatbelt, and the container tier (unchanged non-goals from
  Plan 1 §3).
- Host wiring (`yaah`) / sub-agent supervision integration.

## 4. Parity Map (completion checklist)

| # | Python symbol | Go target | Test |
|---|---|---|---|
| 1 | `owner.fork(..., ForkHints(isolated=True))` | `run.Reversible` → `ForkIsolated` | `TestReversible_ForkIsolated` |
| 2 | non-isolated branch (`non_reversible_run`) | `Options.NonReversible` → run on parent, `isolation="ground"` | `TestReversible_NonReversibleOptOut` |
| 3 | capability minted after fork | `RunContext.Capability` constructed post-fork | `TestReversible_CapabilityAfterFork` |
| 4 | `require real-execution authority` | `containment.RequireExecutionAuthority` | `TestReversible_RefusesWithoutBackend` |
| 5 | `prepare_bound` dispatch | `Handler func(ctx, *RunContext) (Result, error)` | `TestReversible_HandlerRunsConfined` |
| 6 | discard on failure | `mgr.Discard(runScope.ID())` on handler error | `TestReversible_DiscardOnFailure` |
| 7 | `owner.merge(run_scope, scope)` (default disposition) | isolated: `mgr.Seal` + `SettleSelect` (fallback `SettleApply`) | `TestReversible_MergeDefault` |
| 8 | `success_disposition == "seal"` | `Options.Disposition = DispositionSeal` → `mgr.Seal` | `TestReversible_SealDisposition` |
| 9 | `runtime_operation_boundary` | `run` records a run operation (append) | `TestReversible_RecordsRunOperation` |
| 10 | authority digest recorded | evidence (`confinement.Evidence`) persisted in the run record | `TestReversible_RecordsEvidence` |
| 11 | `verify_execution_negotiation` under skew | `run` refuses without a capability backend | (row 4) |
| 12 | `success_disposition == "authority_merge"` | `DispositionAuthorityMerge` — **refuses until Plan 3** | `TestReversible_AuthorityMergeDeferred` |

## 5. Architecture

### 5.1 Where it lives

A new package `github.com/buchenberg/shepherd-runtime-go/run`, importing the root
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
   `ForkIsolated`, so a jail-less host never allocates a worktree.
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
    DispositionAuthorityMerge Disposition = "authority_merge" // Plan 3
)

type Options struct {
    OwnerID       string
    NonReversible bool                          // loud opt-out: run on the parent, isolation="ground"
    Disposition   Disposition                   // default DispositionMerge
    Placement     confinement.Placement         // default PlacementAuto
    Confinement   *containment.ConfinementSpec  // nil ⇒ PermissiveFor(workdir)
    MatchDigest   string                        // authority-surface digest for evidence
    Reviewer      shepherd.MergeReviewer        // merge disposition
    NewSandbox    func(runID string) (sandbox.Sandbox, error) // required for reversible runs
    Snapshot      any
    MaxDiffLines  int
}

type RunContext struct {
    Scope      *shepherd.Scope
    Capability containment.Capability
}

type Result struct {
    Value    map[string]any
    Evidence string // permission-plan digest, when a jailed run recorded one
}

type Outcome struct {
    Scope       *shepherd.Scope
    Output      *shepherd.RetainedOutput // DispositionSeal
    Result      Result
    Isolation   string                   // "isolated" | "ground"
    Disposition Disposition
}

func Reversible(ctx context.Context, mgr *shepherd.ScopeManager, parent *shepherd.Scope,
    opts Options, handler Handler) (Outcome, error)

type Handler func(ctx context.Context, rc *RunContext) (Result, error)
```

The handler runs confined commands through `rc.Capability.LaunchConfined(spec, req)`
or `rc.Scope.Sandbox().Exec(ctx, req)`; it never needs to know about forks or
settlement — that is the wrap's job.

## 7. Phase 2 — The execution-bound handler seam

Go has no driver protocol; the analogue is a **handler** that structurally
receives execution authority through `RunContext`, mirroring `prepare_bound`
receiving the `ExecutionCapability` per call (never stored). The negotiation rule
maps to:

- A handler is *execution-bound* by definition (it receives `RunContext`).
- `containment.RequireExecutionAuthority` is the version-skew guard: no jail
  backend for a jail placement ⇒ refuse before dispatch (Plan 1 §16.5).
- A "pure-data" path (no handler) never receives a `RunContext` — the structural
  analogue of a driver that doesn't implement `prepare_bound`.

`verify_execution_negotiation` has a Go analogue for **host-supplied handlers**:
`run.VerifyHandler(h Handler) error` probes that an execution-bound handler
refuses to run when handed a capability with no backend (never a silent
in-process fallback). Kept minimal; the driver *binding* machinery is out of
scope (host integration).

## 8. Phase 3 — Recorded-operation boundary + evidence persistence

Python brackets dispatch in `runtime_operation_boundary`, yielding a durable
operation id the run records. Go's recording is `store.Append(ctx,
trace.TrustedAppendContext, trace.AppendBatch{...})` returning a receipt. Phase 3
adds:

1. **A run operation record** — an append bracketing the handler, capturing:
   `isolation`, `disposition`, `authority_evidence` (the `confinement.Evidence`
   digest), the confined `ExecResult.ExitCode`/`Denied`, and the run scope id.
2. **Evidence persistence** — the digest Plan 1 produced on `ExecResult` is
   written into that record, closing Plan 1 §16.4's "surfaced but not persisted".
3. **Schema discipline.** If the run operation needs a new record shape, it
   follows the kernel's schema rules (canonical JSON, content-addressed ids, law
   coverage) — same discipline as every other record. Preferred: use an existing
   record kind (a runtime fact) carrying the fields, avoiding a kernel ABI change.

## 9. Phase 4 — Dispositions + the non-reversible opt-out

- **Merge (default):** for an isolated run this is **physical propagation via
  settlement** — `mgr.Seal(ctx, runScope.ID())` then
  `mgr.Settle(ctx, out.ID(), SettleSelect)`, falling back to `SettleApply` when
  the parent moved. `CommitMerge` is *not* used: it refuses isolated children
  (their physical propagation belongs to settlement). The review gate runs via
  `ProposeMerge` before sealing; a denial discards the run and returns
  `*SupervisorDeniedError`.
- **Seal:** `out := mgr.Seal(ctx, runScope.ID())`; the caller settles later
  (`SettleSelect`/`Apply`), enabling fork-and-choose (`Outcome.Output`).
- **`authority_merge`:** Python's `merge_with_authority` (binding roots +
  decision provider + `effective_match_digest` + `permission_plan_digest` +
  descriptor). **Refuse loudly** (`ErrAuthorityMergeUnsupported`) until Plan 3
  (the permission-plan compiler) provides the digests. The signature is defined
  now so callers compile; the behaviour refuses.
- **`non_reversible_run`:** run on the parent scope with `isolation="ground"`,
  recorded so the defaulted-Permissive/loud-opt-out population stays countable
  (the same discipline as `MayResolution.Source`). Refuse if combined with a
  non-merge disposition (mirrors `_command_envelope.py:126`).

## 10. Phase 5 — Test & conformance matrix

Mirrors the Python PD2 behaviours; runs against the real runtime (+ Landlock where
a jail is exercised). Build the harness on a git worktree sandbox.

| Test | Action | Expected |
|---|---|---|
| `TestReversible_ForkIsolated` | reversible run on a repo | child scope created, `Isolation=="isolated"`, parent's tree untouched |
| `TestReversible_NonReversibleOptOut` | `NonReversible: true` | runs on parent scope, `Isolation=="ground"`, no fork record |
| `TestReversible_CapabilityAfterFork` | inspect `RunContext.Capability` | backend bound; factory called only after fork |
| `TestReversible_RefusesWithoutBackend` | jail placement, nil backend | `ErrExecutionAuthorityRequired` / `ErrJailNotEstablished`; **no scope allocated** |
| `TestReversible_HandlerRunsConfined` | handler writes in worktree | write lands in child; `ExecResult.AuthorityEvidence` non-empty |
| `TestReversible_DiscardOnFailure` | handler returns error | `mgr.Discard` called; parent unchanged; worktree destroyed |
| `TestReversible_MergeDefault` | handler succeeds | child merged into parent; changes land |
| `TestReversible_MergeDeniedByReviewer` | rejecting `MergeReviewer` | `*SupervisorDeniedError`; parent untouched |
| `TestReversible_SealDisposition` | `DispositionSeal` | `Outcome.Output` sealed; `Settle(SettleSelect)`/`SettleApply` integrate it |
| `TestReversible_RecordsRunOperation` | any run | a trace record carries isolation/disposition/exit |
| `TestReversible_RecordsEvidence` | jailed run | the record carries the permission-plan digest |
| `TestReversible_AuthorityMergeDeferred` | `DispositionAuthorityMerge` | refuses with `ErrAuthorityMergeUnsupported` |
| `TestReversible_NonReversibleWithSealRefused` | `NonReversible` + `Seal` | validation error (mirrors `_command_envelope.py:126`) |
| `TestVerifyHandler_RefusesWithoutCapability` | handler handed a backend-less capability | refuses, never in-process |
| `TestReversible_Recovers` | run, restart, `RecoverScopes` | run scope + evidence recoverable from the trace |

## 11. Parity Comparison: Reversible Execution

| Capability / Attribute | Python Shepherd | Go (this plan) |
|---|---|---|
| **Reversible-by-default execution** | `_run_execution_bound` PD2 | **Parity**: `run.Reversible` |
| **Isolated fork as the default** | `ForkHints(isolated=True)`, no knob | **Parity**: `ForkIsolated`, no knob |
| **Capability minted after fork (proof)** | `ExecutionCapability` post-fork | **Parity**: `RunContext.Capability` post-fork |
| **Discard on failure** | `owner.discard(run_scope)` | **Parity**: `mgr.Discard` |
| **Merge on success** | `owner.merge` / `prepare_merge` diff | **Parity**: isolated propagation via `Seal` + `SettleSelect`/`SettleApply` (physical, digest-stable); review gate via `ProposeMerge` |
| **Seal disposition** | `success_disposition="seal"` | **Parity**: `DispositionSeal` + `Settle*` |
| **authority_merge disposition** | `merge_with_authority` | **Deferred** to Plan 3 (compiler); refuses now |
| **Loud non-reversible opt-out** | `non_reversible_run` | **Parity** (to build): `Options.NonReversible` + recorded |
| **Recorded operation bracket** | `runtime_operation_boundary` | **To build** (Phase 3): run operation record |
| **Authority evidence persisted** | `permission_plan_digest` in the run | **To build** (Phase 3) |
| **Execution-bound driver protocol** | `ExecutionBoundDriver`/`prepare_bound` | **Adapted**: `run.Handler` + `RunContext`; driver binding out of scope |
| **Recovery across restart** | trace-projected registry | **Parity/stronger**: `RecoverScopes` |
| **Review/supervision gate** | `merge:proposed` events | **Parity/stronger**: `Supervisor.ReviewMerge`, `MergeReviewer` |

## 12. Sequencing, Dependencies, Risks

**Order:** Phase 1 (wrap) → Phase 2 (handler seam) → Phase 4 (dispositions, merge
first) → Phase 3 (recorded operation + evidence) → Phase 5 (tests throughout).

**Dependencies:** Plan 1 (done); the runtime's Scope/Settlement/Merge-gate
(done); a git worktree sandbox with `WithExec(landlock, jail)` for jailed runs.

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

## 13. Open Questions / Decisions

1. **Package**: `run` (proposed) vs methods on the root `shepherd` package. The
   wrap is orchestration; a `run` package keeps the hub lean. *Recommend `run`.*
2. **Recorded-operation shape**: reuse a runtime fact record vs a new typed
   record. *Recommend reuse first; add a typed record only if the fidelity
   demands it.*
3. **`authority_merge`**: define the signature now and refuse, or omit until
   Plan 3. *Recommend define-and-refuse so callers compile.*
4. **Evidence carriage**: on `ExecResult` (Plan 1) already; also on the run
   operation record (this plan). *Recommend both — the result is the seam, the
   record is the durable evidence.*
5. **Handler granularity**: one confined command per wrap vs a multi-command
   handler. *Recommend a handler may issue several confined calls; the wrap owns
   the transaction, not the command.*
