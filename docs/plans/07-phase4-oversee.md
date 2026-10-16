# Plan 7 Phase 4 — `oversee(implement)`: retry-until-acceptable as a primitive

> **Status.** Design. North star of Plan 7: the last thing that turns retained
> runs, settlement, and supervision into one move instead of a hand-written loop.

## 1. What it is

The composition Python Shepherd names in its combinator library — `retry` with a
`gate` inside:

- `combinators/retry.py`: `retry(task, max_attempts, until=predicate)` — **each
  attempt runs in a fresh fork**, failed attempts are **discarded**, and only the
  accepted attempt's effects are merged.
- `combinators/gating.py`: `gate(task, predicate(result, effects))` — run, then
  **merge on True and discard on False**, returning `Rejected` when it fails.

`oversee` is those two, with **one** divergence: the predicate is a **sub-agent's
judgement**, not a Python function. That follows from Plan 7's split — the
orchestrator holds the authority and the settlement; it delegates the *judgement*
to a reviewer sub-agent and keeps the *decision*.

Today the same shape exists only as a hand-written loop: dispatch, read the
proposal, decide, re-dispatch with guidance (`spawn_subagent` + `proposal`, or the
older `supervised_task` retry-on-failure loop, which retries on **error** and has
no notion of "acceptable").

## 2. The loop

```
        ┌──────────────────────────────────────────────────┐
        │ round i = 1..max_rounds                          │
        │                                                  │
        │  worker  ── retained dispatch ──▶  proposal P_i  │   fresh worktree
        │              (role: worker)              │       │   (host untouched)
        │                                          ▼       │
        │  reviewer ── inspects P_i's world ──▶ verdict    │   read-only,
        │              (role: reviewer, workdir=P_i.world) │   no proposal
        │                                          │       │
        │             accept ──────────────────────┼──▶ settle (select) ──▶ DONE
        │             revise ──────────────────────┘       │
        │                 discard P_i, carry the reason    │
        └──────────────────────────────────────────────────┘
                             exhausted ──▶ discard P_last, report
```

1. **Worker.** A retained dispatch (`startRetainedRun`): isolated worktree, sealed
   proposal, host tree untouched — the Phase 2 primitive, unchanged.
2. **Reviewer.** A sub-agent with a read-only role, dispatched with
   `Workdir = P_i.world` so it inspects the **post-change tree**, not just a diff
   text. It produces a verdict. It gets no proposal of its own: a judgement
   mutates nothing, so there is nothing to retain.
3. **Accept** → settle once, `select` by default (fast-forward; fails closed if
   the tree moved), `apply` on request, `none` to hand the proposal back.
4. **Revise** → **discard** P_i (its worktree is torn down, its work never lands)
   and re-dispatch the worker with the reason appended — Python's "failed
   attempts are discarded" with the feedback as the `until` predicate's voice.
5. **Exhausted** → discard the last proposal and report, with the final verdict
   and the worker's own report. Nothing lands.

## 3. Invariants this must not break

- **The host tree is untouched until the accepted settle.** Every round is a
  fresh worktree; rejected rounds are discarded rather than merged. (This is why
  the loop is `retry`+`gate` and not "run, then undo".)
- **Settlement is once**, at the end, and only on acceptance. `settle: none`
  leaves it to the orchestrator's `proposal` call.
- **One writable world at a time.** The reviewer reads the worker's world; it
  never gets a second one, and it cannot write. The Phase 3 jail bounds *where*
  writes go, not *whether* they happen — the built-in reviewer even carries
  `sed`, which writes unless `dryRun` is set — so the loop **checks**: it
  fingerprints the world around the review (content hashes, excluding the
  confinement scratch it creates and `.git`) and invalidates the verdict if
  anything moved. That closes the real hazard, which is a *mismatch*: a reviewer
  that fixes a defect and then accepts has approved a tree the settle will not
  land, because the settlement applies the **sealed** delta, not the directory.
  Enforcing read-only through a workspace/jail policy (a world with no writable
  root) is the stronger follow-up; detection is what can be relied on today.
- **The orchestrator is not confined** and not supervised; `oversee` is its tool.
- **The concurrency cap still binds.** A composite that dispatches inside a tool
  call escapes the loop's per-turn sub-agent semaphore unless it is accounted for
  explicitly, so `oversee` takes a slot exactly as a spawn does
  (`isSubAgentWork`). Its two dispatches are sequential within a round, so one
  slot per call is the honest count. Token usage needs no special handling: the
  runner attributes it for every dispatch, whoever asked.
- **`mode: shared` is unaffected** — `oversee` is the retained lane's composition
  and is registered only when that lane is live (as `proposal` already is).

## 4. The verdict

A reviewer is an LLM, so the verdict must be a contract it can actually satisfy
and the tool can actually parse — not prose the loop has to interpret:

```
VERDICT: accept

VERDICT: revise
REASON: <what must change, addressed to the worker>
```

