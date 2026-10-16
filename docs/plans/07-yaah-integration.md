# Implementation Plan: yaah Integration — Shepherd Mechanics for Sub-agents (Go)

> **The principle (non-negotiable).** The **orchestrating agent runs as normal on
> the HOST** — its own workspace, unconfined, unchanged. The **shepherd
> mechanics — scope isolation, Landlock confinement, checkpoint/rollback — apply
> to SUB-AGENTS only.** This is the model proven in Terminal Bench: the
> orchestrator solves the task on the host; speculative and retryable work is
> what gets isolated and undone. Confining the orchestrator would not be that
> model, and this plan does not.
>
> **Scope.** **Plan 7**, the first host integration. Level A: put the
> **containment** layer (Landlock) on the sub-agent execution path that already
> carries the rest of the shepherd mechanics. Level B (reversible runs for
> every sub-agent, not just `supervised_task`) is a later phase (§9).

## 1. Objective & Context

`yaah` does not depend on `shepherd-runtime-go`; it pins `croft v0.3.0` +
`shepherd-kernel-go v0.16.0`. But it **already runs the shepherd machinery for
sub-agents**:

- **Workdir dispatch (the shepherd path).** A sub-agent dispatched with a
  `workdir` gets (a) a `PathValidator` **rooted at its worktree**
  (`runner.go:984`, `pv.WithWorkspaceRoot(workdir)`), and (b) an **isolated**
  scope — `mgr.ForkIsolated(rootScopeID, subTraceID, nil,
  NewWorktreeSandbox(repoPath, workdir))` (`runner.go:877-883`). `supervised_task`
  uses exactly this path and adds checkpoint/rollback. **Its commands still run on
  the host** (`exec.CommandContext`), bounded by the worktree only *lexically*
  — not by the kernel.
- **Non-workdir dispatch.** `spawn_subagent` without a workdir inherits the host
  validator/workspace — host execution, the root's shared sandbox.

**What is missing is the kernel boundary.** The rooted validator is lexical; a
sub-agent's shell can still write anywhere the host user can. This plan puts
**Landlock confinement** on the workdir/isolated sub-agent path, rooted at that
sub-agent's worktree — while the orchestrator's `localWorkspace` keeps running
commands on the host, unconfined.

## 2. The Split (explicit)

| Actor | Workspace | Exec | Scope | Islands |
|---|---|---|---|---|
| **Orchestrator** (main registry) | `localWorkspace(pv)` (`tools.go:333`) | `exec.CommandContext`, **unconfined** | root scope (in-place git) | **unchanged** — runs as normal on the host |
| **Sub-agent, workdir/isolated** | `localWorkspace` **rooted at the worktree** + **Landlock confiner** | `LaunchConfined`, rooted at the worktree | `ForkIsolated` worktree | **this plan** |
| **Sub-agent, non-workdir** | host `localWorkspace` | unconfined (today) | fork of root | unchanged by this plan (opt-in isolation, §7.4) |
| **Isolated (containerd) session** | `sandboxWorkspace` (croft) | in-container | pure-causal root | **unchanged** — the container is the boundary |

The orchestrator row is the Terminal-Bench contract: **do not touch it.**

## 3. Where the Split Is Wired

- Orchestrator workspace: `internal/tools/tools.go:333` (`newLocalWorkspace(pv)`) — **no confiner**.
- Sub-agent registry: `internal/agent/runner/runner.go:971-987` — the `WithWorkspaceRoot(workdir)` block. This is where a **confiner** is attached, keyed on the workdir/isolated dispatch.
- Sub-agent scope: `createSubAgentScope`/`subAgentSandbox` (`runner.go:759-922`) — already `ForkIsolated` for a workdir.
- Sub-agent workspace injection: `wiring.go:557-563` (`subAgentWorkspace`, nil in local mode) and the registry's `SetPathValidator`/`SetWorkspace`.

## 4. Scope & Non-Goals

**In scope:** release mechanics (Phase 0); Landlock confinement on the
workdir/isolated sub-agent workspace (Phase 1); capability probe + config
(Phase 2).

