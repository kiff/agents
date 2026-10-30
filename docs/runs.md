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
- `done` (#249): the calls that acted and went through, over all the run's
  parts, in order: each one's `call_id`, `tool` and `input`, with `resource` and
  `invocation` when KIFF named them. Only a Write call that doesn't only read
  counts, and only once KIFF allowed it and the vendor didn't fail. A call
  refused unmade, held, refused or made in a trial doesn't. It comes from the
  calls' results, never the model's words. A run that fails after a refund went
  through still lists the refund, so whoever shows the failure can say what was
  done. Stored at each pause and at the end, and shown by the API. The inputs
  together stay within `MaxDoneBytes` (64 KB), so the run fits its store item.
  Past that, a call is still listed, without its `input`.
- `source`: set when a schedule or an event started the run (the schedule's ID).
- `version`: every change is a compare-and-set on it.

## Statuses

| From | To | When |
|---|---|---|
| PENDING | RUNNING | claimed |
| PENDING | CANCELLED, ERROR | cancelled, or not claimed within 5 min |
| RUNNING | COMPLETED | answered, a cap stopped it, or the model's turn was refused (`stop` says which; `refusal`, the category) |
| RUNNING | PAUSED | KIFF holds a call |
| RUNNING | ERROR, CANCELLED | failed or lease ran out; asked to stop |
| PAUSED | RUNNING | every requirement resolved: claimed again, same ID |
| PAUSED | CANCELLED | cancelled: KIFF's held calls are withdrawn, then the run continues and stops |

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
- `emit` gets `agent.Event`s as the run goes (`text`, `thinking` when the
  definition shows it, `tool_call`, `tool_result`); `nil` in the background.
- `Execute` leases the run until the caller's deadline plus a minute
  (`runner.MaxRun`, 15 min, if the caller has none). The loop stops 10 s
  before the caller's deadline (`StopTimeout`), to store where the run
  stopped: the recording is ended (or paused) once, then the run is saved,
  retrying a save that fails. If either still fails, the run stays RUNNING
  until its lease runs out, and the next sweep marks it ERROR (or CANCELLED,
  if it was asked to stop), unless it was acting (below).
- **Never twice.** Each gateway call goes with a `kiff_operation_id` of the
  run's own: a hash of the run and the model's call, or of the run and the
  model's own id when it names one (`run.OperationOf`). Before a turn's
  acting calls go out, the run stores the turn (`Acting`), and again with
  their results: its last safe step. A turn that only reads isn't stored.
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
  Each outcome opens with what happened to the call (`runner.HeldOutcome`),
  since the model last saw KIFF hold it outside its Card:
  `KIFF held this call for the owner, who approved it. KIFF then sent it,
  once. The tool answered: …`, or that it was refused, expired or withdrawn
  and not sent.
- Cancelling a paused run withdraws KIFF's held calls first
  (`server.Server.Withdraw`), records what became of each and sets `cancel`;
  it is then claimed again, records the outcomes and ends CANCELLED. A run
  cancelled while RUNNING that then pauses does the same once KIFF resolves it.

## A halt

The owner halts one agent, or every agent of the account.
kiff/agents stops its side; KIFF refuses the agent's calls on its own.

- The halt is stored first (`registry.Agents.Halt`), then each active run is
  cancelled as above. A run claimed meanwhile checks the halt once claimed, so
  either the halt cancelled it or it sees the halt.
- While halted, `Start` refuses every run, trials included
  (`registry.ErrHalted`); a stored one ends CANCELLED with `runner.FailedHalted`,
  never started. A paused one is **not collected**: collecting an approved hold
  is what has KIFF send it, and a halt holds here even when KIFF can't be asked.
  Each held call is recorded as not sent (`runner.NotCollected`), and the run
  ends CANCELLED with `runner.FailedHalted` at its first checkpoint, before any
  model turn.
- A resume keeps the halt's time. A run created at or before it never runs, so
  a retry or a queued invocation from before the halt can't come back.
- An agent is halted while it or its tenant is. Resuming the tenant releases
  the agents halted only through it, those made during the halt too; an agent
  halted on its own stays halted.
- **In flight:** a run already past its checkpoint keeps the calls of the turn
  it is in: a write it is sending reaches KIFF, which refuses it once KIFF
  holds the halt, and a vendor read under way (a gateway read) finishes. Each
  keeps whatever it returns. A halt stops new sends; it doesn't undo what a
  vendor did.

## Chat

```go
c := &chat.Chat{Runner: rn, Tenant: "t1", Agent: "rae"}

r, err := c.Send(ctx, "Offer store credit first.", emit) // opens the session
r, err = c.Send(ctx, "And for Acme, ask me.", emit)       // same session, new run
```

A new message carries on the session's finished runs (what the owner and the
agent said; never tool calls), up to the agent's `History` and, when set, its
`HistoryTokens`. The runs that no longer fit are summarised once, and the
summary leads what the model reads, with the owner's first message as they
wrote it. Before the message itself come where things stand now (the tools'
`State`) and, when the agent keeps them, what the chat read earlier, each with
its age. A run that is paused or still going is left out until it ends.

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
sw := run.Sweeper{Runs: runs, Ended: rn.Ended, Expires: expiresFromKeepRules,
	Continue: executeInBackground}
n, err := sw.Sweep(ctx, time.Now(), 100)
```

Marks ERROR (or CANCELLED, if asked to stop) a RUNNING run whose lease ran
out, and a PENDING run not claimed within `run.PendingTimeout` (5 min). `Ended`
ends the run's recording first.

A RUNNING run whose lease ran out while it held an acting turn is carried on
instead, when `Hold` and `Continue` are set: its recording held, it is stored
PAUSED on that turn, each call without a result a `resume` requirement, with
its restart 5 min, then 20 min, then 1 h away (`run.ResumeWaits`). Continuing
repeats each gateway call by its operation ID, so KIFF gives back its first
outcome, or makes it once if it never got it; an `unknown` outcome is settled
once by KIFF's reconcile (`Runner.Settle`). Past 3 restarts, the run ends ERROR
with `run.NeedsYou`. Its note, `GET …/runs/{run}` and the runs list
(`?state=retrying|needs_you`) say where it stands (`run.Work`).

## Requests sent more than once

`POST /agents/{id}/runs` with `dedup_key` (a door message's id) and `source`
(`email`, `whatsapp`), in the background: the run's ID comes from the key
(`server.RequestRunID`), it is stored PENDING before it is handed on, and the
same key sent again gets that run, never a second one (200; 202 and handed on
again if it is still PENDING).

### One run at a time per door thread

Such a request with a `session_id` whose run from the same `source` is still
at work (PENDING, queued, or RUNNING) waits for it: it's stored PENDING with
`after`, the run it waits for (the end of the thread's queue), is answered 202
and isn't handed on. The sweep looks at it every `run.QueueLook` (1 min):
once the run before it is no longer at work (done, failed, stopped, or paused
for its owner), it's let go (`after` cleared) and `Continue` starts it. So a
thread's messages run in the order they came, one at a time. `Execute` leaves
a queued run alone (`run.ErrQueued`). A person's own message isn't queued.

Two messages that come together can both be queued behind the same run, since
each reads the thread before the other is stored. When that run is done, the
sweep lets only the older go, by when each came and then by ID. The newer then
waits for the older (`after` moves to it). The thread is read from the
source's last 20 runs: on a door busier than that, a message can miss the
thread's run at work and start beside it.

Then, read apart (`Runs.DuePaused`), PAUSED runs due `run.ResolvedGrace`
(1 min) after their last requirement is answered or expires: the sweep calls
`Continue` for each. One still paused is looked at again after 2, 4, 8 …
minutes, up to an hour (`retries`, `retry`). A day after it resolved
(`run.PausedGiveUp`), the sweep claims it, ends its recording and marks it
ERROR.

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
| `A` | `created\|tenant\|id` | every run, for operators (`ListAll`) |
| `PA` | `created\|tenant\|id` | PAUSED runs, for operators (`ListPaused`) |
| `W` | `due\|tenant\|id` | RUNNING and PENDING runs the sweep must look at |
| `WP` | `due\|tenant\|id` | PAUSED runs the sweep must look at |

An ended run carries a TTL on every item from its `expires`. Local tests:
`DYNAMODB_ENDPOINT=http://localhost:8000 go test ./store/dynamodb/`.
