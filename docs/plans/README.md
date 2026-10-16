# Execution & Containment Roadmap

How the Go runtime reaches functional parity with Python Shepherd's execution
boundary: confinement (Plan 1), reversible execution (Plan 2), authority policy
(Plan 3), the second enforcement surface (Plan 4), and the optional syscall
filter (Plan 5).

Each plan follows the same discipline: a parity checklist mapping Python symbols
to Go targets with a covering test, an explicit scope/non-goals section, and a
test matrix. Plans 1–3 are written and implemented; 4–5 are scoped here and get
their own documents when started.

> **Vocabulary — "paddock".** The Go code renames Python Shepherd's `jail` to
> **`paddock`** (an enclosed livestock area — it fits the shepherd/croft theme;
> "jail" was the one term that did not). This rename covers **Go vocabulary and
> wire values**: `PlacementPaddock` (wire `"paddock"`), `PaddockMonitor` (wire
> `"syscall_paddock"`), `ErrPaddockNotEstablished`. The **Python reference keeps
> its own spelling** (`JailNotEstablished`, `JAIL_MONITOR`, `"syscall_jail"`,
> placement `"jail"`), and the plan tables show Python symbols in their original
> form. **Consequence:** because the monitor id is a plan-descriptor field, Go
> permission-plan digests diverge from Python's by construction. That is the one
> intentional vocabulary divergence; the semantics are unchanged.

## Status

| Plan | Goal | Covers | Doc | Status |
|---|---|---|---|---|
| **1** | Linux Landlock containment | paddock, contract, probe, may=/grants lowering, placement, evidence, denial classification | [`01-landlock-containment.md`](01-landlock-containment.md) | ✅ implemented |
| **2** | Reversible execution runs | fork-isolated-by-default, capability-after-fork, discard-on-failure, merge/seal, recorded run operation, authority merge | [`02-reversible-runs.md`](02-reversible-runs.md) | ✅ implemented |
| **3** | Authority declarations & permission-plan compiler | `May[…]`/grant authoring, paddock **+ carrier** monitor compiler, `authority_merge`, evidence persistence | [`03-authority-and-plan-compiler.md`](03-authority-and-plan-compiler.md) | ✅ implemented |
| **4** | Second enforcement surface | Seatbelt (macOS), egress broker, egress pinning (Landlock v4 net / Seatbelt) | [`04-egress-and-seatbelt.md`](04-egress-and-seatbelt.md) | 🚧 Phases 2–3 (Linux) done; 1 (Seatbelt) planned |
| **5** | Syscall filter | seccomp-bpf tier (optional; reference does not implement it) | — | 💤 signposted |

## Plan 1 — Linux Landlock containment (done)

A self-contained `sandbox/containment` contract + `sandbox/confinement` authority
lowering in the root module, and a `sandbox/landlock` **nested module** (its own
`go.mod`/version stream) with the raw-syscall engine, the confine-then-exec
trampoline, the fail-closed probe, and the backend. `GitSandbox` gains paddocked
`Exec` via `WithExec`. Verified against the live kernel (13 Landlock tests + a
full-stack integration test). See the plan for the module layout (§15) and the
as-built status/deviations (§16).

## Plan 2 — Reversible execution runs

**Gap closed:** Plan 1 runs a confined command but does not *wrap* it in a
reversible transaction. Python's `_run_execution_bound` forks an isolated child,
mints the capability only after the fork succeeds, dispatches inside a recorded
operation, then merges (default), seals, or discards.

**Key fact:** the Go runtime **already implements the substrate** —
`ScopeManager.ForkIsolated`, `Capture/ApplyWorkspace`, `Seal` + `Settle`, the
merge gate, `RecoverScopes`. Plan 2 is a **composition** layer (`run` package),
not new substrate. See [`02-reversible-runs.md`](02-reversible-runs.md).

## Plan 3 — Authority declarations & the permission-plan compiler

**Gaps closed:** the *authoring* half of authority (where grants come from) and
the *monitor-assignment* half (which monitor enforces what).

- **3a — Authority declarations.** A Go-native `May[…]`/grant surface. Today
  `BindingRootGrant` is a pure input with no producer; Python derives grants from
  task-signature annotations. Deliverable: a declaration API a task/tool uses to
  bind roots (`ReadOnly`/`ReadWrite`), lowered through `confinement.LowerGrants`
  with the existing disjointness guard.
- **3b — Permission-plan compiler.** Python's `install()` compiles a
  `PermissionPlan` naming **both** the syscall paddock (pre-action) and the carrier
  check-at-commit, and `run_driver` consumes it. Deliverable: the compiler
  (paddock + carrier assignments, `SpecDigest`, descriptor, `Digest`) and its
  consumption by the Plan-2 `run` package, mapping the carrier monitor to the
  existing `DeltaApplier`/settlement check-at-commit.
- **3c — `authority_merge` disposition.** The `DecisionProvider` gate over the
  run's proposed changes; the plan digests (`permission_plan_digest`,
  descriptor) ride in the run record. (Reference's carrier-candidate cohort lane
  is a follow-up.)
- **3d — Evidence persistence.** Plan 2 Phase 3 records the run operation; the
  chain of authority-evidence digests (spec → plan → run) becomes citable at
  settlement.

Depends on Plan 2.

## Plan 4 — Second enforcement surface: Seatbelt + egress

**Gaps closed:** macOS containment and network/egress enforcement.

