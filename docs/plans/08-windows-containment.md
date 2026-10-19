# Implementation Plan: Windows Write-Restricted Containment for Shepherd Go

> **Status: planned (spike-validated).** The mechanism is proven on a real,
> non-elevated Windows host (spike evidence in §11). This plan defines the
> Windows member of the `ContainmentBackend` family: the Win32 engine, the
> fail-closed probe, the backend, the placement wiring, and the honest
> capability reporting. It is a **self-contained component** (module boundary in
> §13); host integration (`yaah`) is a follow-on, exactly as Plan 1's §10.3 was.

## 1. Objective & Context

The containment family is documented in `vcs-core/_containment.py:1-18` as
*"Seatbelt (macOS), Landlock+seccomp (Linux), AppContainer, container, and VM
tiers."* Two members are built — Seatbelt (Python) and Landlock (Go) — and
**AppContainer is named but never built** (`_containment.py:52`).

Windows is the runtime's most-used developer platform, and today it has **no
containment at all**:

- `detect_containment_backend()` (Python `_execution_capability.py:276-288`)
  returns `None` for anything that is not darwin/linux.
- Go's `landlock.Detect()` returns `nil` off Linux; `sandbox/git`'s
  `WithExec()` therefore resolves to the advisory tier on Windows.
- `docs/plans/README.md:259` lists *"Windows containment"* as an explicit
  non-goal, and `docs/plans/07-yaah-integration.md:76` records that Windows
  *"stay[s] advisory"*.

**Goal:** give the runtime a **native Windows** containment tier with the same
contract, the same fail-closed probe, the same placement semantics, and the
same denial classification as the Landlock member — so that a retained
sub-agent dispatch on Windows is OS-confined rather than merely directory-
isolated.

**This is not a parity gap.** The Python reference has no Windows backend
either. It is a *family extension*: the **contract** is parity with the Linux
member; the **mechanism** is a different Windows primitive (§5.1).

## 2. What "Parity" Means Here

Parity is defined against **this repo's own Linux member**
(`sandbox/landlock`), not against Python:

| Axis | Parity target | Note |
|---|---|---|
| `containment.Backend` interface | byte-for-byte the same interface | no new contract, no new spec types |
| `ConfinementSpec` / `NetworkPolicy` / `NetMode` | same types | no new vocabulary |
| `LaunchConfined` (resolve → profile → probe → launch) | same order, same fail-closed points | |
| `ResolveSpec` canonicalization | same | |
| `Probe` (liveness / per-root / deny-closed) | same three checks, same semantics | §8.2 |
| Exit-code-aware "confine failed" | same sentinel value (rc 3) | |
| Placement `paddock` / `advisory` / `auto` | same | |
| Denial classification (`ErrSandboxDenied`) | same | |
| Permission-plan digest | same monitor id (`syscall_paddock`) | the monitor is the *enforcement* monitor; see §10.3 |
| **Mechanism** | **different** | ACL + mandatory integrity, not a syscall hook (§5.1) |
| **Enforcement tier label** | **different** | `native-access-check-deny`, not `native-syscall-deny` (§10.2) |
| **Network axis** | **narrower** | only `allow_all` is representable (§10.1) |

## 3. Parity Scope & Explicit Non-Goals

| Component | File | Go target | In scope? |
|---|---|---|---|
| `ContainmentBackend` interface, error taxonomy | `sandbox/containment/backend.go` | **reused unchanged** | ✅ (no work) |
| `ConfinementSpec`, `NetworkPolicy`, `NetMode`, `ResolveSpec` | `sandbox/containment/spec.go` | **reused unchanged** | ✅ (no work) |
| `LaunchConfined`, `Capability` | `sandbox/containment/confined.go` | **reused unchanged** | ✅ (no work) |
| `may=` lowering, per-binding grants, disjointness guard | `sandbox/confinement/*` | **reused unchanged** | ✅ (no work) |
| Placement, permission plan, evidence digest | `sandbox/confinement/*` | **reused unchanged** | ✅ (no work) |
| Win32 token engine (restricted token, integrity, default DACL) | — | `sandbox/winacl` nested module | ✅ |
| Win32 grant engine (per-root SID, DACL merge, mandatory label) | — | `sandbox/winacl` nested module | ✅ |
| Win32 runner (spawn a child under the restricted token) | — | `sandbox/winacl` nested module | ✅ |
| The backend (`ProfileFor` / `Probe` / `Launch` / `Available` / `SupportsNetwork`) | — | `sandbox/winacl` nested module | ✅ |
| `GitSandbox` opt-in `Exec` on Windows | `sandbox/git/backend.go` | honest `Capabilities` on Windows | ✅ (§10.4) |
| `sandbox/win32` **container** tier (Hyper-V / Windows Sandbox) | — | — | ❌ non-goal |
| **AppContainer** backend | — | — | ❌ non-goal (§3.1) |
| Network egress enforcement (WFP / firewall / proxy pinning) | — | — | ❌ non-goal (§10.1) |
| Read-side confinement | — | — | ❌ non-goal (§5.3) |
| `yaah` host wiring | — | — | ❌ deferred (§12.3) |

### 3.1 Why not AppContainer

