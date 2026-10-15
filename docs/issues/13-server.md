# The server on Lambda: what #13 delivers

**Issue:** [#13](https://github.com/kiff/agents/issues/13) · **Status:** partly delivered. The routes, auth and streaming are built. `cmd/lambda` doesn't wire the runner yet, so on Lambda no run can start. Nothing is deployed.
**PRs:** #24 (routes that need no runs), #25 (`cmd/lambda`), #30 (run routes), #35 (streaming), #39 (follow-ups)

## What it's for

Step 2. tafs cloud needs one API to start a taf's runs, follow them, cancel
them, and read and remove what's kept for an agent. It runs on Lambda, and
only tafs cloud may call it.

## What it delivers

| Package | What |
|---|---|
| `server` | The HTTP API on the standard library's `net/http`, with the `Runner` port the run routes use |
| `server/lambdaurl` | The adapter for a Lambda function URL with `AWS_IAM` auth, streaming responses |
| `cmd/lambda` | One function: function URL requests go to the server, EventBridge Scheduler firings go to the schedules ([#17](17-schedules.md)), anything else is refused |

### Routes on `main`

| Route | Does |
|---|---|
| `GET /health` | Liveness; the only route with no caller |
| `GET /agents`, `GET /agents/{id}` | The tenant's definitions |
| `PUT /agents/{id}` | Store a definition: it must pass `Check`, and its gateway must be on the server's list |
| `DELETE /agents/{id}` | Remove the agent and what's kept for it (below) |
| `GET /agents/{id}/memories`, `DELETE /agents/{id}/memories/{name}` | Read and forget notes |
| `GET /sessions?agent_id=&limit=`, `GET /sessions/{id}`, `DELETE /sessions/{id}` | Threads and their events |
| `POST /schedules`, `GET /schedules?agent_id=`, `GET`/`PUT`/`DELETE /schedules/{id}`, `POST /schedules/{id}/enable`, `/disable` | Schedules, each with its `next` firing in UTC |
| `GET /schedules/{id}/runs` | The runs one schedule started, newest first |
| `POST /agents/{id}/runs` | `{input, session_id?, background?}`. Streams the run as SSE (below). With `background`, answers 202 with the run. **Only served when a `Runner` is set** |
| `GET /agents/{id}/runs/{run_id}` | The run: status, stop, output, error, cost, turns, requirements. Never its paused turn, claim, lease or version |
| `POST /agents/{id}/runs/{run_id}/cancel` | PENDING becomes CANCELLED; RUNNING stops at its next checkpoint; PAUSED is 409 until [#15](15-continue-on-hold.md) can withdraw its held calls |
| `POST /agents/{id}/runs/{run_id}/resume?after=N` | Replays the run's recorded events after `Seq` N, then follows the run until it's final or paused, then `end` (below) |

### The stream

Start and resume share one stream. The SSE event name says what the data is;
it is not the kind of the agent event inside it.

| SSE event | `id:` | Data | Sent |
|---|---|---|---|
| `run` | none | The run as stored | Once, first; start only |
| `delta` | none | An `agent.Event`: text as it's written, tool calls as they're made. For showing only; what's kept is in the recorded events | Start in the foreground only |
| `event` | the event's `Seq` | One recorded session event of the run | As it's recorded; resume replays them |
| `end` | none | The run as stored at its stop. Its status is empty when the run couldn't be read back, and the client reads it | Once, last |

A client that loses the stream calls `resume?after=` with the last `id` it
saw: the recorded events come again from there, none twice. `delta` events
are never replayed.

### The caller and the tenant

- **The tenant is the verified caller's**, never the request body's.
  `Server.Auth` is a port. On Lambda, `lambdaurl.Auth(roles)` accepts a
  request only when Lambda verified its signer as one of the configured
  assumed roles, and only then reads the tenant from the `Kiff-Tenant`
  header. tafs cloud's role is the one caller, and it verifies the person
  and their tenant before calling, so there's no shared secret.
- A role entry must stop at the role name and its `/`, so `tafs-cloud` can't
  also match `tafs-cloud-evil`.
- A body that names another tenant, or another ID than the path, is refused
  (400) rather than read.

### Errors

| Status | When |
|---|---|
| 400 | What the caller can fix, with its message: a definition or schedule that fails `Check`, a gateway not on the list, unknown fields |
| 401 | A caller with no tenant |
| 404 | Anything outside the tenant; a route the API doesn't have (JSON, not Go's plain text) |
| 405 | A route that exists, with the wrong method, with `Allow` (#39) |
| 409 | A run that can't take the action, a transition in progress, a schedule that exists: read it again |
| 413 | A body over 4 MB |
| 500 | Says only "internal error". The cause goes to the log, never to the caller |

### Removing an agent

`DELETE /agents/{id}` removes, in order:

1. its schedules, out of EventBridge and the store;
2. its runs;
3. its sessions and notes;
4. its definition, last, so a retry after a failure still finds the agent.

Withdrawing its held calls in KIFF first is [#15](15-continue-on-hold.md)'s.
Until then tafs revokes a taf's Cards before removing it, which withdraws
every hold that names them.

### Streaming

The function URL's invoke mode is `RESPONSE_STREAM`. `lambdaurl.Serve`
runs the handler in its own goroutine and streams what it writes, so a run's
events reach the client as they happen (#35). A client that goes away stops
being followed, and the run carries on (#39).

### Configuration

Everything account-specific comes from the environment, never from code:

| Variable | What |
|---|---|
| `AGENTS_TABLE` | The DynamoDB table |
| `AGENTS_CALLERS` | Assumed-role ARN prefixes allowed to call, comma-separated |
| `AGENTS_GATEWAYS` | The gateways an agent may name; KIFF's by default |
| `SCHEDULE_GROUP`, `SCHEDULE_TARGET`, `SCHEDULE_ROLE` | EventBridge Scheduler's group, the function each firing invokes, and the role it invokes with |

Setup names every missing variable at once. The function builds for
`provided.al2023` on arm64.

**Before the first deploy**, IAM must hold this, because who may invoke the
function is the only guard on who the caller is:

- only `SCHEDULE_ROLE` may call `lambda:InvokeFunction` directly;
- tafs cloud's role gets `lambda:InvokeFunctionUrl` (plus
  `lambda:InvokeFunction` only under `lambda:InvokedViaFunctionUrl`, if the
  account needs both);
- `iam:PassRole` on `SCHEDULE_ROLE` is limited to the operators who deploy;
- the function URL is created with `--invoke-mode RESPONSE_STREAM`.

## What holds, and the test that guards it

| Rule | Test |
|---|---|
| Every route but `/health` needs a verified caller; the tenant comes from it | `TestEveryRouteNeedsAVerifiedCaller`, `TestACallerWithNoTenantIsRefused`, `TestPutAgentTakesTheTenantFromTheCaller` |
| Only a configured role's tenant header is trusted, and no role entry can match others | `TestAuthTrustsTheTenantOnlyFromACallerRole`, `TestAuthRefusesARoleThatCouldMatchOthers` |
| Internal errors and private causes stay inside | `TestInternalErrorsStayInside`, `TestServeKeepsAPanicPrivate` |
| Removing an agent removes what's kept for it, runs included | `TestDeleteAgentRemovesWhatIsKeptForIt`, `TestDeleteAgentDeletesItsRuns` |
| A run streams `run`, `delta`, `event` (with its `Seq` as `id`) and `end`, and resumes from the last cursor | `TestStartStreamsTheRun`, `TestStartThenResumeFromTheLastCursor`, `TestResumeReplaysThenFollows` |
| A run in another agent's session is refused | `TestStartRefusesAnotherAgentsSession`, `TestRunnerContractRefusesAnotherAgentsSession` |
| A background run that can't be dispatched is ERROR at once | `TestStartThatCantGoToTheBackground` |
| A cancel that races a save reads the run again | `TestCancelReadsARunSavedMeanwhileAgain` |
| A dropped client stops being followed, on Lambda too | `TestADroppedClientIsNotFollowed`, `TestAGoneClientStopsTheFollowerOnLambdaToo` |
| Responses stream through the function URL | `TestServeStreams`, `TestServeFlushCommits`, `TestServeEndsTheHandlerWhenClosed` |
| A wrong method is 405 with `Allow`; an unknown path is JSON 404 | `TestWrongMethodAndUnknownPath` |
| Without a Runner there is no start route | `TestNoRunnerNoStart` |
| `cmd/lambda` refuses events it doesn't know, and needs its configuration | `TestOtherEventsAreRefused`, `TestSetupNeedsItsConfiguration` |

Tests drive the routes against `store/memory`. No test touches AWS.

## Not delivered yet

- **The runner in `cmd/lambda`.** It needs the agents' KIFF keys through a
  Secrets Manager adapter ([#16](16-registry.md), not written) and a model
  key. Until then `POST /agents/{id}/runs` isn't served on Lambda, and a
  schedule firing logs `errNoRuns` and starts nothing.
- **Background runs** as an async invocation of the same function, with
  Lambda's retries off and an on-failure destination that marks the run
  ERROR.
- **The sweep on a schedule:** `run.Sweeper{Runs, Ended: runner.Ended,
  Expires}`, with `Expires` from the agent's keep rules.
- **429 at the tenant's concurrency cap.** The run store can't count a
  tenant's running runs yet.
- **`POST /schedules/{id}/trigger`** (ARCHITECTURE.md lists it).
- **`continue` and `/kiff/alerts`** are [#15](15-continue-on-hold.md)'s;
  `/evals`, `/usage` and `/ops/*` are [#14](14-console.md)'s.
- **The deploy:** the function, its URL, the table (in an EU region), the
  schedule group and role, and the IAM above. Nothing has been created on
  AWS.