- **4a — Seatbelt backend (macOS).** Python's macOS jail is `/usr/bin/sandbox-exec
  -p <SBPL profile>` via `subprocess` — **no cgo needed**. Deliverable: a
  darwin build-tagged backend (`available` = sandbox-exec present; `ProfileFor` =
  the SBPL lowering; `Probe` = `touch` canary; `Launch` = exec sandbox-exec),
  mirroring `_seatbelt_containment.py`. Caveat: `sandbox-exec` is deprecated
  (same risk the reference carries).
- **4b — Egress broker (Go).** A local CONNECT/HTTP proxy with a host allowlist,
  porting `_egress_broker.py` (`HostGrant`/`EgressPolicy`/`EgressBroker`) onto
  `net`/`net/http`. Consumes `ConfinementSpec.Network.AllowedHosts` (the
  `broker_grants` seam Plan 1 already models).
- **4c — Egress pinning.** Pin the sandbox's egress to the broker so it cannot
  bypass it. On Linux, extend the ruleset with `handled_access_net` +
  `LANDLOCK_ACCESS_NET_CONNECT_TCP` rules when ABI ≥ 4 (this host reports ABI 11)
  — **ahead of the reference**, whose Linux jail is FS-only. On macOS, Seatbelt's
  `(deny network-outbound)` + broker. Note: Landlock net rules are **port-scoped,
  not address-scoped**; true loopback-only pinning needs a network namespace
  (`croft`) — best-effort otherwise.

Depends on Plan 1 (spec/NetworkPolicy) and Plan 3 (network-axis lowering).

## Plan 5 — Syscall filter (seccomp-bpf)

**Signposted, not built.** Python's containment family names "Landlock+seccomp",
but the reference ships **no** seccomp backend, so this is not a parity gap. A
seccomp tier would deny whole dangerous syscalls (`mount`, `ptrace`, `keyctl`,
`bpf`, raw `socket`) regardless of arguments, complementing Landlock's
object-based paddock. Go can install cBPF via `SECCOMP_SET_MODE_FILTER`, but the
filter is arch-specific and fiddly; defer until a concrete need (untrusted code
without containerd) justifies it.

## Small items

- **Sandbox file IO** — ✅ done: `GitSandbox.ReadFile`/`WriteFile` with
  symlink-safe path containment (resolve the parent and the target, refuse `..`
  and escaping-symlink paths); `FileIO` capability now reported.
- **Kernel decision note** — ✅ done: the Path B fence decision for the Linux
  Landlock member is recorded in the kernel's
  [`gap-report.md`](../../../shepherd-kernel-go/docs/history/gap-report.md) and
  the `DEVELOPMENT.md` backlog item is marked resolved.
- **Foundation/`confinement` normalization** — ✅ done:
  `golang.org/x/text/unicode/norm` (Plan 1 §16.2 #5).
- **Release mechanics** — open: drop the nested module's dev `replace` and tag
  `sandbox/landlock/v0.1.0` once the root runtime is published.

## Gap inventory → plan

| Gap (vs Python) | Status | Closed by |
|---|---|---|
| Landlock enforcement, contract, spec, probe | ✅ | Plan 1 |
| `may=` / per-binding grants lowering | ✅ | Plan 1 |
| Disjointness / alias guard (incl. NFC) | ✅ | Plan 1 (x/text) |
| Placement, detection, denial classification, evidence digest | ✅ | Plan 1 |
| Grant *source* (signature annotations) | ✅ | Plan 3a (`confinement.Declaration`) |
| Permission-plan compiler (+ carrier monitor) | ✅ | Plan 3b (`confinement.Install`) |
| `authority_merge` disposition | ✅ | Plan 3c (`DecisionProvider` gate) |
| Evidence persistence into the trace | ✅ | Plan 2 §8 / Plan 3d |
| Reversible-by-default execution | ✅ | Plan 2 |
| Capability proof-carrying / after-fork | ✅ | Plan 2 |
| ExecutionBoundDriver / negotiation | ✅ | Plan 2 (adapted) |
| Sandbox file IO | 🟡 | small item |
| Seatbelt (macOS) | ❌ | Plan 4a |
| Egress broker | ✅ | Plan 4b (`sandbox/egress`) |
| Egress pinning (Linux) | ✅ (ahead of ref) | Plan 4c |
| seccomp | ➖ not a gap | Plan 5 (optional) |
| Container / VM / AppContainer | ➖ | `croft` (container); VM/AppContainer signposted in both |

## Dependency graph

```
Plan 1 (landlock, done)
  ├─ Plan 2 (reversible runs) ── Plan 3 (authority + plan compiler)
  │                                   └─ Plan 4c (egress pinning / network lowering)
  └─ Plan 4a (Seatbelt)   Plan 4b (broker)
                              Plan 5 (seccomp, optional)
```

## Decisions & invariants

- **`golang.org/x/text` is permitted.** Adopted for the confinement NFC fold
  (`v0.21.0`, pinned to keep the module's `go 1.21` directive). This is the one
  deliberate relaxation of the root module's "kernel + stdlib only" footprint.
- **Nested module with a dev-time `replace`.** `sandbox/landlock` is its own
  module; its `go.mod` carries `replace github.com/buchenberg/shepherd-runtime-go
  => ../..` for local development, dropped at release when the root is tagged.
  A repo-root `go.work` was tried and rejected (it propagates the nested
  require into the root build).
- **Vocabulary — "jail" became "paddock".** The Go code renames Python's `jail`
  to `paddock` (see the roadmap's vocabulary note). Wire values changed too
  (`syscall_paddock`, placement `paddock`), so Go permission-plan digests diverge
  from Python's by construction. Semantics unchanged.
- **Non-goals (unchanged):** Windows containment; VM / AppContainer tiers
  (signposted in both implementations); host-wiring `yaah`.
