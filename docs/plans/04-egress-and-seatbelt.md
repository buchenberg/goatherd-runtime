# Implementation Plan: Egress Mediation & the Seatbelt Backend (Go)

> **Scope.** This is **Plan 4** of the containment/execution work. Plans 1–3
> shipped the Linux filesystem paddock (the syscall deny) and the authority spine.
> The containment axis has two gaps left:
>
> 1. **macOS** — Python's `SeatbeltContainmentBackend`; the Go port has no macOS jail.
> 2. **Network egress** — the jail is host-blind; a *broker* evaluates the
>    host predicate the jail could not, and egress is *pinned* so the broker is
>    the only route out.
>
> **Egress is not macOS-specific.** The broker is a plain loopback CONNECT proxy
> (stdlib sockets) on either platform. What differs is the *pinning*: Python's
> Linux jail is filesystem-only (it never did networking), so the reference puts
> pinning on Seatbelt's `(deny network-outbound)`. Linux can pin too — and better
> than the reference (§8).
>
> **Status:** Phase 2 (the portable egress broker, `sandbox/egress`) and Phase
> 3's **Linux** half (Landlock net pinning in `sandbox/landlock`) are implemented
> and kernel-verified. Phase 1 (Seatbelt, macOS) and the strong netns/`croft` pin
> remain — Phase 1 is being built on a Mac.
>
> **Contract note (Phase 3).** The `containment.Backend` interface evolved:
> `ProfileFor(roots []string, network NetworkPolicy) string` (was `(roots,
> allowNetwork bool)`) and a new `SupportsNetwork(mode NetMode) bool` fail-closed
> gate. The backend-private profile is now a JSON object
> (`{"roots":[…],"net":{"mode":…,"port":…}}`). A Seatbelt backend must implement
> the same contract.

## 1. Objective & Context

Shipped today:
- **Plan 1** — `sandbox/containment` (contract), `sandbox/landlock` (the FS
  syscall deny), `sandbox/confinement` (authority lowering), `GitSandbox` exec.
- **Plan 2/3** — `run.Reversible` (reversible execution + authority merge).

Gaps this plan closes:
- No `Backend` for macOS (Seatbelt).
- `NetworkPolicy`/`NetMode` ship as **spec only** (Plan 1 §2): the jail ignores
  the network axis and nothing enforces `AllowedHosts`.

## 2. What "Parity" Means Here (the reference surface)

| Python | File | Role |
|---|---|---|
| `SeatbeltContainmentBackend`, `lower_to_seatbelt`, `/usr/bin/sandbox-exec` | `_seatbelt_containment.py` | the macOS jail |
| `Verdict`, `HostAtom`, `HostGrant` | `_egress_broker.py:48-101` | the host predicate |
| `EgressDecision`, `EgressPolicy.decide` | `_egress_broker.py:103-133` | the pinned evaluation order |
| `EgressBroker` | `_egress_broker.py:139-271` | the loopback CONNECT proxy |
| `policy_from_grants`, `allow_hosts` | `_egress_broker.py:274-281` | construction |
| `NetworkPolicy` / `NetMode`, `broker_grants` | `_execution_capability.py:74-105` | the network axis (Plan 1 has the spec) |

## 3. Scope & Explicit Non-Goals

**In scope:** the Seatbelt backend (macOS); the portable egress broker; egress
pinning per platform (Linux Landlock v4 / macOS Seatbelt / netns routed to
`croft`).

**Non-goals:**
- **seccomp** — Plan 5.
- **SOCKS5 / transparent redirect / DNS stub / escalate→supervisor** — the
  reference's own increment 2/3 backlog; not needed for parity.
- **Container-tier networking** — `croft`/containerd (netns for the strong,
  address-scoped case).
- **Host wiring** (`yaah`).

## 4. Parity Map (completion checklist)

