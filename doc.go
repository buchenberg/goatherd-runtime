// Package goatherd is the runtime layer of the shepherd kernel: the scope
// tree an agent operates in, and the orchestration that composes the
// kernel's components around it.
//
// A [Scope] is an owner's causal unit of work — a branch of activity with
// an append-only trace of its records, an optional sandboxed workspace, and
// a lifecycle (active, merged, discarded, halted). [ScopeManager] owns the
// tree: forking (inheriting or isolating the workspace), merging back
// through a reviewable proposal, discarding, checkpointing, and settling
// sealed outputs. [RecoverScopes] rebuilds the tree from the trace after a
// restart — the trace is the registry.
//
// On top of the scope tree sit the merge gate ([MergeProposal],
// [MergeReviewer]) and settlement ([RetainedOutput], [Scope.Seal],
// [ScopeManager.Settle]).
//
// The kernel's components publish as their own packages, imported directly:
//
//   - github.com/buchenberg/goatherd-kernel/trace — the record model,
//     canonical JSON, the SQLite trace store, and the effect bus
//   - github.com/buchenberg/goatherd-runtime/sandbox — the Sandbox
//     interface and the backend registry
//   - github.com/buchenberg/goatherd-runtime/sandbox/git — the git
//     backend (in-place and detached-worktree modes); self-registers as
//     "git"
//   - github.com/buchenberg/goatherd-runtime/substrate — the Substrate
//     contract and the echo/KV/workspace substrates
//   - github.com/buchenberg/goatherd-runtime/materialize — the
//     materialize orchestrator
//   - github.com/buchenberg/goatherd-runtime/task — the task runtime:
//     StartTaskSync/StartTask, Run, TaskControl, ChildHandle with spawn/
//     adopt/abandon relations
//   - github.com/buchenberg/goatherd-runtime/run — the reference reversible
//     run composition: fork, handler, review, seal/settle
//   - github.com/buchenberg/goatherd-runtime/supervisor — the rules
//     engine that observes the effect bus and can inject, fork, discard,
//     halt, or deny
//
// task, materialize, and substrate are landed public primitives, fully tested,
// but no default host composition uses them yet. run is the reference composed
// path; the runtime does not promise a single integrated pipeline for the
// primitives until a real caller lands.
//
// Compatibility: the record digests, canonical JSON, and the vector
// fixtures are frozen ABI surface shared with the Python reference; the
// runtime layer follows the module's semver. Pre-1.0, minor releases may
// break — see the CHANGELOG for the v0.11.0 import-path migration table.
package goatherd
