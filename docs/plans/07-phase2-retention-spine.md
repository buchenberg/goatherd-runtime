# Plan 7 · Phase 2 design — the retention spine

> Companion to [`07-yaah-integration.md`](07-yaah-integration.md) §7.2. This is
> the concrete shape of Phase 2: **how `spawn_subagent` folds onto the retained
> run**, and **where settlement lands on the orchestrator's tool surface.**

## 0. What Phase 2 is

Make the **retained run** the default unit of sub-agent work: every dispatch
executes over a *bound world* (an isolated worktree), its output is **sealed**
into a proposal, and the orchestrator **settles it once**. The orchestrator's own
host path is untouched.

## 1. The primitive already exists in the kernel

Nothing here is new substrate. `goatherd-kernel` ships the whole retained-run
machine, and `yaah` already imports it:

| Kernel symbol | Role |
|---|---|
| `ScopeManager.ForkIsolated(parentID, childOwnerID, snapshot, sb)` | a child scope owning its own sandbox — the **bound world** |
| `ScopeManager.Seal(ctx, scopeID) (*RetainedOutput, error)` | freeze the scope's workspace delta |
| `RetainedOutput.ID()` / `.Diff()` / `.Changes()` / `.Action()` | the **proposal** (id, diff, changed paths, disposition) |
| `ScopeManager.Settle(ctx, outputID, action)` | settle **once**: `SettleSelect` / `SettleApply` / `SettleRelease` / `SettleDiscard` |
| `ErrOutputConsumed` | consume-once guard (a second settle refuses) |
| `ErrApplyConflict` | `SettleApply` refuses overlapping paths |
| `ProposeMerge` / `RecordMergeProposal` / `RecordMergeDecision` | explicit review provenance: proposal on the child, verdict on the parent |

**yaah already drives all of it — in exactly one place:** `forkVariants` /
`chooseVariant` in `internal/tools/supervised_session.go`. `provisionVariant`
creates a worktree, `ForkIsolated`s a scope on it, seeds the fork point
(`sb.Apply(forkPoint)`), and runs the sub-agent with `subParams.Workdir = wtPath`;
`forkVariants` then `Seal`s each branch; `chooseVariant` `SettleSelect`/`SettleApply`s
the winner and `SettleDiscard`s the loser.

So Phase 2 is not new machinery. It is **generalizing the fork path into the
default dispatch** and **exposing settlement to the orchestrator**.

## 2. Today vs target

| Surface | Today | Phase 2 (`mode: retained`) |
|---|---|---|
| `spawn_subagent` | runs against the host/shared workspace; returns the result text | forks a bound world, runs **there**, seals a **proposal**, returns it |
| host tree | mutated directly by the sub-agent | **untouched until settlement** |
| `supervised_task` (non-review) | in-place + checkpoint/rollback/retry | unchanged (the retry spelling) |
| `supervised_task review:true` | interactive `fork`/`choose`/`accept` | unchanged — the **interactive superset** of the same primitive |

## 3. The fold: `spawn_subagent` → a proposal

The retained dispatch is `provisionVariant` + `runVariantPrompt` + `Seal` with the
orchestrator as the reviewer, inlined into the tool call:

```
dispatchRetained(ctx, prompt, role, subParams):
    wt   = worktreePath(...)                                    # bound world
    sb   = git.NewWorktreeSandbox(repoPath, wt); sb.Create(ctx)
    child= mgr.ForkIsolated(sessionScopeID, ownerID, nil, sb)   # isolated child scope
    sb.Apply(ctx, forkPoint)                                    # seed at the basis
    subParams.Workdir = wt
    result, err = runner(ctx, prompt, subParams)                # the body
    out, serr  = mgr.Seal(ctx, child.ID())                      # freeze the delta
    return proposalEnvelope{ out, result, err, serr }
```

Sealing happens **even on a run error** — the partial work stays reviewable —
exactly as `forkVariants` already does. Teardown belongs to the settlement
(`SettleSelect`/`SettleApply` merge then release the worktree; `SettleDiscard`
destroys it), so a stranded worktree is impossible.

### The proposal envelope

`spawn_subagent`'s existing result becomes a proposal; the sub-agent's report is
still carried, joined by what it *changed*:

```json
{
  "status": "proposal",
  "proposal_id": "out_01H...",
  "result": "…the sub-agent's report…",
  "diff": "…unified diff (clipped)…",
  "files": ["internal/foo/foo.go", "internal/foo/foo_test.go"],
  "basis": "…fork digest…",
  "parent_moved": false,
  "next": ["select", "apply", "release", "discard"]
}
```