AppContainer is the *identity* route: the token carries no ambient read
access, so **every** path the child may read must be pre-granted through a
capability or an explicit ACE. The runtime's read model is "read the workspace
and whatever else the agent asks for" — supporting that under AppContainer
means mutating the host's DACLs wholesale, which is a standing side effect on
the user's machine and a cost a write-only restriction does not need.

The mechanism chosen instead keeps the caller's own identity, so **reads stay
ambient** and only writes are narrowed — which is exactly the Landlock
semantics the `may=` model is built on. The same reasoning is recorded
independently in
`deepseek-harness/.agents/notes/implemented/feature/2026-08-08-windows-acl-restricted-token-sandbox.md`
("Why not AppContainer?"), whose implementation is the prior art this plan
ports.

Microsoft's newer `Experimental_CreateProcessInSandbox` (Windows 11, experimental)
is the AppContainer-shaped route, and it is **signposted, not built** (§3.2).

### 3.2 Signposted: `Experimental_CreateProcessInSandbox`

A first-class Microsoft sandbox-spec API (documented 2026-06-01) exists:
`processmodel.dll` exports `Experimental_CreateProcessInSandbox`, which takes
a compiled FlatBuffer `SandboxSpec` with `fs_read_write`, `fs_read_only`,
`network_policy`, `capabilities`, `integrity`, `disallow_win32k_system_calls`,
and `ui_restrictions`. It is a near-perfect structural analogue of
`ProfileFor(writable_roots, network)` + `Probe` + `Launch`.

It is **not** built here because: the `SandboxSpec.fbs` schema is unpublished
and there is no public header (`GetProcAddress` only); the mxc OS-version
policy floors the BaseContainer tier at 25H2+ (build 26600+) with the OS
feature enabled, and falls back to AppContainer + host-side DACL augmentation
on earlier releases; the caller must not itself be an AppContainer process;
and the identity must not collide with an installed MSIX package family name.
**Revisit when the schema ships** — it would replace the network gap (§10.1)
and the read-grant model at once.

## 4. Parity Map (completion checklist)

| # | Symbol | Go symbol (as built) | Test | Done |
|---|---|---|---|---|
| 1 | `ContainmentBackend` | `containment.Backend` (unchanged) | interface conformity | ✅ |
| 2 | `SandboxDenied` | `containment.ErrSandboxDenied` (unchanged) | `TestWinACL_DeniedClassified` | ➖ (the classifier is shared) |
| 3 | `JailNotEstablished` | `containment.ErrPaddockNotEstablished` (unchanged) | `TestWinACL_ProbeCatchesAMisloweredProfile` | ✅ |
| 4 | token construction | `winacl.createRestrictedToken` | `TestWinACL_AvailableOnWindows` | ✅ |
| 5 | integrity lowering | `winacl.lowerIntegrity` | `TestWinACL_AvailableOnWindows` | ✅ |
| 6 | default DACL merge | `winacl.setDefaultDacl` | `TestWinACL_WriteLandsInsideAGrantedRoot` | ✅ |
| 7 | per-root write SID | `winacl.WriteSidFor` | `TestWriteSidForIsDeterministic` | ✅ |
| 8 | grant (allow ACE + ambient-delete deny + Low label) | `winacl.GrantWrite` | `TestWinACL_WriteLandsInsideAGrantedRoot` | ✅ |
| 9 | revoke | `winacl.RevokeWrite` | `TestWinACL_RevokeRemovesTheCapabilityAce` | ✅ |
| 10 | exact-ACE skip (idempotence) | `winacl.hasExactACE` | `TestWinACL_GrantIsIdempotent` | ✅ |
| 11 | `profile_for` | `(*winacl.Backend).ProfileFor` | `TestProfileForReadOnly`, `TestProfileForWorkspaceWrite` | ✅ |
| 12 | `probe` (three checks) | `(*winacl.Backend).Probe` | `TestWinACL_ProbePassesForAConformantProfile` | ✅ |
| 13 | `launch` | `(*winacl.Backend).Launch` | `TestWinACL_WriteLandsInsideAGrantedRoot` | ✅ |
| 14 | `available` | `(*winacl.Backend).Available` | `TestWinACL_AvailableOnWindows` | ✅ |
| 15 | `supports_network` | `(*winacl.Backend).SupportsNetwork` | `TestWinACL_NetworkModeWithoutEnforcementIsRefused` | ✅ |
| 16 | `detect_containment_backend` | `winacl.Detect()` | `TestBackendImplementsTheContract` | ✅ |
| 17 | runner (spawn under token) | `winacl.MaybeRunRunner` | `TestWinACL_*` (the test binary is the runner) | ✅ |
| 18 | denial classification | `containment.DeniedExit` | shared with the Linux member | ➖ |
| 19 | honest `Capabilities` | `sandbox/git` `Capabilities` | unchanged — the backend is injected | ✅ (no work) |
| 20 | placement resolution | `confinement.Placement` (unchanged) | `TestWinACL_NetworkModeWithoutEnforcementIsRefused` | ✅ |

## 5. Architecture

### 5.1 The mechanism is a different kind of boundary

