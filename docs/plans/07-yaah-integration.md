# Implementation Plan: yaah Integration — Retained Sub-agents, Confined (Go)

> **The principle (non-negotiable).** The **orchestrating agent runs as normal on
> the HOST** — its own workspace, unconfined, unchanged. Shepherd's mechanics —
> **retained runs, settlement, and Landlock confinement** — apply to **SUB-AGENTS
> only.** This is the model proven in Terminal Bench: the driver solves the task
> on the host; the dispatched run *bodies* are what get isolated, retained, and
> undone. Confining the orchestrator would not be that model, and this plan does
> not.
>
> **The spine (what "shepherd as intended" means here).** Shepherd's unit is the
> **retained run**: every dispatched body executes over a *bound world*, its
> output is sealed to one side as a **proposal**, and it is **settled once** —
> while a **jail**, compiled from the run's grants, enforces writes at the
> syscall *inside that world*. So this plan is ordered **retention first,
> confinement as an attribute of the retained run** — because a jail bounds
> writes *inside a bound world*, and you cannot confine a body that has no bound
> world yet. **Isolation precedes confinement.**
>
> **This is the boundary Python ships, not an analogy** (§11).

## 1. Objective & Context

`yaah` now depends on `shepherd-runtime-go v0.17.0` + `shepherd-kernel-go
v0.17.0` (Phase 0, done — the modules are split, and yaah was ported onto them),
plus `croft v0.3.0`. But it **already owns the settlement machine** — on one
lane:

- **`supervised_task` *is* a retained run.** It forks an isolated child scope
  owning its own worktree and "seals it as a **retained output** — frozen"
  (`internal/tools/supervised_session.go:103-118`); the tree is never touched
  until you settle, and settlement is `choose` / `select` / `apply` / `discard`.
  That is exactly Python Shepherd's retained run + settlement (§11).
- **`spawn_subagent` is not.** Without a `workdir` it inherits the host validator
  and mutates the host directly — no bound world, no retention, no settlement.
  **Shepherd has no such lane.**

**The two gaps to intent are therefore only two:** (a) confinement is not
attached to the retained run, and (b) **retention is not the default** — it is
opt-in per role, while a direct, unretained, host-mutating lane is the norm.

This plan makes the **retained run the unit of sub-agent work**, attaches the
**kernel boundary** to it, and leaves the orchestrator's host path untouched.

## 2. The Split (explicit)

| Actor | World | Exec | Settlement | Status |
|---|---|---|---|---|
| **Orchestrator** (main registry) | host `localWorkspace(pv)` (`tools.go:333`) | `exec.CommandContext`, **unconfined** | n/a — it is the driver | **unchanged** — runs as normal on the host |
| **Sub-agent, retained** (default target) | forked worktree — the **bound world** | `LaunchConfined`, rooted at that world | **sealed proposal**; `choose`/`select`/`apply`/`discard`, once | **this plan** |
| **Sub-agent, shared** (`agents.subagent.mode: shared`, opt-out) | host `localWorkspace` | unconfined (today) | none — direct host mutation | **migration only** |
| **Isolated (containerd) session** | croft `sandboxWorkspace` | in-container | `apply_out` | **unchanged** — the container is the boundary |

The orchestrator row is the Terminal-Bench contract: **do not touch it.**

## 3. Where the Split Is Wired

- Orchestrator workspace: `internal/tools/tools.go:333` (`newLocalWorkspace(pv)`) — **no confiner**.
- Sub-agent bound-world + confiner: `internal/agent/runner/runner.go:971-987` — the `WithWorkspaceRoot(workdir)` block.
- Sub-agent scope fork: `createSubAgentScope`/`subAgentSandbox` (`runner.go:759-922`) — `ForkIsolated` worktree.
- Retention & settlement: `internal/tools/supervised_session.go` (seal + `choose`/settle), `internal/tools/apply_out.go` (isolated accept seam).
- Dispatch surface: `spawn_subagent` / `supervised_task` (`internal/tools/supervised_task.go`; `wiring.go:542-726` — roles split by `roles[name].Supervised`).

