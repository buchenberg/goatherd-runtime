# Changelog

Notable changes to `shepherd-runtime-go`. This project follows
[Semantic Versioning](https://semver.org/); while pre-1.0, minor releases may
contain breaking changes, which are called out below.

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
