# Sessions: what #1 delivers

**Issue:** [#1](https://github.com/kiff/agents/issues/1) · **Status:** delivered, except SQLite (deferred) and tafs reading its thread from it (moves with [kiff/tafs#42](https://github.com/kiff/tafs/issues/42))
**PRs:** #4, #5, #6, #33; reshaped by #18 (sessions as threads of runs, under [#12](12-runs.md))

## What it's for

tafs couldn't remove what its agents recorded, or choose what they record.
This issue gives every agent a record it owns: sessions, events and memory
notes, with a rule per kind of event that says whether it's kept, for how
long, or never written. Every other issue builds on these types.

## What it delivers

| Package | What |
|---|---|
| `session` | `Session`, `Event`, `EventKind`, keep rules (`Policy`, `Rule`, `Keep`), the `Recorder`; ports `Store` and `Notes`; `Remove` (an agent's sessions and notes); `Cost` |
| `session/sessiontest` | The checks every `Store` must pass |
| `store/memory` | The in-memory store, for tests and local runs |
| `store/dynamodb` | The DynamoDB store, for tafs cloud |

### The types

- **Session**: a thread of runs for one agent (`KindChat` for a
  conversation, `KindRun` for a job). It has no status of its own: since #18,
  status belongs to the run.
- **Event**: one thing that happened, numbered per run without gaps. The
  kinds are owner message, agent message, tool call, tool result, report,
  memory note, usage and error.
- **Usage** events carry tokens and cost in millionths of a dollar, so
  `session.Cost` sums a run's cost exactly.

### Keep rules

Each event kind has a rule: **Never** written, kept **UntilEnd** of its run,
or **Kept** for a retention. The rule is applied when the event is written,
not afterwards.

- A policy that leaves a kind out is refused.
- A tool call and its result always share one rule (`Policy.Check`).
- `DefaultPolicy(retention)` keeps tool calls and results only until the run
  ends, and everything else for the retention (tafs uses 30 days).
- "Until the end" means the end of the event's own run, not the session's.

### Recording

```go
rec, _ := session.NewRecorder(store, session.DefaultPolicy(30*24*time.Hour))
// Open the session, then Start a run in it (with the run's creation time).
// The Recording is the run's only writer: Record never waits; one goroutine
// writes events in order, in batches of up to 25.
// Pause holds the run; Resume picks it back up in any process; End drops
// its until-end events.
```

- A failed write keeps its events queued and the next attempt retries them.
  A retry never stores an event twice: the store skips a sequence number it
  already has, and refuses a gap.
- `Flush` and `End` honour their context; `End` can be called again after a
  timeout.
- A body over `MaxBody` (256 KB) is recorded as an `OmittedBody` placeholder
  that says how big it was, so a large tool result never ends a run. One
  `Append` stays under 2 MB.

### Expiry

- **A session expires as a whole** when its last kept event has expired.
  From then on `Read` and `List` leave it out; `Purge`, or DynamoDB's TTL,
  removes it.
- **A session holding a paused run never expires.** `Hold` pins it until the
  run ends, and retention starts again from the run's end.
- Until-end events also carry a TTL of `UntilEndTTL` (8 days, a day past
  KIFF's longest hold), so a run that crashes before it ends can't keep what
  tools returned for good.
- **Memory notes** have no TTL. They live until forgotten, or until the agent
  is removed (`session.Remove`).

## Storage on DynamoDB

One table, on-demand, string keys `pk` and `sk`, TTL on `ttl`, no secondary
indexes, backups or streams. Each key part is length-prefixed, so no tenant,
agent or name can collide with another.

```
session  pk=S|tenant|id     sk=M            the session, its open runs and held runs
event    pk=S|tenant|id     sk=E|run|seq    one event
ended    pk=S|tenant|id     sk=R|run        a run that ended, so it can't begin again
index    pk=A|tenant|agent  sk=minute|id    an agent's sessions, newest first, for List
note     pk=N|tenant|agent  sk=name         one memory note
```

- `Append` is one transaction, conditioned on the run being open and on its
  last sequence number.
- `List` reads the index newest first and stops at its limit (#33): it costs
  the same however long an agent's history is. An append that moves a
  session into a later minute moves its index entry in the same transaction.
- `Delete` removes the events, then the session item and its index entry in
  one transaction, so no index entry outlives its session (#33).
- Tests never touch AWS. They run against DynamoDB Local and refuse any
  endpoint that isn't localhost.

## What holds, and the test that guards it

| Rule | Test (in `sessiontest` unless named) |
|---|---|
| A retried append stores nothing twice; a gap is refused, whole | `RetriedAppendStoresOnce` |
| A "never write" kind is never stored | `NeverIsNotWritten` |
| Purge removes what's past retention | `PurgeRemovesExpired` |
| Removing an agent leaves nothing of it | `RemoveLeavesNothing` |
| One tenant never sees another's | `TenantsAreApart` |
| Until-end events go only with their own run | `UntilEndIsDroppedWhenItsRunEnds` |
| A held session doesn't expire, and expires one retention after its run ends | `HeldSessionDoesNotExpire` |
| An ended run takes no events, holds or ends; only a paused run resumes | `EndedRunTakesNoEvents`, `OnlyAnOpenRunTakesEvents`, `OnlyAPausedRunResumes` |
| A paused run resumes in a fresh recorder and numbers on | `PausedRunResumesWhereItStopped` |
| Four runs recording at once in one session all land without gaps | `ConcurrentRunsInOneSession` |
| `List(n)` is exactly the newest n | `ListLimitIsTheNewest` |
| A session goes as a whole once its last event expires | `SessionExpiresAsAWhole` |
| No item that expires is written without a TTL (DynamoDB) | `TestItemsCarryTTL`, `TestAHeldSessionCarriesNoTTL` |
| `List(5)` over 60 sessions reads at most 13 items (DynamoDB) | `TestListReadsOnlyWhatItReturns` |
| A retried `Delete` leaves nothing (DynamoDB) | `TestARetriedDeleteLeavesNothing` |

Each PR broke its own fixes on purpose, one at a time, and showed a test
fail for each. The tables are in the PR bodies.

**Benchmarks:** `BenchmarkRecord` (about 3 µs per event, mostly JSON) and
`BenchmarkResume` (one read of a run's state).

## Not delivered yet

- **SQLite.** Deferred until something needs it: tests and evals use the
  memory store.
- **tafs cloud reading a taf's thread** in the shape its `handler.ts` serves
  today. That happens when tafs switches over ([kiff/tafs#42](https://github.com/kiff/tafs/issues/42)).
- **The real table** in tafs' AWS account, in an EU region, comes with the
  first deploy of `cmd/lambda` ([#13](13-server.md)).

## Where it differs from the issue

- **No session status.** The issue gave a session a status. Since #18 a
  session is a thread of runs, and status lives on each run.
- **"Delete everything for an agent"** is `session.Remove` for sessions and
  notes. Runs are deleted separately by the run store; `DELETE /agents/{id}`
  does both ([#13](13-server.md)).
