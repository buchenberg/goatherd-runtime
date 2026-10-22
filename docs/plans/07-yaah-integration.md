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

`yaah` now depends on `goatherd-runtime v0.17.0` + `goatherd-kernel
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

1. ✅ **`goatherd-runtime v0.17.0`** tagged (annotated, on `main`) and
   published; the tag's `release-verify` CI confirms it resolves from a clean
   consumer.
2. ✅ **`sandbox/landlock/go.mod` replace dropped** (PR #2) — the nested module
   now resolves the runtime from the proxy; **`sandbox/landlock/v0.1.0`** tagged
   and verified.
3. ✅ **yaah ported** (PR #336). This was **not** a version bump: kernel
   `v0.17.0` is *post-extraction* (trace-only — no `ScopeManager`, `sandbox/`,
   `substrate/`, `task/`, `supervisor/`), so yaah had to move onto the runtime.
   A **pure import rewrite** — 44 files, 89/89 lines, no source changes:
   `.../goatherd-kernel/trace → .../goatherd-kernel` (package `trace`, 40);
   `.../goatherd-kernel → .../goatherd-runtime` (30);
   `.../sandbox`, `.../sandbox/git`, `.../supervisor` → the runtime (19).
   `go.mod` pins both at `v0.17.0`. Green on ubuntu/macos/windows.

   The suite passing with only import edits is the proof that runtime `v0.17.0`
   is API-compatible with the runtime code yaah already used.

**Follow-ups noted during Phase 0 (not blocking):** the runtime's
`release-verify` CI scans only the **root** `go.mod` and only fires on `v*` tags,
so nested modules are uncovered — the exact hole that let the Landlock `replace`
linger; the runtime `CHANGELOG` still reads `[Unreleased]` (the kernel convention
renames it after tagging); and Plan 7's own branch is unmerged.

### Phase 1 — Confinement seam spike ✅ done

The runtime primitives were done; the **yaah** wiring was unproven. This phase
de-risked only the seam, on the lane that already exists (the sub-agent workdir
dispatch), with the smallest blast radius and **no behavior flip** (PR #337).

1. ✅ `localWorkspace` gained an optional confiner; `NewConfinedLocalWorkspace`
   runs `Exec` under the syscall jail with the workspace as the sole writable
   root. Placement resolves it: `paddock` fails closed, `auto` degrades to
   advisory, `advisory` never jails.
2. ✅ Attached in the sub-agent registry's workdir block (`runner.go`), **guarded
   on `workdir != ""`** — only the retained lane has a bound world to jail
   against. Gated on the new `agents.subagent.containment`
   (`off|advisory|auto|paddock`, **default `off`** → no behavior flip); an
   unknown value is rejected at config validation.
3. ✅ Trampoline hook installed in `main.go` (`TestMain` mirrors it for the
   `tools` tests). The rc-3 fail-closed path is covered (`paddock` + no backend
   refuses); a failed jail launch now returns its error, not a silent success.
4. ✅ The orchestrator stays untouched — `TestOrchestrator_ExecUnconfined` pins
   that a host write *outside* the workspace still succeeds.

Tests: `TestConfinedWorkspace_WriteOutsideDenied` proves **real** Landlock
(in-root write lands, outside is refused); `..._AdvisoryWhenNoBackend`,
`..._PaddockFailsClosed`, `..._BoundBackendUnavailable*` cover the placement
branches. Green on ubuntu/macos/windows.

**Carried to Phase 3:** the writable root is the worktree **only** (matching the
runtime's `PermissiveFor`). A confined sub-agent building with a cache outside
its worktree (`$GOCACHE`, `npm`) would be denied — which is why this lands off by
default, and why the writable-root set is a Phase-3 decision. Phase 3 answered
the cache half by **redirecting** the environment into the root (PR #339) and the
`/dev` half by granting that subtree (the runtime's `devWriteRoot`, shipped as
`sandbox/landlock v0.1.1`) — but the acceptance then found the half the redirect
cannot answer: a retained worktree's own git metadata lives in the *original*
repository, so index/object writes are refused. The default therefore stays
`off`; see Phase 3 below.

### Phase 2 — The retention spine ✅ done (the parity move)

> **Concrete design:** [`07-phase2-retention-spine.md`](07-phase2-retention-spine.md).

The retained run is now the **default** unit of sub-agent work (yaah PR #338):

1. ✅ `spawn_subagent` (retained mode) runs each sub-agent in an **isolated
   worktree** — its bound world — seeded at the dispatch basis, and **seals** its
   delta into a retained output, returned as a
   `proposal {proposal_id, result, diff, files, next}`.
2. ✅ A new **`proposal` tool** settles one proposal exactly once — `select`
   (fast-forward), `apply` (three-way, path-disjoint), `release`, `discard`,
   plus `list`/`show` — mirroring Shepherd's `run select | apply | release |
   discard`. `supervised_task` remains the interactive spelling of the same
   primitive.
3. ✅ `agents.subagent.mode: retained | shared` (default **`retained`**;
   `shared` is the migration/benchmark opt-out). Retained needs tracing + a root
   scope, and is local-mode only (the container is the substrate otherwise).
4. ✅ **Acceptance:** the host tree is untouched until settlement (pinned by
   `TestRetained_HostUntouchedUntilSettle`; `select` lands it, `discard`/`release`
   do not). Retained-mode prompt guidance tells the orchestrator to read and
   settle. Green on ubuntu/macos/windows.

The naive first draft committed to `main` by mistake (branch protection caught
it); the work was moved onto a branch and landed via PR #338.

### Phase 3 — Confinement on the unified retained path

> **Status.** **Complete** (yaah PRs #339, #343, #344, #345). The confiner rides
> the retained workdir lane, the **env redirect + scrub** is in, the **resolved
> placement is recorded** on the trace (`yaah.subagent.confinement.v1`), `yaah
> doctor` reports the capability, and the **default is `auto`** — the acceptance
> ran, blocked the flip while a real gap was open, and the gap was settled as a
> design decision rather than by widening the jail (below). The config key stayed
> **`containment`** (shipped in Phase 1), not the `placement` spelling below.

1. ✅ The confiner rides the one retained launch — a retained dispatch sets the
   worktree as the sub-agent's `Workdir`, and the confiner is attached on that
   lane, so the kernel boundary and the settlement boundary are the same
   boundary. Config: `agents.subagent.containment: auto | off | advisory |
   paddock` (alias `jail`); `network` defaults to `allow_all`. `paddock` fails
   closed.
2. ✅ Config `agents.subagent.network: allow_all | deny_all | broker` (+
   `network_hosts`), with `allow_all` the default (yaah PR #348). `broker` starts
   a session broker, pins egress to its port, routes through it
   (`HTTP(S)_PROXY`), and admits only the allowlist. A restrictive posture is
   **refused** when it cannot be applied — no usable jail, `containment: off`, a
   workdir-less dispatch, or Landlock ABI < 4 — rather than running open, and the
   tools that reach the network from yaah's own process (`http`, `webfetch`) are
   omitted from a restrictive host-lane dispatch. Two limits stated rather than
   implied: the pin is port-scoped (netns needed for loopback-only), and the
   isolated lane's egress is the container's (see the config reference).
3. ✅ **The resolved placement is recorded** on the sub-agent's trace
   (`yaah.subagent.confinement.v1`: requested · resolved · backend · tier ·
   writable root), so advisory is never read as jailed. The advisory resolution
   is recorded too — it is a record with no workspace, and claims no writable
   root (yaah PRs #343, #344).
4. ✅ `internal/doctor` reports the capability (`CheckContainment`: backend /
   mode / placement; a fail-closed `paddock` with no usable backend is a `FAIL`,
   and an unset setting now reports the real default `auto`).

**Default flip (landed, yaah PR #345):** an unset `agents.subagent.containment`
means `auto` — jail where the host can, degrade visibly to advisory where it
cannot. The opt-out is explicit (`off`/`none`/`advisory`), so the unconfined host
path is a choice rather than an omission.

The flip needed a decision first, not more evidence: the acceptance (below) found
that the retained lane **cannot run git write commands under the jail**. That is
now settled as **by design**, and the design is stated in three places — the
sub-agent's prompt, the refusal message, and the config docs:

- the sub-agent works in a linked `git worktree` whose index and object store
  live in the *original* repository, outside the writable root, so `git
  add`/`commit`/`stash` are refused by the kernel (read-only git is fine);
- that is consistent with what retention already meant — the run is the unit,
  settled once — so the sub-agent **prepares** changes and the **orchestrator
  commits** when it settles the proposal;
- widening the jail to include `<repo>/.git` is explicitly rejected: it would let
  a confined sub-agent rewrite the host repository's refs and objects.

Because the raw refusal is a bare `Permission denied`, a denied write now carries
an explanation naming the writable root (and the git rule when applicable), and a
confined sub-agent is told the rule before it meets it.

#### The acceptance, and what it found — and why the flip is blocked (yaah PR #344)

The gate Phase 2 used too: a **real** workload under paddock, not a unit test of
the seam. `TestConfinedBuild_Acceptance` runs a genuine `go build`, a genuine
`git config`/`git status`, and a genuine `git add` inside a real Landlock jail,
in the layout the default lane really uses (a **linked worktree**, not a plain
checkout — the first version of the test used `git init` and that mistake hid
everything below). It found three showstoppers, all invisible to the seam tests:

1. **The pinned backend predated the fix.** yaah pinned `sandbox/landlock
   v0.1.0`, which **predates the `/dev` write grant** (below) — so nothing could
   open `/dev/null` for reading and writing, and `git init` died with
   `could not open '/dev/null': Permission denied`. The fix was already on
   runtime `main`; it had simply **never been tagged**. Published
   `sandbox/landlock/v0.1.1` (release-verify green) and bumped yaah. *Lesson: a
   merged fix with no tag is not a shipped fix.*
2. **The redirect hid a read git needs.** Moving `HOME` moves the global git
   config with it, so a confined `git commit` refused with `Author identity
   unknown`. Fixed by a **shim**: `GIT_CONFIG_GLOBAL` points at a scratch file
   that `include`s the user's real global configs, in git's own order. Not a
   direct pointer (`GIT_CONFIG_GLOBAL` replaces *both* defaults and takes one
   path — and a colon-separated list is not honored, verified on git 2.55) and
   not a copy (a config can carry `http.<url>.extraheader` tokens). The shim
   carries paths, so nothing enters the worktree, and includes still resolve
   against the real files. Credential *stores* (`~/.ssh`,
   `~/.git-credentials`, `~/.npmrc`, `~/.netrc`) are deliberately still not
   reachable: the **unconfined worktree bootstrap** is where authenticated
   inputs get prepared.
3. **The retained lane cannot write git metadata under the jail — settled by
   design.** A retained dispatch works in a **linked** worktree, whose index and
   object store live in the original repository's `.git/worktrees/<name>` and
   `.git/objects`, outside the sole writable root. So inside the jail
   `git status`/`config`/`diff` work and `git add` dies with
   `index.lock: Permission denied`. Granting `<repo>/.git` would hand a confined
   sub-agent write access to the host repository's refs and objects — exactly
   what retention exists to prevent — so the rule is accepted and **stated**:
   the sub-agent prepares, the orchestrator commits at settlement. The refusal is
   explained rather than left raw, and the acceptance pins both (it fails if the
   write ever starts succeeding, so the design note cannot go stale).

All three are pinned by the acceptance: disabling the redirect's shim, or
re-pinning v0.1.0, fails it.

**Remaining on this axis:** the macOS Seatbelt tier (Plan 4a). The read-only
review world that used to sit here is moot: there is no reviewer sub-agent any
more (see Phase 4).

#### The writable-root problem — solved by redirecting ✅ (yaah PR #339)

A strict jail admits **exactly one writable root** (the worktree). A sub-agent
that builds writes a **cache outside it** (`$GOCACHE`, `~/.npm`, `$TMPDIR`), so
those writes would be denied. Python's answer is **not to widen the jail** — the
v0 lowering admits exactly one root, and `writable_roots` is only ever the
`ReadWrite` grant roots. Instead it **redirects the environment into that root**
and scrubs the evidence (`providers.py`, `ClaudeHeadlessProvider`):

> the env redirect — `HOME`/`CLAUDE_CONFIG_DIR`/`TMPDIR` pointed into
> `<working_path>/.claude-scratch` (the v0 lowering admits exactly one writable
> root, so housekeeping writes redirect in; the scratch is scrubbed before return
> so the captured delta stays the agent's intended writes only), auto-updater off
> (it writes to the install dir: denied).

Four disciplines, adopted as-is:

1. **Redirect, don't widen.** The retained launch passes
   `TMPDIR`/`HOME`/`XDG_CACHE_HOME`/`GOCACHE`/`npm_config_cache` →
   `<worktree>/.yaah-scratch/…`, all inside the writable root.
2. **Scrub before `Seal`.** Remove the scratch before the workspace is sealed, so
   the proposal's delta is the sub-agent's *intended* writes, not cache noise.
   (The kernel captures the whole worktree delta; the scrub is what keeps it
   clean.)
3. **Warm outside the jail.** `WorktreeBootstrap` already runs *before* the body,
   unconfined — it is where `go mod download` / `npm ci` populate dependencies
   that the confined body then only *reads*. Reads are free (Landlock governs
   writes only), so a shared read-only module cache still works; only *new*
   downloads need a write.
4. **Disable what cannot be redirected.** Auto-updaters and anything that writes
   to a fixed install path are off, as Python turns off `DISABLE_AUTOUPDATER`.

**The trade-off, stated:** redirecting `GOCACHE` means a **cold build cache per
dispatch**. Two exits: warm it via the bootstrap, or opt in to a narrow
**"warm cache roots" allowlist** (a shared `GOCACHE`/`GOMODCACHE` as extra
writable roots). Python refuses the latter deliberately; a build cache is a
low-sensitivity target and yaah's body is a compiler, not an LLM CLI — so
**redirect is the default and the allowlist is an explicit opt-in** for repos
that need fast builds.

#### `/dev` — the allowance that has no narrower form ✅ (runtime PR #4)

Landlock governs writes by the paths a ruleset **covers**, so a confined body
could not even run `cmd > /dev/null`. Seatbelt already allowed it (every Python
profile appends `(allow file-write* (subpath "/dev"))`); the Go Landlock backend
had no equivalent, so the Linux tier silently denied it. Fixed in the runtime
(`devWriteRoot`).

**The allowance is the whole `/dev` subtree, and cannot be narrower:**
`landlock_add_rule(LANDLOCK_RULE_PATH_BENEATH)` requires a **directory** — a rule
on a device *file* is rejected with `EINVAL` (verified: `/dev/null` → EINVAL,
`/dev/pts` → ok) — and Landlock has **no deny rules**, so `/dev/shm` cannot be
carved out. That is the same trade-off Seatbelt makes; the alternative is denying
`/dev/null` outright, which breaks shells and build tools. The consequence is
documented and pinned by a test.

**Shipped as `sandbox/landlock/v0.1.1`.** The fix merged but was never a release
— `v0.1.0` predates it — so yaah stayed pinned to a jail that could not open
`/dev/null`, and the acceptance caught it (see Phase 3 above). A merged fix with
no tag is not a shipped fix; the release-verify workflow proves the tag resolves
from a clean consumer.

### Phase 4 (north star) — `oversee(implement)` ❌ removed

> **Record:** [`07-phase4-oversee.md`](07-phase4-oversee.md).

Phase 4 shipped as the `oversee` tool (PR #347) — a retained worker, a reviewer
sub-agent inspecting its world, settle on acceptance or discard-and-redispatch —
and was then **removed**. The design was checked against the reference and did not
hold: shepherd's `gate` predicate is a *callable* evaluated in the orchestrator's
process (`JudgePredicate`, run by `eval_predicate`), never a second agent, and the
"LLM-as-judge" mention in `_scope/scope.py` is a caller-supplied function.

Everything the loop needed was compensation for an untrusted separate context: a
verdict token (a sub-agent's prose can quote an injected verdict), a whole-tree
fingerprint around the review (the reviewer's jail still granted the worktree as
writable), and a round cap — and the write check could only detect, never prevent.

The shape it was reaching for already existed: `spawn_subagent` returns a sealed
proposal, `proposal show` returns its diff, and `select`/`apply`/`discard` is the
gate — with `discard` + re-dispatch as the retry. The orchestrator is the
predicate, which is exactly where the authority sits.

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
| 3 | `TestDoctor_Containment` | `yaah doctor` | reports the ABI / advisory; an unset setting reports the real default (`auto`) |
| 3 | `TestConfinedBuild_Acceptance` | a real `go build`, `git config`, `git status` and `git add` under paddock, in a LINKED worktree | the build succeeds and its cache lands in the worktree scratch; both halves of a split global identity resolve; read-only git works; **`git add` is refused by the kernel AND explained (the by-design git rule)**; an out-of-root write is still refused |
| 3 | `TestConfinementScratchEnv_PointsGitAtGlobalConfig` | global configs (both, one, none, pre-set pointer) and the redirect | a shim includes whichever real files exist, in git's order, carrying paths not values |
| 3 | `TestConfinementScratchEnv_RefusesSymlinkedScratch` | `.yaah-scratch` (or its `home`) planted as a symlink | refused before anything is created; the target is untouched |
| 3 | `TestConfinementHint_ExplainsRefusedWrites` | a kernel-refused write (git metadata, and a plain path) | the hint names the writable root, and the git rule only for git metadata; no hint without a refusal |
| 3 | `TestBuildSubAgentSysPrompt_ConfinementGuidance` | an enforced vs an unenforced placement | a confined sub-agent is told the writable root and the git rule up front; the others are not |

## 9. Risks & Mitigations

- **Confining the orchestrator by mistake** — the whole point is the split.
  *Mitigation:* the confiner attaches only on the retained launch;
  `TestOrchestrator_ExecUnconfined` pins it.
- **Flipping retention regresses the proven loop.** *Mitigation:* the
  `mode: shared` kill-switch, and the benchmark harness as the Phase-2 gate.
- **Per-sub-agent worktree cost** (a fork + a seal per dispatch). *Mitigation:*
  measure against the harness; `shared` remains available for exploratory work.
- **Version skew (kernel v0.16.0 → v0.17.0).** *Mitigation:* Phase 0's build gate.
- **Release tagging dependency.** *Mitigation:* Phase 0, or an interim local
  replace. **This bit twice:** the `/dev` fix merged but was never tagged, so
  yaah stayed pinned to a backend that could not open `/dev/null` — a merged fix
  with no tag is not a shipped fix. Mitigated by tagging on merge for any
  boundary-relevant fix, and by the acceptance below, which fails loudly against
  a stale pin rather than degrading silently.
- **Confinement vs. a linked worktree's git metadata.** A retained worktree's
  index and object store live in the *original* repository's `.git`, outside the
  jail's writable root, so a confined sub-agent cannot `git add`/`commit` there.
  *Mitigation:* **settled by design** — the sub-agent prepares changes and the
  orchestrator commits at settlement — and stated where it will be met: the
  sub-agent's prompt, the refusal message, and the config docs. The acceptance
  pins it with the real linked layout. Widening the jail to include
  `<repo>/.git` is explicitly rejected — it would give a confined sub-agent write
  access to the host repository, which retention exists to prevent.
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
9. **Where this plan lives:** `goatherd-runtime/docs/plans/` (the numbered series); yaah's docs link to it.

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
