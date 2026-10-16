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

Nothing here is new substrate. `shepherd-kernel-go` ships the whole retained-run
machine, and `yaah` already imports it:

| Kernel symbol | Role |
|---|---|
| `ScopeManager.ForkIsolated(parentID, childOwnerID, snapshot, sb)` | a child scope owning its own sandbox — the **bound world** |
| `ScopeManager.Seal(ctx, scopeID) (*RetainedOutput, error)` | freeze the scope's workspace delta |
| `RetainedOutput.ID()` / `.Diff()` / `.Changes()` / `.Action()` | the **proposal** (id, diff, changed paths, disposition) |
| `ScopeManager.Settle(ctx, outputID, action)` | settle **once**: `SettleSelect` / `SettleApply` / `SettleRelease` / `SettleDiscard` |
| `ErrOutputConsumed` | consume-once guard (a second settle refuses) |
| `ErrApplyConflict` | `SettleApply` refuses overlapping paths |
| `MergePolicy` / `CommitMerge` | the merge gate (approve/deny a settlement) |

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

## 9. Open questions

- **Read-only roles.** `spawn_subagent` returns the report; in `retained` mode it
  also returns an (empty-changeset) proposal. Shepherd retains *every* run, so
  the default is to retain; the envelope simply has `files: []`.
- **Cost of a worktree per dispatch.** A fork + seal per sub-agent is heavier
  than a shared exec. Measure against the harness; `mode: shared` remains for
  pure-recon roles.
- **The non-review `supervised_task` retry loop.** Keep it as-is in Phase 2
  (re-dispatch is now `discard` + spawn); unify the supervised retry into the
  retained primitive in Phase 4 (`oversee`).