## 4. Scope & Non-Goals

**In scope:** release mechanics (Phase 0); a confinement seam spike on the
existing retained lane (Phase 1); **the retention spine** — the retained run as
the default for all sub-agents (Phase 2); confinement on the unified retained
path + placement/`doctor` (Phase 3); the north-star outline (Phase 4).

**Non-goals:**
- **Confining the orchestrator.** Explicitly out — it must run as normal on the host.
- **Changing the isolated (containerd) session** — already bounded by the container.
- **Network confinement by default** — the default is a *write*-jail (`allow_all`); egress is opt-in.
- **A `ReadOnly` sub-agent binding.** yaah has no signature-grant DSL; the worktree is one `ReadWrite` binding (§11 A).
- **Windows** (unsupported) and **macOS enforcement** (Seatbelt is Plan 4 Phase 1); both stay advisory.

## 5. Completion Checklist

**Phase 1 — confinement seam spike**

| # | Target | Test |
|---|---|---|
| 1 | the orchestrator's `localWorkspace` exec is **unconfined** (host) | `TestOrchestrator_ExecUnconfined` |
| 2 | the retained-lane workspace carries a Landlock confiner | `TestRetained_WorkspaceConfined` |
| 3 | a retained command writing in its world succeeds; outside is refused | `TestRetained_ExecWriteOutsideDenied` |
| 4 | advisory fallback when no backend (macOS/Windows/ABI<1) | `TestRetained_ExecAdvisory` |
| 5 | the trampoline hook is installed | (build/run) |

**Phase 2 — retention spine**

| # | Target | Test |
|---|---|---|
| 6 | **every** sub-agent dispatch returns a **proposal** (a sealed retained output), not a host mutation | `TestSpawn_ReturnsProposal` |
| 7 | the host tree is **untouched** until settlement | `TestSpawn_HostUntouchedUntilSettle` |
| 8 | settlement is **once** — a second settle is refused | `TestSpawn_SettleOnce` |
| 9 | `agents.subagent.mode: shared` preserves the legacy direct lane | `TestSpawn_ModeSharedOptOut` |

**Phase 3 — confinement on the unified retained path**

| # | Target | Test |
|---|---|---|
| 10 | the confiner rides the one retained launch (no special-casing) | `TestRetained_ConfinerOnUnifiedPath` |
| 11 | the **resolved** placement is recorded on the trace (advisory ≠ jailed) | `TestSubAgent_RecordsResolvedPlacement` |
| 12 | config `agents.subagent.placement: auto\|jail\|advisory` selects behavior | `TestConfig_Placement` |
| 13 | *(optional)* a **per-dispatch** placement override, matching Python's per-`run` `placement=` | `TestDispatch_PlacementOverride` |
| 14 | `yaah doctor` reports the capability | `TestDoctor_Containment` |

## 6. Architecture

### 6.1 The retained run is the unit (the spine)

A sub-agent dispatch yields a **proposal** — the five parts of a retained run,
all of which `supervised_task` already realizes and `spawn_subagent` must be
folded onto:

```
dispatch(sub-agent) →
  basis        a fork point       (git worktree created at the current tree)
  bound world  the worktree       (the ONLY writable root; the orchestrator's repo is not granted)
  body         launched confined  (LaunchConfined, rooted at the worktree)
  output       a sealed proposal  (the worktree delta, frozen — the host is untouched)
  settlement   once               (choose / select / apply / discard; consume-once)
```

Nothing about the *orchestrator* changes: it still edits its own host tree with
the ordinary tools. What changes is that a sub-agent's work stops being a
side-effect on that tree and becomes a **value the orchestrator settles**.

### 6.2 The retained launch gains a confiner

`localWorkspace` gains an optional confiner; the orchestrator's instance never
sets it, a retained sub-agent's instance does.

