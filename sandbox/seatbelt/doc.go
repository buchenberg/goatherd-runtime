// Package seatbelt is the macOS Seatbelt (sandbox-exec/SBPL) containment
// backend — the macOS member of the containment family, with no container and
// no elevation. It mirrors sandbox/landlock and sandbox/winacl and satisfies
// the containment.Backend contract.
//
//   - ProfileFor lowers canonical writable roots and the network pin to a
//     backend-private JSON profile ({"roots":…,"net":{"mode":…,"port":…}}),
//     exactly like the other members. The JSON is opaque to callers and is
//     validated and rendered to SBPL at probe/launch time.
//   - Probe is exit-code-aware and fail-closed. It proves sandbox-exec applies
//     the profile, that an out-of-root write is denied, that every writable
//     root accepts writes, and that an in-working-root path outside every root
//     is denied. Under deny_all it also proves a loopback connect is refused;
//     under broker it proves a non-pinned port is refused and the pinned port
//     is admitted. Each probe uses a unique canary name and removes only what
//     it created. A probe that cannot establish confinement is
//     ErrPaddockNotEstablished, never silent success.
//   - Launch runs /usr/bin/sandbox-exec -p <SBPL> <command> in the request's
//     directory and environment. The probe runs first, so a profile that
//     cannot be applied is refused before launch; a launched command's own
//     exit status is returned as the result. Seatbelt applies the profile to
//     the child, so no self-reexec trampoline is needed here (Landlock and
//     winacl confine in-process and do need one).
//
// # The profile
//
// The SBPL allows the default posture, denies all file writes, then re-allows
// writes beneath each canonical writable root and /dev (the reference's device
// allowance). Writes beneath each root's .git are denied explicitly, so a
// confined process cannot rewrite repository history even when the whole
// repository is a writable root. Under deny_all it denies network-outbound;
// under broker it denies network-outbound and then admits only the broker's
// loopback port.
//
// # What it does not confine
//
// Reads and exec are not confined. (allow default) leaves them open so
// toolchains can load shared libraries and run system binaries, matching the
// reference and the Landlock member's read posture; this backend does not
// claim read or exec confinement. The broker's host allowlist is cooperative,
// not kernel-pinned: the profile pins the broker's loopback port, but which
// hosts the broker admits is the broker's decision. Address-scoped egress needs
// a network namespace, not this backend. /dev is writable as a subtree, not
// just /dev/null, the same allowance the reference makes.
//
// /usr/bin/sandbox-exec is deprecated by Apple but still present; a host
// without it reports Available false and degrades to advisory or refuses,
// never silently running unconfined.
//
// Darwin-gated mechanics live in mechanism_darwin.go; mechanism_other.go
// mirrors winacl: non-darwin builds carry the same types and pure lowering but
// report the backend unavailable and never touch the machine.
package seatbelt