`parent_moved` is the drift signal the orchestrator needs to choose `select`
(fast-forward) vs `apply` (three-way), mirroring Shepherd's own rule.

## 4. Where settlement lands: a `proposal` tool

A new tool on the orchestrator's surface — the direct analogue of Shepherd's
`run select | apply | release | discard`. One verb, one proposal id:

```json
{ "action": "select|apply|release|discard|list|show",
  "proposal_id": "out_01H...",
  "note": "why" }
```

| action | kernel | semantics |
|---|---|---|
| `select` | `SettleSelect` | fast-forward onto the parent; **fails closed** if the parent moved since the basis |
| `apply` | `SettleApply` | three-way onto a moved parent; **path-disjoint or refused** (`ErrApplyConflict`) |
| `release` | `SettleRelease` | keep the landed work, drop the record |
| `discard` | `SettleDiscard` | throw the work away; destroy the worktree |
| `list` / `show` | — | inspect open proposals (mirrors the `subagent_jobs` tool) |

Placement in the wiring: registered beside `SupervisorTool`, under the **same
guard** (`scopeMgr != nil && sessionSandbox == nil`), carrying the same
`ScopeManager` and the session's `sessionScopeID` (the settlement parent).

**Why a separate tool, not a `supervisor` action:**

- The supervisor tool supervises a *running* scope (`inject`/`halt`/`status`) and
  drives the *interactive review cycle*; settlement is a different verb on a
  *finished* run.
- Shepherd keeps them apart too: `workspace.run(...)` is one surface,
  `run select/apply/discard` is another.