| # | Python symbol | Go target | Test |
|---|---|---|---|
| 1 | `Verdict` | `egress.Verdict` | `TestEgressPolicy_Decide` |
| 2 | `HostAtom` (`exact`/`in`/`contains`/`matches`) | `egress.HostAtom` | `TestHostAtom_Matches` |
| 3 | `HostGrant` (positive ∧ ¬negative, verdict) | `egress.HostGrant` | `TestEgressPolicy_DenyWins` |
| 4 | `EgressPolicy.decide` (union, default-deny, escalate→deny-loud) | `egress.EgressPolicy.Decide` | `TestEgressPolicy_Decide` |
| 5 | `EgressDecision` | `egress.EgressDecision` | (above) |
| 6 | `EgressBroker` (loopback CONNECT proxy) | `egress.Broker` | `TestBroker_AllowsAndDenies` |
| 7 | `policy_from_grants` / `allow_hosts` | `egress.PolicyFromGrants` / `egress.AllowHosts` | `TestAllowHosts` |
| 8 | `SeatbeltContainmentBackend` | `seatbelt.Backend` (nested module, darwin) | `TestSeatbelt_*` |
| 9 | `lower_to_seatbelt` (SBPL profile) | `seatbelt.ProfileFor` | `TestSeatbelt_ProfileFor` |
| 10 | jail network pinning (Seatbelt `deny network-outbound`) | `seatbelt` profile + broker port | `TestSeatbelt_Network` |
| 11 | jail network pinning (Linux) | `landlock` net rules (ABI ≥ 4) | `TestLandlock_NetworkDenyAll`, `TestLandlock_NetworkPinned` | ✅ |
| 12 | `broker_grants` → policy lowering | `egress.PolicyFromNetwork` | `TestPolicyFromNetwork` | ✅ |

## 5. Architecture

