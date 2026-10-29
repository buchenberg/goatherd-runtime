# Changelog

Notable changes to `goatherd-runtime`. This project follows
[Semantic Versioning](https://semver.org/); while pre-1.0, minor releases may
contain breaking changes, which are called out below.

## [v0.21.0] - 2026-10-29

### Added
- **Explicit review provenance**: `ScopeManager.RecordMergeProposal` and
  `ScopeManager.RecordMergeDecision` write the proposal on the child's owner
  path and the verdict on the parent's owner path, causally joined. The pair is
  independent of physical integration, so a shared-sandbox merge and an
  isolated Seal + Settle can carry the same trace shape. `run.Reversible` now
  records both halves when its reviewer or authority decision runs.

### Removed
- **`ScopeManager.CommitMerge`**, `DraftsOnlyReviewer`, and
  `DefaultProposalDiffLines`. `CommitMerge` had no production caller and only
  supported shared-sandbox children; every isolated run must propagate through
  Seal + Settle. Review is now an explicit propose → record → review → decide
  sequence, not a helper that also merges. This is a breaking pre-1.0 removal.

### Changed
- **Package support is explicit**: `run` is the reference composed path (fork →
  review → seal/settle). `task`, `materialize`, and `substrate` are landed
  public primitives with tests/examples, but no default host composition uses
  them yet; README and `doc.go` now say so and list `run` in the package index.

## [v0.20.0] - 2026-10-28

### Added
- **`effect` package**: typed workspace-change effects with causal linkage and
  live emission — plan 05 Phase 1, and the container tier's parity gap against
  the Python reference's `OverlayEffectExtractor`. `FromChanges` maps a
  backend-neutral `[]Change` onto `ChangeEffect` values carrying a schema ref
  and kind label (`shepherd2.effect.file.{created,patched,deleted}.v1`,
  `file:{created,patched,deleted}`); `Emit` appends each as a capture fact and
  publishes it on a `trace.EffectBus`. Append strictly precedes publish, so a
  subscriber can fetch the record the moment it sees the event, and a receipt
  that does not name exactly one fact is an error rather than an event pointing
  at nothing. Causality is operation-level: `opID` becomes the payload's
  `caused_by` and the event's sole causal parent. Directories are omitted, and
  an unrecognised `Kind` fails rather than being silently dropped. `Emit` takes
  a two-method `FactAppender` (satisfied by `*goatherd.Scope`) so the package
  stays a leaf the root can depend on.
- **`sandbox/croft` nested module** (v0.1.2): `goatherd-croft` v0.8.0 behind the
  `Sandbox` registry seam as backend `"containerd"`, so a container workspace is
  addressable through `OpenSandbox` exactly like the git backend. Croft is a
  *world* on the substrate axis, not a paddock: `Capabilities` reports
  `ContainContained` and never a syscall-tier claim. Both `Create` and `Exec`
  are bounded by a backend ceiling when the caller supplies no timeout. A
  `ConfinementSpec`, an explicit paddock `Placement` or an `AuthorityMatch` is
  refused rather than silently satisfied by the coarse container boundary, and
  an unknown `Placement` is refused rather than treated as permissive — the
  container-tier authority vocabulary is an open decision (goatherd-croft bead
  `croft-ku0`). Implements `DeltaApplier`, propagating `ErrDeltaConflict`
  unwrapped so settlement fails closed; exposes `Changes` and `ReleaseState`
  beyond the interface for callers to find by type assertion. Deliberately does
  **not** implement `Addressed` (croft identity is not a path) or
  `DeltaBaseAnnotator` (croft already carries `BaseKey` and pins by lease).

### Platform support
- **`sandbox/croft` is Linux-only, with a stub elsewhere.** The implementation
  is gated `//go:build linux`. `doc.go` and `config.go` carry no build tag, so
  `BackendName`, `Config`, `Limits` and `SecretMount` compile on every platform
  from one declaration that cannot drift between them; `backend_other.go`
  provides a `Sandbox`/`New` that registers no backend and returns
  `ErrUnsupportedPlatform`. This is what lets a host ship Windows and macOS
  binaries while depending on the module: `goatherd-croft` v0.8.0 does not
  compile on Windows (`syscall.O_NOFOLLOW` in its staging-write path), filed
  upstream as goatherd-croft bead `croft-itw`. Off-Linux,
  `OpenSandbox("containerd", ...)` reports an unknown backend naming what *is*
  available.
- Consumers should require **`sandbox/croft` v0.1.2 or later**. v0.1.0 resolves
  from the module proxy and works on Linux but does not build on Windows, and
  the proxy cache is permanent so it cannot be re-pointed; v0.1.1 added the
  platform gate, v0.1.2 the `Create` ceiling and fail-closed placement.

### Known limitations
- **Container state digests are not content-stable.** croft mints a
  sequence-derived snapshot key per capture, so two captures of unchanged
  content digest differently — which `Sandbox.Capture`'s contract asks backends
  to avoid, because settlement's `select` compares digests as content equality.
  Hashing the state's fields does not help: the inputs differ on every capture.
  Recorded in the adapter's package doc and pinned by a test written to invert
  itself when goatherd-croft bead `croft-dlb` lands. Safe while settlement never
  sees a container state; a host must not enable retained outputs in container
  mode before then.
- **Effect volume is unbounded.** `Emit` appends one fact per effect with no
  cap, batching or summary record, and the bus drops oldest-first when a
  subscriber's buffer fills — so a supervisor can miss events on a large change
  set while the durable trace stays complete. Plan 05 §11 records this as not
  mitigated; the right bounding shape depends on consumers that do not exist
  yet.
- **`effect` produces observations, not reversible actions.** The Python
  reference's `FileCreate`/`FilePatch`/`FileDelete` carry content and implement
  `reverse()`. These effects carry metadata only, because a containerd change
  set is already classified by the daemon and reading content back would mean a
  round trip per file. Rollback remains the workspace-state layer's job
  (`Capture`/`Apply`).

### Changed
- **Plan 05 amended as-built** (§4, §5, §6, §11). Four claims did not survive
  contact with the code: `FromChanges([]croft.Change)` in a core package would
  have pulled containerd v2 and the OCI tree into the core, defeating the nested
  module; the cited precedent `shepherd.run.executed.v1` does not exist and the
  kernel's real convention is a `shepherd2.` prefix; "parity" with
  `OverlayEffectExtractor` overstated a metadata-only effect stream; and `Emit`
  taking `*goatherd.Scope` would have coupled a leaf package to the runtime
  root. The parity map now points at the tests that exist, and row 8 is marked
  pending — the croft→`effect.Change` converter belongs in `sandbox/croft`,
  which pins published core versions and so cannot land until this release is
  tagged.
- Plan 5's status in `docs/plans/README.md` moves from scoped to Phase 1 landed.
- Nested `go.mod` tidy: `sandbox/winacl` gains `golang.org/x/text` as an
  indirect requirement; stray blank lines removed from `sandbox/landlock` and
  `sandbox/winacl`. Neither carries a `replace` directive, so both remain
  consumable as published — as does `sandbox/croft`.
- README project-name and link corrections.

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
- **Confinement/containment seam** (was: two near-synonym packages split by a
  private distinction): the spec vocabulary — `ConfinementSpec`, `NetworkPolicy`,
  `NetMode`, `ReadOnlySpec`, `PermissiveFor`, `ResolveSpec`, `Canonical`,
  `ErrUnsupportedConfinementSpec` — moves from `containment` to `confinement`
  (where the lowering lives), and the placement tiers (`Placement`,
  `ResolvePlacement`) move from `confinement` to `containment` (where the
  enforcement lives). The dependency arrow now runs containment ->
  confinement, with no cycle.
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
