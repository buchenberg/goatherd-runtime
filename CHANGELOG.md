# Changelog

Notable changes to `goatherd-runtime`. This project follows
[Semantic Versioning](https://semver.org/); while pre-1.0, minor releases may
contain breaking changes, which are called out below.

## [v0.19.0] - 2026-10-22

### Changed
- **Module renamed**: `github.com/buchenberg/shepherd-runtime-go` is now
  `github.com/buchenberg/goatherd-runtime`; the root package `shepherd` is now
  `goatherd`. Requires `github.com/buchenberg/goatherd-kernel` v0.18.0 (the
  renamed kernel; see its v0.18.0 entry for the import-path migration).
- **Nested modules move with it**: `sandbox/landlock` v0.1.2 and
  `sandbox/winacl` v0.1.1. Both now carry a relative `replace` pointing at the
  repo root so in-repo builds resolve the runtime locally; `replace`
  directives of dependencies are ignored, so external consumers (yaah) are
  unaffected and pin the published versions.
- **Go-native record vocabulary renamed**: `shepherd.{scope,merge,workspace,
  checkpoint,supervisor,run_output}.*` schema refs are now `goatherd.*`. The
  cross-language surface is untouched: `shepherd2.*` refs and
  `shepherd.kernel.abi.v0` keep their names, and the golden vectors are
  byte-identical.

### Breaking (on-disk state)
- Workspace state refs inside sandboxed repos move from `refs/shepherd/states/`
  to `refs/goatherd/states/`. Sandboxes created by v0.18.0 keep their old refs;
  re-create the scope (or `git branch -m` the refs) after upgrading. The git
  backend's temp-dir prefix (`shepherd-index-*` -> `goatherd-index-*`) and the
  winacl probe env (`SHEPHERD_WINACL_TEST_VALUE` -> `GOATHERD_WINACL_TEST_VALUE`)
  are renamed likewise.

## [v0.18.0] - 2026-10-19

### Added
- `sandbox/winacl` — a **nested module** carrying the Windows containment
  backend: a write-restricted token, per-root write capability SIDs, a Low
  integrity level, and a Low no-write-up mandatory label. It implements the
  same `containment.Backend` contract as the Linux `sandbox/landlock` member,
  so a host selects a backend per platform and passes it to the sandbox
  constructor — nothing in the root module changes. Unprivileged: no
  elevation, no container, no hypervisor. See
  [`docs/plans/08-windows-containment.md`](docs/plans/08-windows-containment.md).
  The module carries its own version stream (`sandbox/winacl/vX.Y.Z`), like
  `sandbox/landlock`.
- `docs/plans/08-windows-containment.md` — the Windows containment plan, with
  the mechanism's spike evidence preserved under `docs/plans/spikes/winacl/`.

### Changed
- The shared confinement denial classifier now recognizes Windows'
  "Access is denied." alongside "permission denied" and "operation not
  permitted", so a Windows denial is recorded as a discarded run rather than
  an ordinary failure.

## [v0.17.0] - 2026-10-16

Initial extraction. The runtime layer moves out of
`github.com/buchenberg/shepherd-kernel-go` into its own module, leaving the
kernel behind as the trace store, canonical model, effect bus, and projections.

### Added
- The root `shepherd` runtime package: the scope tree, merge gate, settlement,
  recovery, checkpoints, and workspace operations.
- `sandbox` + `sandbox/git`, `substrate`, `materialize`, `task`, `supervisor`.
- `internal/gittest`, `internal/rttest` (Go `internal/` is module-scoped, so
  these cross the boundary by moving).

### Changed
- Imports of `github.com/buchenberg/shepherd-kernel-go/{sandbox,substrate,materialize,task,supervisor}`
  and the root package become `github.com/buchenberg/shepherd-runtime-go/...`.
- `github.com/buchenberg/shepherd-kernel-go/internal/testvectors` becomes the
  kernel's public `github.com/buchenberg/shepherd-kernel-go/testkit`.
- Imports of `github.com/buchenberg/shepherd-kernel-go/trace` become
  `github.com/buchenberg/shepherd-kernel-go` — the kernel's trace package is now
  the module root (package `trace`).
- Pins `github.com/buchenberg/shepherd-kernel-go` to the tagged `v0.17.0` and
  drops the local-development `replace`; the module now resolves from the
  module proxy like any other dependency.
