# Architecture

How kiff/agents is put together, and the order it is built in. Read this before
taking an issue: every package below has its place here, and a change that
doesn't fit is a change to this page first. What each issue has delivered so
far, with the tests that guard it, is in [docs/](docs/README.md).

## In one picture

```
 console ─────────────────┐  (operators: runs, evals, cost)
 tafs app ──► tafs cloud ─┴─► server (cmd/lambda) ──► run loop ──► model (Anthropic first)
   phone       auth, push      agents, runs,           │  │
   signs                       sessions, memory,       │  └──► tools ──► KIFF gateway ──► the company's tools
   answers                     schedules               │                 (Cards checked at api.kiff.dev)
     │                              ▲                   ▼
     └──► KIFF (holds, answers) ────┘ hold answered   stores: sessions, runs, events,
                                      → continue      memory, agents, schedules, evals (DynamoDB)
```

KIFF Cloud decides; kiff/agents runs. Cards, decisions, held calls and the
owner's answers live in KIFF and nowhere else. kiff/agents keeps what an agent
did, under keep rules the operator sets, and never decides on KIFF's behalf.

## The model

| Thing | What it is | In tafs |
|---|---|---|
| **Agent** | A stored definition: instructions, model and price, tools, keep rules, cost and turn caps, how much history to bring along. Built afresh for each request, for its tenant. | A taf |
| **Session** | A thread of runs for one agent. It holds the runs, in order. | A chat conversation; or the one scheduled run and its continuations |
| **Run** | One execution of the agent: one owner message, or one scheduled or triggered job. It has a stable ID and a status for its whole life, through any number of pauses. | One message, or one job |
| **Event** | One thing that happened in a run: a message, a tool call and result, usage, a status change. Numbered without gaps; streamed as it happens; kept by the keep rules. | What the app shows of a run |
| **Requirement** | What a paused run waits for. Here, one call KIFF holds for the owner. | A held call on the "Needs you" screen |
| **Memory** | Notes an agent keeps across sessions, by name; the owner can read and forget each. | What a taf remembers |
| **Schedule** | When an agent runs by itself: cron, time zone, the input, on or off. Each firing is an ordinary run. | A taf's schedule |
| **Eval run** | One run of a suite: its cases, each one an ordinary run in an eval session, with the judge, tool-call and scorer results, the score and the cost per item. | How good a taf is, over time |

The tenant is the KIFF account. It is never taken from a request body: the
server gets it from the verified caller, and every store call is scoped by it.

## A run's life

| From | To | When |
|---|---|---|
| PENDING | RUNNING | An invocation claims it (compare-and-set) and takes its lease |
| PENDING | CANCELLED | Cancelled before it starts |
| PENDING | ERROR | Its invocation failed, or no invocation claimed it in time |
| RUNNING | COMPLETED | The agent answered, or a cap stopped it |
| RUNNING | ERROR | It failed, or its lease ran out (crash, timeout) |
| RUNNING | CANCELLED | Cancelled; it stops at its next checkpoint |
| RUNNING | PAUSED | KIFF holds at least one of its calls |
| PAUSED | RUNNING | Every requirement resolved; continue claims it with a new lease, same run ID |
| PAUSED | CANCELLED | Cancelled; its held calls are withdrawn in KIFF first |

COMPLETED, ERROR and CANCELLED are final.

- **Completed** means the agent answered, or a cap stopped it (cost, turns,
  output length); the stop reason says which. **Error** means it failed.
- **Paused** means KIFF holds at least one of its calls. The run records one
  requirement per held call: KIFF's operation ID and the call itself (its
  tool-use ID, tool name and input). It also keeps the turn it paused in: the
  model's message and the results of the calls KIFF didn't hold. Requirements
  and that turn belong to the run, not to its events, so the keep rules never
  drop them before the run ends: continuing never depends on what was kept.
- **A hold that nobody answers expires** in KIFF: after the Card's hold expiry
  (10 minutes by default, 7 days at most), and expiry means refused; a held
  call never runs on silence. Each requirement records that expiry. Once it
  has passed with no answer recorded, the sweep asks KIFF for the call's
  outcome by its operation ID, as any continue does, and continues the run
  with it: refused if the hold expired, or the owner's answer if it came in
  time and its alert was late or lost. Retention starts again once the run
  ends, so a paused run keeps what it holds for 7 days at most.
