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
// # Digest stability
//
// sandbox.Sandbox.Capture requires that two captures of identical content at
// an unchanged timeline position produce equal WorkspaceState digests, because
// settlement's select precondition compares digests as content equality.
//
// croft-dlb gives croft that property. Capture diffs the active layer against
// its parent and, when the change set is empty, returns the parent's
// remembered state verbatim instead of committing a fresh sequence-keyed
// layer — so Revision and the lineage fields are equal by construction,
// including across an Apply (the applied state is remembered as given).
// Unchanged content at a moved timeline still digests differently, which is the
// contract's intent: select treats it as moved and apply merges.
//
// WorkspaceState.ContentDigest rides in the kernel state's Data. It is the
// daemon differ's content-addressed digest of the workspace against the image
// rootfs, so a host can compare content across a moved timeline when it wants
// content equality rather than timeline identity. The adapter always asks
// croft for it; the no-change fast path does not need it.
package croft
