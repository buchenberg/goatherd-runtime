# Implementation Plan: Authority Declarations & the Permission-Plan Compiler (Go)

> **Scope.** This is **Plan 3** of the containment/execution work. Plan 1
> (`01-landlock-containment.md`) shipped the paddock, its contract, and the
> `may=`/grant *lowering*. Plan 2 (`02-reversible-runs.md`) shipped the
> reversible-run wrap. Two gaps remain in the authority spine:
>
> 1. **Where grants come from.** Plan 1's `BindingRootGrant` is a pure input with
>    no producer; Python derives grants from task-signature annotations.
> 2. **The monitor-assignment compiler.** Python's `install()` compiles one
>    authority declaration into a `PermissionPlan` naming *both* the syscall paddock
>    (pre-action) and the carrier check-at-commit, and the run driver consumes it.
>
> Plan 3 builds the Go authority-declaration surface, the compiler, the evidence
> chain, and the `authority_merge` disposition — which then unblocks Plan 2
> Phase 4.
>
> **Status:** Phases 1 (declarations) and 2 (the compiler) are implemented in
> [`sandbox/confinement`](../../sandbox/confinement) — [`declaration.go`](../../sandbox/confinement/declaration.go)
> and [`plan.go`](../../sandbox/confinement/plan.go). This **supersedes** Plan 1's
> minimal `PermissionPlan`/`JailPlan` (the git backend now cites
> `PlanForSpec(...).Digest()`). Phases 3 (consume in `run` + evidence chain) and 4
> (`authority_merge`) remain.

## 1. Objective & Context

Plan 1 shipped (in `sandbox/confinement`): `ResolveMay`/`MayResolution`,
`LowerMay`, `LowerGrants`, `ValidateDisjointRoots`, `Placement`, and a **minimal**
`PermissionPlan` (`MatchDigest`, `JailPlan`, `Digest`, `SpecDigest`,
`Evidence`, `JournalRecord`). Plan 2 shipped `run.Reversible`, which records the
evidence digest.

What is missing:

- **A producer for grants.** `BindingRootGrant` exists; nothing builds it from a
  declaration. Python's dialect extracts `May[GitRepo, ReadWrite]` from task
  signatures (`run_driver.py`), rebases per-binding grants, and lowers them.
- **The two-monitor plan.** Python's `install()` — the seam where the paddock lane
  and the carrier lane meet — assigns `JAIL_MONITOR` (pre-action) **and**
  `CARRIER_MONITOR` (check-at-commit), with completeness/tamper bases, and exposes
  `plan.confinement` (behaviour-preserving) and `plan.digest` (authority evidence).
  Go's `PermissionPlan` is paddock-only and carries none of that.
- **The `authority_merge` disposition.** Plan 2 defines it and refuses; it needs
  the plan digests and a decision provider.

## 2. What "Parity" Means Here (the reference surface)

| Python | File | Role |
|---|---|---|
| `MayResolution`, `resolve_may`, `lower_may_resolution_to_confinement` | `dialect/confinement.py` | whole-workspace lowering (Plan 1 has it) |
| `BindingRootGrant`, `lower_grants_to_confinement`, `validate_disjoint_roots` | `dialect/confinement.py` | per-binding lowering (Plan 1 has it) |
| task-signature `May[Repo, ReadOnly\|ReadWrite]` → grants | dialect annotations / `run_driver.py` | **the grant *source*** |
| `MonitorAssignment` (`completeness_basis`, `tamper_basis`, `route`, `evidence`) | `dialect/permission_plan.py:101` | one monitor's responsibility + basis |
| `PermissionPlan` (`.confinement`, `.digest`, `.to_descriptor`) | `dialect/permission_plan.py:154` | the compiler output + evidence |
| `install(authority, working_path)` | `dialect/permission_plan.py:259` | the two-monitor compiler |
| `CarrierCheckAuthority` | `dialect/permission_plan.py` | the carrier-only plan input |
| `JAIL_MONITOR` / `CARRIER_MONITOR` | `dialect/permission_plan.py:59` | monitor ids (Plan 1 has them) |
| `merge_with_authority(...)` | `_vcscore_lifecycle.py:1317` | the authority-merge disposition |
| `PermissionPlan` evidence validation | `_vcscore_lifecycle.py` (`validate_permission_plan_evidence`) | digest/descriptor/route checks |

## 3. Scope & Explicit Non-Goals

