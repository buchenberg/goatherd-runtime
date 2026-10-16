# Implementation Plan: Container-Tier Parity (Go)

> **Scope.** This is **Plan 5** of the containment/execution work — the last tier.
> Plans 1–4 shipped the native syscall paddock, the authority spine, reversible
> runs, and egress. The **container tier** already exists as `croft` (containerd
> overlayfs) and `croft-broker` (multi-tenant). What's missing, versus Python's
> container tier, is **four things** — one of which is a *mapping*, not a
> mechanism:
>
> 1. **Overlay changes → typed, causally-linked effects** (Python's
>    `OverlayEffectExtractor`). The real gap.
> 2. The **`copy` carrier** (Python's universal, all-platforms floor).
> 3. The **`clonefile` carrier** (macOS APFS copy-on-write).
> 4. *(Non-goals:)* remote/cloud providers and the macOS VM-overlay path.

## 1. Objective & Context

`croft` is a library over containerd v2's overlayfs snapshotter: `Create`,
`Exec`, `ReadFile`/`WriteFile`, `Capture`, `Apply`, `ReleaseState`, `Changes`,
`Diff`, `ApplyDelta`, with cgroup limits, network posture, and secret mounts. It
is **ahead** of Python on durability (states pinned by leases, outliving
sandboxes) and multi-tenancy (`croft-broker`).

The gap is the **effect-extraction tier**. Python's container tier exists to mine
the overlay upper layer into typed, causally-linked effects feeding the v1 fold
trace:

- `OverlayEffectExtractor` (`shepherd_runtime/device/container/overlay_extractor.py`):
  `FileCreate` / `FilePatch` / `FileDelete`, `caused_by` → the causing
  `IntentEffect` (D25), **whiteout** detection (D9, char device 0,0), and VM
  upper-layer extraction on macOS (`vm_extraction.py`, SSH+tar).

Go **rejected the fold model** (snapshots beat replay — `parity-plan.md` §4), so
it does not walk the overlay by hand: **croft's `Changes` already returns the
classification.** `Change{Path, Kind: ChangeAdd|ChangeModify|ChangeDelete, IsDir,
Mode, Size}` plus `Diff`/`content`. containerd resolves whiteouts in its own
snapshotter diff, so the only missing piece is the **mapping to typed records**
with causality.

## 2. What "Parity" Means Here (the reference surface)

| Python | File | Role |
|---|---|---|
| `OverlayEffectExtractor` | `shepherd_runtime/device/container/overlay_extractor.py` | overlay → typed effects |
| `FileCreate` / `FilePatch` / `FileDelete` | `shepherd_core.effects` | the effect vocabulary |
| `caused_by` (D25), whiteouts (D9) | `design/DECISIONS.md` | causality + deletion semantics |
| `ContainerDevice.extract_effects` | `device/container/device.py` | the orchestration seam |
| `_copy_carrier.py` | `vcs_core/` | the universal carrier floor |
| `_clonefile_carrier.py` | `vcs_core/` | macOS APFS CoW carrier |
| `_kernel_overlay.py`, `_fuse_overlay.py` | `vcs_core/` | overlay carriers (Go: `croft`) |
| `shepherd_sandboxes.{daytona,e2b,k8s,modal,prime}` | `packages/sandboxes/` | remote providers (non-goal) |

Go surfaces this composes with:

| Primitive | API |
|---|---|
| change classification | `croft.Changes(ctx, ws) ([]Change, error)`, `Change{Path,Kind,IsDir,Mode,Size}` |
| unified diff | `croft.Diff(ctx, ws) (string, error)` |
| tree merge | `croft.ApplyDelta(ctx, ws) error` |
| effect bus | `trace.NewEffectBus(n)`, `(*EffectBus).Publish(EffectEvent)`, `Subscribe(id) <-chan EffectEvent` |
| effect record | `trace.EffectEvent{SchemaRef, KindLabel, Payload, CausalParents, …}` |
| schema library | `trace.StaticSchemaLibrary`, `trace.ShepherdSchemas()` |
| append | `(*Scope).AppendFact(ctx, schemaRef, kindLabel, payload)` |

## 3. Scope & Explicit Non-Goals

**In scope:** the change→effect adapter + schemas + live bus emission (Phase 1);
the `copy` carrier (Phase 2); the `clonefile` carrier (Phase 3).

**Non-goals:**
- **Remote/cloud providers** (`daytona/e2b/k8s/modal/prime`). Vendor-SDK debt; the
  reference keeps them *optional* integrations. A generic remote `Sandbox` seam is
  a possible later proof, not parity work.
- **macOS VM-overlay extraction.** Requires a Linux VM substrate; superseded by
  the `clonefile` carrier for the local macOS case.
- **The fold model / effect *replay*.** Rejected by design (`parity-plan.md` §4).
  This plan *produces typed effects as records and bus events*; it does not
  reintroduce `state = fold(effects)`.

## 4. Parity Map (completion checklist)

| # | Python symbol | Go target | Test |
|---|---|---|---|
| 1 | `FileCreate` | `shepherd.effect.file.created.v1` record | `TestEffect_FromChanges` |
| 2 | `FilePatch` | `shepherd.effect.file.patched.v1` record | `TestEffect_FromChanges` |
| 3 | `FileDelete` | `shepherd.effect.file.deleted.v1` record | `TestEffect_FromChanges` |
| 4 | `OverlayEffectExtractor.extract` | `effect.FromChanges(owner, op, []croft.Change)` | `TestEffect_FromChanges` |
| 5 | `caused_by` (D25) | `CausalParents = [operation record id]` | `TestEffect_Causality` |
| 6 | effect emission | `(*EffectBus).Publish` + `AppendFact` | `TestEffect_EmittedOnBus` |
| 7 | whiteouts (D9) | inherent: `croft.Changes` reports `ChangeDelete` | `TestEffect_FromChanges` |
| 8 | `ContainerDevice.extract_effects` | the run/handler calls `effect.FromChanges` on the sandbox | `TestEffect_ContainerRun` (live) |
| 9 | `_copy_carrier.py` | `sandbox/copy` backend | `TestCopyCarrier_*` |
| 10 | `_clonefile_carrier.py` | `sandbox/clonefile` (darwin) | `TestClonefile_*` (macOS) |
| 11 | `_kernel_overlay` / `_fuse_overlay` | `croft` (already) | croft's live suite |

## 5. Architecture

### 5.1 The mapping (not a mechanism)

```
run / handler
   │  exec + mutate the workspace
   ▼
croft.Changes(ctx, ws)  ──►  []Change{Kind: Add|Modify|Delete, Path, IsDir, Mode, Size}
   │
   ▼
effect.FromChanges(owner, opID, changes)
   │  ChangeAdd→created, ChangeModify→patched, ChangeDelete→deleted
   ▼
typed records  ──►  (*EffectBus).Publish(EffectEvent)   (live supervision)
               └─►  (*Scope).AppendFact(...)             (durable trace)
```

Because croft already classifies, this is **one adapter** plus a schema series —
no overlay walking, no whiteout logic (containerd resolves it), no VM reader.

### 5.2 Causality

Python's D25 links each file effect to the *causing intent*. Go links to the
**operation/scope**: `FromChanges` takes the run/operation id and sets
`CausalParents = [opID]`. Per-intent granularity, where a consumer needs it, uses
the runtime's operation journal (the same journal settlement already records).

### 5.3 Live, not commit-time

Python reconciles effects at commit. Go's `EffectBus` is real-time, so the adapter
**publishes each effect as it is produced** (`SchemaRef = the effect schema`,
`KindLabel = "file:created"`, `Payload = {path, kind, is_dir, mode, size}`), and
`AppendFact` writes the durable record. Supervisors can watch container changes
live — **ahead** of the reference.

## 6. Phase 1 — Typed effect extraction

New runtime package `shepherd-runtime-go/effect`:

```go
// ChangeEffect is one typed workspace change.
type ChangeEffect struct {
    SchemaRef  string // shepherd.effect.file.{created,patched,deleted}.v1
    KindLabel  string // "file:created" | "file:patched" | "file:deleted"
    Path       string
    IsDir      bool
    Mode       fs.FileMode
    Size       int64
}

// FromChanges maps croft's classified changes to typed effects. It is the Go
// analogue of OverlayEffectExtractor.extract — and needs no overlay walk,
// because croft (containerd) already resolves creates/modifies/deletes and
// whiteouts.
func FromChanges(changes []croft.Change) []ChangeEffect
```

And an emitter that records + publishes:

```go
// Emit appends each effect on the scope's owner path (caused by opID) and
// publishes it on the bus for live supervision.
func Emit(ctx context.Context, scope *shepherd.Scope, bus *trace.EffectBus, opID string, effects []ChangeEffect) error
```

Schemas are **runtime-level** (`shepherd.effect.file.*.v1`) — schema refs are
validated only for non-emptiness, so no kernel ABI change (the same pattern as
`shepherd.run.executed.v1`).

## 7. Phase 2 — The `copy` carrier

`shepherd-runtime-go/sandbox/copy` — the universal, all-platform `Sandbox`
backend (mirrors `_copy_carrier.py`): each scope is a plain recursive copy of its
base; no CoW, no union mount, no kernel/FUSE, no privileges.

| Sandbox method | Implementation |
|---|---|
| `Create` | copy the base tree into the scope dir (or create empty) |
| `Destroy` | `RemoveAll` the scope dir |
| `Capture` | walk the tree → content-derived `WorkspaceState` (path→sha256 tree digest) |
| `Apply` | restore the captured tree (delete extraneous, write captured) |
| `Diff` | unified text diff against the state |
| `ApplyDelta` | tree merge (three-way on the captured base) |
| `Exec` | `WithExec`-style confined exec (reuse `containment`/`landlock`), else `ErrUnsupported` |
| `ReadFile`/`WriteFile` | within the scope dir (reuse the git backend's symlink-safe containment) |

## 8. Phase 3 — The `clonefile` carrier (macOS)

`sandbox/clonefile` (darwin build-tagged) — a copy-on-write specialization of the
copy carrier: each scope is an APFS clonefile (`clonefile(2)`, or `cp -c -R`) of
its base, sharing blocks until write; everything else is the copy carrier's
lifecycle. Mirrors `_clonefile_carrier.py`. Cannot be verified off macOS.

## 9. Test & conformance matrix

| Phase | Test | Action | Expected |
|---|---|---|---|
| 1 | `TestEffect_FromChanges` | Add/Modify/Delete + a directory | the right schema/kind each (deletes from `ChangeDelete`) |
| 1 | `TestEffect_Causality` | opID → effects | `CausalParents == [opID]` |
| 1 | `TestEffect_EmittedOnBus` | subscribe, emit | every effect arrives with `SchemaRef`/`Path` |
| 1 | `TestEffect_ContainerRun` (live) | a real croft run that writes files | effects mirror `croft.Changes` |
| 2 | `TestCopyCarrier_CaptureApply` | mutate, capture, mutate, apply | the workspace returns to the captured tree |
| 2 | `TestCopyCarrier_Diff` | capture, modify | the unified diff names the file |
| 2 | `TestCopyCarrier_ApplyDelta` | baseline-relative merge | lands; a conflict refuses |
| 2 | `TestCopyCarrier_Capabilities` | report matrix | `Isolated`, `Containment` honest |
| 3 | `TestClonefile_CloneIsCoW` (macOS) | clone, write, capture | the clone shares blocks; diff is exact |

## 10. Parity Comparison: Container Tier

| Capability | Python | Go (after Plan 5) |
|---|---|---|
| Isolation | Podman + OverlayFS | `croft` (containerd overlayfs, namespaces/cgroups/netns) — **parity** |
| Resource limits | Podman/pod limits | cgroup v2 — **parity** (live-tested) |
| Network / secrets | egress broker + jail; secret axis | `Config.Network` / `Config.Secrets` — **parity** (live-tested) |
| Fork / rollback / checkpoints | carriers + `ContainerDevice` stacking | `Capture`/`Apply` (O(1), states outlive sandboxes) — **ahead** |
| **Overlay → typed effects + causality** | `OverlayEffectExtractor` (fold trace) | `effect.FromChanges` + bus — **parity** (Phase 1), **live** |
| Carrier family | kernel/fuse overlay, copy, clonefile | `croft` (kernel overlay) + copy + clonefile — **parity** |
| Remote providers | daytona/e2b/k8s/modal/prime | — **non-goal** |
| Multi-tenancy | — | `croft-broker` — **ahead** |
| macOS | VM overlay (SSH+tar) | `clonefile` carrier — **parity for the local case**; VM **non-goal** |

## 11. Sequencing, Dependencies, Risks

**Order:** Phase 1 (effect adapter — the real gap) → Phase 2 (copy carrier) →
Phase 3 (clonefile, on a Mac).

**Dependencies:** `croft` (`Changes`/`Diff`/`ApplyDelta`), the kernel
`EffectBus` + store + schema library, the runtime `Sandbox` seam, and the Plan 1
`containment`/`landlock` for the copy carrier's confined `Exec`.

**Risks / mitigations:**
- *Double-recording* — `Capture` already records a workspace state; the effect
  adapter records *per-file* effects, a distinct stream. Document the split.
- *Causality granularity* — operation-level linkage is exact; per-intent needs
  the operation journal (recorded as a follow-up).
- *croft state identity* — its `Capture` is snapshot-keyed (not content-derived),
  so it is *identity*, not content equality. Content-addressing croft states is a
  separate **ahead** item, not required for parity.
- *Effect volume* — a large change set emits many records; bound with a summary
  effect + per-file records optionally (a knob), or emit the diff once.

## 12. Decisions

1. **Where the adapter lives:** a new `effect` package (recommended) vs inside
   `materialize`. *Recommend `effect` — it is a distinct concern.*
2. **Schemas:** runtime-level `shepherd.effect.file.*.v1` (free-form refs, no
   kernel ABI change). *Recommend this over a kernel schema addition.*
3. **Emission:** publish **and** append (live + durable). *Recommend both.*
4. **Causality:** operation-level now; per-intent via the journal later.
5. **Per-file vs diff-only effects:** per-file records (parity with Python's
   vocabulary) with an optional summary; the diff stays on the workspace state.
6. **Copy carrier location:** `sandbox/copy` in the root module (stdlib only —
   no new dependency). Clonefile is darwin-gated.
