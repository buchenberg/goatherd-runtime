# goatherd-runtime

The runtime layer of the Shepherd kernel: the execution scope tree, the merge gate, settlement and recovery, the sandbox backends, the substrate/materialize layer, the task runtime, and the supervision rules engine.

It composes [`github.com/buchenberg/goatherd-kernel`](https://github.com/buchenberg/goatherd-kernel) — the trace kernel (record model, canonical JSON, SQLite store, effect bus, projections) — and adds the primitives that *use* a trace: branching execution, workspace capture/restore, reversible settlement, and supervised task handles.

> **Module scope.** The kernel module owns the durable trace: the record model, the store, the effect bus, and the execution/history projections. This module owns everything above it. Trace types (`trace.Record`, `trace.SQLiteTraceStore`, `trace.WorkspaceState`, …) come from `github.com/buchenberg/goatherd-kernel` (package `trace`, the kernel's module root).

## Packages

| Package | Contents |
|---|---|
| `goatherd` (module root) | the runtime hub — `Scope`, `ScopeManager`, checkpoints, workspace operations, the merge gate, settlement (`Seal`, `Settle`, `RetainedOutput`), recovery (`RecoverScopes`) |
| `effect` | typed workspace-change records and effect-bus emission |
| `sandbox` | the `Sandbox` interface and the backend registry |
| `sandbox/confinement` | authority declarations, grants, confinement specs, and network-policy lowering |
| `sandbox/containment` | the syscall-deny paddock contract and placement tiers |
| `sandbox/egress` | the allowlist egress policy and loopback CONNECT broker |
| `sandbox/git` | the git backend (in-place, worktree); self-registers as `"git"` |
| `sandbox/croft` | the containerd backend (nested module); self-registers as `"containerd"` |
| `substrate` | the `Substrate` contract, the registry (unknown substrate refs fail the materialization), the echo/KV/workspace substrates |
| `materialize` | the materialize orchestrator (owns the append; substrates never do) |
| `task` | the task runtime — `StartTask`/`StartTaskSync`, `Run`, `TaskControl`, the task `Registry`, `ChildHandle` |
| `run` | the reference reversible-run composition — fork, handler, review, seal/settle |
| `supervisor` | the rules engine — `Supervisor`, `SupervisionRule`, the built-in rules (`DestructiveToolRule`, `HighErrorRateRule`, `StuckDetectionRule`, `DestructiveToolGuard`) |

`run` is the reference composed path (reversible fork → review → seal/settle).
`task`, `materialize`, and `substrate` are **landed public primitives**, fully
tested, but no default host composition uses them yet. They are not a promised
integrated pipeline until a real caller lands.

## Containment backends

The execution boundary is enforced by **containment backends**: each lowers a set of writable roots to an OS-specific profile, probes it fail-closed, and launches a command confined by it. Each lives in its own nested module, so the root runtime keeps a small dependency footprint.

| Module | Platform | Mechanism | Enforcement tier |
|---|---|---|---|
| `sandbox/landlock` | Linux | an unprivileged kernel LSM (Landlock): self-restrict, then exec | `native-syscall-deny` |
| `sandbox/winacl` | Windows | a write-restricted token, per-root capability SIDs, a Low integrity level, and a mandatory label | `native-access-check-deny` |
| `sandbox/seatbelt` | macOS | a per-process Seatbelt SBPL profile applied by `sandbox-exec`, with a probe-first launch | `native-sandbox-deny` |

All three implement the same `containment.Backend` contract, so a host picks a backend per platform and passes it to the sandbox constructor — the root module does not change:

```go
sb := git.NewLocalGitSandbox(repoPath, git.WithExec(backend, confinement.PlacementPaddock))
```

A backend that cannot enforce the requested network mode refuses it rather than running with the axis silently ignored. Each backend's own package documentation states what it does *not* confine.

`sandbox/croft` is the separate **world** backend, not a containment member: it puts containerd behind the same `Sandbox` seam and reports `contained`, because namespaces/cgroups are its boundary. Do not layer Landlock inside the container; the container profile may block the Landlock syscalls and the namespace already supplies the boundary.

## Dependency

The runtime requires the kernel for the trace store, the record vocabulary, and the shared public `testkit` package:

```
require github.com/buchenberg/goatherd-kernel v0.18.0
```

The root module also has one direct non-kernel dependency, `golang.org/x/text`, used by confinement path normalization; its SQLite driver and support modules are indirect. The sandbox backends live in nested modules and carry their own dependency sets — `sandbox/croft` is the one that pulls containerd and the OCI tree. The root carries no `replace` directive, so modules resolve from the proxy like any other dependency.

## What You Can Do With It

### Fork and branch execution

Create isolated execution branches for speculative work. Fork records the branch point in the trace; merge or discard the branch later:

```go
mgr := goatherd.NewScopeManager(store)

// A root scope carries the workspace it operates on.
parent, _ := mgr.Create("agent:main", git.NewLocalGitSandbox(repoPath))
child, _ := mgr.Fork(parent.ID(), "agent:experimental", nil)

// Child runs in isolation...
// If it works:
parent.Merge(child)
// If it fails:
parent.Discard(child)  // child's records remain, but semantically abandoned
```

### Roll the workspace back

Checkpoints capture workspace state plus an opaque caller snapshot (typically
conversation history). A restore rewinds both:

```go
cp, _ := mgr.CreateCheckpoint(ctx, scope.ID(), conversationSnapshot)

// ...the sub-agent makes destructive changes...

snapshot, _ := mgr.RestoreCheckpoint(ctx, cp.ID)  // workspace reverted, snapshot returned
// Checkpoints are single-use: restore marks them used and releases the snapshot.
```

Reusable (non-consuming) workspace states are available when you need to apply
the same state more than once — that is what fork-and-choose requires:

```go
forkState, _ := scope.CaptureWorkspace(ctx)   // no lifecycle, stays valid
// ...variant A...
scope.ApplyWorkspace(ctx, forkState)          // rewind to the fork point
// ...variant B...
```

Checkpoint snapshots persist in the trace database (bounded by a per-store
blob limit), so they survive the process.

### Recover after a restart

Scopes, checkpoints, and sealed outputs are rebuilt from the durable trace
after a crash or restart — the trace is the registry. Sandboxes are never
invented: the host's resolver decides which worktrees and containers to
re-adopt and which to orphan-clean:

```go
mgr, err := goatherd.RecoverScopes(store, goatherd.RecoverOptions{
    // backend and path come from the fork record the trace durably holds;
    // both are empty for roots, and shared children are never resolved —
    // they inherit the parent's re-adopted sandbox.
    Resolver: func(scopeID, backend, path string) (sandbox.Sandbox, error) {
        return mySandboxFor(scopeID, backend, path) // nil = recover this scope causally only
    },
})
// The tree, terminal states, fork baselines, checkpoints, and retained
// outputs are back. A checkpoint whose recorded revision no longer matches
// the re-adopted workspace is CheckpointInvalid — never silently restorable.
cp := mgr.LatestCheckpoint(scopeID)
```

Recovered children still propose merges against their recovered fork
baselines, and unconsumed outputs settle exactly as they would have
before the restart. Long-lived hosts can call `mgr.PruneTerminal()` to
unregister merged/discarded scopes when their settled state is no longer
needed in-process.

### Isolate a branch in its own worktree

The git backend has two modes. In-place operates on your repository directly.
Worktree allocates a detached `git worktree` per scope, so a speculative branch
cannot touch the parent tree and `Discard` becomes a physical rollback:

```go
sb := git.NewWorktreeSandbox(repoPath, worktreePath)
sb.Create(ctx, sandbox.SandboxSpec{})
defer sb.Destroy(ctx)

// Scope-scoped ownership: the sandbox is destroyed by Discard/Merge/Halt.
child, _ := mgr.ForkIsolated(parent.ID(), "agent:experimental", nil, sb)
```

Forking is `Create` then `Apply(forkState)`: the worktree lands on the fork HEAD,
then the fork-point state is replayed so the variant sees the parent's exact
(uncommitted) tree. Captured state survives teardown because worktrees share the
repository's object store.

### Supervise sub-agents

Define rules that watch for dangerous patterns and intervene automatically:

```go
sup := supervisor.NewSupervisor(mgr, bus)
sup.AddRule(supervisor.DestructiveToolRule())          // advisory warning on destructive calls
sup.AddRule(supervisor.DestructiveToolGuard())         // deny blockable destructive calls at check time
sup.AddRule(supervisor.HighErrorRateRule(0.5, 10))     // alert at 50% errors
sup.AddRule(supervisor.StuckDetectionRule(3))          // detect loops
sup.Start("orchestrator")

for iv := range sup.Interventions() {
    switch iv.Type {
    case supervisor.InterventionInject:
        sup.Inject(iv.ScopeID, "try a different approach")
    case supervisor.InterventionHalt:
        sup.Halt(iv.ScopeID)
    }
}
```

### Review before merge / settle outputs

Review is an explicit two-part provenance record, not an implicit side effect
of a merge helper. `ProposeMerge` is the query; `RecordMergeProposal` writes
the child's proposal; the caller reviews; `RecordMergeDecision` writes the
verdict on the parent. Integration remains `Merge` for shared-sandbox children
and `Seal` + `SettleSelect`/`SettleApply` for isolated ones.

```go
proposal, err := mgr.ProposeMerge(ctx, child.ID(), 1000)
proposedID, err := mgr.RecordMergeProposal(ctx, proposal)
if deny := reviewer(*proposal); deny != nil {
    _, _ = mgr.RecordMergeDecision(ctx, proposal, false, deny.Error(), "discarded", proposedID)
    _ = mgr.Discard(child.ID())
    _, _ = mgr.RestoreCheckpoint(ctx, cp.ID) // shared-sandbox rollback
    // retry with the denial reason injected as guidance
} else {
    _, _ = mgr.RecordMergeDecision(ctx, proposal, true, "", "merged", proposedID)
    _ = parent.Merge(child)
}
```

`run.Reversible` does this pair automatically for its reviewer/authority
paths; an isolated approval records the settlement action (`selected` or
`applied`) as the decision action. A denial returns
`*SupervisorDeniedError` and never touches the parent.

For isolated children (worktrees), physical propagation belongs to
settlement: seal each output, compare the frozen `Changes`, then settle
exactly one — best-of-n in a dozen lines:

```go
var outs []*goatherd.RetainedOutput
for i := 0; i < n; i++ {
    child, _ := mgr.ForkIsolated(parent.ID(), fmt.Sprintf("try-%d", i), nil, worktreeFor(repo, i))
    // ... the attempt does its work in the worktree ...
    out, _ := mgr.Seal(ctx, child.ID())
    outs = append(outs, out) // out.Changes and out.Diff are frozen at seal time
}

if err := mgr.Settle(ctx, outs[winner].ID, goatherd.SettleSelect); err != nil { // parent unmoved: fast-forward
    // parent moved since the fork? three-way instead:
    //   disjoint paths  -> mgr.Settle(ctx, outs[winner].ID, goatherd.SettleApply)
    //   path overlap    -> ErrApplyConflict, output stays unconsumed
}
for _, out := range losers {
    _ = mgr.Settle(ctx, out.ID, goatherd.SettleDiscard)
}
```

Every settlement is consume-once (`ErrOutputConsumed` on a second settle);
`selected` requires the parent to be exactly at the fork baseline, `applied`
requires path-disjoint parent changes, `released` keeps the sealed state
addressable without integrating it, and `discarded` tears the child down
keeping the sealed state as a record. Rule engines join the same gate via
`Supervisor.ReviewMerge`, which presents proposals to the rules as
`merge:proposed` events.

### Materialize recorded intents

Declarations are commitments: records that say what an agent *intended* to do.
Materialization is the other half of that rhythm — applying recorded
declarations through a typed substrate and appending the receipts as
captures, so the trace describes both sides of the transition:

```go
registry := substrate.NewSubstrateRegistry()
registry.Register(substrate.NewWorkspaceSubstrate(sandbox))  // or a KV, echo, custom substrate

receipt, err := materialize.Materialize(ctx, store,
    appendCtx.ToOperationContext(trace.OpMaterialize),
    materialize.MaterializationRequest{
        AppendIntentID:            "run:42:materialize",
        TargetTraceOwnerID:        "agent:main",
        TargetRecordIDs:           declarationIDs, // workspace.file.write.v1 records
        TargetThroughOwnerOrdinal: materialize.MaxOwnerOrdinal,
    },
    registry,
)
// receipt.Outcome, receipt.ProducedRecordIDs — applied captures, caused by
// their declarations, witness-stamped with the substrate that applied them.
```

The transition is idempotent once an intent has *completed*: a same-intent
retry replays the stored receipt from the completed-intent ledger without
redispatching the substrate, and a same-intent request with different content
is a conflict, not a silent second write. Dispatch itself is at-least-once:
in the window between the substrate call and the ledger write — including a
crash there — the substrate runs again. Deterministic substrates (the
reference Echo and KV substrates) converge to the identical receipt in that
window; a non-deterministic one fails loudly as an intent conflict rather
than double-writing. The substrate is selected by the declarations' own
witnesses — the `substrate_ref` stamped when they were appended — never
guessed from schemas.

`WorkspaceSubstrate` bridges the kernel's `Sandbox` interface, so recorded
file-write and exec intents reach a real workspace (git or worktree)
through the same declare→capture rhythm, gated on the backend's
capabilities: the git backend honestly reports `clean_failure` instead of
half-applying. Outcome honesty is the contract: `clean_failure` is claimed
only when nothing was attempted — a failure *after* a sandbox call was made
is reported as `split_state` even when no capture landed, because backends
write files in chunks and the world may have changed without a trace. The
reference KV substrate (`substrate.NewKVSubstrate`) keeps a world-side SQLite
store separate from the trace, and the echo substrate is the test fixture.

### Run tasks

Executions give runs identities: a create/complete/fail lifecycle folded from
retained facts, parent/child relations, and a synchronous task facade over the
store. Every id is derived exactly as the Python reference derives it —
`ExecutionIDFor` and the full run sequence are pinned by Python-generated
vectors — so a Go-written trace is readable by `shepherd2` and vice versa.

```go
reg := task.NewRegistry()
reg.Register("MyTask", func(c *task.TaskControl) (map[string]any, error) {
    c.Publish(ctx, "note", map[string]any{"msg": "working"})
    child, err := c.Spawn(ctx, "LeafTask", inputs, reg)
    if err != nil { return nil, err }
    exec, err := c.AwaitTerminal(ctx, child)
    return map[string]any{"child": exec.ExecutionID}, nil
})

run, err := task.StartTaskSync(ctx, store, reg, "MyTask", "run:fixed-id", inputs)
exec, err := run.Wait(ctx)      // terminal projection: succeeded / failed

// or async: StartTask returns a live handle; Wait blocks until terminal
run, err = task.StartTask(ctx, store, reg, "MyTask", "", inputs)
```

Spawns and adopts are recorded as parent-owned relation facts; abandoning a
child writes an abandoned relation under the same relation id, so the
effective-history fold drops it. The whole tree projects from one terminal
frontier, and re-opening the store file re-projects it identically. The
underlying projection (`trace.ProjectEffectiveHistoryFromStore`) lives in the
kernel module.

## Concepts

### Scopes

A **scope** represents a branch of execution. Fork creates a child scope with its own trace owner; merge propagates causal links; discard abandons the branch. Lifecycle events are recorded in the trace.

A scope also carries an optional **sandbox** — its execution substrate. A scope created without one is *pure-causal*: it participates in the trace DAG but workspace operations return `ErrNoSandbox`. `Fork` inherits the parent's sandbox without owning it; `ForkIsolated` attaches a new sandbox that the child owns, so `Discard`/`Merge`/`Halt` destroy it.

### Sandboxes

A **Sandbox** is the substrate a scope forks, snapshots, restores, diffs, and executes in. It is the seam that keeps the runtime independent of any particular isolation technology:

```go
type Sandbox interface {
    Backend() string
    Capabilities() SandboxCapabilities
    Create(ctx context.Context, spec SandboxSpec) error
    Destroy(ctx context.Context) error
    Capture(ctx context.Context) (WorkspaceState, error)
    Apply(ctx context.Context, ws WorkspaceState) error
    Diff(ctx context.Context, ws WorkspaceState, maxLines int) (string, []string, error)
    Exec(ctx context.Context, req ExecRequest) (ExecResult, error)
    ReadFile(ctx context.Context, path string) ([]byte, error)
    WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error
}
```

Backends need not implement everything. `Capabilities()` reports what is available, and unimplemented operations return `ErrUnsupported` rather than pretending to succeed. `Capture` and `Diff` are **non-mutating**: the caller's git index is left as it was found.

Three backend modes ship, packaged on purpose:

| Backend | Mode | Lifecycle | Isolated | Containment | Package |
|---|---|---|---|---|---|
| `git` | in-place (`git.NewLocalGitSandbox`) | No — validates only | No | `uncontained` | `sandbox/git` |
| `git` | worktree (`git.NewWorktreeSandbox`) | Yes — real worktrees | Yes | `buffered` | `sandbox/git` |
| `containerd` | container (`croft.New(Config{...})`) | Yes — container task and snapshots | Yes | `contained` | `sandbox/croft` |

A backend is a materializer or an isolation boundary, not necessarily both; the
git backends have zero external dependencies and work on any platform, while the
containerd backend is Linux-only and requires a reachable daemon.

Any type implementing the `Sandbox` interface is swappable in — the
runtime never inspects a backend beyond the opaque
`WorkspaceState.Backend` string. For config-driven selection, backends
register factories and the host resolves a configured name:

```go
// Config-driven construction: the git backend (sandbox/git) self-registers; a
// host-supplied backend registers itself the same way when its package is
// imported.
sb, err := sandbox.OpenSandbox("git", git.GitSandboxConfig{RepoPath: repoPath})

_, err = sandbox.OpenSandbox("podman", nil)    // -> ErrUnknownSandboxBackend, listing what IS registered
backends := sandbox.SandboxBackends()          // sorted names, for config diagnostics

// Direct constructors remain available:
inPlace := git.NewLocalGitSandbox(repoPath) // materializes on your repository. Destroy is a no-op.
worktree := git.NewWorktreeSandbox(repoPath, wt) // a detached git worktree this scope owns

err = sb.Create(ctx, sandbox.SandboxSpec{})    // OpenSandbox does NOT provision; Create is the lifecycle step
defer sb.Destroy(ctx)

ws, err := sb.Capture(ctx)
err = sb.Apply(ctx, ws)
diff, files, err := sb.Diff(ctx, ws, maxLines)

caps := sb.Capabilities()   // capability negotiation: Lifecycle/Exec/FileIO/Diff/Isolated/Containment
```

## API

### Scope Manager

```go
mgr := goatherd.NewScopeManager(store)

scope, err := mgr.Create("owner-id", sb)                            // sb may be nil (pure-causal)
child, err := mgr.Fork("parent-scope-id", "child-owner-id", nil)    // inherits parent's sandbox
child, err := mgr.ForkIsolated("parent-scope-id", "child-owner-id", nil, sb)
err := parent.Merge(child)                                          // takes *Scope, not an ID
err := parent.Discard(child)
err := mgr.DestroyScopeSandbox(ctx, "scope-id")                     // release provisioned resources
scope, ok := mgr.Get("scope-id")
active := mgr.ActiveScopes()

cp, err := mgr.CreateCheckpoint(ctx, "scope-id", snapshot)          // single-use
snapshot, err := mgr.RestoreCheckpoint(ctx, cp.ID)
latest := mgr.LatestCheckpoint("scope-id")
mgr.PruneCheckpoints(ctx, "scope-id")                 // also deletes persisted blobs

// Merge gate (plan 04): propose, record, review, decide
proposal, err := mgr.ProposeMerge(ctx, "child-scope-id", 2000)      // query; classifies create/modify/delete
proposedID, err := mgr.RecordMergeProposal(ctx, proposal)
// reviewer decides; then RecordMergeDecision(approved, reason, action, proposedID)
// and Merge (shared sandbox) or Seal + Settle (isolated)

// Retained outputs: seal, then settle exactly once
out, err := mgr.Seal(ctx, "scope-id")                              // scope stays active
got, ok := mgr.Output(out.ID)
outs := mgr.OutputsForScope("scope-id")
err = mgr.Settle(ctx, out.ID, goatherd.SettleSelect)               // or SettleApply / SettleRelease / SettleDiscard

// Restart durability: rebuild the registry from the trace
mgr, err = goatherd.RecoverScopes(store, goatherd.RecoverOptions{ /* Resolver: re-adopt sandboxes */ })
pruned := mgr.PruneTerminal()                                      // unregister merged/discarded scopes
```

### Scope workspace and checkpoint operations

```go
scope := goatherd.NewScope(store, "owner-id").WithSandbox(sb, false)  // owns=false: do not Destroy

ws, err := scope.CaptureWorkspace(ctx)          // reusable state, no lifecycle
err = scope.ApplyWorkspace(ctx, ws)
diff, files, err := scope.DiffWorkspace(ctx, ws, 2000)

cp, err := scope.CreateCheckpoint(ctx, snapshot)
snapshot, err := scope.RestoreCheckpoint(ctx, cp)
```

### Supervisor

A rule-based engine that watches the effect bus and emits **interventions** when rules match. Built-in rules guard against destructive operations, high error rates, and stuck detection.

```go
sup := supervisor.NewSupervisor(mgr, bus)
sup.AddRule(supervisor.DestructiveToolRule())
sup.AddRule(supervisor.HighErrorRateRule(0.5, 10))
sup.AddRule(supervisor.StuckDetectionRule(3))
sup.Start("orchestrator")
defer sup.Close()

for iv := range sup.Interventions() { ... }

err := sup.Inject(scopeID, "guidance text")
err := sup.Halt(scopeID)
```

### Materialization

```go
registry := substrate.NewSubstrateRegistry()
err := registry.Register(substrate)               // Substrate interface: SubstrateRef,
                                                  // DeclarationSchemas, CaptureSchemas,
                                                  // Containment, Materialize
substrate, err := registry.Get(substrateRef)      // ErrUnknownSubstrate when missing

receipt, err := materialize.Materialize(ctx, store, opCtx, req, registry)
// opCtx.Operation must be trace.OpMaterialize
// req: AppendIntentID, TargetTraceOwnerID, TargetRecordIDs,
//      TargetThroughOwnerOrdinal (materialize.MaxOwnerOrdinal), CaptureTraceOwnerID
// receipt: Outcome (success | clean_failure | split_state), SubstrateRef,
//          TargetRecordIDs, ProducedRecordIDs, WorldSideAnchors

echo := substrate.NewEchoSubstrate("my.echo.v1", "example.write.v1")
kv, err := substrate.NewKVSubstrate("world.sqlite")   // kv.sqlite.local.v1
ws := substrate.NewWorkspaceSubstrate(sandbox)       // workspace.sandbox.v1

result, err := substrate.Materialize(ctx, records)   // substrates never append;
                                                     // dispatch owns the append
```

### Trace schemas

The checkpoint and workspace schemas embed a backend-neutral `revision` and
`state_digest` rather than git-specific SHAs, so a trace reads the same whether
the workspace was a git tree, a worktree, or a container snapshot:

| Schema | Mode | Payload |
|---|---|---|
| `goatherd.checkpoint.created.v2` | declaration | `checkpoint_id, backend, revision, state_digest, has_snapshot` |
| `goatherd.checkpoint.restored.v2` | capture | `checkpoint_id, backend, revision, state_digest` |
| `goatherd.workspace.captured.v1` | declaration | `backend, revision, state_digest` |
| `goatherd.workspace.applied.v1` | capture | `backend, revision, state_digest` |

These are kernel extensions, not part of `shepherd.kernel.abi.v0`, so they do not
affect the golden vectors. Trace writes are advisory: a failed append never fails
the workspace operation, because the rollback guarantee does not depend on the
audit record.

### Constants

```go
goatherd.SchemaScopeForked          // "goatherd.scope.forked.v1"
goatherd.SchemaScopeMerged          // "goatherd.scope.merged.v1"
goatherd.SchemaScopeDiscarded       // "goatherd.scope.discarded.v1"
goatherd.SchemaSupervisorInject     // "goatherd.supervisor.inject.v1"
goatherd.SchemaSupervisorHalt       // "goatherd.supervisor.halt.v1"
goatherd.SchemaCheckpointCreated    // "goatherd.checkpoint.created.v2"
goatherd.SchemaCheckpointRestored   // "goatherd.checkpoint.restored.v2"
goatherd.SchemaWorkspaceCaptured    // "goatherd.workspace.captured.v1"
goatherd.SchemaWorkspaceApplied     // "goatherd.workspace.applied.v1"

goatherd.CheckpointValid            // "valid"
goatherd.CheckpointUsed             // "used"
goatherd.CheckpointInvalid          // "invalid"

sandbox.ErrNoSandbox               // scope has no sandbox
sandbox.ErrUnsupported             // backend does not implement the capability
```

The `trace.*` constants (modes, trusted contexts, the ABI/canonical versions)
live in the kernel module.

## Build & test

```bash
go build ./...
go test ./...
go vet ./...
gofmt -l .            # must be empty
```

## History

Extracted unchanged from `github.com/buchenberg/goatherd-kernel` (fresh
history; the original history remains in that repository's log).
