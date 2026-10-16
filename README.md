# shepherd-runtime-go

The runtime layer of the [Shepherd](https://arxiv.org/abs/2605.10913) kernel:
the execution scope tree, the merge gate, settlement and recovery, the sandbox
backends, the substrate/materialize layer, the task runtime, and the supervision
rules engine.

It composes [`github.com/buchenberg/shepherd-kernel-go`](https://github.com/buchenberg/shepherd-kernel-go)
— the trace kernel (record model, canonical JSON, SQLite store, effect bus,
projections) — and adds the primitives that *use* a trace: branching execution,
workspace capture/restore, reversible settlement, and supervised task handles.

## Contents

| Package | Contents |
|---|---|
| `shepherd` (root) | the runtime hub — `Scope`, `ScopeManager`, checkpoints, workspace operations, the merge gate, settlement (`Seal`, `Settle`, `RetainedOutput`), recovery (`RecoverScopes`) |
| `sandbox` | the `Sandbox` interface and the backend registry |
| `sandbox/git` | the git backend (in-place and worktree); self-registers as `"git"` |
| `substrate` | the `Substrate` contract, the registry, the echo/KV/workspace substrates |
| `materialize` | the materialize orchestrator (owns the append) |
| `task` | the task runtime — `StartTask`/`StartTaskSync`, `Run`, `TaskControl`, the task `Registry`, `ChildHandle` |
| `supervisor` | the rules engine — `Supervisor`, `SupervisionRule`, the built-in rules |

## Dependency

The runtime requires the kernel for the trace store and the record vocabulary,
and for the shared, public `testkit` package (the vector schema both suites
decode). Only the trace kernel is pulled in — the runtime carries no other
third-party dependency.

For local development this repository pins the sibling checkout with a
`replace` directive in `go.mod`:

```
require github.com/buchenberg/shepherd-kernel-go v0.16.0
replace github.com/buchenberg/shepherd-kernel-go => ../shepherd-kernel-go
```

A released tag replaces that with a tagged requirement (and drops the
`replace`, which a tagged module must not carry).

## Build & test

```bash
go build ./...
go test ./...
go vet ./...
gofmt -l .            # must be empty
```

## History

Extracted unchanged from `github.com/buchenberg/shepherd-kernel-go` (fresh
history; the original history remains in that repository's log).