```go
type localWorkspace struct {
    pv       *PathValidator
    confiner containment.Backend      // nil ⇒ the orchestrator's host path (unconfined)
    place    confinement.Placement    // auto | paddock | advisory
    network  containment.NetworkPolicy
}

// orchestrator (unchanged):
func newLocalWorkspace(pv *PathValidator) *localWorkspace { return &localWorkspace{pv: pv} }

// retained sub-agent path:
func newConfinedLocalWorkspace(pv *PathValidator, c containment.Backend, place confinement.Placement, net containment.NetworkPolicy) *localWorkspace {
    return &localWorkspace{pv: pv, confiner: c, place: place, network: net}
}
```

`Exec` chooses by whether a confiner is present:

```go
func (w *localWorkspace) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
    if w.confiner == nil || w.place == confinement.PlacementAdvisory {
        return w.advisoryExec(ctx, req)   // the orchestrator's host path — TODAY'S BEHAVIOR
    }
    spec := containment.ConfinementSpec{WritableRoots: []string{w.WorkDir()}, Network: w.network}
    res, err := containment.LaunchConfined(w.confiner, spec, containment.LaunchRequest{
        Command: append([]string{req.Command}, req.Args...), WorkingRoot: w.WorkDir(),
        Cwd: cwdOr(req.Cwd, w.WorkDir()), Stdin: req.Stdin,
    })
    if err != nil { return ExecResult{ExitCode: -1, Stderr: err.Error()}, nil }
    if req.SeparateStreams { return ExecResult{res.ExitCode, res.Stdout, res.Stderr}, nil }
    return ExecResult{res.ExitCode, res.Stdout + res.Stderr, ""}, nil
}
```

### 6.3 Wiring

- **Phase 1 (narrow):** attach the confiner in the sub-agent registry's
  workdir/isolated block (`runner.go:971-987`) — the *existing* retained lane —
  after `reg.SetPathValidator(pv)`.
- **Phase 3 (unified):** once Phase 2 has folded `spawn_subagent` onto the
  retained path, the confiner is attached on the **one** retained launch; the
  Phase-1 special case is removed.

The orchestrator's `newLocalWorkspace(pv)` (tools.go) is untouched — its `Exec`
stays `advisoryExec`.

### 6.4 The trampoline hook (mandatory)

`landlock` confines by re-exec'ing the running binary; add to `yaah/main.go`:

```go
if code, handled := landlock.MaybeRunTrampoline(os.Args); handled { os.Exit(code) }
```

Any captured entry point (test binaries) adds the same line to `TestMain`.

## 7. Phases

### Phase 0 — Release mechanics ✅ done

The prerequisite: publish the split modules and port yaah onto them.

1. ✅ **`shepherd-runtime-go v0.17.0`** tagged (annotated, on `main`) and
   published; the tag's `release-verify` CI confirms it resolves from a clean
   consumer.
