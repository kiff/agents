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
| **Event** | One thing that happened in a run: a message, a tool call and result, a component the agent showed, usage, a status change. Numbered without gaps; streamed as it happens; kept by the keep rules. | What the app shows of a run |
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
  requirement per held call: the call itself (its tool-use ID, tool name and
  input), its `kiff_operation_id` (the outcome is collected by repeating the
  call with it), KIFF's exception ID (what alerts name and withdraw takes) and
  the hold's expiry. It also keeps the turn it paused in: the
  model's message and the results of the calls KIFF didn't hold, and its own
  earlier turns since its input (their thinking left out, the latest 24 within
  512 KiB, a long result cut at 8 KiB), so a continued run knows what it already did.
  Requirements
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
  alert, or by asking KIFF, never from a request that claims it. A continue
  on a run its alerts haven't resolved asks KIFF first (the agent's read-only
  `kiff_pending`, with its own key) and records what KIFF answered, as the
  alert would: the app that just answered a hold can have the run go on at
  once, whether or not the alert reaches it (kiff/tafs#206). Continuing
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
  Cancelling a paused run first withdraws its held calls in KIFF, so an answer that comes later forwards nothing. A
  hold-answered alert for a cancelled run is still recorded on it, so a call
  KIFF forwards always has a record here. Removing an agent does the same
  before it deletes anything; tafs also revokes the taf's Cards first, which
  withdraws every hold that names them.
- **Steer** (#102) keeps a message the owner wrote on a run that hasn't
  ended, as a cancel is kept (at most 20 messages, 64,000 bytes together). The
  loop takes it at its next checkpoint (before a model turn, and once more,
  last, when the agent has answered), marks it taken,
  records it as an owner message and gives it to the model after what it read
  last. A paused run takes it once it continues. One never taken, because the
  run ended first, stays on the run, not taken, for the caller to send next.
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
| `chat` | A conversation: one chat session, each message a run through `server.Runner`; `Conversation` turns finished runs into history | Built (#2, #12): a thin layer over the runner (`Start` in the conversation's session, then `Execute`); `cmd/chat` runs on the same runner |
| `model`, `model/anthropic` | The model port; Anthropic first | Built (#2) |
| `workspace`, `tool/workspace` | A run's workspace: files and commands, no credentials, no way out; the `workspace` tool on it (an `Own` tool), opt-in per definition | Port, tool and the in-memory adapter, `workspace/inmemory` (#86). Next: the AWS adapter (its own function, no permissions, no route out) and its infra |
| `drafts`, `drafts/kiffapi`, `tool/drafts` | A taf's KIFF draft: the setup agent submits it (`kiff_draft`), its claim secret is kept from the model, and tafs cloud has its key claimed into the agents' secrets (`POST /drafts/{id}/claim`) | Built (#91) |
| `tool/show` | The `show` tool: UI components (steps, notice, confirm, approval, sign-in, result) recorded as `component` events, kept like messages, and drawn by the app (contract 1 on #91) | Built (#91) |
| `tool/planque` | The `planque` tool: ask planque for a vendor grant for a taf, and follow it, with the agent's own KIFF key; opt-in per definition, on the server's list of planques | Built (#91) |
| `tool`, `tool/kiff`, `tool/memory` | Tools; KIFF's gateway over MCP; memory notes | Built (#2). Step 4: `tool/kiff` records a held call's operation ID, exception ID and expiry (#44); collecting the outcome by the operation ID is next |
| `hook` | Before a run (input checks), around each tool call, after a run (output checks); background hooks for notifications | To build, when a check needs it |
| `registry` | Stored agent definitions, each bounded as stored; built per request for their tenant, with its gateway on the server's list and its KIFF key by reference; ports `Agents`, `Secrets` | Built (#16): definitions, `Build`, memory and DynamoDB stores, the routes, removing an agent and what is kept for it, the Secrets Manager adapter (`registry/secretsmanager`; by example in [docs/registry.md](docs/registry.md)). Removing an agent withdraws its paused runs' holds first (#15) |
| `schedule` | Schedules: five-field cron in a time zone, at most every 15 minutes, on or off; ports `Schedules`, `Scheduler` (EventBridge Scheduler, `schedule/eventbridge`). A firing reads the stored schedule and heals the Scheduler when the two disagree | Built (#17): the core, memory and DynamoDB stores, the EventBridge adapter, the routes (#13), firings starting runs (one per firing, named by its key), `/schedules/{id}/trigger`, triggers (`POST /agents/{id}/events`, one run per event key) |
| `server`, `server/lambdaurl` | The HTTP API below, SSE streaming, auth hand-off from tafs cloud; the `Runner` port | Built (#13): agents, memory, sessions, schedules and run routes; the IAM function URL adapter, streaming. `continue` and the `/kiff/alerts` handler (step 4, #15). The `/ops` run routes (#14). A tenant's active runs are capped (429). Still to come: `/evals`, `/usage`, the rest of `/ops` (step 3) |
| `eval` | Suites of cases: judge, expected tool calls, scorers in code; CI exit codes and a JSON report | Built (#3): the harness, Refunds and Chargebacks, the Managed Agents baseline reader; `Policy` and `Load` grade runs of this runtime (#12). Still to come: `cmd/eval` starting cases as runs, stored eval runs (step 3) |
| `console` | The operators' web UI over the API: runs, one run's timeline, evals, sessions and memory, agents and schedules, cost. Go, templ, htmx and plain CSS, like kiff.dev: no Node | To build |
| `attach`, `store/s3` | Files a person attaches to a chat message (kiff/tafs#209): what can be attached and how much, the upload URL for exactly the declared file, its bytes read back and held to their real type, Word and Excel turned into text, binding to its message, loading for a turn within a budget, and removal with the message, the chat, the agent or the tenant; ports `Store`, `Blobs` (S3, `store/s3`) | Built (kiff/tafs#209). See [docs/issues/209-attachments.md](docs/issues/209-attachments.md) |
| `store/memory`, `store/dynamodb` | Adapters for every store port | Built for sessions, runs, agents, schedules and attachments; grow with each port |
| `cmd/chat`, `cmd/eval` | Terminal chat, eval runner | Built |
| `cmd/lambda` | The server on Lambda | Built (#13): one function behind an `AWS_IAM` function URL in `RESPONSE_STREAM` mode; the runner, with agents' keys and the model key from Secrets Manager; background runs as asynchronous invocations of the same function; the sweep every minute, which also continues resolved paused runs; schedule firings, each starting one run and executing it in its own invocation. Still to come: the deploy |
| `cmd/kiffalerts` | KIFF's alerts on Lambda | Built (#13): its own function behind a URL with no IAM auth, serving only `POST /kiff/alerts`; KIFF accounts from Secrets Manager; it records answers only, and `cmd/lambda`'s sweep continues the resolved runs. The answer to the alert that leaves a run ready names it (`{"run":{"id","agent"}}`), so tafs cloud continues it at once through `…/continue` (kiff/tafs#138). Still to come: the deploy |

## The API

Only what tafs needs, in this shape. tafs cloud calls it with the tenant it has
verified; the phone never calls it directly.

| Route | Does |
|---|---|
| `GET /agents`, `GET /agents/{id}` | The tenant's agents |
| `PUT /agents/{id}`, `DELETE /agents/{id}` | Store a definition; remove an agent and everything kept for it. On an agent with versions, a put is its next version, live at once |
| `DELETE /tenant` | Delete the account's data, when its owner deletes the account (kiff/tafs#185): every agent as `DELETE /agents/{id}` removes it, then the tenant's own halt, its skills, its AI use, its sealing key and its drafts' records. Safe to repeat |
| `GET /agents/{id}/versions`, `GET /agents/{id}/versions/{n}`, `POST /agents/{id}/versions`, `POST /agents/{id}/versions/{n}/live` | Its versions (kiff/tafs#107): list them, oldest first, the live one marked; read one with its definition; when one goes live, record the KIFF Cards it runs under (`cards: [{id, revision}]`), for going back; store the next one (`definition`, `note`, `by`, `draft`, `revisions`), live unchanged; make any one live, forward or back (while a switch is under way, only its version) |
| `POST /agents/{id}/versions/{n}/pending`, `DELETE /agents/{id}/switching` | A switch to version *n* that waits for its Card revision (kiff/tafs#107): it begins only while no run of the agent is going or waiting on its owner (409 otherwise, naming those runs in `blocking`: `id`, `session`, `status`, `trigger`), and from then on nothing of the agent runs (no run starts, a stored one doesn't start, a paused one stays paused, a schedule's firing is skipped) and its definition isn't replaced, until `…/versions/{n}/live` ends it or it is cancelled. Trials still run. The mark shows in the versions list as `switching`. With `{"draft", "replace": true}`, a switch to *n* under way waits for that draft instead, keeping the mark, so no run starts in between (409 if it isn't switching to *n*) |
| `POST /agents/{id}/halt`, `POST /agents/{id}/resume`, `GET /agents/{id}/halt`; `POST /halt`, `POST /resume`, `GET /halt` | A halt of the agent, or of the whole tenant (and any agent made while it lasts). The halt is stored first, then every active run is cancelled as `…/cancel` does: a pending one at once, a running one at its next checkpoint, a paused one with its held calls withdrawn in KIFF. A paused run that can't be withdrawn here is still marked, and ends once KIFF, which refuses a halted agent's calls, resolves it. While halted nothing of it runs: no run starts, trials included (409 `halted`); a stored one ends `CANCELLED`, never started; a paused one isn't collected (collecting an approved hold would have KIFF send it): each held call is recorded as not sent, and it ends `CANCELLED` before any model turn; a schedule's firing is skipped. A run past its checkpoint keeps the calls of its turn: a write reaches KIFF, which refuses it once halted, and a vendor read under way finishes. A resume keeps the halt's time, and a run created at or before it never runs. An agent resumed on its own stays halted while its tenant is; resuming the tenant leaves an agent halted on its own halted. The answer: `halted`, `own`, `tenant`, `halted_at`, and how many runs it `stopped` (or only marked: `unstopped`). KIFF's side (its gateway refusing the agent's calls) is KIFF's own route; tafs cloud asks both |
| `POST /agents/{id}/versions/{n}/trial`, `GET /agents/{id}/trials/{run_id}` | A trial of version *n*, in the background, in a session of its own: its gateway calls carry `KIFF-Trial` (the version's draft, the one its switch waits for while one is under way, or `live`) and go only to a gateway that declares trials, so KIFF decides each write and sends none; it writes no notes. Read back: the run, and KIFF's decision on each write (`trial_write` events, kept like the agent's messages). A trial takes `notify` as a run does |
| `PUT /agents/{id}/trial`, `GET /agents/{id}/trial`, `POST /agents/{id}/trial/messages`, `DELETE /agents/{id}/trial` | The agent's open trial (kiff/tafs#173), one at a time: the owner chats with version *n* (`{n, draft}`, the draft KIFF confirmed as the version's **trial Card**) while the live version keeps its schedule and Cards. Each message is a trial run in the trial's one session, its calls under `KIFF-Trial: <draft>`; KIFF sends them while that draft is trying, decided on the trial Cards only. Opening another while one is open is 409: end it first. Ending it (`{why: installed, back or idle, calls, held, refused}`, the counts KIFF answered) deletes its chat and keeps one line on the version (`trials`). A trial unused for 7 days takes no message (409), and the function's sweep ends it: KIFF first, with the agent's own key (`DELETE /v1/me/agent-drafts/{draft}/trial`, which answers the counts), then here |
| `POST /agents/{id}/runs` | Start a run. Streams events (SSE) by default; with `background`, answers 202 with the run ID once the run is stored as PENDING. With `notify: {url, ref}` (kiff/tafs#206), the run is announced when it pauses and when it ends: `{run, agent, status, ref}` is POSTed to `url`, which must be one of the server's `AGENTS_NOTIFY_URLS` exactly. The note is unsigned and follows no redirect: `ref` is the caller's own value of 16 to 128 bytes, and the caller reads the run back to know what happened. A note that fails is dropped, and never fails the run. With `requester: {kind: member, door, manager or agent; id}` (#219), the run says who asked (`agent`: another agent of the account asking on its own, such as Coach handing a taf a task; its chats are its own, listed for no person), a new session is theirs (its `owner`, `member:<id>`), and a named `session_id` must be theirs or have no owner (404 otherwise). With `chat: "live"` (and a requester, no `session_id`), the message goes on in the requester's own latest chat while it's recent (30 minutes) and not archived, else a new one: never another person's. The run's `new_session` (always present, in the 202 and the stream's `run` event) says whether it opened its session. Each member or manager may send an agent 10 messages in 10 minutes, runs and steers together (429 past it); a door's sender has the door's own caps, and others aren't held back. A `member` requester's run starts only when the account says they may use the agent (#221): asked of KIFF with the agent's own key (`POST /v1/me/access`), kept a minute, and no answer is no. Refused: 403 `{"error": "no_access", "need": "use"}`. The same check at each checkpoint stops a going run, `CANCELLED` with why; a paused one collects nothing |
| `GET /usage/ai?period=` | AI fair use (kiff/tafs#193): a chat started with `ai: {period, bound_micro_usd}` is counted toward that billing period, and the runner adds its cost each time it pauses or ends, so a continued run is counted once in all. Past the bound, a new chat isn't started (409 `ai_fair_use:`); a run already going finishes. A bound of 0 counts without refusing: tafs cloud sends it for our own accounts. Each go on the owner signs in KIFF raises the bound tafs cloud sends by one more, so the tafs ask again at each further full bound. The read answers `spent_micro_usd`. tafs cloud names the period and the bound from the account's plan. Scheduled and event runs aren't counted: their work is paid per task |
| `GET /agents/{id}/runs` | The agent's runs, newest first (`limit`, 20 by default, 100 at most), without their paused turn. `active=true` keeps the PENDING, RUNNING and PAUSED ones among its 200 newest: what it is doing and waiting on (kiff/tafs#206) |
| `GET /agents/{id}/runs/{run_id}` | A run: status, output, stop reason, requirements, cost |
| `POST /agents/{id}/runs/{run_id}/resume` | Pick a stream back up from the last event index seen |
| `POST /agents/{id}/runs/{run_id}/cancel` | Cancel. With `requester` in the body (#219), a run another requester asked for is 404; without one, the caller cancels for itself |
| `POST /agents/{id}/runs/{run_id}/messages` | Steer: a message the owner wrote while the run goes, taken at its next checkpoint (202). 409 for a run that ended or was asked to stop: send it as a new run. With `requester` (#219), the message counts toward their flood guard, and a run another requester asked for is 404; without one, the caller steers for itself, unchecked and not counted |
| `POST /agents/{id}/runs/{run_id}/continue`, `POST /agents/{id}/holds/{exception}/continue` | Continue a paused run whose requirements KIFF has resolved, named by its ID or by one of its held calls. Holds no alert has resolved yet are asked of KIFF first; one KIFF still holds is 409, and KIFF not answering is 503 |
| `POST /agents/{id}/attachments?owner=member:<id>`, `GET /agents/{id}/attachments/{att}?owner=member:<id>` | A file the person will attach to a message in their chat (kiff/tafs#209): `{media_type, name, size, sha256}` answers 201 with the attachment and an `upload` (a PUT to a URL with headers to send as given, for 5 minutes) for exactly that file; a type that can't be attached, or a file over its limit, is 400 saying which. The run's start names it in `attachments: [id]` (a person's own chat message only, never a door's); the input may then be empty. Reading answers a download `url` for a minute, to the person who attached it only: not the service, an admin or the account's owner |
| `GET /sessions?agent_id=&owner=`, `GET /sessions/{id}?owner=`, `DELETE /sessions/{id}`, `POST /sessions/{id}/archive?owner=`, `POST /sessions/{id}/unarchive?owner=` | Threads and their runs. With `owner` (a requester, `member:<id>`), only that person's threads and those from before owners: another person's is 404 (#219). Archiving puts a chat away: it says `archived`, and a live message no longer goes on in it. With `owner=member:<id>`, the account must say they may use or oversee the agent (#221), else 403 `no_access` |
| `GET /agents/{id}/memories?owner=`, `DELETE /agents/{id}/memories/{name}?scope=`, `POST /agents/{id}/memories/{name}/share?scope=` | Read and forget notes. Each note has a `scope`: none for a shared note, or a requester's key (`member:<id>`) for a person's own (#219). With `owner`, the shared notes and that person's own; without it, every note. Forgetting names the note's scope (none: the shared one). Sharing makes a person's note shared, keeping where it came from: 404 if they have none of that name, 409 if a shared one of that name says something else |
| `/schedules` (create, list, get, update, delete, enable, disable, trigger, runs) | Schedules and the runs they started |
| `POST /agents/{id}/events` | Work that came in (a Stripe event, an e-invoice), verified by the caller: one background run per event key |
| `POST /kiff/alerts` | KIFF's signed hold-answered alert: resolve the requirement, continue the run in the background. Answers 200 once the answer is recorded, whatever the run's state (a repeat, a cancelled run), so KIFF stops redelivering |
| `GET /evals`, `GET /evals/{id}` | Eval runs and their cases, each linked to the run it produced |
| `GET /usage?agent_id=&from=&to=` | Runs, tokens and cost per agent and per day: what a price is set from |
| `GET /ops/runs`, `GET /ops/runs/paused`, `GET /ops/tenants/{tenant}/runs/{id}` | Operators only, read-only, across tenants: a run's state, cost and held tools, never its content (no input, output, steering, requester or session events) |
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
- **History sent to the model** is bounded: the finished runs of the
  session, the owner's and the agent's messages, not tool calls and their
  results, up to the agent's `history` runs and, when set, its
  `history_tokens`. In a chat, the runs that no longer fit are summarised by
  the model once, kept as a `summary` event, and the summary comes first;
  the owner's first message is always kept as they wrote it. Without a
  summary, the agent is told that earlier messages aren't shown. Dropping a
  tool call always drops its result with it, so what the model gets is
  valid. A new message while a run in the session is paused starts a new
  run; history leaves the paused run out until it ends.
- **What a chat read** can be kept for its next messages (#172): a read-only
  call and its result, as one `kept_read` event, with the `UntilIdle` rule.
  Only in chats, only reads, sealed with the tenant's own key before it is
  stored (`session.Sealer`), used while the chat goes on (its `Retention`,
  idle) and never longer than its `Max` after the read; a disconnected tool's
  reads aren't used. No view of a session shows them. Each comes with its age.
- **Where things stand now**: a tool that holds what it made outside the
  chat (planque's grants, the drafts the setup agent submitted) says, at the
  start of each run, where each one the chat's components name stands, from
  its own source (`tool.Stater`). Neither this nor the kept reads is
  recorded: each run makes its own.
- **Memory** is the agent's notes, written through its memory tool. Each update
  costs a tool call, so the turn and cost caps bound it; notes live until
  forgotten or the agent is removed. A note is shared, or one requester's
  (#219). A person's run reads the shared notes and their own, and writes
  and forgets only their own; a door's sender reads the shared ones and
  changes none; a run no one asked for (a schedule's) reads and writes the
  shared ones. Each note keeps the session, run and requester it came from.
  Notes from before scopes are shared. A person's note becomes shared only
  through the share route.
- **Attached files** (kiff/tafs#209) are their message's: an owner's message
  records each by reference (never its bytes), the runner loads their bytes
  for a run's turns (the newest within 20 MB and about 100,000 tokens; an
  older one reads as out of view), and each goes when its message does, by the definition's retention
  for owner messages, or with its chat when deleted, its agent or its tenant.
  An upload no message named is removed after an hour. The function's sweep
  removes what is due; the bucket's lifecycle rule, anything left 38 days on.
- **Removing an agent** withdraws its paused runs' held calls in KIFF, then deletes its attached files, sessions, runs, events, notes and schedules.

## Running it

- **On Lambda.** One function serves the API. A background run is stored as
  PENDING before the 202, then executed by an asynchronous invocation of the
  same function, with Lambda's own retries off. The invocation claims the run
  with a compare-and-set and takes a lease that ends a minute after the
  invocation's own deadline: a healthy run can't outlive it, so it needs no
  heartbeat, and every write the run makes checks it still holds the lease. A
  crash or a timeout can't write ERROR itself, so a scheduled sweep (every
  minute) does: it marks ERROR a RUNNING run whose lease ran out, or a PENDING
  run no invocation claimed in time. The function has no on-failure
  destination: the sweep ends a run whose invocation failed within a minute of
  its lease ending, or, when it failed before claiming the run, once the
  PENDING run has waited 5 minutes. The same sweep asks KIFF about
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
   cancel withdraws held calls in KIFF (shipped: KIFF
   withdraws an agent's own held call, and tells alert targets when a hold
   expires).
5. **The registry:** agents stored, built per request for their tenant.
6. **Schedules:** EventBridge Scheduler; then triggers.
7. **tafs switches over:** tafs cloud calls the server; Managed Agents retired.