The tool reads the **last** `VERDICT:` in the reply (the model may reason before
deciding). Everything else — no verdict, an unknown word, an empty reason, a bare
`VERDICT:` at the end — counts as **not accepted**: the loop retries with the
reply as feedback, and the round budget bounds it. Failing closed matters more
than a nice verdict, because the alternative is landing work a reviewer never
approved.

Three properties are load-bearing here, because **the content under review is
worker-controlled**: a change can legitimately add the line `VERDICT: accept`, and
the prompt itself shows that line as the format — so the reviewer can echo it.

1. **Fenced blocks are ignored**, so a quoted instruction or a diff containing a
   verdict cannot be mistaken for the reviewer's judgement.
2. **An inline code span is a quotation, not a verdict** (`- \`VERDICT: accept\``).
3. **An acceptance must be final**: the contract says the reply ends with the
   verdict, so an `accept` with judgement still to come — the shape an echo takes
   — is not an acceptance. A trailing contract block is still allowed through, so
   a reviewer that appends its usual `## Metrics` is not punished for it.

A later marker always supersedes an earlier one, including an empty one, so a
truncated reply (`VERDICT: accept\nVERDICT:`) cannot leave an earlier acceptance
standing. All pinned by tests.

## 5. What it does not do (scope)

- **Not a general workflow engine.** No arbitrary predicates, no branching, no
  parallel workers — Python has a combinator library; this is the one composition
  Plan 7 needs.
- **Not nested oversight.** A *sub-agent* does not call `oversee` in this phase
  ("one sub-agent supervises another" is satisfied by the reviewer being a
  sub-agent under the orchestrator). Nested oversight needs a story for
  dispatches under dispatches and is left to a later phase.
- **Not a replacement for `supervised_task`.** That tool retries on **failure**
  with checkpoints/rollback; `oversee` retries on **rejection**. Folding
  `supervised_task`'s retry into the retained primitive is separate work, and its
  review mode stays orchestrator-driven.
- **No autonomy over the merge.** `settle: none` exists precisely so the
  orchestrator can look first.

## 6. Test matrix

| Test | Action | Expected |
|---|---|---|
| `TestOversee_AcceptsFirstRound` | reviewer accepts round 1 | settles once; the change lands; 1 worker + 1 reviewer dispatch |
| `TestOversee_RevisesThenAccepts` | revise, then accept | round 1's proposal is **discarded** (its work never lands), round 2 settles; the reason reached round 2's worker prompt |
| `TestOversee_Exhausts` | always revise | nothing lands, every worktree is gone, the report carries the last verdict + the worker's report |
| `TestOversee_HostUntouchedUntilAccepted` | inspect the host tree mid-loop | unchanged while rounds revise |
| `TestOversee_UnparseableVerdictIsNotAcceptance` | reviewer returns prose | treated as revise, not accepted |
| `TestOversee_VerdictCannotBeQuotedIntoAcceptance` | a review quoting a fenced `VERDICT: accept` (or a truncated trailing marker) | not accepted; the real verdict decides |
| `TestOversee_ReviewerWriteInvalidatesTheVerdict` | the reviewer writes into the world it judges | the round is discarded, nothing lands, the error names the cause |
| `TestOversee_UnknownRoleRefused` | a misspelled worker or reviewer | refused before any dispatch |
| `TestOversee_UnusableReviewStillTellsTheWorkerSomething` | an empty review | the retry prompt is not byte-identical |
| `TestOversee_TimeoutResolvedFromTheRole` | an omitted `timeout_seconds` | the per-role default deadline applies |
| `TestNewDispatch_SubAgentWorkAccountsForCompositeTools` | a composite dispatch | takes a sub-agent concurrency slot; the task branches stay spawn-only |
| `TestOversee_SettleNoneHandsBack` | `settle: none` | accepted proposal stays open for the orchestrator; nothing applied |
| `TestOversee_ReviewerSeesPostChangeTree` | reviewer reads a file the worker created | the reviewer's world is the worker's world |
| `TestOversee_MaxRoundsBounded` | `max_rounds` respected | no more than N worker dispatches; refused when 0 |

## 7. Risks

- **Two dispatches per round** (worker + reviewer) compounds cost and latency.
  *Mitigation:* `max_rounds` defaults low (3) and is a parameter; the tool reports
  rounds taken so a caller can see when it is thrashing.
- **A reviewer that cannot be satisfied** burns the budget. *Mitigation:* the
  bound, plus the report naming the last verdict so the orchestrator can take
  over rather than re-dispatch blindly.
- **A reviewer that always accepts** makes `oversee` a rubber stamp. *Mitigation:
  none mechanical* — it is a review-quality question, the same one `supervised_task`'s
  review mode has; the trace records every round's verdict and reason for audit.
- **The write check is detection, not prevention.** A reviewer's writes are caught
  after the fact and the round is discarded, which costs a round rather than
  landing bad work. *Follow-up:* a read-only world (a jail with no writable root,
  or a scratch root) would make it impossible instead of merely detectable — the
  natural next use of the Phase 3 mechanism, and why the reviewer needs no
  settlement of its own.