**In scope:** the Go authority-declaration surface, the two-monitor
`PermissionPlan` compiler, plan consumption in `run` + the evidence chain, and a
Go-shaped `authority_merge` disposition.

**Non-goals:**
- **Egress broker / network enforcement** — Plan 4 (the plan only *names* the
  carrier and paddock; the network axis stays spec-only).
- **Seatbelt** — Plan 4.
- **seccomp** — Plan 5.
- **The full vcs-core carrier-candidate machinery** of `merge_with_authority`
  (per-substrate prepared candidates, cohort adoption). Go's `authority_merge` is
  a **simplified analogue**: a decision provider gating the sealed output before
  settlement. The reference's richer lane is recorded as a follow-up.
- **Host wiring / driver binding** (`run_driver.py`'s shape) — host integration.

## 4. Parity Map (completion checklist)

| # | Python symbol | Go target | Test | Done |
|---|---|---|---|---|
| 1 | task-signature `May[…]` → grants | `confinement.Declaration` (Go authoring surface) | `TestDeclaration_MayAndGrants` | ✅ |
| 2 | rebased per-binding grants | `confinement.BindingRootGrant` producer | `TestInstall_MayAndGrants` | ✅ |
| 3 | `MonitorAssignment` | `confinement.MonitorAssignment` (full fields) | `TestMonitorAssignment_Present` | ✅ |
| 4 | `PermissionPlan` (paddock-only today) | `confinement.PermissionPlan` (two-monitor) | `TestInstall_MayAndGrants`, `TestMonitorAssignment_Present` | ✅ |
| 5 | `PermissionPlan.confinement` | `(*PermissionPlan).Confinement()` | `TestPermissionPlan_ConfinementIsThePaddockSpec` | ✅ |
| 6 | `PermissionPlan.digest` / `to_descriptor` | `(*PermissionPlan).Digest()` / `Descriptor()` | `TestPermissionPlan_DigestStable` | ✅ |
| 7 | `install(authority, working_path)` | `confinement.Install(decl, workingPath)` | `TestInstall_MayAndGrants` | ✅ |
| 8 | `CarrierCheckAuthority` | `confinement.CarrierCheckAuthority` | `TestInstall_CarrierOnly` | ✅ |
| 9 | `install` refuses un-enforceable clause | `Install` fail-closed (propagates lowerer errors) | `TestInstall_RefusesUnsupported` | ✅ |
| 10 | evidence validation (`validate_permission_plan_evidence`) | `confinement.ValidatePlanEvidence` | `TestValidatePlanEvidence` | ✅ |
| 11 | `permission_plan` drives the run's confinement | `run.Options.Plan` → `RunContext.Confinement = plan.Confinement()` | `TestReversible_PlanDrivesConfinement` | 🚧 Phase 3 |
| 12 | `permission_plan_digest` in the run | the run record carries the **plan** digest (not just the spec digest) | `TestReversible_RecordsPlanDigest` | 🚧 Phase 3 |
| 13 | `success_disposition == "authority_merge"` | `run` authority-merge disposition + `DecisionProvider` | `TestReversible_AuthorityMerge` | 🚧 Phase 4 |
| 14 | `merge_with_authority` (cohort lane) | Go simplified analogue (decision gate) | (row 13); richer lane → follow-up | 🚧 Phase 4 |

## 5. Architecture

### 5.1 The compiler seam (mirrors `run_driver.py`)

Python: `install(authority, working_path) → PermissionPlan`; `plan.confinement`
feeds `launch_confined`; `plan.digest` is authority evidence. Go:

```
confinement.Install(decl, workingPath) → PermissionPlan{paddock, carrier}
        │                                          │
        │ .Confinement() ─► run.RunContext.Confinement ─► LaunchConfined
        │ .Digest()      ─► run evidence ─► shepherd.run.executed.v1
        └ .Descriptor()  ─► citable authority record
```

Behaviour-preserving: `plan.Confinement()` is exactly the spec the Plan-1
lowerers already produce; routing through the plan changes *where the spec comes
from*, not the spec.

### 5.2 The two monitors

- **`PaddockMonitor`** (wire `syscall_paddock`; Python's `JAIL_MONITOR`/`syscall_jail`
  — see the vocabulary note) — pre-action; carries the `ConfinementSpec`; the
  Landlock syscall deny enforces it.
- **`CARRIER_MONITOR`** (`carrier_check_at_commit`, `commit`) — the completeness
  backstop for managed writes that bypass cooperative paths. In Go this maps to
  the runtime's existing check-at-commit seam: `Scope.Seal` + the Plan-2
  reviewer/`DeltaApplier` gate. It lowers to no `ConfinementSpec`.

### 5.3 Migration note

Plan 1's minimal `confinement.PermissionPlan` (`MatchDigest`/`Assignments`/
`JailPlan`) is **superseded** by the full shape. This is a breaking change to a
pre-1.0 type; `confinement.JailPlan` is replaced by `Install`. Callers (tests,
`run`) migrate.

