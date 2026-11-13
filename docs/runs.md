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
- `sent`: how many of the run's calls that act may have reached the vendor:
  those in `done`, and those whose outcome isn't known (KIFF sent one and lost
  the answer, or its result never came back). Counted from KIFF's word on each
  call. Shown by the API, always, 0 included: 0 means nothing the run did can
  have changed anything; more than `done` lists means something may have,
  though no result said so.
- `tally`: the run's count of its calls, by tool, from their record: `sent`,
  `held`, `refused`, `failed` and `unknown` (sent, with no word on whether it
  took effect), each left out at 0. It covers the calls that act and those
  KIFF refused, reads included. A caller shows it beside the run's words, so
  the count the owner reads is the record's, never the model's. Absent for a
  run that counted nothing, and for one that ended before it was kept.
- `source`: set when a schedule or an event started the run (the schedule's ID).
- `ended`: when the run moved to a final status, kept however it's written after.
- `refused`, `declined`: how many of its calls KIFF refused outright (outside the
  Card), and how many it held that the owner then refused, over all its parts.
- `usage_tries`, `usage_retry`: its usage report KIFF hasn't taken yet
  ([What a run reports to KIFF](#what-a-run-reports-to-kiff)).
- `version`: every change is a compare-and-set on it.

## What a run reports to KIFF

Every run that ends having used AI (`cost_micro_usd` above 0) is reported to KIFF once,
with its agent's own key: `PUT /v1/me/runs/{run}/usage` (`kiff.UsageOf`). KIFF keeps one
row per run and works out its credits from it, so a run's usage outlives the run, which its
agent's keep rules may delete. A run that used no AI, such as one refused before its first
turn, isn't reported.

```json
{
  "agent": "rae",
  "session": "ff4be946685b1e5b376c6966ed4d77b3",
  "trigger": "schedule",
  "source": "sch_7Kq2",
  "requester": { "kind": "member", "id": "user_01" },
  "started": "2026-11-07T08:00:00Z",
  "ended": "2026-11-07T08:00:41Z",
  "cost_micro_usd": 30000,
  "turns": 4,
  "status": "COMPLETED",
  "stop": "answered",
  "failed": false,
  "refused": 1,
  "kept_until": "2026-11-14T08:00:41Z"
}
```

- **Only what the run recorded,** never anything the model said: what started it and who
  asked, when it began and ended, what its turns cost, and how it ended.
- **`failed`** is an `ERROR`: the run failed on our side (it couldn't start, or its invocation,
  kiff/agents or the model failed it). A cap, a cancel or a refusal isn't a failure.
- **`refused` and `declined`** don't change what the run cost: they say what was blocked.
- **A crash isn't paid twice.** A turn of an invocation that stopped before it stored what it
  cost isn't in `cost_micro_usd`, so a run carried on after it counts what it stored.
- **`kept_until`** is when kiff/agents may delete the run (absent: kept until deleted). Until
  then, whoever shows the report can open the run.

**Sent from the store** (`run.Reporting`), on the write that moves the run to a final status,
whichever path makes it: the run itself, a cancel or the sweep. A report KIFF doesn't take
(no 2xx within 5 s) never fails the write. It's kept on the run (`usage_tries`,
`usage_retry`), and the sweep sends it again after 5 min, 20 min and 1 h (`run.UsageWaits`),
then gives it up and logs it. Once KIFF doesn't take one in a sweep, that sweep sends no other,
and the rest wait for its next look, so a sweep never spends its time on a KIFF that's down. KIFF
keeps a run's first report, so one sent twice changes nothing. A run whose agent was removed can't be reported, since its key went with it.

## Where a run's turns run

Every turn goes to the Claude API, with KIFF's key, unless the function is
configured otherwise. **The Bedrock backup is off by default**, and only these
settings turn it on:

| Setting | What it does |
|---|---|
| `AGENTS_MODEL_SOURCE` | `anthropic` (the default) or `bedrock`: where new runs go |
| `AGENTS_BEDROCK_FALLBACK` | `on`: a new run whose first turn the Claude API refuses for credits, or can't serve once its retries are spent, goes to Bedrock |
| `AGENTS_BEDROCK_DAILY_USD` | the most Bedrock's turns may cost in a UTC day; **without it, Bedrock refuses every turn** |

- **Bedrock** is Claude in Amazon Bedrock, in our own AWS account: bedrock-runtime,
  from the function's region, through the EU inference profiles
  (`eu.anthropic.claude-sonnet-5-5`, `eu.anthropic.claude-opus-5-5`), signed with
  the function's own role. `infra/aws.sh` grants exactly that, with
  `AGENTS_BEDROCK=on`.
- **A run stays where it started.** Only a new run's first turn falls back, and
  only before anything of it streamed. A run under way that the Claude API
  refuses fails with its reason, as it would without a backup. That holds for a
  run the sweep carries on after its invocation crashed too: where its turns ran
  is stored with each turn that acts, before its calls go out. A run the sweep
  restarts from its start (it crashed before any turn acted) sends none of its
  earlier turns, so its next turn is a new run's first.
- **What falls back:** the credit refusal (400 `invalid_request_error`, "credit
  balance"), or 500, 502, 503, 504 or 529 once the SDK's retries are spent.
  **Never** any other 400, a context too long, a refused key (401, 403), or a
  rate limit (429).
- **The daily cap:** Bedrock's turns count against a day's total, kept in the
  usage table, at Bedrock's price (Anthropic's list ×1.1 for the EU profiles).
  Past the cap, or with none set, a turn that would go there fails with
  its own words, alongside the Claude API's.
  - **It holds however many turns start at once.** Before a turn runs, its worst
    case is reserved against the cap in one conditional write (`model.WorstCase`:
    its text counted a token a byte at the dearest input price, a PDF eight
    tokens a byte with a 10,000-token floor, an image at least 4,800, the API's
    tool instructions, and all of its `MaxTokens` as output, at Bedrock's price,
    never more input than the model's context window). A turn whose worst case
    doesn't fit is refused before it runs. Once it's done, the day is settled
    to what the turn cost; a turn that fails gives its reservation back.
  - **So the cap is reached early, never passed:** a day's last turns can be
    refused while reservations are out, though what they'd have cost fits.
  - **A settle that fails** leaves the worst case counted, the safe side, and
    never fails the turn: the turn's real cost is below its worst case. A turn
    that cost more than it is logged, a bound that broke.
  - **A run already on Bedrock** whose next turn doesn't fit fails there,
    mid-way, with that reason: it doesn't move to the Claude API.
- **What Bedrock doesn't serve:** fast output and Anthropic's web search and
  fetch (a turn there runs without them), and the Models API (the context
  window comes from a fixed table). A fallback turn starts a fresh prompt cache.
- **The record:** a run stores `model_source` and `model_region` (the caller's
  region; Bedrock doesn't say where in the EU it ran), and each turn's usage
  carries `source` and `region`. A model that runs in one place names none.
  A run from before this, with turns or a turn it acted in, goes on on the
  Claude API. A new run's summary of an earlier conversation that no longer
  fits is a turn of its own, before the run's first: it can fall back on its
  own, and its usage event, not the run, says where it ran.
- **Errors** from Bedrock have AWS credentials (key ids, session tokens, SigV4
  signatures) taken out of their text before they're logged.
- **The Eval** runs on Bedrock with `-model-source bedrock` (or
  `AGENTS_MODEL_SOURCE=bedrock`), its judge too, without the daily cap, and its
  report names where the turns ran.

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
- `emit` gets `agent.Event`s as the run goes (`text`, `thinking` or `update`
  when the definition shows them, `tool_call`, `tool_result`); `nil` in the
  background.
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
- **The AI provider refusing our balance** (`model.ErrProviderBalance`: the
  Claude API's 400 "credit balance") stops the run with words of its own,
  `runner.FailedAIUnavailable`: it's on our side, and none of the person's
  credits were used, as KIFF counts a failed run free. The run's `code` is
  `ai_unavailable`, for apps to treat it apart from a credits refusal. The
  API's words stay in the logged error, which the `credit balance` alarm
  matches.
- **Why a call failed, after the run.** A run drops its calls' results when
  it ends, but each call's `Called` line is kept, and a call that wasn't done
  keeps its result's words as its `reason`: on one line, at most 300
  characters, with anything shaped like a credential hidden first (a key's
  known prefix, a Bearer token, a JWT, a run of 24 or more base64 or hex
  characters). Later runs read it on the record line, quoted as the tool's
  answer: data, never what was done. It can still name what the vendor's
  error named, such as a customer, as the result did while the run lived.
- **The count of what a run did.** The runner counts a run's calls that act,
  by tool and by what came of each, from the same outcome each call's
  `Called` line has (`agent.TallyOf`): sent, held, refused (by KIFF or the
  owner, or its hold expired: not sent), failed, and sent with no word on
  whether it took effect. After the results of every turn that acted, the
  model reads the count so far (`agent.TallyLine`): "Count so far of this
  run's calls that act, from its record: stripe_refund: 3 sent, 0 held,
  1 refused, 0 failed. Any number you give of calls sent, held, refused or
  failed is one of these, never your own total." Reads aren't counted, nor a
  call refused back unmade (beside a read, or after a refused call:
  ARCHITECTURE.md › Stop after a refused call).
  - **Record totals** (`AGENTS_TOTALS=record`, `runner.Runner.RecordTotals`;
    off unless set, and on in the Eval unless `AGENTS_TOTALS=off`). Asked to
    copy the count, a model still wrote its own ("six submitted" with five sent
    and one held), so with record totals on it gives none: a read KIFF refused
    is counted as refused, and the line becomes `agent.RecordTallyLine`:
    "Count so far of this run's calls that act, and of those KIFF refused, from
    its record: … This count is shown beside your words from the record, so
    never write a number of what was sent, held, refused or failed, whether
    calls or the items they acted on ("six disputes"), nor that none were: say
    what was done and what waits, in words." Its caller shows the run's count
    (`tally`) beside its words; that's why it's a setting, turned on once the
    callers show it. The run's `tally` is kept either way.

  The count is a line after the results, never inside one, so clearing a read result or cutting a kept one can't lose
  it. Across a pause the count is kept with the paused turn
  (`run.Turn.Tally`), without the calls the run waits for: the turn that
  says what it waits for reads them as held (a call whose outcome comes
  later, as sent), and the continued run counts each as it ended. At its end
  the run keeps the whole count (`run.Run.Tally`, the API's `tally`). It's
  never recorded as an event: the `Called` lines are the record, and the count is what
  they add up to (`TestARunsActsAreCountedAfterTheirResults`).
- **Dates.** Each run reads today's date just before its input, in UTC:
  "Today is Friday 6 November 2026 (2026-11-06), 10:15 UTC."
  (`runner.Today`). It's never recorded, so a later run reads only its own.
  A vendor's times in a gateway call's result are dated beside their
  numbers, which stay as they came: `"due_date":1793892934 (2026-11-05)`.
  - A number is dated when the tool's `outputSchema` marks its field as a
    time: format `unix-time`, `timestamp` or `date-time` on a number, or a
    unit in its description.
  - Otherwise, when the field's name reads as one: `*_at`, the word `date`,
    `created`, `due…`, `expires…`, `period_…` (a `period`'s `start` too) or
    `timestamp`, in snake_case or camelCase.
  - Only between 2000 and 2100, in seconds or milliseconds. Never an id or an
    amount, nor a time given as text.

Runnable: [`runner/example_test.go`](../runner/example_test.go).

## The skills a session loads

A definition names its skills (#101); the context carries their names and
descriptions, and `read_skill` reads one on demand. A session can also load
some outright (the lighter Coach: the service decides,
never the model): `POST /agents/{id}/runs` takes `skills: ["name@version"]`,
and `PUT /agents/{id}/sessions/{session}/skills {"add": […]}` adds one
mid-conversation, as the service. Each must be one the definition names, by
name and version (`registry.ErrSkillNotNamed`); the session keeps them
(`Session.Skills`), added once. Every turn's context carries their bodies
after the instructions, in the definition's order (never the order they
were named in), each as `# Skill: <name>`; one added mid-conversation is
read before the next model turn (`Agent.Reload`) and appended, never
inserted, so the cached prefix holds. No tool's answer ever carries one.

A run keeps the first skill it took up beyond what its session held when it
started, by name, as `first_skill` on its head: one added to the session
mid-run, or one the agent read with `read_skill` and found. It's set once,
with the run's next save, and empty for a run that took up none. It counts a
run's cost by the kind of work it did (a new job, a change, a question). It
changes nothing the model reads, the API's run view doesn't show it, and
nothing logs it.

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

### A step handed to the owner

`your_step` (tool/kiff) hands the owner a step the agent may never take; the
run pauses on it as on a held call, by KIFF's hand-off id, and collects
"done", "skipped" or no answer.

- **What prepared it:** the raise names the run's calls that acted, went
  through and that KIFF recorded (`Run.Done`, which the runner and the loop
  give tools as `run.DoneFrom`), about the same case (`kiff_about`) when the
  step names one, newest first, at most 20, by operation ID (`prepared_by`).
  KIFF refusing one of them (`prepared_by_invalid`) has the step raised again
  without them: it reaches its owner either way.
- **Its check:** once "done" is collected, the run's first read through the
  gateway about the same case is the step's check (`POST
  /v1/me/handoffs/{id}/check` with the read's operation ID): a read (a tool
  KIFF marks read-only, or a read of one that also writes), sent and not
  failed, in this run's execution. Once per step, never for a write, another
  case's read or another run's. With no such read, or none about a case, KIFF
  settles the step "not checked" after its hour.

## A run that reads page after page, and its cost cap

A run never ends on nothing because it kept reading: a taf's, a chat's or Coach's, `CloseInText` or not.

- **Page after page of the same list asks the owner.** `LoopPages` (10) reads
  in a row of the same read tool, with the same arguments but their cursor
  (`starting_after`, `cursor`, `page`, `offset`…, `kiff_` arguments, or a
  value that changed since the previous page to one that page held as an id),
  pause the run on a loop card. The run's own tools between pages (its notes,
  its cards: not through the gateway) don't count and don't reset it; a
  gateway write or a read of another list does. The card is a `confirm` (`show.Component`, id
  `tafs-loop-…`, `loop: {tool, pages}`) that stands in place of the tenth
  read, whose page isn't kept. Its words are tafs cloud's, in the owner's
  language, from `loop`.
  - **Read on** (`tafs_loop_continue`): the run goes on, told to read that page
    again; after 10 more pages it asks again. It never lifts the cost cap.
  - **Answer with what's read** (`tafs_loop_summarise`), or **no answer** by
    the card's wait: one turn with no tool use (`LoopNudge`), from what was
    read, saying what's left unread. The run ends answered.
  - **A run with no person behind it** (a schedule's, a door's: no member
    asked for it) has no one to answer the card, so it shows none and doesn't
    pause. It keeps the tenth page and closes in one turn with no tool use
    (`LoopEndNudge`: no one can be asked, so answer from what was read and say
    what's left). The run ends answered, and its report is that answer.
  - Only the answer recorded on the run counts: the person's, through
    `POST …/cards/{card}/answer`. A tool's result or the model's words never
    answer it.
  - Ten different reads, or the same read with other arguments, ask nothing.
- **At its cost cap, a run still answers.** It holds back its closing
  turn's cost: a tenth of the cap, or its latest turn's cost when that's more
  (at most half). It stops its tool calls before a turn that would leave no
  room for it, then takes one turn with no tool use (`CostNudge`): answer from
  what was read, say plainly what's left unread, and never give a partial
  count as the total. The run still ends `cost_cap`, within `MaxCost`.

At its turn limit too, a run that stopped on its tool results gets one turn
with no tool use to say what came of it (`CapNudge`). A run that came back
from a pause already at its cost cap has nothing held back, and stops. What
stays `CloseInText`'s alone is the quiet end: a run that ends on its tool
results under its caps, with nothing said, is nudged to say so only when it
closes in text (Coach); a taf's quiet scheduled run stays quiet.

## When a call's outcome comes later

Some writes end elsewhere: a reply to a ticket is answered by the customer,
days later; a transfer settles when the bank says so. The connection's
profile says so (planque's `completes`), and the relay answers such a call
`pending`, with what the run waits for. KIFF's gateway vouches for it: it
writes the wait in the call's `_meta` (`dev.kiff/call.wait {grant, event,
key, timeout_s, words}`) only for a planque connection's tool naming the
caller's own grant, and strips `pending` and `wait_for` from every
forwarded answer. `tool/kiff` reads the wait from `_meta` only, never from
the tool's content, and the result's state is `waiting`
(`tool.StateWaiting`, `Result.Wait`).

- The run pauses as it does for a held call, on a requirement of kind
  `event` (`run.KindEvent`) with the grant, the event and the key, and
  `expires` set from the connection's timeout, within a minute and three
  days (`run.WaitFor`). The pause's note names it (`waits`), and the agent's
  last turn says in a sentence what it waits for (`agent.WaitPauseNote`).
- The call itself is done: it was sent. It is in the run's `done` from the
  pause, and the session's record says `sent; its outcome came later, as the
  vendor's event`, or that it didn't come in time, when the run goes on.
- The event answers the wait through the API
  (`POST /agents/{id}/runs/{run_id}/waits/{call}/answer {payload}`,
  `run.AnswerWait`), by the service that matched it to the run on
  (tenant, grant, event, key): the event's data is the call's result,
  said to be the outcome that came later, and the run continues as after a
  card. A second event for the same call, or one after the wait ran out, is
  refused (`ErrNotOpen`, 409 `wait_not_open`).
- With no event by the deadline, the sweep continues the run as it does a
  card nobody answered (`resolved` is answered or expired), and the result
  says the outcome didn't come in time, with `{"timed_out": true, "event"}`:
  the agent never assumes it happened or didn't.

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

## Credits before each run

No new run starts while the account's tafs wait for credits: KIFF keeps the
balance, and `Start` asks it before each run (`runner.Runner.Allowance`).

- **The question:** `GET /v1/me/credits/allowance`, with the run's agent's own
  key (`kiff.AllowanceChecker`). KIFF answers for that agent: a taf's run waits
  with the balance; Coach's goes on within its fair use while a plan runs.
- **A refusal** stores nothing, and is a `*server.CreditsRefusal` with KIFF's
  code: `credits_used_up`, `limit_reached` or `no_plan`. The API answers 409
  with that code; a code it doesn't know reads as `credits_used_up`. A
  schedule's firing is `skipped_credits` (schedules.md), and isn't tried again.
- **An allowed answer is kept a minute per agent** (`kiff.AllowanceKept`), so a
  chat doesn't wait on KIFF each time; a refusal is never kept, so a raise or a
  pack lets the next run start at once. For up to a minute after the balance
  is used, a run can still start. That, and runs already going, are the only
  ways past it.
- **KIFF not answering** fails the start, as any KIFF outage does: the API
  answers 502 (`kiff_unanswered`), and a firing is tried again by the
  scheduler. A 404 is no exception: KIFF's route is live, so a 404 is a fault
  and fails the start, never a run let through unchecked.
- **A run already going finishes**, and a run stored before (a repeated
  delivery of a firing, a paused run carried on) asks nothing.
- A chat's AI bound (`ai`), from before, is ignored: nothing is counted toward
  a period, and `GET /usage/ai` reads what was.

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

A RUNNING run's lease reaches `run.LeaseSpan` (2 min) ahead of its last
renewal, never past the invocation's deadline plus a minute: the invocation
renews it on a heartbeat (`Runner.Heartbeat`, 30 s) while it goes, a model
turn or a tool call in flight included (`run.Renew`). So a dead invocation
is due for the sweep within about two minutes, not at its deadline, and a
live one, however long its call, is never taken for dead; a beat that finds
the lease lost stops the invocation, since the sweep may have carried the
run on. Until the sweep reaches it, a stopped run says so (`run.WorkAt`:
retrying, `run.Stopped`), never "working".

A RUNNING run whose lease ran out before any turn acted (a model turn, a
look-up) sent nothing: its last safe step is its start. The sweep stores
it PAUSED on an empty turn, with the same restarts (`run.ResumeWaits`,
`run.MaxResumes`), and continuing restarts it from its input, what the
owner wrote meanwhile included, its cost and turns counted on from the
run's (`agent.Continue`, the restart branch). Its input is recorded only
if its first start never got to recording it. Past 3 restarts, the run
ends ERROR with `run.NeedsYou`, as a crashed acting run does.

A PENDING run no invocation claimed (a lost dispatch) is dispatched again by
the sweep, once per look, up to `run.MaxResumes` times, its wait pushed out
each time; then it ends ERROR with `run.NeverStarted`, which reads as
`needs_you`. A run queued behind another (`After`) is the queue's.

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
runs.ListTenantPaused(ctx, "t1", 100)                        // a tenant's PAUSED runs, across its agents
```

`GET /runs/paused?tool=your_step&as=service` serves the last: the caller
tenant's paused runs, newest first, from its own paused list, so a sweep of a
whole account's waiting steps is one read, whatever its agents' histories.
`tool` keeps the runs waiting on a call of that tool; `limit` is 1 to 100
(100 by default); read as the service or as one `requester`, as an agent's
runs are. Another tenant's runs are never in it. A run paused before this list
existed is only in the operators' list until it leaves PAUSED: read it there,
or wait it out, rather than counting on this route for it.

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
| `PT\|tenant` | `created\|tenant\|id` | a tenant's PAUSED runs (`ListTenantPaused`), kept with `PA` in the same writes |
| `W` | `due\|tenant\|id` | RUNNING and PENDING runs the sweep must look at |
| `WP` | `due\|tenant\|id` | PAUSED runs the sweep must look at |

An ended run carries a TTL on every item from its `expires`. Local tests:
`DYNAMODB_ENDPOINT=http://localhost:8000 go test ./store/dynamodb/`.
