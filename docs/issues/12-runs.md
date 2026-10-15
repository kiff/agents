# Runs: what #12 delivers

**Issue:** [#12](https://github.com/kiff/agents/issues/12) · **Status:** nearly delivered. Chat as a thread of runs is in review (#34). Continuing a paused run in production waits on [#15](15-continue-on-hold.md).
**PRs:** #18 (shared types), #20 (the `run` package and stores), #26 (a run's source), #28 (the loop pauses and continues), #29, #31, #32, #36 (the runner), #37 (evals read runs, see [#3](03-evals.md)), #38

## What it's for

Step 1 of ARCHITECTURE.md's order of work. A run is one execution of an
agent: one owner message, or one scheduled or triggered job. It needs a
stable ID and a status for its whole life, through any number of pauses,
so that:

- a run that KIFF holds can wait for the owner, for days if the Card allows,
  and continue in a fresh process under the same ID;
- a crash or a timeout never leaves a run looking like it's still going;
- the server, schedules, evals and the console all read one record of what
  ran.

## What it delivers

| Package | What |
|---|---|
| `run` | `Run`, `Status`, `Trigger`, `Requirement`, `Turn`; the transition table (`CanMove`); `Change`, `Claim`, `Save`; the `Sweeper`; port `Runs` |
| `run/runtest` | The checks every `Runs` store must pass |
| `store/memory`, `store/dynamodb` | `Runs` adapters |
| `session` | Sessions as threads of runs: events carry their run and are numbered per run (#18; see [#1](01-sessions.md)) |
| `agent` | The loop pauses on a hold and continues from a stored run (#28; see [#2](02-chat-loop.md)) |
| `runner` | Executes runs, as the server's `Runner` (#36) |

### A run's life

| From | To | When |
|---|---|---|
| PENDING | RUNNING | An invocation claims it and takes its lease |
| PENDING | CANCELLED | Cancelled before it starts |
| PENDING | ERROR | No invocation claimed it within `PendingTimeout` (5 minutes), or its invocation failed |
| RUNNING | COMPLETED | The agent answered, or a cap stopped it |
| RUNNING | ERROR | It failed, or its lease ran out |
| RUNNING | CANCELLED | Asked to stop; it stops at its next checkpoint |
| RUNNING | PAUSED | KIFF holds at least one of its calls |
| PAUSED | RUNNING | Every requirement resolved; claimed again, same run ID |
| PAUSED | CANCELLED | Cancelled (its held calls are to be withdrawn in KIFF first, [#15](15-continue-on-hold.md)) |

COMPLETED, ERROR and CANCELLED are final. Staying in a status that isn't
final is allowed, so a running run can record its cost and a paused one an
answer.

### The rules every write follows

- **Compare-and-set.** `Change` applies a change to a run as read and stores
  it through `Runs.Put`, conditioned on its `Version`. A write from a stale
  read gets `ErrConflict`. A change works on its own copy of the
  requirements and turn, so an edit in place never leaks into the run as read.
- **What a run is never changes.** Tenant, ID, agent, session, trigger,
  source and creation time (`run.Same`). A change to any of them is
  `ErrFixed` (#26, #29).
- **One claim.** `Claim` takes a PENDING run, or a PAUSED one whose
  requirements KIFF has all resolved, and makes it RUNNING under a lease
  that ends `LeaseGrace` (a minute) after the invocation's own deadline. Of
  two claims, one wins. A PENDING run asked to stop is never claimed.
- **The lease.** `Save` is the claimant's write. Once the lease has run out,
  or another invocation has the run, nothing it writes lands
  (`ErrLeaseLost`). If the run changed under it but is still its own (a
  cancel set by the server), `Save` re-reads and writes again, up to
  `SaveTries` (5) times, and returns the run as stored, so the claimant sees
  the cancel (#38).
- **Leaving RUNNING ends the claim.** A final status drops the paused turn,
  which holds what tools returned.
- **A requirement fails closed.** `Requirement.Resolved` is true once KIFF
  reported it answered, or once its hold's expiry has passed (expiry means
  refused). A requirement with neither is not resolved (#32).

### The sweep

`run.Sweeper{Runs, Ended, Expires}.Sweep` ends the runs nobody is left to
end:

- a RUNNING run whose lease ran out becomes ERROR (or CANCELLED if it was
  asked to stop). Its recording is ended first, through `Ended`; if that
  fails, the run stays due for the next sweep;
- a PENDING run nobody claimed within 5 minutes becomes ERROR (or CANCELLED);
- each run it ends gets its expiry from `Expires`, the agent's keep rules;
- a run that moved on meanwhile is left alone.

`runner.Runner.Ended` is the `Ended` to use. Scheduling the sweep is
[#13](13-server.md)'s.

### The runner

`runner.Runner` implements `server.Runner`, so the run routes
([#13](13-server.md)) work on it.

- **`Start`** stores a PENDING run, in the given session (which must be the
  agent's, `session.ErrNotFound` otherwise) or in a new one: a chat session
  for the owner's runs, a run session for schedules and events.
- **`Execute`** claims the run, builds its agent for its tenant
  (`registry.Builder`, [#16](16-registry.md)), records it, runs the loop, and
  stores where it stopped:

  | Stop | Stored |
  |---|---|
  | Answered, or a cap | COMPLETED, with stop, output, cost, turns, and `Expires` from the keep rules |
  | KIFF holds a call | PAUSED, with requirements and the paused turn |
  | Cancel | CANCELLED |
  | Failure | ERROR, with a public message (`FailedBuild`, `FailedStart`, `FailedRun`, `FailedTime`) |

- **`Run.Error` is shown to the tenant**, so it says what failed, never why.
  `Execute` returns the cause for the server to log.
- **The caller can leave.** The run carries on to its next stop, and resume
  picks the stream back up.
- **Time to store the stop.** The loop stops `StopTimeout` (10 seconds) short
  of the deadline, so the stop is stored inside the lease. Runs default to
  `MaxRun` (15 minutes, Lambda's limit).
- **Recording first.** The recording is paused or ended before the run is
  stored, so a run is never stored as ended while what it kept until its end
  is still kept.
- **History** is the owner's and the agent's messages from the session's
  last finished runs created before this one, up to the definition's
  `History` (at most `MaxHistory`, 50). Paused runs and runs still going are
  left out. Which runs count is read from the run store, not from events.
- **Continue.** A PAUSED run continues in any process once KIFF has resolved
  it. The held calls' outcomes come from `Runner.Collect`, which
  [#15](15-continue-on-hold.md) supplies. Until it's set, `Execute` on a
  paused run returns `ErrNoCollect` and changes nothing. A failed `Collect`
  puts the run back to PAUSED unchanged.
- **Cancel during a held turn.** The run pauses with `Cancel` set. Once KIFF
  resolves the hold, it's claimed again, records the outcomes, and ends
  CANCELLED at its first checkpoint, so a call KIFF forwards always has a run
  here to record it.
- **An agent removed before its run executes** gets `UnknownKeep` (7 days) as
  its run's expiry, so nothing is kept for good by accident.

## Storage on DynamoDB

On the same table as sessions, in partitions of their own:

```
run   pk=R|tenant|id                sk=M             the run as JSON, without its paused turn
turn  pk=R|tenant|id                sk=T|n           part n of the paused turn (300 KB each)
list  pk=L|tenant|agent             sk=created|id    an agent's runs, by creation
from  pk=O|tenant|trigger|source    sk=created|id    the runs one source started (a run with a Source only)
due   pk=W                          sk=due|key       a run the sweep must look at, by when
```

- The paused turn is written in parts inside the run's own transaction, so a
  run is never read with half a turn. A turn over 3 MB is refused.
- A run read across query pages is checked against its version afterwards,
  and read again if it moved.
- The due partition holds entries only for PENDING and RUNNING runs, written
  and removed with the run's own change. The sweep reads it with no index and
  no scan. Its ceiling is about 300 run starts a second across all tenants;
  past that, shard it.
- An ended run carries its TTL on every item it has.

## What holds, and the test that guards it

| Rule | Test (in `runtest` unless named) |
|---|---|
| Every pair of statuses follows the transition table | `TheTransitionTableHolds` |
| Of two claims, one wins | `OneClaimWins` |
| A stale write is refused | `AStaleWriteIsRefused` |
| Nothing is written after the lease | `NoWriteAfterTheLease`, `ASaveAfterAnotherClaimIsRefused` |
| A cancel keeps the claimant's save | `ACancelKeepsTheClaimantsSave`, `ASaveGivesUpOnARunThatKeepsChanging` |
| A run cancelled while held ends once KIFF resolves it | `ARunCancelledWhileHeldEndsOnceResolved`, `TestACancelDuringAHeldBatchEndsOnceResolved` (runner) |
| A paused run continues only once resolved | `APausedRunContinuesWhenResolved`, `TestARequirementIsResolvedByAnAnswerOrExpiry` (run) |
| A paused turn over DynamoDB's item size is kept, and never read torn | `ABigPausedTurnIsKept`, `TestABigTurnIsNeverReadTorn` (DynamoDB) |
| The sweep marks what's due, leaves what moved on, never ends a just-claimed run | `SweepMarksWhatIsDue`, `SweepLeavesWhatMovedOn`, `ASweepNeverEndsAJustClaimedRun`, `AContinuedRunThatCrashesIsSwept` |
| Swept runs expire; a run asked to stop is swept CANCELLED | `SweptRunsExpireAndCancelsHold` |
| A finished run keeps no paused turn | `AFinishedRunKeepsNoTurn` |
| What a run is never changes | `WhatARunIsNeverChanges` |
| A source lists only its own runs | `ListBySource` |
| Removing an agent leaves nothing | `DeleteAgentLeavesNothing`, `TestRunItems` (DynamoDB) |
| A run started and executed reaches its answer, once | `TestExecuteRunsToAnAnswer`, `TestExecuteClaimsOnce` (runner) |
| The run outlives its caller; the lease ends at the deadline plus the grace | `TestExecuteOutlivesItsCaller`, `TestExecuteLeasesToTheDeadline`, `TestTheLoopLeavesTimeToStoreTheStop` (runner) |
| History is the session's finished runs, bounded | `TestHistoryIsTheSessionsFinishedRuns`, `TestHistoryLeavesOutARunStillGoing`, `TestHistoryIsBoundedByTheDefinition`, `TestHistoryWhenAPausedRunsEventsExpired` (runner) |
| A hold pauses; a paused run continues in a fresh Runner | `TestAHoldPausesTheRun`, `TestAPausedRunContinues` (runner) |
| A new message while a run is paused is a run of its own | `TestANewMessageWhilePaused` (runner) |
| A failed collect pauses again | `TestAFailedCollectPausesAgain` (runner) |
| The tenant never sees a private cause | `TestTheServerShowsNoPrivateCause` (runner, through the server) |

Every PR broke its own fixes on purpose, one at a time, and showed a test
fail for each. The tables are in the PR bodies.

**Benchmarks:** `BenchmarkStart` (create, then claim: about 25 µs) and
`BenchmarkContinue` (about 65 µs) in `run`; `BenchmarkStartAndExecute` and
`BenchmarkContinue` in `runner`; `BenchmarkPauseAndContinue` in `agent`. CI
compares them with `main` on every PR.

## Not delivered yet

- **Chat as a thread of runs.** One session per conversation, each message
  a run through `Runner.Start` and `Execute`. In review as #34.
- **Continuing in production** needs `Runner.Collect`, and requirements that
  carry KIFF's IDs and the hold's expiry. Both are
  [#15](15-continue-on-hold.md). Until then a paused run waits, visibly, and
  never continues.
- **The sweep on a schedule** still needs wiring in `cmd/lambda`
  ([#13](13-server.md)): nothing runs it yet.

## Known limits, accepted

- A continued run sees only the turn it paused in, not its own earlier turns
  (#18).
- The turn that says what a paused run waits for costs one model call per
  pause. It stays, because it's what the owner reads (#28).
