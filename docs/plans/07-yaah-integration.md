# Implementation Plan: yaah Integration (Level A — host write-jail) (Go)

> **Scope.** This is **Plan 7** — the first *host* integration of the containment
> component. Plans 1–4 shipped the component; this plan wires it into `yaah`, the
> agent harness, whose local command path is currently **unconfined**.
>
> **Two levels, this plan is Level A.** A *lightweight write-jail* on yaah's
> local exec path (Landlock), adopted with no change to yaah's agent loop.
> **Level B** — reversible-by-default runs (yaah adopting the runtime's
> `Scope`/`run.Reversible`) — is a later plan (§9).

## 1. Objective & Context

`yaah` (`github.com/buchenberg/yaah`) does not depend on `shepherd-runtime-go`; it
pins `croft v0.3.0` + `shepherd-kernel-go v0.16.0`. Its **local** command path
runs commands directly on the host:

```go
// yaah/internal/tools/workspace.go:179 — localWorkspace.Exec
cmd := exec.CommandContext(ctx, req.Command, req.Args...)
```

Every tool that runs a command funnels through `Workspace.Exec` (e.g.
`bash.go:76` call `ws.Exec(ctx, shellCommand(ws, params.Command))`). So confining
that one method confines every host command — the write-jail this plan delivers.

The container backend (`workspace.backend=containerd`, `sandboxWorkspace` over
croft) is already isolated; **this plan is about the `local` backend**.

## 2. What the Component Provides (recap)

- `containment.Backend` — `Name`/`Available`/`SupportsNetwork`/`ProfileFor`/`Probe`/`Launch` (+ `LaunchConfined`, the probe-first verb).
- `sandbox/landlock` — the Linux backend (`landlock.New()`), a nested module.
- `containment.ConfinementSpec` — `WritableRoots` + `Network{Mode, AllowedHosts, BrokerPort}`.
- `sandbox/egress` — the broker (for opt-in network confinement).

## 3. Scope & Explicit Non-Goals

**In scope:** tag the runtime + the nested module (Phase 0); wire the Landlock
write-jail into `localWorkspace.Exec` (Phase 1); capability probe + config
(Phase 2).

**Non-goals:**
- **Level B** reversible runs (§9) — outline only.
- **Network confinement by default.** Level A is a **write-jail**; the default
  network policy is `allow_all` (yaah's dev flows — `go mod`, `npm`, `curl` — need
  egress). Confining egress is opt-in via the broker.
- **Windows / macOS enforcement.** macOS needs the Seatbelt backend (Plan 4
  Phase 1); Windows is unsupported. Both fall back to advisory.
- Changing the container (`croft`) backend — already isolated.

## 4. Completion Checklist

| # | Target | Test |
|---|---|---|
| 1 | `yaah` imports `sandbox/landlock`; the trampoline hook is installed in `main.go` | `TestYaah_TrampolineHookInstalled` (build/run) |
| 2 | `localWorkspace.Exec` runs through `LaunchConfined` when a backend is available | `TestLocalWorkspace_ExecJailed` |
| 3 | a write inside the workspace succeeds; one outside is refused | `TestLocalWorkspace_ExecWriteOutsideDenied` |
| 4 | advisory fallback when no backend (macOS/Windows/ABI<1) | `TestLocalWorkspace_ExecAdvisory` |
| 5 | placement/config: `auto` \| `jail` \| `off` selects the behavior | `TestLocalWorkspace_Placement` |
| 6 | network policy is `allow_all` by default; opt-in deny/broker | `TestLocalWorkspace_NetworkPolicy` |
| 7 | a launch failure surfaces as `ExecResult.ExitCode == -1` | `TestLocalWorkspace_LaunchFailureIsNegativeOne` |
| 8 | `yaah doctor` reports the containment capability | `TestDoctor_Containment` |

## 5. Architecture

### 5.1 The single choke point

```
tool (bash, go_refactor, …) ──► Workspace.Exec(req)
                                     │
             localWorkspace (host)   │        sandboxWorkspace (croft) — already isolated
                                     ▼
        placement == off        → exec.CommandContext        (today)
        placement != off        → containment.LaunchConfined(landlock.Detect(), spec, req)
                                    │ spec = {WritableRoots: [WorkDir()], Network: allow_all}
                                    └─ probe-first; refuses (ErrPaddockNotEstablished) rather than running unconfined
```