Landlock is a **kernel LSM**: the process applies a ruleset to itself, and the
kernel checks *every write syscall* against it. There is no Windows equivalent.
Windows enforces access through an **authorization model** — the security
reference monitor evaluates the object's DACL plus the caller's token — and
there is no unprivileged, per-process, path-scoped "deny writes below X"
primitive.

The Windows member therefore composes **four** kernel-enforced layers to reach
the same observable property ("writes succeed only beneath the granted roots"):

| # | Layer | Mechanism | What it closes |
|---|---|---|---|
| 1 | Write-restricted token | `CreateRestrictedToken` with `WRITE_RESTRICTED` | the access check runs **twice** — once against the normal SIDs, once against the restricting SIDs — and a write must satisfy **both** |
| 2 | Restricting SIDs | `[logon SID, Everyone, <per-root write SID>]` | the write pass-2 check admits a write only where a restricting SID holds a Write ACE |
| 3 | Low integrity + mandatory label | token at `S-1-16-4096`; granted roots carry a Low **no-write-up** label | the integrity check denies a write to any object not labeled Low — this is what actually stops a write to an *ungranted* directory that Everyone can write |
| 4 | Ambient-delete deny | a Deny ACE for `FILE_DELETE_CHILD` on the world SID, container-inherit only | Windows also authorizes a delete from the **parent directory's** right, which no restricting SID co-signs; without this, a confined child could delete a file outside every root |

Layer 3 is load-bearing and easy to miss: the write pass-2 check alone does
**not** stop a write outside the roots, because `Everyone` is in the
restricting list (it must be — without it, early DLL init dies with
`0xC0000142` and CNG crashes `pwsh` with `0xE0434352`) and `Everyone` holds
ambient Write on much of the disk. The Low label is what denies it.

Layer 4 is likewise load-bearing: the delete authority that reaches a file
through its parent's `FILE_DELETE_CHILD` is not a write-class access, so
`WRITE_RESTRICTED` does not intersect it.

### 5.2 The identity: one write SID per root

Each granted root gets a **deterministic** capability SID, derived from the
root's canonical path:

```
writeSid(root) = "S-1-4-" + (sha256(root)[0:4] mod (2^30-1) + 1) + "-" + (sha256(root)[4:8] mod (2^30-1) + 1)
```

The SID names no account, so its power is defined *solely* by the ACEs that
name it. Its determinism buys a **standing-grant reuse cache**: the ACE for a
given root materializes once per root per machine, and every later provision
hits an exact-ACE skip instead of re-propagating the tree (§12.2).

The canonical path is the *input* to the derivation, so two spellings of one
root converge on one identity; a renamed root derives a new SID and leaves the
old ACEs as inert residue.

### 5.3 What is deliberately *not* confined

