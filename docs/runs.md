# Runs

A run is one execution of an agent: one owner message, or one scheduled or
triggered job. It keeps one ID and one status for its whole life, through any
number of pauses. A session is the thread of runs it belongs to.

Packages: `run` (the type, its moves, the store port), `runner` (start and
execute), `chat` (a conversation), `agent` (the loop), `eval` (grading runs).

## A run, as stored

A run KIFF holds a call of, as `Runs.Get` returns it (JSON):

```json
{
  "id": "5c4c1b44224543331c22207410d6bc18",
  "tenant": "t1",
  "agent": "rae",
  "session": "ff4be946685b1e5b376c6966ed4d77b3",
  "trigger": "owner",
  "status": "PAUSED",
  "input": "Refund order 42.",
  "stop": "paused",
  "output": "The refund of order 42 waits for your answer.",
  "cost_micro_usd": 0,
  "turns": 2,
  "requirements": [
    {
      "call_id": "toolu_01",
      "tool": "stripe_refund",
      "input": { "order": 42, "amount": 90000 },
      "operation": "op-refund-42",
      "exception": "exc_123",
      "expires": "2026-10-16T09:10:00Z",
      "answered": "2026-10-16T09:03:12Z",
      "answer": "approved"
    }
  ],
  "paused": {
    "reply": {
      "role": "assistant",
      "blocks": [
        { "kind": "tool_use", "call_id": "toolu_01", "tool": "stripe_refund",
          "input": { "order": 42, "amount": 90000 } }
      ]
    }
  },
  "created": "2026-10-15T23:13:49.935718Z",
  "updated": "2026-10-15T23:13:49.936651Z",
  "version": 3
}
```

- `requirements`: one per call KIFF holds, from the gateway's result `_meta`
  (`dev.kiff/call`):
  - `operation`: the call's `kiff_operation_id`. Repeating the identical call
    with it collects the outcome.
  - `exception`: KIFF's ID for the owner's decision. Its `hold_answered` alert
    names it, and withdrawing the call takes it.
  - `expires`: when the hold stops waiting; expiry means refused. A
    requirement with no expiry resolves only when answered.
  - `answered`, `answer`: when and what KIFF answered (approved, refused,
    expired, invalidated), from its `hold_answered` alert (`run.Answer`).