Because `ResolvePath` already enforces *lexical* containment and every command
funnels through `Exec`, this change is localized to one method plus a config knob
— no tool changes.

### 5.2 Levels

- **A (this plan):** the write-jail. Replace the local `exec.CommandContext` with
  `LaunchConfined`. The agent loop, sessions, and tools are untouched.
- **B (later):** reversible-by-default runs — the session adopts the runtime's
  `Scope`/`ScopeManager` + `run.Reversible`, so a tool run is an isolated fork
  merged/sealed/discarded. Out of scope here.

## 6. Phase 0 — Release mechanics (prerequisite)

The nested `sandbox/landlock` module carries a **dev-time** `replace
github.com/buchenberg/shepherd-runtime-go => ../..`. yaah cannot consume it until
the runtime is published.

1. Tag `github.com/buchenberg/shepherd-runtime-go` (e.g. `v0.18.0`).
2. Drop the nested module's `replace`; tag `sandbox/landlock/v0.1.0`.
3. **Verify the version skew:** yaah pins kernel **v0.16.0**; the runtime pins
   **v0.17.0**. Adding the runtime bumps yaah's kernel to v0.17.0 (MVS). Build
   `go build ./...` and `go test ./...` in yaah against v0.17.0 and fix any
   breakage from the runtime extraction (packages moved to
   `shepherd-runtime-go/...`). This is the main risk (§11).
4. *(Interim, if not tagged yet)* yaah may use a local `replace
   github.com/buchenberg/shepherd-runtime-go => ../shepherd-runtime-go` during
   development; **do not ship** it.

## 7. Phase 1 — Level A: the write-jail

### 7.1 `localWorkspace` gains a confiner

```go
type localWorkspace struct {
    root           string
    shell, shellFlag string
    confiner       containment.Backend      // landlock.Detect(); nil ⇒ advisory
    placement      confinement.Placement    // auto | jail | off
    network        containment.NetworkPolicy
}
```

`Exec` maps the request:

```go
func (w *localWorkspace) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
    if w.confiner == nil || w.placement == confinement.PlacementAdvisory {
        return w.advisoryExec(ctx, req)          // today's exec.CommandContext path (off / no backend)
    }
    spec := containment.ConfinementSpec{
        WritableRoots: []string{w.WorkDir()},
        Network:       w.network,                 // default allow_all
    }
    launch := containment.LaunchRequest{
        Command:     append([]string{req.Command}, req.Args...),
        WorkingRoot: w.WorkDir(),
        Cwd:         cwdOr(req.Cwd, w.WorkDir()),
        Stdin:       req.Stdin,
    }
    res, err := containment.LaunchConfined(w.confiner, spec, launch)
    if err != nil {
        return ExecResult{ExitCode: -1, Stderr: err.Error()}, nil  // "could not start"
    }
    if req.SeparateStreams {
        return ExecResult{ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr}, nil
    }
    return ExecResult{ExitCode: res.ExitCode, Stdout: res.Stdout + res.Stderr}, nil // combined, like today
}
```

Notes:
- **Combined streams.** yaah's local path keeps stdout+stderr interleaved unless
  `SeparateStreams` (`workspace.go:36-39`); the jail captures them separately, so
  concatenate for the default case (a documented, minor fidelity difference —
  true interleaving is not preserved).
- **`ExitCode`.** yaah uses `-1` for "could not start" (`workspace.go:31-34`); a
  `LaunchConfined` error (probe failure, no jail) maps to `-1`.
- **`placement = off`** keeps today's behavior exactly (the escape hatch).

### 7.2 The trampoline hook (mandatory)

`landlock` confines by re-exec'ing **the running binary**; the binary must handle
the sentinel first. Add to `yaah/main.go`:

```go
func main() {
    if code, handled := landlock.MaybeRunTrampoline(os.Args); handled {
        os.Exit(code)
    }
    if err := yaah.Execute(); err != nil { … }
}
```

Any other captured entry point (test binaries) adds the same line to `TestMain`.

### 7.3 Config

