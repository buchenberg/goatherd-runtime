# Spike: Windows write-restricted containment

The validation artifact behind
[`../../08-windows-containment.md`](../../08-windows-containment.md) §11. It is a
standalone Go program, deliberately **not** part of any module in this repo —
it has its own `go.mod` and is not part of the release surface.

## What it proves

The Windows mechanism Plan 8 specifies — a write-restricted token, per-root
capability SIDs, a Low integrity level, a Low no-write-up mandatory label, and
an ambient-delete deny — reaches the same observable property as the Landlock
member: **writes succeed only beneath the granted roots**.

## Running it

```sh
cd docs/plans/spikes/winacl
go run .
```

It creates a temp tree with two granted roots and one ungranted sibling,
applies the grants, mints the token, and spawns children under it. It prints
one line per claim and exits `0` only if all of them hold.

Expected output (paths and SIDs vary):

```
spike: granted roots   [...\rootA ...\rootB]
spike: ungranted       ...\outside
spike: write SID ...\rootA -> S-1-4-...
spike: grants applied
spike: PASS A  write inside each granted root ALLOWED
spike: PASS B  write outside every root DENIED
spike: PASS C  delete outside every root DENIED
spike: PASS D  read outside every root ALLOWED (read boundary: not governed)
spike: PASS E  three-part probe conformant: liveness + per-root + deny-closed
spike: PASS F  read-only lowering denies every write
spike: PASS G  the unconfined host can still write outside
SPIKE PASS
```

Child processes print their own denial lines on stderr (`child: write denied:
... Access is denied.`); those are the *expected* denials, not failures.

## The claims

| # | Claim | Why it matters |
|---|---|---|
| A | write **inside** a granted root is allowed | the paddock admits what it should |
| B | write **outside** every root is denied | the paddock holds |
| C | **delete** outside every root is denied | proves the ambient-`FILE_DELETE_CHILD` layer is in place — `WRITE_RESTRICTED` does not intersect the parent directory's delete right |
| D | **read** outside every root is allowed | the documented read boundary (Landlock parity: reads are not governed) |
| E | the three-part probe is conformant | shepherd's `Probe` ports 1:1 |
| F | the read-only lowering denies every write | zero writable roots = ReadOnly |
| G | the unconfined host is unaffected | the grant does not damage the user's tree |

## Three traps it found

All three are recorded in the plan (§7.1, §7.2) because they cost real
debugging time and none is obvious from the API documentation:

1. **`windows.OpenCurrentProcessToken()` opens with `TOKEN_QUERY` only.**
   `CreateRestrictedToken` needs `TOKEN_DUPLICATE` on the existing token
   handle and fails with `ERROR_ACCESS_DENIED` otherwise — *even with no flags
   and no restricting SIDs*, which reads like a privilege wall and is not.
   The spike opens the token explicitly.
2. **`AddMandatoryAce` needs a hand-built ACL.** `golang.org/x/sys/windows`
   exposes the `ACL` struct but no `InitializeAcl`/`AddAce`, and its fields are
   unexported. The spike writes the 8-byte header directly and calls
   `AddMandatoryAce` through an `advapi32.dll` lazy proc. And `AclSize` is the
   buffer's **capacity**, not its used size: `AddAce` refuses with
   `ERROR_NOT_ENOUGH_MEMORY` when the new ACE would not fit within it.
3. **`windows.GetNamedSecurityInfo` already frees the descriptor.** It returns
   a Go-allocated copy, so freeing it again corrupts the heap — the symptom
   is `STATUS_HEAP_CORRUPTION` in the *child* process, far from the call that
   caused it. The spike does not free it.

## Dependency note

The spike pins **`golang.org/x/sys v0.19.0`** with a `go 1.21` directive — the
same version and language level the plan specifies for the `sandbox/winacl`
nested module, so it needs no toolchain bump. That it builds and passes at
v0.19.0 is itself the empirical confirmation of the plan's §7.1 claim: the
whole required `x/sys/windows` surface is present at that version.

The two `advapi32` calls the spike reaches through lazy procs —
`CreateRestrictedToken` and `AddMandatoryAce` — are **absent from `x/sys` at
every version**, so the backend uses the same approach.