- `paused`: the turn the run paused in (the model's message, and the results of
  the calls KIFF didn't hold). The API never shows it; it exists to continue.
- `source`: set when a schedule or an event started the run (the schedule's ID).
- `version`: every change is a compare-and-set on it.

## Statuses

| From | To | When |
|---|---|---|
| PENDING | RUNNING | claimed |
| PENDING | CANCELLED, ERROR | cancelled, or not claimed within 5 min |
| RUNNING | COMPLETED | answered, or a cap stopped it (`stop` says which) |
| RUNNING | PAUSED | KIFF holds a call |
| RUNNING | ERROR, CANCELLED | failed or lease ran out; asked to stop |
| PAUSED | RUNNING | every requirement resolved: claimed again, same ID |
| PAUSED | CANCELLED | cancelled; the API allows it once KIFF can withdraw the held calls (#15) |

COMPLETED, ERROR and CANCELLED are final. `run.CanMove(from, to)` is the table.

## Start and execute

```go
rn := &runner.Runner{Runs: runs, Sessions: sessions, Builder: builder}

r, err := rn.Start(ctx, server.Start{
	Tenant: "t1", Agent: "rae", Trigger: run.ByOwner,
	Input: "Refund order 42.",
})                                      // r.Status == run.Pending

r, err = rn.Execute(ctx, "t1", r.ID, emit) // claims, runs the loop, stores the stop
fmt.Println(r.Status, r.Stop, r.Turns)  // COMPLETED answered 2
```

- No `Session`: `Start` opens one (a chat for `ByOwner`, a job session
  otherwise). With one, the run joins that conversation.
- `emit` gets `agent.Event`s as the run goes (`text`, `tool_call`,
  `tool_result`); `nil` in the background.
- `Execute` leases the run until the caller's deadline plus a minute
  (`runner.MaxRun`, 15 min, if the caller has none). The loop stops 10 s
  before the caller's deadline (`StopTimeout`), to store where the run
  stopped: the recording is ended (or paused) once, then the run is saved,
  retrying a save that fails. If either still fails, the run stays RUNNING
  until its lease runs out, and the next sweep marks it ERROR (or CANCELLED,
  if it was asked to stop). Nothing schedules the sweep yet (#13).
- A failed run is stored ERROR with a public message (`runner.FailedBuild`,
  `FailedStart`, `FailedRun`, `FailedTime`); the cause is returned, never
  stored.

Runnable: [`runner/example_test.go`](../runner/example_test.go).

## When KIFF holds a call

```go
r, _ = rn.Execute(ctx, "t1", r.ID, nil)
fmt.Println(r.Status, r.Output)
// PAUSED The refund waits for your answer.
for _, q := range r.Requirements {
	fmt.Println(q.CallID, q.Tool, string(q.Input))
}
// c1 stripe_refund {"order":42}
```

- Every call in the turn runs; the run stops after it, whatever its caps.
- The agent gets one more turn, tools offered but forbidden
  (`tool_choice: none`), to say what it waits for. That turn is not part of what
  a continued run sees.
- Once KIFF resolves every requirement, `Execute` on the same ID continues
  from `paused`, with each held call's outcome from `Runner.Collect` (#15).
- A run cancelled while KIFF holds a call stays PAUSED with `cancel` set; once
  resolved it is claimed again, records the outcomes and ends CANCELLED.

## Chat

```go
c := &chat.Chat{Runner: rn, Tenant: "t1", Agent: "rae"}

r, err := c.Send(ctx, "Offer store credit first.", emit) // opens the session
r, err = c.Send(ctx, "And for Acme, ask me.", emit)       // same session, new run
```

A new message carries on the session's finished runs (what the owner and the
agent said; never tool calls), up to the agent's `History`. A run that is
paused or still going is left out until it ends.

## Writing runs directly

The runner does this; use it only for a runner of your own.

```go
r := run.New("t1", "rae", sessionID, run.BySchedule, "Daily payouts.", now)
r.Source = scheduleID
err := runs.Create(ctx, r)

r, err = run.Claim(ctx, runs, "t1", r.ID, invocationID, deadline, now)
r, err = run.Save(ctx, runs, r, now, func(n *run.Run) error {
	n.Status, n.Output = run.Completed, "Paid 3 payouts."
	return nil
})
```

| Error | Means |
|---|---|
| `run.ErrConflict` | changed since read; read it again |
| `run.ErrLeaseLost` | the claim or lease is gone; write nothing more |
| `run.ErrTransition` | the status can't move so |
| `run.ErrCancelled` | a pending run asked to stop; it never starts |
| `run.ErrNotReady` | a paused run still waits for KIFF |
| `run.ErrFixed` | tenant, ID, agent, session, trigger, source or created changed |

`Save` re-reads a run a cancel changed under it, and writes again while the
claim holds (up to `run.SaveTries`, 5); the run it returns shows `Cancel`.

## The sweep

```go
sw := run.Sweeper{Runs: runs, Ended: rn.Ended, Expires: expiresFromKeepRules}
n, err := sw.Sweep(ctx, time.Now(), 100)
```

Marks ERROR (or CANCELLED, if asked to stop) a RUNNING run whose lease ran
out, and a PENDING run not claimed within `run.PendingTimeout` (5 min). `Ended`
ends the run's recording first.

## Listing

```go
runs.List(ctx, "t1", "rae", 20)                              // an agent's, newest first
runs.ListSource(ctx, "t1", run.BySchedule, scheduleID, 20)   // one schedule's
```

## Evals

```go
rec, _ := session.NewRecorder(store, eval.Policy(30*24*time.Hour)) // keeps tool calls
// ... run the case as a run with Trigger: run.ByEval ...
er, err := eval.Load(ctx, store, runs, "t1", runID)
res, err := evalCase.Grade(ctx, er, judge)
```

`eval.Policy` keeps tool calls and results; the default rules drop them when
the run ends, and `Calls()` would grade nothing.

## Storage (DynamoDB)

One table, `pk`/`sk`, TTL on `ttl`, on-demand. Key parts are length-prefixed
(`R|2:t1|32:5c4c…`); `created` and `due` are zero-padded Unix nanoseconds.

| pk | sk | holds |
|---|---|---|
| `R\|tenant\|id` | `M` | the run, without its paused turn |
| `R\|tenant\|id` | `T\|n` | part n of the paused turn (300 KB parts, 3 MB max) |
| `L\|tenant\|agent` | `created\|id` | an agent's runs |
| `O\|tenant\|trigger\|source` | `created\|id` | one source's runs |
| `W` | `due\|tenant\|id` | runs the sweep must look at |

An ended run carries a TTL on every item from its `expires`. Local tests:
`DYNAMODB_ENDPOINT=http://localhost:8000 go test ./store/dynamodb/`.