The network axis splits into two responsibilities, exactly as the reference
names them ("a syscall jail is host-blind — it can pin egress to a loopback port
but cannot allowlist *which host*", `_egress_broker.py:4-9`):

```
NetworkPolicy{Mode, AllowedHosts}
        │
        ├─ broker: egress.Broker filters *which host*  (portable; Go net)
        │     AllowHosts(policy.AllowedHosts) → loopback CONNECT proxy
        │
        └─ jail:   pin egress so the broker is the only route out (per-platform)
              Linux:  Landlock ≥ ABI 4 NET_CONNECT_TCP rules (port-scoped)
              macOS:  Seatbelt (deny network-outbound) except the broker
              strong: a network namespace (croft)
```

The pin is what makes the broker **unbypassable by construction**.

## 6. Phase 1 — The Seatbelt backend (macOS)

Python execs `/usr/bin/sandbox-exec -p <SBPL profile>` via `subprocess`; **no cgo
is needed** — Go execs the same binary with a profile string. A new **nested
module** `sandbox/seatbelt` (darwin build-tagged), mirroring `sandbox/landlock`:

```go
type Backend struct{}
func New() *Backend
func (b *Backend) Name() string            { return "seatbelt" }
func (b *Backend) EnforcementTier() string { return "native-syscall-deny" }
func (b *Backend) Available() (bool, string)          // /usr/bin/sandbox-exec present
func (b *Backend) ProfileFor(roots []string, allowNetwork bool) string // SBPL
func (b *Backend) Probe(profile, workingRoot string, roots []string) error
func (b *Backend) Launch(profile string, req containment.LaunchRequest) (containment.LaunchResult, error)
```

`ProfileFor` lowers writable roots to `(allow file-write* (subpath "…"))` plus a
deny-default, and adds `(deny network-outbound)` when `allowNetwork` is false —
the SBPL lowering the reference tests assert (`test_containment_backend.py:39-60`).
`Launch` runs `sandbox-exec -p <profile> <command>`. Caveat: `sandbox-exec` is
deprecated by Apple (still present) — the same risk the reference carries.

## 7. Phase 2 — The egress broker (portable) — **implemented**

New package `sandbox/egress` (root module; stdlib only). A faithful port of
`_egress_broker.py`:

- `Verdict` (`allow`/`deny`/`escalate`), `HostAtom` (`exact`/`in`/`contains`/
  `matches`), `HostGrant` (positive ∧ ¬negative), `EgressDecision`,
  `EgressPolicy.Decide` (union-across-grants, deny-wins within a grant,
  default-deny, escalate→deny-loud), `PolicyFromGrants`, `AllowHosts`.
- `Broker` — a loopback CONNECT proxy: accept → read the request head → parse
  `CONNECT host:port` → decide → 200 + splice, or 403; records every decision.
- Deviations recorded: an unknown host-atom op fails **closed** (deny + record)
  rather than raising; the read deadline is cleared before splicing (Python
  leaves a 3 s socket timeout in place, which truncates long tunnels).

## 8. Phase 3 — Egress pinning (per platform)

- **Linux — Landlock ≥ ABI 4** (kernel 6.7+): **implemented**. The ruleset gains
  `handled_access_net` + `LANDLOCK_ACCESS_NET_CONNECT_TCP`; a `DenyAll` mode
  denies every TCP connect, a `Broker` mode pins egress to the broker's port.
  `SupportsNetwork` gates it (ABI ≥ 4), and the probe validates the pin
  fail-closed (`TestLandlock_Network*`). Unprivileged. **Port-scoped, not
  address-scoped** — a documented limitation; the reference's Linux jail does no
  networking at all, so this is *ahead* of parity.
- **macOS — Seatbelt**: `(deny network-outbound)` with the broker's loopback port
  allowed (Phase 1, on a Mac).
- **Strong (address-scoped) — network namespace** (`CLONE_NEWUSER|CLONE_NEWNET`)
  or `croft`/containerd. Out of the unprivileged path; routed to `croft`.
- **Wiring**: `egress.PolicyFromNetwork(NetworkPolicy)` builds the policy from
  `AllowedHosts`; the run starts the broker, reads its port, and passes it to the
  jail's network pin.

## 9. Test & conformance matrix

| Phase | Test | Action | Expected |
|---|---|---|---|
| 2 ✅ | `TestHostAtom_Matches` | each op; bad op | matches / fail-closed error |
| 2 ✅ | `TestEgressPolicy_Decide` | allow, deny, escalate, default-deny | correct verdict + kind |
| 2 ✅ | `TestEgressPolicy_DenyWins` | positive + negative in one grant | negative excludes |
| 2 ✅ | `TestAllowHosts` | exact allowlist | allows listed, denies others |
| 2 ✅ | `TestBroker_AllowsAndDenies` | CONNECT an allowed loopback echo / a denied host | 200 + echo spliced / 403; decisions recorded |
| 2 ✅ | `TestBroker_BadRequest` | non-CONNECT bytes | 400 |
| 2 ✅ | `TestPolicyFromNetwork` | `NetworkPolicy{BROKER, hosts}` | policy with those grants; non-broker refused |
| 3 ✅ | `TestLandlock_SupportsNetwork` | probe the capability | reflects the ABI |
| 3 ✅ | `TestLandlock_NetworkDenyAll` | ABI ≥ 4; deny_all | a connect is denied at the syscall |
| 3 ✅ | `TestLandlock_NetworkPinned` | ABI ≥ 4; pin a port | the pinned port admits; others denied |
| 3 ✅ | `TestLandlock_ProbeNetwork` | deny_all / broker profiles | the probe validates the pin fail-closed |
| 2+3 ✅ | `TestE2E_ReversibleBrokerEgress` | reversible run + worktree + broker + pin | a through-broker CONNECT is admitted (200) and the broker records an allow; a direct connect bypassing the broker is refused by the pin |
| 1 | `TestSeatbelt_*` | macOS | deny/allow as the reference asserts (on a Mac) |

## 10. Parity Comparison: Network & macOS

| Capability | Python Shepherd | Go (this plan) |
|---|---|---|
| Egress broker | `_egress_broker.py` (stdlib sockets) | **Parity**: `sandbox/egress` (stdlib) |
| Policy evaluation order | union, deny-wins, default-deny, escalate→deny-loud | **Parity** |
| Egress *pinning*, Linux | ❌ (FS-only jail) | ✅ **Ahead**: Landlock ≥ ABI 4 net rules (port-scoped), kernel-verified |
| Egress *pinning*, macOS | Seatbelt `(deny network-outbound)` | **Parity**: `sandbox/seatbelt` |
| macOS jail | `_seatbelt_containment.py` | **Parity**: `sandbox/seatbelt` (nested module) |
| Strong (address-scoped) pin | — | netns via `croft` |
| SOCKS5 / DNS stub / escalate→supervisor | backlog (increments 2–3) | **Non-goal** (matches reference) |

## 11. Sequencing, Dependencies, Risks

**Order:** Phase 2 (broker, portable — done) → Phase 1 (Seatbelt) → Phase 3
(pinning: Landlock net, then macOS, then netns/croft).

**Dependencies:** Plan 1 (`Backend` contract, `NetworkPolicy`), `x/sys ≥ v0.19.0`
for the Landlock net constants (already pinned).

**Risks / mitigations:**
- *Broker bypass* → the pin (Landlock net / Seatbelt) makes the broker the only
  egress route; test the pin against a direct connect.
- *Landlock net is port-scoped* → document; route address-scoped needs to a netns.
- *`sandbox-exec` deprecation* → same risk as the reference; unavailable ⇒
  `Available()` false ⇒ advisary/refuse.
- *Broker SSRF* → the policy allowlist is the gate; the broker dials only hosts
  the policy admits.

## 12. Open Questions / Decisions

1. **Broker package**: `sandbox/egress` (proposed) vs part of `containment`.
   *Recommend `sandbox/egress` — it is a distinct axis.*
2. **Seatbelt module**: nested module `sandbox/seatbelt` mirroring `landlock`.
   *Recommend yes.*
3. **Landlock net**: extend the existing ruleset vs a separate `net` profile.
   *Recommend extending the ruleset (one `handled_access_net`, one rule per pin).*
4. **Escalate**: keep deny-loud (parity), or wire a supervisor path.
   *Recommend deny-loud for now — the reference's escalate path is unbuilt.*
