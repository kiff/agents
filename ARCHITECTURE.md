# Architecture

How kiff/agents is put together, and the order it is built in. Read this before
taking an issue: every package below has its place here, and a change that
doesn't fit is a change to this page first.

## In one picture

```
 tafs app ──► tafs cloud ──► server (cmd/lambda) ──► run loop ──► model (Anthropic first)
   phone       auth, push      agents, runs,           │  │
   signs                       sessions, memory,       │  └──► tools ──► KIFF gateway ──► the company's tools
   answers                     schedules               │                 (Cards checked at api.kiff.dev)
     │                              ▲                   ▼
     └──► KIFF (holds, answers) ────┘ hold answered   stores: sessions, runs, events,
                                      → continue      memory, agents, schedules (DynamoDB)
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

The tenant is the KIFF account. It is never taken from a request body: the
server gets it from the verified caller, and every store call is scoped by it.

## A run's life

```
            ┌──────────── cancel ─────────────┐
            ▼                                 │
 PENDING ─► RUNNING ─► COMPLETED              │
              │  ├───► ERROR                  │
              │  └───► CANCELLED ◄────────────┤
              ▼                               │
           PAUSED ── every requirement ──► RUNNING (same run ID)
              │       resolved, continue
              └──────────── cancel ───────────┘
```

- **Completed** means the agent answered, or a cap stopped it (cost, turns,
  output length); the stop reason says which. **Error** means it failed.
- **Paused** means KIFF holds at least one of its calls. The run records one
  requirement per held call, with KIFF's operation ID, and stops there.
- **Continue** happens once every requirement is resolved. KIFF resolves them,
  only KIFF: the server learns of an answer from KIFF's signed hold-answered
  alert, or by asking KIFF, never from a request that claims it. Continuing
  collects each held call's outcome from the gateway by its operation ID (an
  approved call is forwarded by KIFF, once; a refused one comes back as
  refused) and the loop carries on with those results, under the same run ID.
- **Every status change is a compare-and-set** on the stored run. Two continues
  for the same pause make one continuation; the second gets the first's
  answer. A continue during a transition gets 409 and retries. A held call can
  never be collected twice.
- **Cancel** records the intent; the loop stops at its next checkpoint (before a
  model turn, after a tool batch). It doesn't undo what a tool already did.
- **A paused run never expires.** Retention doesn't touch it, however long the
  owner takes; cancelling it releases it.

## Packages

Ports and adapters, as in AGENTS.md: a core package holds types, the ports it
needs and the logic; adapters import the core; wiring is in `cmd/`.

| Package | Holds | Status |
|---|---|---|
| `session` | Sessions, events, keep rules, the Recorder; ports `Store`, `Notes` | Built (#1). Gains run IDs on events |
| `run` | Run, Status, Requirement, transitions; port `Runs` | To build |
| `agent` | The loop: model → tools → model, caps, stop on a hold | Built (#2). Returns *paused* with requirements instead of ending; resumes from a paused run |
| `chat` | A message as a run in the current conversation | Built (#2). Becomes a thin layer over runs |
| `model`, `model/anthropic` | The model port; Anthropic first | Built (#2) |
| `tool`, `tool/kiff`, `tool/memory` | Tools; KIFF's gateway over MCP; memory notes | Built (#2) |
| `hook` | Before a run (input checks), around each tool call, after a run (output checks); background hooks for notifications | To build, when a check needs it |
| `registry` | Stored agent definitions; port `Agents` | To build |
| `schedule` | Schedules; port `Scheduler`, EventBridge Scheduler as the adapter | To build |
| `server` | The HTTP API below, SSE streaming, auth hand-off from tafs cloud | To build |
| `eval` | Suites of cases: judge, expected tool calls, scorers in code; CI exit codes and a JSON report | Built (#3). Reads runs once they exist |
| `store/memory`, `store/dynamodb` | Adapters for every store port | Built for sessions; grow with each port |
| `cmd/chat`, `cmd/eval` | Terminal chat, eval runner | Built |
| `cmd/lambda` | The server on Lambda | To build |

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
| `POST /kiff/alerts` | KIFF's signed hold-answered alert: resolve the requirement, continue the run in the background |
| `GET /health` | Liveness |

Errors say what to do: 404 for anything outside the caller's tenant, 409 for a
transition in progress or a run that can't take the action, 429 when the
tenant's concurrency is used up.

## What is kept, and for how long

- **Keep rules** per event kind (kept, until the run ends, never) with a
  retention, applied when an event is written (#1). The default keeps what tools
  return only while the run needs it.
- **Sessions and runs expire as a whole** when their last kept event has; paused
  runs never do. DynamoDB's TTL removes them for free.
- **History sent to the model** is bounded: the last few runs of the session, the
  owner's and the agent's messages, not tool calls and their results. Dropping a
  tool call always drops its result with it, so what the model gets is valid.
  Long threads get a summary later, when one needs it.
- **Memory** is the agent's notes, written through its memory tool. Each update
  costs a tool call, so the turn and cost caps bound it; notes live until
  forgotten or the agent is removed.
- **Removing an agent** deletes its sessions, runs, events, notes and schedules.

## Running it

- **On Lambda.** One function serves the API. A background run is stored as
  PENDING before the 202, then executed by an asynchronous invocation of the same
  function; a run that crashes is marked ERROR, never left RUNNING. Runs must
  fit Lambda's 15 minutes; the turn and cost caps keep them well inside.
- **Schedules** are EventBridge Scheduler entries that start ordinary background
  runs; a run that pauses counts as paused, not failed.
- **Woken by work.** A trigger (a Stripe event, an e-invoice, a hold answered)
  starts or continues a run the same way a schedule does; the clock is the
  safety net, not the driver.
- **Fast.** Starting an agent and recording an event stay cheap next to the model
  call; benchmarks in CI guard the hot paths.
- **Frugal.** On-demand tables with TTL; nothing kept that the rules don't keep;
  tests run against local stores and never create anything on AWS.

## Order of work

Each step is useful to tafs on its own:

1. **Runs:** the `run` package, runs in the session store, events carry their
   run, the loop pauses and resumes. Evals read runs.
2. **The server:** runs, sessions and memory routes; `cmd/lambda`.
3. **Continue on a hold:** `/kiff/alerts`, requirements resolved from KIFF,
   continue in the background.
4. **The registry:** agents stored, built per request for their tenant.
5. **Schedules:** EventBridge Scheduler; then triggers.
6. **tafs switches over:** tafs cloud calls the server; Managed Agents retired.