`workspace.containment: auto | jail | off` (default **auto** → jail when the
backend is available, else advisory), mapping onto the existing `Placement` enum:
**`off → confinement.PlacementAdvisory`**, **`jail → confinement.PlacementPaddock`**,
**`auto → confinement.PlacementAuto`**. And `workspace.network: allow_all |
deny_all | broker` (default **allow_all** — the write-jail; network confinement
opt-in). Resolved in `wiring.go` next to the workspace-backend selection.

### 7.4 Selection

- `workspace.backend=local` (default) → `localWorkspace` with the confiner.
- `workspace.backend=containerd` → `sandboxWorkspace` (croft) — unchanged; the
  jail is moot (the container is the boundary).

## 8. Phase 2 — Capability probe + `yaah doctor`

- `internal/doctor` gains a **containment** check: `landlock.New().Available()`
  (Landlock ABI + network support), reporting `native-syscall-deny (ABI N)` or
  "advisory (no jail backend)". Mirrors the existing checks (`doctor.go`).
- `yaah config` documents the two knobs.

## 9. Phase 3 (later) — Level B: reversible runs

Outline only. The session adopts the runtime's `ScopeManager` + `run.Reversible`
so a tool run is an isolated fork, executed confined, then merged/sealed/
discarded with authority evidence. This touches yaah's loop/session model and is
a separate plan once Level A is proven.

## 10. Test & conformance matrix

| Phase | Test | Action | Expected |
|---|---|---|---|
| 0 | (build) | `go build ./...` in yaah with the runtime dep | compiles against kernel v0.17.0 |
| 1 | `TestLocalWorkspace_ExecJailed` | `bash`-style exec writes in the workspace | exit 0; file lands |
| 1 | `TestLocalWorkspace_ExecWriteOutsideDenied` | exec writes outside `WorkDir()` | denied; no file |
| 1 | `TestLocalWorkspace_ExecAdvisory` | `confiner == nil` (simulated) | runs unconfined; behavior unchanged |
| 1 | `TestLocalWorkspace_Placement` | `off` / `auto` | `off` = direct exec; `auto` jails when available |
| 1 | `TestLocalWorkspace_NetworkPolicy` | default vs `deny_all` | default reaches the network; `deny_all` refuses a connect |
| 1 | `TestLocalWorkspace_LaunchFailureIsNegativeOne` | a non-executable command | `ExitCode == -1` |
| 1 | `TestLocalWorkspace_CombinedStreams` | default (no `SeparateStreams`) | stdout+stderr present in `Stdout` |
| 2 | `TestDoctor_Containment` | `yaah doctor` | reports the ABI / advisory |
| A | (live) | a jailed `bash` tool call on Linux | write contained; agent loop unaffected |

## 11. Risks & Mitigations

- **Version skew (kernel v0.16.0 → v0.17.0).** *Mitigation:* Phase 0's build gate;
  fix import-path moves from the runtime extraction before wiring.
- **Release tagging dependency.** *Mitigation:* Phase 0 first, or an interim local
  `replace` that must not ship.
- **macOS / Windows.** *Mitigation:* advisory fallback (`auto` → no backend ⇒
  today's behavior); macOS enforcement arrives with Seatbelt (Plan 4 Phase 1).
- **The trampoline hook is load-bearing.** Without it, every jailed exec fails
  closed (rc 3 → `ErrPaddockNotEstablished`). *Mitigation:* a build/run test
  asserting the hook is installed (§4 #1).
- **Network default UX.** Confining egress by default would break `go mod`/`npm`.
  *Mitigation:* default `allow_all` (write-jail); egress opt-in.
- **Combined-stream fidelity.** *Mitigation:* documented; `SeparateStreams`
  preserves the exact split, and tools that parse stdout already set it.

## 12. Decisions

1. **Level A first** (write-jail), Level B later. *Recommended — smallest change,
   immediate value.*
2. **Default placement `auto`** (jail when available, else advisory). *Recommended
   — no behavior change on hosts without a jail.*
3. **Default network `allow_all`**; egress confinement opt-in. *Recommended — the
   write-jail is the value; network default-deny would break dev flows.*
4. **The seam is `localWorkspace.Exec`**, not per-tool. *Recommended — one
   choke point; tools unchanged.*
5. **The trampoline hook lives in `yaah/main.go`** (and any captured entry point).
6. **Where this plan lives:** in `shepherd-runtime-go/docs/plans/` (the numbered
   series), because it consumes this component; yaah's docs link to it.