- One file, one concern (yaah's convention).

The prompt delta (`prompts.SubAgentToolsGuidance`) gains the proposal workflow:
*dispatch → read the proposal → `select`/`apply`/`discard`*. And the guidance must
say the host tree is **not** changed until settlement — the single most
surprising behavior change.

## 5. Config & rollout

- **`agents.subagent.mode: retained | shared`** (default **`retained`**).
- Per-role `roles[name].supervised: true` still selects the interactive review
  spelling (`supervised_task review:true`) — a superset of the same primitive.
- **`shared` is the escape hatch** and the benchmark baseline: it is today's
  behavior, verbatim.
- **Rollout gate:** yaah's own harness (`docs/shepherd-benchmark.md`) — the
  orchestrator loop must not regress. Phase 2 lands behind the flag first, flips
  the default once the harness is green.

## 6. Invariants (what the tests pin)

1. **The host tree is untouched until settlement.**
2. `select` fails closed on a moved parent; `apply` refuses on path overlap.
3. Settlement is **once** — a second settle is `ErrOutputConsumed`.
4. The orchestrator's own exec stays **unconfined** (the Phase 1 guard).
5. `mode: shared` reproduces today's behavior exactly.

## 7. Edge cases

- **Sub-agent run error** → seal the partial; the proposal is reviewable
  (`forkVariants` already does this).
- **Provisioning failure** (a stale worktree on the path) → the existing
  `recovery.IsRegisteredWorktree` guard, already in `provisionVariant`.
- **Orchestrator edits during a run** → `parent_moved`; `select` refuses →
  `apply` (disjoint) or `discard`.
- **`review:true` session already open** → unchanged (one at a time).
- **Background/parallel spawn** → each dispatch gets its own proposal; N
  proposals, N settlements.
- **Confinement (Phase 3)** → the body's `Exec` runs confined, rooted at the same
  worktree the proposal is sealed against — the kernel boundary and the
  settlement boundary are one boundary.

## 8. Factoring note

`provisionVariant`, `worktreePath`, `runWorktreeBootstrap`, and the
`recovery.IsRegisteredWorktree` guard are currently private to the review
session. Phase 2 lifts them into a shared **retained-run provisioner** used by
both the review session and the default dispatch — one code path, two callers.

## 9. Decisions

1. **Retain always — including read-only roles.** Every dispatch produces a
   proposal; a role that writes nothing simply yields an empty-changeset
   proposal (`files: []`). This is Shepherd's rule (it retains *every* run), and
   it keeps the surface uniform — the orchestrator always settles, never asks
   "was this one retained?". The fork's cost is controlled by the **carrier**
   (§10), not by skipping the fork. `mode: shared` survives as a config-level
   escape hatch, never as a per-role default.
2. **The non-review `supervised_task` retry loop stays as-is in Phase 2** —
   re-dispatch is now `discard` + spawn — and that *is* the retry-until-acceptable
   loop, with the orchestrator as the predicate (see Phase 4's record: the
   reviewer-in-the-loop built there was removed).

## 10. Cost of isolation: a worktree per dispatch

Pin **1** makes every dispatch fork a bound world, so the cost question is real.
It is smaller than it looks. A retained dispatch does:

1. capture the basis — `git add -A` (stat-cache-aware → O(changed));
2. `git worktree add --detach <wt> HEAD` — a full checkout of the tracked tree;
3. `Apply(forkPoint)` — `reset --hard` + `clean -fd` + `read-tree --reset -u`
   (a *second* materialization, to seed the fork point);
4. `runner(...)` — the sub-agent (the actual work);
5. `Seal` — capture + diff (O(changed));
6. settle — merge onto the parent, then `git worktree remove` (teardown).

**Measured** on this machine (ext4, **no** CoW; git 2.55), setup + teardown only
(the sub-agent's runtime is excluded):

| repo | tracked | checkout | `worktree add` | apply step | teardown | **≈ overhead** |
|---|---|---|---|---|---|---|
| goatherd-runtime | 92 | 1.2 M | 0.02 s | 0.02 s | ~0.01 s | **~0.05 s** |
| naah | 260 | 1.6 M | 0.02 s | 0.02 s | ~0.01 s | **~0.05 s** |
| yaah | 688 | 6.2 M | 0.31 s | 0.10 s | 0.01 s | **~0.42 s** |
| pi | 1902 | 28 M | 0.13 s | 0.32 s | — | ~0.45 s |
| aider | 691 | 76 M | 0.20 s | 0.39 s | — | ~0.6 s |
| terminal-bench | 2474 | 170 M | 0.70 s | 0.56 s | — | ~1.3 s |
| opencode | 8448 | 150 M | 0.51 s | 0.77 s | — | ~1.3 s |
| hermes | 18302 | 245 M | 1.00 s | 1.41 s | 0.20 s | **~2.6 s** |

**Reading it:** the cost tracks *checkout bytes*, not file count; the object
store is **shared**, so a worktree duplicates no objects; the delta side
(capture/diff) is O(changed) and effectively free. For repos to ~18 k files the
overhead is **~0.05 s – 2.6 s**, against a sub-agent whose run is usually
seconds-to-minutes. **The exception is `WorktreeBootstrap`** (`npm install`,
`go mod download`) — a per-dispatch bootstrap is where minutes hide, and it
dominates everything above.

### It is a carrier choice, not a fixed cost

`ForkIsolated` takes any `sbx.Sandbox`, and Plan 5 defines the carrier family —
so "a worktree per dispatch" is a *default*, not the mechanism:

| carrier | cost | where |
|---|---|---|
| `git worktree` | full checkout ×2 (measured above); shared object store | all platforms (today's default) |
| `clonefile` | `clonefile(2)` — O(1) metadata, blocks shared until write | macOS APFS |
| reflink / overlay | O(1) upper over a shared lower | Linux btrfs/XFS (reflink), containerd overlayfs (`croft`) |
| `copy` | plain recursive copy — O(tree) | universal floor |

On APFS the same dispatch is a metadata clone; on a reflink filesystem, a
near-free CoW copy. The measured table is the **worst case** (ext4, git
worktree) — the ceiling, not the floor.

### Mitigations, in priority order

1. **Carrier selection** — the provisioner takes a carrier, not a hardcoded
   worktree: clonefile (APFS) → reflink/overlay (Linux) → worktree (portable) →
   copy (floor). This is the main lever, and it is Plan 5's, not new work here.
2. **Warm reuse for sequential units** — a multi-unit sub-agent (or a supervised
   session) holds **one** worktree for its lifetime, not one per unit.
3. **Bootstrap discipline** — cache it, or skip it for read-only roles; it is
   the single largest term.
4. **Amortize parallel fan-out** — N branches can share one lower through the
   overlay carrier.
5. **Measure in the harness** — add a scenario reporting dispatch overhead and
   set a budget (e.g., < 1 s for repos under ~5 k files, bootstrap excluded).
6. **`mode: shared`** stays available as the config-level escape hatch when a
   session would rather not pay the fork at all.

**Bottom line:** retain-always is affordable because the fork is a *carrier
choice*. The plan is to pay the worktree cost in Phase 2 (portable, correct) and
let the cheap carriers land with Plan 5 — the ceiling measured above is the
number to beat.