**Non-goals:**
- **Confining the orchestrator.** Explicitly out — it must run as normal on the host.
- **Level B** (reversible runs for non-supervised sub-agents) — §9.
- **Changing the isolated (containerd) session** — already bounded by the container.
- **Network confinement by default** — the default is a *write*-jail (`allow_all`); egress is opt-in.
- **Windows** (unsupported) and **macOS enforcement** (Seatbelt is Plan 4 Phase 1); both stay advisory.

## 5. Completion Checklist

| # | Target | Test |
|---|---|---|
| 1 | the orchestrator's `localWorkspace` exec is **unconfined** (host) | `TestOrchestrator_ExecUnconfined` |
| 2 | the sub-agent workdir workspace carries a Landlock confiner | `TestSubAgent_WorkspaceConfined` |
| 3 | a sub-agent command writing in its worktree succeeds; outside is refused | `TestSubAgent_ExecWriteOutsideDenied` |
| 4 | the sub-agent scope is `ForkIsolated` (already) with a rooted validator | `TestSubAgent_ScopeIsolated` |
| 5 | advisory fallback when no backend (macOS/Windows/ABI<1) | `TestSubAgent_ExecAdvisory` |
| 6 | config `agents.subagent.containment: auto\|jail\|off` selects behavior | `TestConfig_SubAgentContainment` |
| 7 | `yaah doctor` reports the capability | `TestDoctor_Containment` |
| 8 | the trampoline hook is installed | (build/run) |

## 6. Architecture

### 6.1 The sub-agent workspace gains a confiner

`localWorkspace` gains an optional confiner; the orchestrator's instance never
sets it, the sub-agent's workdir instance does.