- **Continue** happens once every requirement is resolved. KIFF resolves them,
  only KIFF: the server learns of an answer from KIFF's signed hold-answered
  alert, or by asking KIFF, never from a request that claims it. Continuing
  collects each held call's outcome from the gateway by its operation ID (an
  approved call is forwarded by KIFF, once; a refused one comes back as
  refused) and the loop carries on with those results, under the same run ID.
- **Every status change is a compare-and-set** on the stored run. A held call
  can never be collected twice. A continue that repeats one already applied
  (the same KIFF alert delivered twice) gets 200 with the run as it is now. Any
  other continue on a run that isn't PAUSED gets 409; the caller reads the run
  rather than retrying.
- **Cancel** records the intent; the loop stops at its next checkpoint (before a
  model turn, after a tool batch). It doesn't undo what a tool already did.
  A run asked to stop in a turn where KIFF holds one of its calls pauses as
  usual; once KIFF resolves the hold, it continues, records the outcome and
  stops at its first checkpoint, CANCELLED.
  Cancelling a paused run first withdraws its held calls in KIFF
  (kiff/kiff-cloud#1245), so an answer that comes later forwards nothing. A
  hold-answered alert for a cancelled run is still recorded on it, so a call
  KIFF forwards always has a record here. Removing an agent does the same
  before it deletes anything; tafs also revokes the taf's Cards first, which
  withdraws every hold that names them.
- **A paused run never expires**, and neither does its session: retention
  starts again when the run ends.

## Packages

Ports and adapters, as in AGENTS.md: a core package holds types, the ports it
needs and the logic; adapters import the core; wiring is in `cmd/`.

| Package | Holds | Status |
|---|---|---|
| `session` | Sessions, events, keep rules, the Recorder; ports `Store`, `Notes` | Built (#1). Changes, in a PR of their own: a session becomes a thread of runs; events carry their run; "until the end" means the run's end; a session with a paused run doesn't expire |
| `run` | Run, Status, Requirement (the held call, KIFF's operation ID and the hold's expiry), transitions, the lease, the sweep; port `Runs` | Built (#12): types, transitions with compare-and-set, the lease, the sweep, memory and DynamoDB stores. Paused runs whose holds expired join the sweep in step 4 |
| `agent` | The loop: model → tools → model, caps, pause on a hold, stop on a cancel | Built (#2, #12): a held call pauses the run with its requirements and the turn it paused in; `Continue` carries on from a stored paused run with the held calls' outcomes; a cancel stops it at its next checkpoint |
| `runner` | Executing runs, as the server's `Runner`: a run started in its session (the conversation's, or a new one), claimed under a lease, its agent built for its tenant, recorded, run, and its stop stored (answered or capped, paused with its requirements, cancelled, failed). The history it sends is the session's last finished runs before it. A paused run continues here, in any process, once KIFF has resolved it | Built (#12). Continuing needs `Collect`, KIFF's outcome for each held call, from step 4 |
| `chat` | A message as a run in the current conversation | Built (#2). Becomes a thin layer over runs |
| `model`, `model/anthropic` | The model port; Anthropic first | Built (#2) |
| `tool`, `tool/kiff`, `tool/memory` | Tools; KIFF's gateway over MCP; memory notes | Built (#2). To add in step 4: `tool/kiff` sends and records KIFF's operation ID for each call, and collects a held call's outcome by it |
| `hook` | Before a run (input checks), around each tool call, after a run (output checks); background hooks for notifications | To build, when a check needs it |
| `registry` | Stored agent definitions, each bounded as stored; built per request for their tenant, with its gateway on the server's list and its KIFF key by reference; ports `Agents`, `Secrets` | Built (#16): definitions, `Build`, memory and DynamoDB stores; the routes and removing an agent (#13). Still to come: the Secrets Manager adapter, withdrawing an agent's holds on removal (step 4) |
| `schedule` | Schedules: five-field cron in a time zone, at most every 15 minutes, on or off; ports `Schedules`, `Scheduler` (EventBridge Scheduler, `schedule/eventbridge`). A firing reads the stored schedule and heals the Scheduler when the two disagree | Built (#17): the core, memory and DynamoDB stores, the EventBridge adapter, the routes (#13). Still to come: firings starting runs (wired in `cmd/lambda`), `/schedules/{id}/trigger`, triggers |
| `server`, `server/lambdaurl` | The HTTP API below, SSE streaming, auth hand-off from tafs cloud; the `Runner` port | Built (#13): agents, memory, sessions, schedules and run routes; the IAM function URL adapter, streaming. Still to come: `continue` and `/kiff/alerts` (step 4), `/evals`, `/usage` and `/ops` (step 3), the 429 cap, `/schedules/{id}/trigger` |
| `eval` | Suites of cases: judge, expected tool calls, scorers in code; CI exit codes and a JSON report | Built (#3): the harness, Refunds and Chargebacks, the Managed Agents baseline reader; `Policy` and `Load` grade runs of this runtime (#12). Still to come: `cmd/eval` starting cases as runs, stored eval runs (step 3) |
| `console` | The operators' web UI over the API: runs, one run's timeline, evals, sessions and memory, agents and schedules, cost. Go, templ, htmx and plain CSS, like kiff.dev: no Node | To build |
| `store/memory`, `store/dynamodb` | Adapters for every store port | Built for sessions, runs, agents and schedules; grow with each port |
| `cmd/chat`, `cmd/eval` | Terminal chat, eval runner | Built |
| `cmd/lambda` | The server on Lambda | Built (#13): one function behind an `AWS_IAM` function URL in `RESPONSE_STREAM` mode, and schedule firings. Still to come: the runner, background runs as async invocations, the sweep on a schedule, firings starting runs; the deploy |

## The API

Only what tafs needs, in this shape. tafs cloud calls it with the tenant it has
verified; the phone never calls it directly.

| Route | Does |
|---|---|
| `GET /agents`, `GET /agents/{id}` | The tenant's agents |
| `PUT /agents/{id}`, `DELETE /agents/{id}` | Store a definition; remove an agent and everything kept for it |
| `POST /agents/{id}/runs` | Start a run. Streams events (SSE) by default; with `background`, answers 202 with the run ID once the run is stored as PENDING |
| `GET /agents/{id}/runs/{run_id}` | A run: status, output, stop reason, requirements, cost |
| `POST /agents/{id}/runs/{run_id}/resume` | Pick a stream back up from the last event index seen |
| `POST /agents/{id}/runs/{run_id}/cancel` | Cancel |
| `POST /agents/{id}/runs/{run_id}/continue` | Continue a paused run whose requirements KIFF has resolved |
| `GET /sessions?agent_id=`, `GET /sessions/{id}`, `DELETE /sessions/{id}` | Threads and their runs |
| `GET /agents/{id}/memories`, `DELETE /agents/{id}/memories/{name}` | Read and forget notes |
| `/schedules` (create, list, get, update, delete, enable, disable, trigger, runs) | Schedules and the runs they started |
| `POST /kiff/alerts` | KIFF's signed hold-answered alert: resolve the requirement, continue the run in the background. Answers 200 once the answer is recorded, whatever the run's state (a repeat, a cancelled run), so KIFF stops redelivering |
| `GET /evals`, `GET /evals/{id}` | Eval runs and their cases, each linked to the run it produced |
| `GET /usage?agent_id=&from=&to=` | Runs, tokens and cost per agent and per day: what a price is set from |
| `GET /ops/runs`, `GET /ops/runs/{id}`, `GET /ops/runs/{id}/events` (SSE while it runs), `GET /ops/sessions/{id}`, `GET /ops/agents`, `GET /ops/agents/{id}/memories`, `GET /ops/schedules`, `GET /ops/evals`, `GET /ops/usage` | Operators only, read-only, across tenants: what the console reads |
| `GET /health` | Liveness |

Errors say what to do: 404 for anything outside the caller's tenant, 409 for a
transition in progress or a run that can't take the action, 429 when the
tenant's concurrency is used up.

## The console

Where we see what the agents do, as the owner sees it on the phone, and more.
It is served by the same server at `/console`, reads the API's operator
routes, and changes nothing: switching a schedule or an agent goes through
tafs.

| Page | Shows |
|---|---|
| Runs | Every run: status, stop reason, cost, trigger; paused runs first, with what KIFF holds |
| A run | Its timeline: messages, tool calls and results while kept, KIFF's decisions linked to app.kiff.dev, usage; live while it runs |
| Evals | Eval runs: cases with their results, score and cost per item over time, each case linked to its run |
| Sessions and memory | Threads with their runs; an agent's notes |
| Agents and schedules | Definitions; schedules with their next firing and history |
| Cost | Per agent, per run and per item over time |

It is for operators (the people building and running tafs), not for a business
owner: the owner's surface is the phone, where answers are signed. Signing in
is KIFF's sign-in, limited to operators. Operators read across tenants through
read-only operator routes of their own, never through the tenant-scoped routes
tafs cloud calls, so the tenant rule above holds unchanged. The console never
uses the phone's routes, and never shows what the keep rules dropped. KIFF's
own decisions stay in app.kiff.dev, linked from here, not copied.

## What is kept, and for how long

- **Keep rules** per event kind (kept, until the run ends, never) with a
  retention, applied when an event is written (#1). "Until the run ends" means
  that run, not the session: one run's tool results don't stay for the next. A
  tool call and its result always share one rule. The default keeps what tools
  return only while the run needs it.
- **Sessions and runs expire as a whole** when their last kept event has. A
  session with a paused run doesn't expire until that run ends. DynamoDB's TTL
  removes them for free.
- **History sent to the model** is bounded: the last few finished runs of the
  session, the owner's and the agent's messages, not tool calls and their
  results. Dropping a tool call always drops its result with it, so what the
  model gets is valid. A new message while a run in the session is paused
  starts a new run; history leaves the paused run out until it ends. Long
  threads get a summary later, when one needs it.
- **Memory** is the agent's notes, written through its memory tool. Each update
  costs a tool call, so the turn and cost caps bound it; notes live until
  forgotten or the agent is removed.
- **Removing an agent** deletes its sessions, runs, events, notes and schedules.

## Running it

- **On Lambda.** One function serves the API. A background run is stored as
  PENDING before the 202, then executed by an asynchronous invocation of the
  same function, with Lambda's own retries off. The invocation claims the run
  with a compare-and-set and takes a lease that ends a minute after the
  invocation's own deadline: a healthy run can't outlive it, so it needs no
  heartbeat, and every write the run makes checks it still holds the lease. A
  crash or a timeout can't write ERROR itself, so two things do: for a
  background run, the function's on-failure destination; for any run, a
  scheduled sweep that marks ERROR a RUNNING run whose lease ran out, or a
  PENDING run no invocation claimed in time. The same sweep asks KIFF about
  paused runs whose holds expired with no answer recorded. A run is never left RUNNING. Runs
  must fit Lambda's 15 minutes; the turn and cost caps keep them well inside.
- **Schedules** are EventBridge Scheduler entries that start ordinary background
  runs; a run that pauses counts as paused, not failed.
- **Woken by work.** A trigger (a Stripe event, an e-invoice, a hold answered)
  starts or continues a run the same way a schedule does; the clock is the
  safety net, not the driver.
- **Fast.** Starting a run (gateway round trip included), recording an event
  and continuing a paused run stay cheap next to the model call. Each has a
  benchmark; CI compares it with `main` and fails on a clear slowdown.
- **Frugal.** On-demand tables with TTL; nothing kept that the rules don't keep;
  tests run against local stores and never create anything on AWS.

## Order of work

Each step is useful to tafs on its own:

1. **Runs:** first, in a PR of its own, the shared types: `Run` and
   `Requirement`, events that carry their run, keep rules scoped to the run.
   Then the `run` package, runs in the session store, the lease and the sweep,
   and the loop pauses and resumes. Evals read runs.
2. **The server:** runs, sessions and memory routes; `cmd/lambda`.
3. **The console, first pages:** runs, one run's timeline, evals (eval runs are
   stored from here on).
4. **Continue on a hold:** `tool/kiff` records KIFF's operation IDs;
   `/kiff/alerts`, requirements resolved from KIFF, continue in the background;
   cancel withdraws held calls in KIFF (kiff/kiff-cloud#1245, shipped: KIFF
   withdraws an agent's own held call, and tells alert targets when a hold
   expires).
5. **The registry:** agents stored, built per request for their tenant.
6. **Schedules:** EventBridge Scheduler; then triggers.
7. **tafs switches over:** tafs cloud calls the server; Managed Agents retired.
