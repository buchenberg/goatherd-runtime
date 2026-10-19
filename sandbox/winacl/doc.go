// Package winacl is the Windows write-restriction containment backend — the
// native Windows member of the containment family, with no container, no
// hypervisor, and no elevation.
//
// # Why Windows needs a different mechanism
//
// Landlock (the Linux member) is a kernel LSM: a process applies a ruleset to
// itself and the kernel checks every write syscall against it. Seatbelt (the
// macOS member) is a per-process sandbox profile. Windows has no equivalent —
// there is no unprivileged, per-process, path-scoped "deny writes below X"
// primitive. Windows enforces access through an authorization model: the
// security reference monitor evaluates an object's DACL against the caller's
// token.
//
// This backend therefore composes four kernel-enforced layers to reach the
// same observable property as the Landlock member — writes succeed only
// beneath the granted roots:
//
//  1. A write-restricted token. CreateRestrictedToken with WRITE_RESTRICTED
//     makes the access check run twice: once against the token's normal SIDs
//     and once against its restricting SIDs. A write is granted only where
//     BOTH checks allow it.
//  2. Per-root capability SIDs as the restricting SIDs. Each granted root
//     gets a deterministic SID (see WriteSidFor) whose ACEs form that root's
//     write allowlist.
//  3. A Low integrity level on the token, and a Low no-write-up mandatory
//     label on each granted root. This is the layer that actually denies a
//     write to an ungranted directory: the write pass-2 check alone does not,
//     because Everyone is in the restricting list and Everyone holds ambient
//     write authority over much of the disk.
//  4. A deny of the ambient FILE_DELETE_CHILD right for Everyone on each
//     granted root. Windows also authorizes a delete from the parent
//     directory's right, which the write-restricted intersection does not
//     reach.
//
// # What is deliberately not confined
//
// Reads are not governed: WRITE_RESTRICTED intersects write accesses only, so
// a confined child reads anything the caller reads. This is Landlock parity
// (Landlock's v1 write mask excludes the read and execute bits), not a gap —
// but it is a boundary, and this backend does not claim read confinement.
// Neither does it claim network confinement: the mechanism has no egress
// control, so SupportsNetwork reports only allow_all, and deny_all and broker
// are refused rather than silently ignored.
//
// NTFS hard links alias the file object rather than the path, so a granted
// root's hard link grants an external alias. That is structurally
// unavoidable for this token shape, and it is why the enforcement tier is
// "native-access-check-deny" rather than "native-syscall-deny", and why the
// containment stamp is "contained" rather than "full".
//
// # The one path the write restriction does not close
//
// The write restriction has a residual path, and it is stated here rather
// than left for a reader to discover.
//
// Everyone is in the restricting list, and it has to be: without it, a body
// that uses CNG cannot start (BCrypt fails to load), and without the logon
// session SID, early DLL initialization fails outright. Both requirements are
// pinned by the test suite.
//
// Because Everyone is in the restricting list, the second access check
// admits a write wherever Everyone holds write. So a directory that grants
// write to Everyone AND carries a Low integrity label is writable by a
// confined child even when it is not a granted root: the low-to-low write is
// not blocked by the integrity check, and Everyone satisfies the second check.
//
// A default Windows install does not produce such a directory — Everyone
// holds no write over the user's trees, and nothing but this mechanism and
// other sandbox tools stamp a tree Low. A tree another sandbox tool has
// stamped Low is where the path is reachable. The behaviour is pinned by
// TestWinACL_EveryoneWritableLowLabeledDirectoryIsNotConfined, so it is a
// known boundary rather than an assumption.
//
// Everything else is closed: with no restricting SIDs at all, every access
// check is refused, and the write allowlist is entirely the restricting list.
// TestWinACL_RestrictingListIsTheWholeBasisOfAWrite pins that.
//
// # Module boundary
//
// This is a nested module so the root runtime keeps its "kernel + stdlib"
// dependency footprint: only this module pulls golang.org/x/sys directly. It
// implements the same containment.Backend interface as the Linux member, so a
// host that wants confinement on both platforms selects a backend per
// platform and passes it to the sandbox constructor.
package winacl
