// Package croft is the containerd sandbox backend: it puts
// github.com/buchenberg/goatherd-croft behind the sandbox package's registry
// seam as backend "containerd", so a container workspace is addressable
// through OpenSandbox exactly like the git backend is.
//
// Croft is a *world* (the substrate axis — a sibling of sandbox/git), not a
// *paddock* (the authority axis, containment.Backend, where landlock and
// winacl live). The container boundary — namespaces, cgroups, an unshared
// network namespace — is what encloses the work, so this backend reports
// ContainContained and never a syscall-tier claim. Do not layer Landlock
// inside the container: a container seccomp profile may block the Landlock
// syscalls outright, and under namespaces it would be redundant.
//
// This is a nested module so the runtime core keeps its "kernel + stdlib"
// dependency footprint: only this module pulls containerd v2 and the OCI
// tree. It is the Go analogue of the Shepherd reference keeping its remote
// sandbox backends in an optional, lazily-imported package.
//
// # Linux only
//
// The implementation is gated //go:build linux. goatherd-croft does not
// compile elsewhere — it uses syscall.O_NOFOLLOW in its staging-write path,
// which Windows does not define — and the substrate it drives is Linux-shaped
// in any case: overlayfs snapshots, bind mounts, FIFOs, and a `/bin/sh -c
// "sleep infinity"` init process.
//
// Every other platform builds backend_other.go instead of the Linux
// implementation. The exported surface that is meaningful everywhere lives in
// the untagged config.go — BackendName, Config, Limits, SecretMount,
// ErrUnsupportedPlatform and the EffectChanger capability interface — while
// backend_other.go supplies Sandbox and New without importing croft, and its New
// returns ErrUnsupportedPlatform. No backend is registered there, so
// OpenSandbox("containerd", ...) fails with the registry's unknown-backend
// error naming what *is* available.
//
// That split is deliberate and load-bearing: it is what lets a host such as
// yaah compile and ship Windows and macOS binaries while still depending on
// this module, instead of inheriting a dependency that cannot build there. It
// matches the runtime's own policy that a non-Linux client cannot select the
// containerd backend, and the sandbox/landlock precedent of stubbing
// non-Linux paths rather than letting them fail to compile.
//
// # Digest stability — a known, recorded gap
//
// sandbox.Sandbox.Capture requires that two captures of identical content at
// an unchanged timeline position produce equal WorkspaceState digests, because
// settlement's select precondition compares digests as content equality.
//
// This backend does NOT satisfy that, and cannot until goatherd-croft ships
// content-derived state identity (croft bead croft-dlb: a no-change capture
// fast path plus an exposed ContentDigest). Croft states are sequence-keyed —
// Capture unconditionally commits a fresh layer and mints a new SnapshotKey,
// even when the active layer is unchanged. Hashing the state's fields does not
// help: the inputs themselves differ on every capture, so any hash of them
// differs too. croft-broker's four-field sha256 is a stable *name* for a given
// state, not content equality, and is deliberately not reproduced here.
//
// The gap is safe today only because settlement never sees a container state:
// yaah gates retained outputs on `sessionSandbox == nil`, so a container
// session produces none. Enabling retained outputs in container mode requires
// croft-dlb first. Per the Capture contract's own instruction, the gap is
// recorded here rather than papered over.
package croft