```go
type localWorkspace struct {
    pv       *PathValidator
    confiner containment.Backend      // nil ⇒ the orchestrator's host path (unconfined)
    place    confinement.Placement    // auto | jail | off
    network  containment.NetworkPolicy
}

// orchestrator (unchanged):
func newLocalWorkspace(pv *PathValidator) *localWorkspace { return &localWorkspace{pv: pv} }

// sub-agent workdir path:
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

### 6.2 Wiring (one place)

In `runner.go`'s workdir/isolated block (§3): after `reg.SetPathValidator(pv)`,
install the confiner when the dispatch is the shepherd path (a `workdir`, or an
isolated sub-agent the config opts into):

```go
if workdir != "" && containmentEnabled(opts.subCfg) {
    reg.SetWorkspace(newConfinedLocalWorkspace(pv, landlock.Detect(), place, net))
}
```

The orchestrator's `newLocalWorkspace(pv)` (tools.go) is untouched — so its
`Exec` stays `advisoryExec`.

### 6.3 The trampoline hook (mandatory)

`landlock` confines by re-exec'ing the running binary; add to `yaah/main.go`:

```go
if code, handled := landlock.MaybeRunTrampoline(os.Args); handled { os.Exit(code) }
```

Any captured entry point (test binaries) adds the same line to `TestMain`.

## 7. Phases

### Phase 0 — Release mechanics (prerequisite)

The nested `sandbox/landlock` module carries a dev-time `replace … => ../..`.

1. Tag the runtime; drop the replace; tag `sandbox/landlock/v0.1.0`.
2. **Version skew:** yaah pins kernel **v0.16.0**, the runtime pins **v0.17.0** —
   adding the runtime bumps yaah to v0.17.0 (MVS). `go build ./...` + `go test
   ./...` in yaah; fix import-path moves from the runtime extraction.
3. *(Interim)* a local `replace` to the runtime checkout — must not ship.

### Phase 1 — Confine the sub-agent worktree workspace

1. Add the confiner fields + `newConfinedLocalWorkspace` to `localWorkspace`.
2. Attach it in the sub-agent registry's workdir block (§6.2).
3. The sub-agent scope stays `ForkIsolated` (already) — the confiner's writable
   root is that worktree, so the kernel boundary and the scope boundary agree.
4. **Leave the orchestrator untouched** — assert it (§4 #1).

### Phase 2 — Config + `yaah doctor`

- `agents.subagent.containment: auto | jail | off` (→ `PlacementAuto` /
  `PlacementPaddock` / `PlacementAdvisory`), and `agents.subagent.network:
  allow_all | deny_all | broker` (default `allow_all`). Resolved in `wiring.go`.
- `internal/doctor` gains a containment check: `landlock.New().Available()`
  (ABI + network support), reporting `native-syscall-deny (ABI N)` or "advisory".

### Phase 3 (later) — Level B: reversibility for every sub-agent

Today only `supervised_task` gets worktree + rollback; `spawn_subagent` shares
the host. Level B routes **all** sub-agents through the shepherd path
(`ForkIsolated` + `Seal`/`Settle`) — outline only.

## 8. Test & Conformance Matrix

| Phase | Test | Action | Expected |
|---|---|---|---|
| 1 | `TestOrchestrator_ExecUnconfined` | a host command outside the workspace | **runs** (unchanged) — the orchestrator is not confined |
| 1 | `TestSubAgent_ExecWriteOutsideDenied` | a sub-agent command writes outside its worktree | refused; no file |
| 1 | `TestSubAgent_ExecWriteInside` | writes in its worktree | lands |
| 1 | `TestSubAgent_WorkspaceConfined` | inspect the sub-agent registry's workspace | has a confiner; the orchestrator's does not |
| 1 | `TestSubAgent_ScopeIsolated` | a workdir dispatch | scope is `ForkIsolated`, validator rooted at the worktree |
| 1 | `TestSubAgent_ExecAdvisory` | no backend (simulated) | runs unconfined; behavior unchanged |
| 1 | `TestSubAgent_LaunchFailureIsNegativeOne` | a non-executable command | `ExitCode == -1` |
| 2 | `TestConfig_SubAgentContainment` | `auto`/`jail`/`off` | selects the placement |
| 2 | `TestDoctor_Containment` | `yaah doctor` | reports the ABI / advisory |
| A | (live) | `supervised_task` on Linux | writes kernelly bounded to the worktree; orchestrator host-normal |

## 9. Risks & Mitigations

- **Confining the orchestrator by mistake** — the whole point is the split.
  *Mitigation:* the confiner is attached **only** in the sub-agent workdir block;
  `TestOrchestrator_ExecUnconfined` pins it.
- **Version skew (kernel v0.16.0 → v0.17.0).** *Mitigation:* Phase 0's build gate.
- **Release tagging dependency.** *Mitigation:* Phase 0, or an interim local replace.
- **macOS / Windows** (no jail). *Mitigation:* `auto` → advisory ⇒ today's behavior.
- **The trampoline hook is load-bearing** (rc 3 → `ErrPaddockNotEstablished`).
  *Mitigation:* install it and assert (§4 #8).
- **Stream fidelity.** yaah's local path combines stdout+stderr unless
  `SeparateStreams`; the jail captures them apart. *Mitigation:* concatenate for
  the default; tools that parse stdout already set `SeparateStreams`.
- **The sub-agent workdir root must equal the confiner's writable root**, or the
  kernel boundary and the lexical/scope boundary disagree. *Mitigation:* derive
  both from `workdir` (§7 Phase 1 #3).

## 10. Decisions

1. **Orchestrator stays unconfined on the host** (Terminal-Bench fidelity). *Non-negotiable.*
2. **Containment rides the existing shepherd (workdir/isolated) sub-agent path**, not `localWorkspace` globally. *Recommended — the split is already there.*
3. **Default placement `auto`** (jail when available, else advisory), **default network `allow_all`** (write-jail; egress opt-in).
4. **The confiner is attached in one place** — the sub-agent registry's workdir block (`runner.go:971-987`).
5. **Trampoline hook in `yaah/main.go`** (and any captured entry point).
6. **Where this plan lives:** `shepherd-runtime-go/docs/plans/` (the numbered series); yaah's docs link to it.