## 6. Phase 1 — Authority declarations

A Go-native authoring surface (no Python decorator syntax):

```go
package confinement

// Declaration is an authority declaration: either a whole-workspace may= profile
// or a set of per-binding grants (mutually exclusive).
type Declaration struct {
    May    string              // "ReadOnly" | "Permissive" | "" (default Permissive)
    Grants []BindingRootGrant  // per-binding (ReadWrite/ReadOnly), absolute roots
    Match  string              // the authority-surface digest (provenance)
}

func MayDeclaration(may string) Declaration
func GrantsDeclaration(match string, grants ...BindingRootGrant) Declaration
```

The *source* in Python is the task-signature annotation; in Go it is whatever a
host/tool builds a `Declaration` from (a task config, a request, a policy file).
The declaration is data; the compiler is the only thing that turns it into a
plan.

## 7. Phase 2 — The permission-plan compiler

```go
package confinement

type MonitorAssignment struct {
    Monitor           string   // PaddockMonitor | CarrierMonitor
    Timing            string   // "pre_action" | "commit"
    CompletenessBasis string
    TamperBasis       string
    Confinement       *containment.ConfinementSpec // paddock only
    Route             string   // carrier route, e.g. "carrier_diff"
    Evidence          []Evidence // (key, digest) pairs
}

type PermissionPlan struct {
    Fallback    string              // "enforce"
    Assignments []MonitorAssignment
}

func (p PermissionPlan) Monitor(name string) (MonitorAssignment, bool)
func (p PermissionPlan) Confinement() (containment.ConfinementSpec, error) // paddock's spec
func (p PermissionPlan) Digest() string
func (p PermissionPlan) Descriptor() map[string]any // schema "shepherd.permission-plan.v1"

type CarrierCheckAuthority struct {
    EffectiveMatchDigest      string
    AuthoritySurfacePlanDigest string
    Route                     string
    CompletenessBasis         string
    TamperBasis               string
}

func Install(decl Declaration, workingPath string) (PermissionPlan, error)
func InstallCarrierCheck(a CarrierCheckAuthority) PermissionPlan
func ValidatePlanEvidence(digest string, descriptor map[string]any, route, matchDigest, surfaceDigest string) error
```

`Install` lowers the declaration via the Plan-1 lowerers (whole-workspace via
`LowerMay`, per-binding via `LowerGrants`), then assigns paddock + carrier. Both
lowerers already fail closed, so `Install` never returns an un-enforceable paddock
clause.

## 8. Phase 3 — Consume the plan in `run` (evidence chain)

- `run.Options` gains `Plan *confinement.PermissionPlan` (mutually exclusive with
  the raw `Confinement`/`MatchDigest`). When set, `RunContext.Confinement =
  plan.Confinement()`.
- The run record gains `permission_plan_digest` and `permission_plan_descriptor`
  (the plan digest, not just the spec digest), so a run's authority is citable as
  a whole.
- Chain: declaration → plan digest → run record; a settlement can re-validate the
  chain with `ValidatePlanEvidence`.

## 9. Phase 4 — `authority_merge` (unblocks Plan 2 Phase 4)

A Go-shaped analogue of `merge_with_authority`:

```go
// DecisionProvider judges a run's proposed changes before integration.
type DecisionProvider func(ctx context.Context, proposal shepherd.MergeProposal) error

// Options gains:
Decision DecisionProvider // required for DispositionAuthorityMerge
```

Flow: `Reversible` (isolated) runs the handler, `Seal`s the output, builds the
proposal via `ProposeMerge`, calls `Decision`; on approval `SettleSelect`/
`SettleApply`; on denial `SettleDiscard` (authority-safe cleanup) and returns the
denial. The plan digests ride in the run record.

**Deferred to a follow-up:** the reference's per-substrate carrier-candidate
cohort lane (`ContainmentSubstrate`, `EffectRecord`, cohort adoption). Go's gate
over the sealed output is the simplified analogue and is recorded as such.

## 10. Test & conformance matrix