2. ✅ **`sandbox/landlock/go.mod` replace dropped** (PR #2) — the nested module
   now resolves the runtime from the proxy; **`sandbox/landlock/v0.1.0`** tagged
   and verified.
3. ✅ **yaah ported** (PR #336). This was **not** a version bump: kernel
   `v0.17.0` is *post-extraction* (trace-only — no `ScopeManager`, `sandbox/`,
   `substrate/`, `task/`, `supervisor/`), so yaah had to move onto the runtime.
   A **pure import rewrite** — 44 files, 89/89 lines, no source changes:
   `.../shepherd-kernel-go/trace → .../shepherd-kernel-go` (package `trace`, 40);
   `.../shepherd-kernel-go → .../shepherd-runtime-go` (30);
   `.../sandbox`, `.../sandbox/git`, `.../supervisor` → the runtime (19).
   `go.mod` pins both at `v0.17.0`. Green on ubuntu/macos/windows.

   The suite passing with only import edits is the proof that runtime `v0.17.0`
   is API-compatible with the runtime code yaah already used.

**Follow-ups noted during Phase 0 (not blocking):** the runtime's
`release-verify` CI scans only the **root** `go.mod` and only fires on `v*` tags,
so nested modules are uncovered — the exact hole that let the Landlock `replace`
linger; the runtime `CHANGELOG` still reads `[Unreleased]` (the kernel convention
renames it after tagging); and Plan 7's own branch is unmerged.

### Phase 1 — Confinement seam spike (thin, on the existing retained lane)

The runtime primitives are done; the **yaah** wiring is unproven. This phase
de-risks only the seam, on the lane that already exists (`supervised_task`),
with the smallest blast radius and **no behavior flip**.

1. Add the confiner fields + `newConfinedLocalWorkspace` (§6.2).
2. Attach it in the sub-agent registry's workdir block (§6.3), writable root =
   the worktree.
3. Install the trampoline hook (§6.4); validate stream capture
   (`SeparateStreams`) and the rc-3 fail-closed path.
4. **Leave the orchestrator untouched** — assert it (#1).

### Phase 2 — The retention spine (the parity move)

> **Concrete design:** [`07-phase2-retention-spine.md`](07-phase2-retention-spine.md)
> — the kernel primitive, the `spawn_subagent` → proposal fold, and the new
> `proposal` settlement tool.

Make the retained run the default for **all** sub-agents; `spawn_subagent`
returns a **proposal** instead of mutating the host.

1. Route every non-container dispatch through the retained path: fork-isolated
   bound world (worktree) → body → **seal** a retained output → return the
   proposal.
2. `spawn_subagent` reports "what I changed" as a proposal the orchestrator
   settles (`choose`/`select`/`apply`/`discard`); `supervised_task` stays the
   supervised spelling of the same primitive.
3. Config `agents.subagent.mode: retained | shared` (default **`retained`**;
   `shared` preserves the terminal-bench baseline during migration).
4. **Acceptance:** the orchestrator loop is unchanged, and yaah's own harness
   (`docs/shepherd-benchmark.md`) still passes. A retained run that is never
   settled leaves the host tree untouched.

### Phase 3 — Confinement on the unified retained path

1. Move the confiner from the Phase-1 special case onto the **one** retained
   launch; writable root = the bound world, so the kernel boundary and the
   settlement boundary are the same boundary.
2. Config `agents.subagent.placement: auto | jail | advisory` (→ `PlacementAuto`
   / `PlacementPaddock` / `PlacementAdvisory`), and `agents.subagent.network:
   allow_all | deny_all | broker` (default `allow_all`). `jail` **fails closed**.
3. **Record the resolved placement** on the sub-agent's trace (requested ·
   resolved · enforcement), so advisory is never read as jailed.
4. `internal/doctor` gains a containment check (`landlock.New().Available()`),
   reporting `native-syscall-deny (ABI N)` or "advisory".

### Phase 4 (north star) — `oversee(implement)`

Shepherd's deferred target: one sub-agent (or the orchestrator) supervises
another, retrying until acceptable — `run → inspect the proposal → keep or
discard → retry`, as a first-class combination rather than the hand-written loop
it is today. Outline only. This is where retained runs + settlement + supervision
meet.

## 8. Test & Conformance Matrix

| Phase | Test | Action | Expected |
|---|---|---|---|
| 1 | `TestOrchestrator_ExecUnconfined` | a host command outside the workspace | **runs** (unchanged) — the orchestrator is not confined |
| 1 | `TestRetained_ExecWriteOutsideDenied` | a retained command writes outside its world | refused; no file |
| 1 | `TestRetained_ExecWriteInside` | writes in its world | lands |
| 1 | `TestRetained_WorkspaceConfined` | inspect the retained registry's workspace | has a confiner; the orchestrator's does not |
| 1 | `TestRetained_ExecAdvisory` | no backend (simulated) | runs unconfined; behavior unchanged |
| 1 | `TestRetained_LaunchFailureIsNegativeOne` | a non-executable command | `ExitCode == -1` |
| 2 | `TestSpawn_ReturnsProposal` | a `spawn_subagent` dispatch | returns a sealed proposal |
| 2 | `TestSpawn_HostUntouchedUntilSettle` | dispatch, edit outside the world | host tree unchanged until settled |
| 2 | `TestSpawn_SettleOnce` | settle, then settle again | second refused |
| 2 | `TestSpawn_ModeSharedOptOut` | `mode: shared` | legacy direct lane preserved |
| 2 | (harness) | `docs/shepherd-benchmark.md` scenarios | orchestrator loop unregressed |
| 3 | `TestRetained_ConfinerOnUnifiedPath` | inspect the one retained launch | confiner present, no special case |
| 3 | `TestSubAgent_RecordsResolvedPlacement` | advisory vs jailed dispatch | the trace records which resolved |
| 3 | `TestConfig_Placement` | `auto`/`jail`/`advisory` | selects the placement; `jail` fails closed |
| 3 | `TestDoctor_Containment` | `yaah doctor` | reports the ABI / advisory |

## 9. Risks & Mitigations

- **Confining the orchestrator by mistake** — the whole point is the split.
  *Mitigation:* the confiner attaches only on the retained launch;
  `TestOrchestrator_ExecUnconfined` pins it.
- **Flipping retention regresses the proven loop.** *Mitigation:* the
  `mode: shared` kill-switch, and the benchmark harness as the Phase-2 gate.
- **Per-sub-agent worktree cost** (a fork + a seal per dispatch). *Mitigation:*
  measure against the harness; `shared` remains available for exploratory work.
- **Version skew (kernel v0.16.0 → v0.17.0).** *Mitigation:* Phase 0's build gate.
- **Release tagging dependency.** *Mitigation:* Phase 0, or an interim local replace.
- **macOS / Windows** (no jail). *Mitigation:* `auto` → advisory ⇒ today's behavior.
- **The trampoline hook is load-bearing** (rc 3 → `ErrPaddockNotEstablished`).
  *Mitigation:* install it and assert (#5).
- **Stream fidelity.** yaah's local path combines stdout+stderr unless
  `SeparateStreams`; the jail captures them apart. *Mitigation:* concatenate for
  the default; tools that parse stdout already set `SeparateStreams`.
- **The bound world must equal the confiner's writable root**, or the kernel
  boundary and the settlement boundary disagree. *Mitigation:* derive both from
  the one fork what the retained run owns (§7.3 #1).

## 10. Decisions

1. **The orchestrator stays unconfined on the host** (Terminal-Bench fidelity). *Non-negotiable.*
2. **The retained run is the unit of sub-agent work; confinement is an attribute of it** — so retention leads and confinement rides it. **Isolation precedes confinement.** *The spine.*
3. **Retention is the default for every sub-agent** (`agents.subagent.mode: retained`); host-sharing is an explicit opt-out, not the norm.
4. **Phase 1 is a thin seam spike only** — on the existing retained lane, no behavior flip — so the Landlock wiring is proven before the retention spine lands.
5. **The confiner is attached in one place** — first the retained-lane block (Phase 1), then the unified retained launch (Phase 3).
6. **Placement is per-dispatch-capable** (`auto` default, `jail` fails closed, `advisory` visible), and the **resolved** placement is recorded on the trace.
7. **The writable root is the sub-agent's bound worktree = one `ReadWrite` binding.** No signature-grant DSL, no `ReadOnly` binding (§11 A).
8. **Trampoline hook in `yaah/main.go`** (and any captured entry point).
9. **Where this plan lives:** `shepherd-runtime-go/docs/plans/` (the numbered series); yaah's docs link to it.

## 11. Python-parity: how Shepherd applies the boundary

Plan 7's split is the shipped Python shape, not a new design. In Python Shepherd
the **driver is plain Python** — "composition is function call … no pipeline
object, no graph DSL, no orchestration framework"
([`first-shepherd-app.md`](file:///home/greg/Code/Agentic/Frameworks/shepherd/docs/shepherd/tutorials/first-shepherd-app.md))
— and it runs **in-process on the host, unconfined**. The syscall jail attaches
to the **run body**: `workspace.run(task, …)` executes a task and launches that
body with `execution.launch_confined(...)`
([`runtime_provider.py`](file:///home/greg/Code/Agentic/Frameworks/shepherd/packages/dialect/src/shepherd_dialect/workspace_control/runtime_provider.py)),
"writes directly only for advisory placement." A **bodyless** task's body is
served by a provider-run agent; a **bodied** task runs as ordinary in-process
Python. So the confined unit is always a *dispatched run body* — exactly yaah's
sub-agent — and never the driver.

The mapping, and where yaah is coarser:

| Python Shepherd | yaah / Go target | Match |
|---|---|---|
| driver = plain Python, host, unconfined | main agent (orchestrator) on the host | ✅ the load-bearer |
| `workspace.run(task, …)` = a **retained run**; body served by a provider agent | a sub-agent dispatch = a **retained run** (Phase 2) | ✅ **the spine** |
| run body launched `launch_confined` under a jailed placement | retained body via `LaunchConfined` | ✅ **this plan** |
| placement is **per-run** (`auto`/`advisory`/`jail`), default `auto`; `jail` fails closed | `agents.subagent.placement` → `auto`/`paddock`/`advisory` | ⚠️ **B — per-config, not per-dispatch** (#13) |
| writable roots lowered from the **grant surface** (`may=` → `ConfinementSpec`): `ReadWrite`→writable, `ReadOnly`→nothing; deny-closed; nested/overlapping roots fail closed | writable root = the sub-agent's worktree | ⚠️ **A — coarser: one worktree = one `ReadWrite` binding; no `ReadOnly` binding** |
| `may=None → Permissive`, recorded with provenance (loud default) | default network `allow_all` (write-jail) | ≈ (§10.6) |
| Landlock = self-restriction-before-exec runner, sentinel `__landlock_confine_exec__`, rc 3 → `JailNotEstablished` | trampoline, `MaybeRunTrampoline`, rc 3 → `ErrPaddockNotEstablished` | ✅ exact |
| Landlock governs **writes only** (reads/exec not governed); **no container**, unprivileged | write-jail; `allow_all` default network | ✅ |
| Seatbelt = `sandbox-exec -p` (macOS) | Plan 4 Phase 1 | ✅ planned |
| remote/cloud placements (E2B, Modal, Daytona, K8s, Prime) | croft/containerd tier (Plan 5) | ✅ same tier |
| **every** run is retained; settle **once** (`select`/`apply`/`release`/`discard`) — the shipped core | `supervised_task` already does this; Phase 2 makes it the **default** for every sub-agent | ✅ **the spine** |
| run record carries `requested_placement`/`resolved_placement`/`enforcement`, so advisory is never mistaken for jailed | advisory fallback exists; **not recorded** | ❌ **D — add** (#11) |

### What the comparison settled

- **A — grant granularity.** Python lowers a *per-binding* grant surface to a
  multi-root deny-closed spec ([`dialect/confinement.py`](file:///home/greg/Code/Agentic/Frameworks/shepherd/packages/dialect/src/shepherd_dialect/confinement.py),
  `lower_grants_to_confinement`; `OverlappingBoundRootsError`). yaah's sub-agent
  has no signature, so its one worktree is the whole grant. This is **more
  permissive** than Shepherd (which would leave a non-granted path read-only) but
  is the honest mapping: the orchestrator's repo is not granted to the sub-agent
  at all — the worktree is. Kept, and documented (§10.7).
- **B — placement is per-run in Python, per-config here.** Default `auto`
  matches; a per-dispatch override is the natural seam (#13).
- **C — retention is the primary mechanism, and it is now the spine.** In Python
  *nothing* touches the working tree until settled, and the jail bounds what a
  retained run may write *inside* its bound world. Confinement without retention
  matches only halfway — which is why Phase 2 (retention) precedes Phase 3
  (confinement on the unified path), rather than confinement riding a
  special-case lane.
- **D — legibility.** Python records requested/resolved placement so a reader
  "can never mistake [advisory] for jailed." Recorded here too (#11).
