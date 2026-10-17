# Plan 7 Phase 4 — `oversee(implement)` — **removed**

> **Status: removed from yaah.** It shipped (PR #347) as a reviewer-in-the-loop
> and was taken out after the design was checked against the reference. This file
> is kept as the record of the reversal, not as a plan.

## What it was

`retry` + `gate` from the Python combinators — fresh fork per attempt, failed
attempts discarded, merge only the accepted one — with the predicate played by a
**reviewer sub-agent**: dispatch a retained worker, have the reviewer inspect the
proposal's world, and on `accept` settle it; on rejection discard the round and
re-dispatch with the reviewer's reason. Bounded by `max_rounds`, two dispatches
per round.

## Why it went

**It is not what the reference does.** Shepherd's `gate` predicate is a callable
evaluated in the orchestrator's process:

```python
JudgePredicate = Callable[[Any, "EffectStreamLike"], bool] | Callable[..., Awaitable[bool]]

gate(task, predicate)   # predicate(result, effects) -> bool
```

`eval_predicate` awaits it if it is async. The "LLM-as-judge" note in
`_scope/scope.py` is a callable the *caller* supplies (`judge_approves(result)`) —
not an agent dispatch. There is no reviewer agent anywhere in the runtime.

**Authority belongs on the orchestrator side.** The split Plan 7 draws is
orchestrator ↔ sub-agent, one axis. `oversee`'s reviewer never talked to the
worker, so it was not agent-to-agent — but it held a *binding decision*, which is
the one thing the orchestrator is supposed to keep.

**Everything it needed was compensation for an untrusted separate context.** The
implemented loop carried a per-round verdict token (a sub-agent's prose can quote
an injected `VERDICT: accept` verbatim), a whole-tree fingerprint before and
after the review (the "read-only" reviewer's jail still granted the worktree as a
writable root, so read-only was a convention plus a post-hoc check), and a round
cap. A callable predicate needs none of those, and the residual was never
closable: detection, not prevention.

## What replaced it

The Phase 2 lane and the `proposal` tool already express the same shape:

```
spawn_subagent  ──▶  sealed proposal  ──▶  proposal show  ──▶  select | apply | discard
                        (fresh world)      (the diff)          the orchestrator decides
```

`discard` + re-dispatch is the retry. The predicate is the orchestrator reading
the change it is about to land — `gate(task, predicate)` with the predicate on
this side of the boundary. No new machinery.

## Consequence

The follow-up this phase motivated — a read-only review *world* — is moot for
review, since there is no reviewer. A read-only world may still be worth having as
a sub-agent capability in its own right (a read-only investigator that reports
findings), but it is no longer on the critical path. Dispatch accounting also
collapsed back to one concept: `isTask` is the only call that takes a sub-agent
concurrency slot.