| Done | Test | Action | Expected |
|---|---|---|---|
| ✅ | `TestDeclaration_MayAndGrants` | build both declaration forms | correct fields; mutually exclusive |
| ✅ | `TestInstall_MayAndGrants` | `Install` over `may=` and grants | two-monitor plan; paddock spec matches the direct lowerer |
| ✅ | `TestInstall_CarrierOnly` | `InstallCarrierCheck` | carrier-only plan; no paddock assignment |
| ✅ | `TestInstall_RefusesUnsupported` | unsupported `may=` / both May+Grants | propagates `ErrUnsupportedMayProfile` / refuses the mixed declaration |
| ✅ | `TestPermissionPlan_ConfinementIsThePaddockSpec` | `plan.Confinement()` | equals the Plan-1 lowerer's spec (behaviour-preserving) |
| ✅ | `TestPermissionPlan_DigestStable` | digest twice, then change the match | stable; changes with input |
| ✅ | `TestMonitorAssignment_Present` | paddock + carrier assignments | correct timing; paddock carries the spec, carrier does not |
| ✅ | `TestValidatePlanEvidence` | matching / mismatched evidence | nil / `ErrPlanEvidence` |
| 🚧 | `TestReversible_PlanDrivesConfinement` | run with `Options.Plan` | `RunContext.Confinement` is the plan's paddock spec (Phase 3) |
| 🚧 | `TestReversible_RecordsPlanDigest` | run with a plan | the run record carries `permission_plan_digest` (Phase 3) |
| 🚧 | `TestReversible_AuthorityMerge` | approving / denying `DecisionProvider` | approves→settle; denies→discard + denial (Phase 4) |
| 🚧 | `TestReversible_AuthorityMergeRequiresDecision` | `DispositionAuthorityMerge`, nil `Decision` | validation error (Phase 4) |

## 11. Parity Comparison: Authority Policy

| Capability / Attribute | Python Shepherd | Go (this plan) |
|---|---|---|
| Grant source | task-signature `May[…]` annotations | `confinement.Declaration` (Go-native) |
| Two-monitor plan compiler | `install()` | `confinement.Install` |
| Monitor responsibility bases | `completeness_basis`/`tamper_basis`/`route` | same fields |
| Plan digest / descriptor as evidence | `PermissionPlan.digest`/`to_descriptor` | `Digest()`/`Descriptor()` |
| Evidence validation | `validate_permission_plan_evidence` | `ValidatePlanEvidence` |
| `authority_merge` | `merge_with_authority` (carrier-cohort lane) | **simplified**: decision gate over the sealed output; cohort lane → follow-up |
| Network axis in the plan | `NetworkPolicy`/broker grants | spec-only here (Plan 4 enforces) |

## 12. Sequencing, Dependencies, Risks

**Order:** Phase 1 (declarations) → Phase 2 (compiler) → Phase 3 (consume +
evidence) → Phase 4 (`authority_merge`, unblocks Plan 2 Phase 4).

**Dependencies:** Plan 1 (`confinement` lowerers), Plan 2 (`run`), the runtime's
`Seal`/`Settle`/`DeltaApplier`.

**Risks / mitigations:**
- *Plan/driver divergence* → `TestPermissionPlan_ConfinementIsPaddockSpec` pins
  `plan.Confinement()` to the direct lowerer's output (behaviour-preserving).
- *Un-enforceable clause* → the lowerers fail closed; `Install` propagates.
- *Evidence spoofing* → `ValidatePlanEvidence` re-checks digest ↔ descriptor ↔
  route ↔ match digest.
- *Breaking `PermissionPlan`* → pre-1.0; migrate `JailPlan` callers in the same
  change.

## 13. Open Questions / Decisions

1. **Declaration shape**: a struct (`Declaration`) vs functional options.
   *Recommend the struct — it is data, and the compiler is the only consumer.*
2. **Carrier wiring**: map `CARRIER_MONITOR` to `Scope.Seal` + the Plan-2
   reviewer, or to `DeltaApplier`/`CommitMerge`. *Recommend `Seal` + reviewer,
   since isolated runs already settle through it.*
3. **`Plan` vs raw `Confinement` on `Options`**: keep both, mutually exclusive
   (plan sets both the spec and the digest). *Recommend both.*
4. **`authority_merge` scope**: the decision gate now, the cohort lane as a
   follow-up. *Recommend the gate — it captures the disposition without porting
   vcs-core's carrier machinery.*