- **Reads.** `WRITE_RESTRICTED` intersects write accesses only. A confined
  child can read anything the caller can read. This is **Landlock parity**
  (Landlock v1's write mask excludes read/exec bits), not a gap — but it is a
  *documented* boundary, and the runtime must not claim read confinement.
- **Network.** The mechanism has no egress control at all (§10.1).
- **Process visibility.** A confined child can see other processes.
- **Hard links.** NTFS hard links alias the *file object*, not the path, so a
  granted root's hard link grants an external alias. Structurally unavoidable
  for this token shape; it is why the enforcement tier is not "full" (§10.2).

## 6. Phase 1 — The contract (no work)

The `containment` and `confinement` packages are reused **unchanged**. The
Windows backend implements the same `containment.Backend` interface and the
same `LaunchConfined` flow (resolve → profile → probe → launch). No new spec
types, no new placement values, no new plan fields.

The one addition is `winacl.Detect()` — mirroring `landlock.Detect()` — which
lives in the nested module for the same module-cycle reason Plan 1 §16.2 #3
records.

## 7. Phase 2 — The Win32 engine (`sandbox/winacl`)

### 7.1 The x/sys surface — and two gaps

`golang.org/x/sys/windows` covers the whole surface **except two calls**:

| Need | x/sys/windows | Resolution |
|---|---|---|
| `CreateRestrictedToken` | **absent** | `advapi32.dll` lazy proc |
| `AddMandatoryAce` | **absent** (no `InitializeAcl` / `AddAce` either) | `advapi32.dll` lazy proc + a hand-built 8-byte ACL header |
| `OpenProcessToken`, `GetTokenGroups`, `SetTokenInformation`, `CreateWellKnownSid`, `StringToSid`, `GetLengthSid`, `ACLFromEntries`, `GetNamedSecurityInfo`, `SetNamedSecurityInfo`, `LocalAlloc`/`LocalFree`, `CreateProcessAsUser`, `CreateJobObject`, `AssignProcessToJobObject`, `ComposeCommandLine` | present | — |

Verified present with identical signatures in **`golang.org/x/sys v0.19.0`** —
the version `sandbox/landlock` already pins — so the nested module needs no
new dependency version and no toolchain bump (v0.19.0 declares `go 1.18`).

**`AddMandatoryAce` needs a hand-built ACL.** x/sys exposes the `ACL` struct
but no `InitializeAcl`/`AddAce`, and the struct's fields are unexported. The
engine writes the 8-byte header directly into a buffer and calls
`AddMandatoryAce` to append the label ACE.

**`AclSize` is the buffer's capacity, not its used size.** `AddAce` refuses
with `ERROR_NOT_ENOUGH_MEMORY` when the new ACE would not fit within
`AclSize`, and writes the *used* size back on success. Setting `AclSize` to
the used size (the natural reading of the field) therefore fails on a buffer
that has exactly enough room — the failure is
`ERROR_NOT_ENOUGH_MEMORY`, which reads like exhaustion and is not.

### 7.2 Three traps that cost real debugging time

1. **`windows.OpenCurrentProcessToken()` opens with `TOKEN_QUERY` only.**
   `CreateRestrictedToken` requires `TOKEN_DUPLICATE` on the existing token
   handle and fails with **`ERROR_ACCESS_DENIED`** otherwise. The backend must
   call `OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY|TOKEN_DUPLICATE|TOKEN_ADJUST_DEFAULT|TOKEN_ASSIGN_PRIMARY, &tok)`
   directly. (Symptom: `CreateRestrictedToken` fails with access-denied even
   with **no flags and no restricting SIDs** — which looks like a privilege
   wall but is not. MSDN confirms ordinary applications may create restricted
   tokens: *"If a CreateProcessAsUser call specifies a restricted version of
   the caller's primary token, this privilege is not required."*)
2. **`AddMandatoryAce` needs a hand-built ACL** whose `AclSize` is the
   *capacity*. See §7.1.
3. **`windows.GetNamedSecurityInfo` already frees the descriptor.** It returns
   a **Go-allocated copy** of the security descriptor, so the DACL and SACL
   pointers point into Go memory and the descriptor must **not** be freed
   again. Freeing it a second time corrupts the heap — the symptom is
   `STATUS_HEAP_CORRUPTION` in the *child* process, far from the call that
   caused it.

### 7.3 Token construction

```
OpenProcessToken(current, TOKEN_QUERY|TOKEN_DUPLICATE|TOKEN_ADJUST_DEFAULT|TOKEN_ASSIGN_PRIMARY)
GetTokenGroups → find SE_GROUP_LOGON_ID → the logon session SID
CreateRestrictedToken(current, DISABLE_MAX_PRIVILEGE|LUA_TOKEN|WRITE_RESTRICTED,
                      SidsToRestrict = [logon SID, Everyone, <write SID per root>])
SetTokenInformation(TokenIntegrityLevel, Low)                    # S-1-16-4096
SetTokenInformation(TokenDefaultDacl, merged)                    # + full-access ACE for a write SID
```

- `DISABLE_MAX_PRIVILEGE | LUA_TOKEN` synthesize the limited-user effect
  token-side, so an elevated caller also derives a filtered token.
- The **logon SID + Everyone keep-alive pair** is present in every mode. It is
  not optional: without the logon SID, early DLL initialization dies with
  `0xC0000142`; without Everyone, CNG (`\Device\CNG`) fails and `pwsh` crashes
  with `0xE0434352`. Everyone confers **no** write authority — the Low label
  denies it — it only keeps the process alive.
- `Authenticated Users` is deliberately **absent** (it closes the `C:\`-root
  tree-creation escape and makes CIM unavailable); `INTERACTIVE`/`LOCAL` are
  absent (the host's Public tree grants write to INTERACTIVE).
- The **default DACL** must name a restricting SID. A restricted token
  inherits the user's default DACL, which names no restricting SID, so every
  object the child creates (anonymous stdio pipes, sync objects) would fail
  the write pass-2 check at creation — `ERROR_ACCESS_DENIED`, surfacing as
  `EPERM` on every piped-stdio grandchild spawn. The merge adds a full-access
  ACE for the *private temp* write SID when one exists, else the workspace
  write SID, else Everyone (read-only mode).

### 7.4 The grant (per root)

Three edits, applied in **one** `SetNamedSecurityInfo` call:

1. **Allow** the write SID `GRANT_MASK` (Write + Delete), inheriting to
   subcontainers and objects.
2. **Deny** the world SID `FILE_DELETE_CHILD`, inheriting to **containers
   only** — the right is evaluated on directories, and inheriting its bit
   (`0x40`, a member of `FILE_ALL_ACCESS`) onto files would refuse every
   `GENERIC_ALL` open inside the root.
3. **Apply** a Low no-write-up mandatory label, inheriting to subcontainers
   and objects.

The DACL is **read-merge-write** against the directory's *current* explicit
DACL so pre-existing explicit ACEs survive, and the whole
get-merge-set sequence runs under a per-path exclusive `LockFileEx` lock so
concurrent sandboxes cannot clobber each other's ACEs.

**Idempotence.** The exact ACE, deny, and label are detected in the current
DACL and the whole apply is **skipped** when they stand. Without this, every
provision re-propagates the identical descriptor across the whole tree.

**Ownership precondition.** The label lives in the SACL, so the grant needs
`WRITE_OWNER` in addition to the owner's implicit `WRITE_DAC`. A full-control
workspace directory satisfies both; a directory granting only Modify must
**fail loudly** rather than silently skip the confinement.

### 7.5 The runner (the Windows analogue of the Landlock trampoline)

Go has no `python -m <module>` entry point, so the Landlock member re-execs
`/proc/self/exe` with a sentinel argv. The Windows member does the same, with
`os.Executable()` and a sentinel:

```
argv = [exe, RunnerSentinel, <profile-json>, <command...>]
```

On the sentinel the runner:

1. parses the profile (canonical roots, per-root write SIDs, mode);
2. mints the restricted token (§7.3);
3. applies the grants for the profile's roots (§7.4) — **idempotent**, so the
   standing-grant cache is reused across runs;
4. spawns the command under the token with `CreateProcessAsUser`, cwd set to
   the profile's working root, stdio inherited;
5. wraps the child in an **unnamed Job Object with `KILL_ON_JOB_CLOSE`**, so
   the whole tree dies with the runner;
6. mirrors the child's exit code.

Because the token is a restricted version of the runner's own primary token,
`CreateProcessAsUser` needs **no** `SE_ASSIGNPRIMARYTOKEN_NAME` privilege —
this is the documented carve-out, and it is what keeps the mechanism
elevation-free.

The runner is a **required host integration point**, exactly as Plan 1 §5.2
records for the Landlock trampoline: a binary that forgets the hook fails
closed (rc 3 → `ErrPaddockNotEstablished`), never silently unconfined.

## 8. Phase 3 — The backend (`sandbox/winacl`)

### 8.1 Profile shape

`ProfileFor(roots, network)` returns a backend-private JSON document:

```json
{"roots":["C:\\...\\worktree"],"sids":["S-1-4-..."],"mode":"workspace-write","net":{"mode":"allow_all"}}
```

Two differences from the Landlock profile, both deliberate:

- The profile carries the **derived write SIDs**, not just the roots. On
  Windows the SID *is* the authority, and the SID derivation must happen
  exactly once per profile so the runner and the granting side agree.
- `mode` selects the restricting-SID list (`read-only` = no write SID;
  `workspace-write` = one write SID per root). This mirrors the reference
  implementation's mode vocabulary, which is the closest existing expression
  of "zero roots = ReadOnly, one root = Permissive, a subset = per-binding
  grants".

The profile is **not** byte-compatible with Python's — it is backend-private
and never crosses implementations, exactly as Plan 1 §16.2 #7 relaxes the
Landlock profile's byte parity.

### 8.2 The probe — the same three checks

`Probe(profile, workingRoot, roots)` ports the Python/Landlock probe
**unchanged in semantics**:

1. **liveness** — a write outside the working root must be **denied**;
2. **per-root conformance** — a canary write beneath **each** declared
   writable root must be **allowed** (catches a profile too strict, which
   would spuriously fail a legitimate body);
3. **deny-closed** — if the working root is not itself a writable root, an
   in-workdir path outside every root must be **denied** (catches a
   mis-lowered broader root).

Each probe write runs as a **confined child** (the runner in probe mode) and
the outcome is read from the exit code, so the probe exercises the *real*
enforcement surface rather than the parent's opinion of it. The exit-code-aware
"confine failed" sentinel (rc 3) is preserved: a broken restriction is
*"no paddock"*, never *"write denied"*, so a broken backend can never pass as
a working one.

The probe's own writes must be cleaned up **parent-side** (the parent is
unconfined), and the canary names must be unique per probe so a probe never
collides with — or deletes — a real workspace file.

### 8.3 Launch

`Launch(profile, req)` runs the runner with the command and returns
`LaunchResult{ExitCode, Stdout, Stderr}`. It does **not** classify the exit
status; a non-zero body exit is returned as-is, and the caller maps an
authority denial to `ErrSandboxDenied`.

## 9. Phase 4 — Placement, denial, evidence

Identical to the Landlock member:

- `paddock` placement with no available backend → the run **refuses**
  (`ErrPaddockNotEstablished`) before any effect.
- `advisory` → run unconfined; the run records containment as uncontained.
- `auto` → `paddock` when the backend is available, else `advisory`.
- A body that exits non-zero because the restriction denied a write → the run
  is recorded **discarded / Failed**, with the paddock monitor's digest as
  authority evidence (the write never reached disk — stronger than the
  carrier's check-at-commit).

## 10. The honest parts

### 10.1 The network axis is narrower on Windows

The mechanism has **no egress control**. Therefore:

| Mode | Representable on Windows? | Action |
|---|---|---|
| `NetAllowAll` | yes | honored |
| `NetDenyAll` | **no** | **refuse** (`ErrUnsupportedConfinementSpec`) |
| `NetBroker` | **no** | **refuse** |

`SupportsNetwork` returns true only for `NetAllowAll`, and `LaunchConfined`
already refuses a mode the backend cannot represent — so the refusal is
structural, not a special case.

**Consequence, and it is a real one:** `ConfinementSpec`'s zero value and
`ReadOnlySpec()` both default to `deny_all`, so a bare spec on Windows
**refuses** rather than running. `yaah`'s `agents.subagent.network` already
defaults to `allow_all` (Plan 7), so the host path is unaffected — but any
caller constructing a spec by hand must set `allow_all` explicitly on Windows.
This must be stated in the config docs, not discovered at runtime.

Closing the gap later means either a WFP/firewall egress fence (needs
elevation) or the AppContainer-shaped `Experimental_CreateProcessInSandbox`
(§3.2). **Do not paper over it with an advisory default** — the plan's own
rule is that the backend never silently ignores the network axis.

### 10.2 The enforcement tier is not "native-syscall-deny"

Landlock's tier label is `native-syscall-deny`. The Windows member's honest
label is **`native-access-check-deny`**: enforcement happens in the security
reference monitor's access check, not in a syscall filter. The distinction is
load-bearing for anyone reasoning about what the boundary covers (§5.3).

### 10.3 The containment stamp is `contained`, never `full`

`shepherd-kernel-go`'s `Containment` enum is
`full | contained | buffered | uncontained`. The Windows backend must stamp
**`contained`**: the hard-link boundary (§5.3) means the backend cannot
govern every file effect, and Plan 1 §10.2's rule — *"Capabilities must be
honest"* — forbids claiming `full`.

### 10.4 `GitSandbox.Capabilities` on Windows

`sandbox/git`'s `WithExec()` must report `Exec: true, Containment: contained`
on a Windows host with the backend available, and `Exec: true,
Containment: uncontained` otherwise — the same mode-aware shape Plan 1 §10.2
defines for Linux.

## 11. Spike evidence (the mechanism is proven)

The mechanism was validated on a **real, non-elevated Windows 11 host** before
this plan was written, with a standalone Go program using
`golang.org/x/sys/windows`. The spike is preserved in the repo at
[`spikes/winacl/`](spikes/winacl/) (its own module, not part of the release
surface) so the evidence below is reproducible rather than asserted. Seven
claims, all passing:

| # | Claim | Result |
|---|---|---|
| A | write **inside** each granted root | **ALLOWED** (both roots) |
| B | write **outside** every root | **DENIED** (`ERROR_ACCESS_DENIED`) |
| C | **delete** outside every root | **DENIED** — the ambient `FILE_DELETE_CHILD` hole is closed |
| D | **read** outside every root | **ALLOWED** — the documented read boundary (Landlock parity) |
| E | the **three-part probe** (liveness + per-root + deny-closed) | conformant, 1:1 with the Landlock probe |
| F | the **read-only** lowering (zero writable roots) | every write denied |
| G | the **unconfined host** | unaffected — writes outside still work |

Claim C is the one that most often goes unproven: the delete denial is
evidence that layer 4 (§5.1) is in place, and layer 4 is what stops a confined
child from deleting outside its roots through the parent directory's right.

The spike also produced §7.2's traps by direct encounter.

### 11.1 Shipped evidence

The backend ships with its own suite (`sandbox/winacl`, 22 tests, green on a
Windows 11 host, unelevated). It pins the same claims through the **backend
interface** rather than the spike's private helpers:

| Claim | Test |
|---|---|
| the backend is available on Windows | `TestWinACL_AvailableOnWindows` |
| write inside a granted root lands | `TestWinACL_WriteLandsInsideAGrantedRoot` |
| every granted root is writable | `TestWinACL_EveryGrantedRootIsWritable` |
| write outside every root is denied | `TestWinACL_WriteIsDeniedOutsideEveryRoot` |
| delete outside every root is denied | `TestWinACL_DeleteIsDeniedOutsideEveryRoot` |
| the read-only lowering denies every write | `TestWinACL_ReadOnlyDeniesEveryWrite` |
| reads are **not** governed (the documented boundary) | `TestWinACL_ReadIsNotGoverned` |
| the probe passes for a conformant profile | `TestWinACL_ProbePassesForAConformantProfile` |
| the probe catches a mis-lowered profile | `TestWinACL_ProbeCatchesAMisloweredProfile` |
| a network mode with no enforcement is refused | `TestWinACL_NetworkModeWithoutEnforcementIsRefused` |
| the grant is idempotent (the exact-ACE skip fires) | `TestWinACL_GrantIsIdempotent` |
| the revoke removes the capability ACE | `TestWinACL_RevokeRemovesTheCapabilityAce` |
| the write identity is deterministic and domain-separated | `TestWriteSidForIsDeterministic`, `TestTempWriteSidIsDomainSeparated` |
| the profile refuses a mismatched write identity | `TestProfileValidateRefusesAMismatchedIdentity` |
| the enforcement tier is not `native-syscall-deny` | `TestEnforcementTierIsNotSyscallDeny` |

## 12. Sequencing, dependencies, Risks

### 12.1 Order (each phase independently testable)

1. `sandbox/winacl` engine: token, SID derivation, grant/revoke, runner.
2. The backend: `Available` / `ProfileFor` / `Probe` / `Launch` / `Detect`.
3. The probe's three checks, exit-code aware.
4. `sandbox/git` opt-in `Exec` on Windows + honest `Capabilities`.
5. Placement wiring and the `deny_all` refusal (§10.1).
6. End-to-end: a retained dispatch confined on a Windows worktree.

### 12.2 Risks and mitigations

| Risk | Mitigation |
|---|---|
| **Eager DACL propagation cost.** `SetNamedSecurityInfo` on a directory with inheritable ACEs walks every descendant immediately — measured in tens of seconds on large trees. A worktree root is per-session, so a **new** worktree means a **new** SID and a **new** propagation. | The exact-ACE skip makes a *re*-provision of the same root O(1). The first provision of a fresh worktree is the cost; **measure it** (a benchmark over a real checkout) before enabling the backend by default. If it is unacceptable, the mitigation is to grant the **workspace** (stable) rather than the **worktree** (per-session) — a policy decision, not a mechanism change. |
| The grant mutates **real** directories (standing ACEs, Low labels) | The workspace ACE is the reuse cache and is deliberately never revoked; temp grants are revocable. The plan must document that a workspace directory carries a Low label afterwards, and that `icacls` cannot revoke the deny (`ERROR_NONE_MAPPED`) — revoke through the module. |
| `CreateRestrictedToken` denied (access) | §7.2 trap 1 — the token handle must carry `TOKEN_DUPLICATE`. A backend that hits this must **fail closed**, never fall back to an unconfined spawn. |
| A host forgets the runner hook | The runner exits with the confine-failed sentinel (rc 3) → `ErrPaddockNotEstablished` — fail-closed, never silently unconfined. |
| The `deny_all` network default refuses on Windows | §10.1 — documented, with `allow_all` as the explicit choice. Never silently degrade the network axis. |
| The `containment` package's `Detect` cannot import the nested module | Same shape as Plan 1 §16.2 #3: `Detect` lives in `winacl`. |
| A tree stamped by another AppContainer tool becomes unreadable to the confined child | Documented boundary of the Low-integrity token; surfaced in the backend's `Available()` reason so an operator can see it. |

### 12.3 Out of scope, stated explicitly

- **`yaah` host wiring.** Plan 1 §10.3 deferred it for Landlock; the same
  deferral applies. The backend ships as a component; wiring it to
  `yaah`'s retained dispatch is a separate plan that must also settle the
  `deny_all` interaction (§10.1) and the worktree-vs-workspace grant scope
  (§12.2).
- **Egress enforcement.** A separate axis, as in Plan 4b/4c for Linux/macOS.
- **Container tier.** Hyper-V / Windows Sandbox containers are a different
  tier with different properties (full VM isolation, no host-path write
  confinement) and are not the "native" tier this plan builds.

## 13. Module & Repo Layout

Same shape as Plan 1 §15: the **contract** and **authority lowering** stay in
the root module; the **backend** is a nested module so the root keeps its
"kernel + stdlib only" footprint (Plan 1 §15.1). The Win32 engine needs
`golang.org/x/sys` directly and is platform-specific, so it must not be
folded into the root module.

```
shepherd-runtime-go/                       module: github.com/buchenberg/shepherd-runtime-go (go 1.21)
│                                            deps: kernel + stdlib ONLY (invariant preserved)
├── sandbox/
│   ├── sandbox.go                         # Sandbox iface (unchanged)
│   ├── containment/                       # CONTRACT — reused unchanged
│   ├── confinement/                       # AUTHORITY LOWERING — reused unchanged
│   ├── git/                               # GitSandbox (honest Capabilities on Windows)
│   ├── landlock/                          # ★ NESTED MODULE (Linux, shipped)
│   └── winacl/                            # ★ NESTED MODULE (Windows, this plan)
│       ├── go.mod                         # requires golang.org/x/sys v0.19.0 (direct) + the root runtime
│       ├── doc.go                         # the package doc: the mechanism, and what it does NOT confine
│       ├── sid.go                         # write identity (sha256 -> S-1-4-x-y), temp identity
│       ├── profile.go                     # the backend-private profile + its consistency validation
│       ├── paths.go                       # canonicalization, root containment helpers
│       ├── backend.go                     # the Backend implementation (portable)
│       ├── mechanism_windows.go           # availability, the three-part probe, launch
│       ├── mechanism_other.go             # //go:build !windows — refuses rather than running unconfined
│       ├── token_windows.go               # restricted token, Low integrity, default DACL
│       ├── acl_windows.go                 # ACE reader, exact-ACE detection, label builder
│       ├── grant_windows.go               # GrantWrite / RevokeWrite
│       ├── runner_windows.go              # MaybeRunRunner: spawn under the token + kill-on-close job
│       ├── backend_test.go                # portable: identity, profile, network, tier
│       └── mechanism_windows_test.go      //go:build windows — the end-to-end suite
```

`x/sys v0.19.0` is the version `sandbox/landlock` already pins and is the
version in which the whole required surface was verified (§7.1), so the nested
module adds no new dependency version.

**No new repository.** The `sandbox/landlock` nested-module precedent (Plan 1
§15.3) applies: a small platform backend stays a nested module with its own
version stream (`sandbox/winacl/vX.Y.Z`).

## 14. Backlog / fence closure

On completion, the records this plan supersedes or amends must be updated, so
the fences reflect reality (the shape Plan 1 §14 established):

- `shepherd-kernel-go/PARITY-PLAN.md:188` — the "OS jails (Seatbelt/Landlock)
  and signature grants" row records that the *containerd* backend provides the
  Linux enforcement story. It should name the Windows native tier (this plan)
  as the Windows story, and the container tier as the Linux story.
- `shepherd-kernel-go/PARITY-PLAN.md:194` — the "Landlock exec-wrapper for
  git-sandbox Exec (Linux)" backlog item gets a Windows sibling: the
  `sandbox/winacl` backend behind the same opt-in `Exec`.
- `shepherd-kernel-go/plans/00-execution-plan.md:598-610` — the **Path B**
  fence ("None of it is ported, and R6 says it must not be by accident")
  needs the Windows decision recorded, the way Plan 1 recorded the Linux
  member's.
- `shepherd-kernel-go/GAP-REPORT.md:374` — the note that overlay/container
  isolation is *"Not viable on Windows (yaah's primary platform)"* is true of
  that tier and should point at this plan's native tier rather than leave
  Windows without an answer.
- `shepherd-runtime-go/docs/plans/README.md` — the status table, the gap
  inventory, the dependency graph, and the non-goals list are updated by this
  plan's landing (already done in the plan-writing commit).

## 15. Implementation status

**The component ships.** The backend is implemented as the nested module
`sandbox/winacl`, with the contract and authority lowering reused unchanged.
22 tests, green on a Windows 11 host, unelevated (§11.1). The root module is
untouched: `sandbox/git`'s `WithExec(b containment.Backend, p)` already takes
an injected backend, so a host selects one per platform and the honest
capability reporting works for Windows with no change to the root module.

### 15.1 What landed

| § | Package / file | Status |
|---|---|---|
| 6 | `sandbox/containment`, `sandbox/confinement` | ✅ reused unchanged |
| 7.1 | `sandbox/winacl/sid.go` | ✅ write identity, domain-separated temp identity |
| 7.1 | `sandbox/winacl/profile.go` | ✅ profile shape + consistency validation |
| 7.3 | `sandbox/winacl/token_windows.go` | ✅ restricted token, Low integrity, default DACL |
| 7.4 | `sandbox/winacl/acl_windows.go` | ✅ ACE reader, exact-ACE detection, label builder |
| 7.4 | `sandbox/winacl/grant_windows.go` | ✅ grant / revoke |
| 7.5 | `sandbox/winacl/runner_windows.go` | ✅ runner, job object, environment block |
| 8 | `sandbox/winacl/backend.go` | ✅ the `containment.Backend` implementation |
| 8 | `sandbox/winacl/mechanism_windows.go` | ✅ availability probe, three-part probe, launch |
| 8 | `sandbox/winacl/mechanism_other.go` | ✅ off-Windows stubs that refuse |
| 11.1 | `sandbox/winacl/*_test.go` | ✅ 22 tests |

### 15.2 Deliberate deviations from the plan text

1. **The grant is applied by the runner, not by the caller.** The plan said
   the grant happened in the "grant" phase; as built, the runner applies it
   before it mints the token, so the grant and the token are one sequence
   rather than two that could disagree. The grant is idempotent, so the
   standing-ACE reuse cache still holds across runs.
2. **The profile's write SIDs are re-derived and compared by the runner.** The
   plan had the runner trust the profile's SIDs. As built, the runner
   re-derives each root's identity and refuses a profile whose SIDs disagree
   — the profile travels as argv, and a mismatch would grant one root's
   authority over another.
3. **The label ACL is rebuilt, not replaced.** The plan implied replacing the
   SACL with a single label ACE. As built, the existing label ACLs are
   dropped (a directory carries one integrity label) and every *other* SACL
   ACE — an audit ACE, say — is preserved, so the apply does not silently
   drop a security policy.
4. **The runner's probe child takes an operation** (`write`, `delete`,
   `read`) rather than only a write, so the shipped suite can pin the delete
   and read boundaries through the same mechanism the probe uses.
5. **The revoke leaves the ambient-delete deny and the Low label standing.**
   The plan left the revoke's scope open. Removing the deny would hand the
   parent-directory delete right back, and removing the label could hand the
   write back to a process still holding the capability. A revoked directory
   therefore keeps a standing Low label — a real change to a real directory,
   and recorded as such.

### 15.3 Still open

- **The eager propagation cost is unmeasured.** `SetNamedSecurityInfo` walks
  every descendant of the granted root, and a worktree root is per-session, so
  a new worktree means a new write identity and a new propagation. The
  exact-ACE skip makes a *re*-provision O(1); the first provision of a fresh
  worktree is the cost. Measure it against a real checkout before enabling
  the backend by default, and decide whether the granted root should be the
  per-session worktree or the stable workspace.
- **`yaah` host wiring** is deferred exactly as Plan 1 §10.3 deferred it for
  Landlock, and it must also settle the `deny_all` interaction (§10.1).
- **The `sandbox/winacl` module is untagged.** It carries a dev-time
  `replace` (the `sandbox/landlock` release discipline), so a release needs
  the `replace` dropped and the module tagged `sandbox/winacl/vX.Y.Z`.

### 15.4 What the implementation changed in this plan

Three findings came out of the implementation and are now recorded in §7.1
and §7.2: the `AddMandatoryAce` capacity semantics, the double-free of the
`GetNamedSecurityInfo` descriptor, and — the one that cost the most time —
`OpenCurrentProcessToken` opening the token without `TOKEN_DUPLICATE`. The
plan's §7.2 originally listed two traps; there are three.
