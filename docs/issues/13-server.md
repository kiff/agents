# The server on Lambda: what #13 delivers

**Issue:** [#13](https://github.com/kiff/agents/issues/13) · **Status:** partly delivered. The routes, auth and streaming are built, and on Lambda runs start, stream and go to the background. The sweep runs on a schedule and continues resolved paused runs, schedule firings start runs, and KIFF's alerts have their own function. Still to do: the 429 cap. Nothing is deployed.
**PRs:** #24 (routes that need no runs), #25 (`cmd/lambda`), #30 (run routes), #35 (streaming), #39 (follow-ups), #46 (runs on Lambda), #47 (the sweep), #48 (firings start runs), #53 (paused runs collect), #56 (KIFF accounts), #57 (`cmd/kiffalerts`), this page's PR (withdraw on Lambda)

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
| `POST /schedules/{id}/trigger` | Fire the schedule now, by hand: an ordinary run of its agent with its input, tagged with the schedule, in the background. Answers 202 with the run. The schedule needn't be on. **Only served when a `Runner` is set** |
| `POST /agents/{id}/runs` | `{input, session_id?, background?}`. Streams the run as SSE (below). With `background`, answers 202 with the run. **Only served when a `Runner` is set** |
| `GET /agents/{id}/runs/{run_id}` | The run: status, stop, output, error, cost, turns, requirements. Never its paused turn, claim, lease or version |
| `POST /agents/{id}/runs/{run_id}/cancel` | PENDING becomes CANCELLED; RUNNING stops at its next checkpoint; PAUSED has its held calls withdrawn in KIFF, then stops ([#15](15-continue-on-hold.md)); 409 on a server with no `Withdraw` |
| `POST /agents/{id}/runs/{run_id}/continue` | `{background?}`, or no body. A PAUSED run KIFF has resolved: streams like a start, or with `background` answers 202. 409 for a run not paused, or still waiting for KIFF ([#15](15-continue-on-hold.md)). **Only served when a `Runner` is set** |
| `POST /agents/{id}/runs/{run_id}/resume?after=N` | Replays the run's recorded events after `Seq` N, then follows the run until it's final or paused, then `end` (below) |

### The stream

Start, continue and resume share one stream. The SSE event name says what the data is;
it is not the kind of the agent event inside it.

| SSE event | `id:` | Data | Sent |
|---|---|---|---|
| `run` | none | The run as stored | Once, first; start and continue only |
| `delta` | none | An `agent.Event`: text as it's written, tool calls and their results as they happen. For showing only; what's kept is in the recorded events | Start and continue in the foreground only |
| `event` | the event's `Seq` | One recorded session event of the run | As it's recorded; resume replays them. Continue sends only what's recorded after the pause |
| `end` | none | The run as stored at its stop. Its status is empty when the run couldn't be read back, and the client reads it | Once, last, when the stream finishes normally |

A client that loses the stream calls `resume?after=` with the last `id` it
saw: the recorded events still kept come again from there, none twice.
Once a run has ended, events kept only until its end (tool calls and
results, by default) are gone and aren't replayed. A stream that closes
without `end` (the client went, or a read failed): read the run, or resume.
`delta` events are never replayed.

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

### Runs on Lambda

`cmd/lambda` builds the `runner.Runner` on the DynamoDB run and session
stores, with the registry's `Builder`:

- **The agents' KIFF keys** come from Secrets Manager through
  `registry/secretsmanager` ([#16](16-registry.md)), named
  `AGENTS_SECRETS_PREFIX` + tenant + `/` + the agent's `key_ref`.
- **The model's key** is the secret named `AGENTS_MODEL_KEY_SECRET`, read the
  first time a model turn needs it and kept in memory. A failed read fails
  that turn (the run is stored ERROR) and the next turn reads again. Setup
  reads no secret, so a cold start that serves no run never touches it.
- **The model key's secret must not be under `AGENTS_SECRETS_PREFIX`,** and is
  given by name, not ARN: any name under the prefix is reachable as some
  tenant's agent key. Setup refuses otherwise.
- **Tools:** the agent's gateway tools through `tool/kiff`, and its memory.
- **Paused runs:** `kiff.Collect` ([#15](15-continue-on-hold.md)) collects
  the outcome of each held call when a paused run is executed again. Without
  it, `Execute` refuses a paused run with `runner.ErrNoCollect`.
- **Cancelling a paused run**, or removing an agent with paused runs:
  `kiff.Withdrawer` takes back each held call at `api.kiff.dev` with the
  run's agent's own key, from the same Secrets Manager adapter. Each
  withdrawal waits at most 15 seconds.

**Background runs.** `POST /agents/{id}/runs` with `background` stores the
run PENDING, then invokes this same function asynchronously (Lambda's
`Event` invocation, to `AWS_LAMBDA_FUNCTION_NAME`, which Lambda sets) with
only the run's name:

```json
{"run": {"tenant": "t1", "id": "5c4c1b44224543331c22207410d6bc18"}}
```

It answers 202 once Lambda has queued the event. If queueing fails, or Lambda
answers anything but 202, the run is stored ERROR and the request gets 500, so
nothing waits on a run no invocation will claim.

The invocation that gets the event calls `Runner.Execute`, which claims the
run, so a repeated event (Lambda delivers at least once) finds it claimed or
ended and does nothing. That invocation never fails on what the runner
stored: a failed run is already ERROR, and a run that couldn't be claimed
stays PENDING until the sweep marks it.

### Schedule firings

EventBridge Scheduler invokes the function for each firing
([#17](17-schedules.md)). `schedule.Service.Fire` reads the schedule as it is
now, and starts its run through `Runner.Start`: `Trigger: run.BySchedule`,
`Source` the schedule's ID, the schedule's input.

- **The run's ID is the firing's key** (`Firing.Key()`: tenant, schedule and
  due time). EventBridge delivers at least once; a repeated delivery finds
  the run stored (`run.ErrExists`) and starts no other.
- **The run executes in the firing's own invocation**, which is already in
  the background, so a firing costs one invocation. A repeated delivery
  executes the run only if it is still PENDING, which means its first
  invocation died before claiming it. A run that is going, ended or PAUSED
  is left as it is. A paused run continues on KIFF's answer, not on a
  repeated firing.
- A run that pauses is stored PAUSED, like any run, not as a failure.
- **Known limit:** if storing the run fails, that firing is lost. The
  invocation fails and is logged, but asynchronous retries are off, and the
  schedule fires again at its next time.

### The sweep

An EventBridge Scheduler schedule invokes the function every minute with
`{"sweep":{}}`. Across tenants, the sweep ends the runs nobody else is left
to end: a PENDING run no invocation claimed within 5 minutes, and a RUNNING
run whose lease ran out (its invocation crashed or timed out). Each becomes
ERROR, or CANCELLED if it was asked to stop
([#12](12-runs.md)).

- A RUNNING run's recording is ended first (`runner.Runner.Ended`), so what
  was kept only until its end goes at once. If that fails, the run stays due
  and the next sweep tries again.
- Its expiry comes from its agent's keep rules (`runner.Runner.Expires`), or
  7 days when the agent can't be read.
- One sweep reads at most 100 due runs. A sweep that fails to read or to end
  a run fails its invocation, so it shows in the function's errors.

**Paused runs.** A PAUSED run is due one minute (`run.ResolvedGrace`) after
its last held call is answered or expires ([#15](15-continue-on-hold.md)).
The sweep reads up to 100 of those apart from the runs above, and sends each
to another invocation as a background run, with the same run event. That
invocation claims it, collects the held calls' outcomes through
`kiff.Collect` and carries on. A run that stays paused is tried again after a
wait that doubles, and is marked ERROR a day after it resolved. So a run goes
on 1–2 minutes after the owner answers on the phone.

### KIFF's alerts

KIFF tells the server when the owner answers a held call, through
`POST /kiff/alerts` ([#15](15-continue-on-hold.md)). KIFF signs each alert
with the alert target's own secret, not with AWS, so the alerts get a
function of their own, `cmd/kiffalerts`, behind a URL with no IAM auth
(`AuthType NONE`, invoke mode `BUFFERED`). It serves `POST /kiff/alerts` at
that exact path and nothing else: any other path is 404, cleaned or not, so
no route of the agents' API is reachable without IAM. Any event that isn't a
function URL request is refused.

`kiffalerts` only records the answer on the run. It executes nothing, and
has no permission to invoke the agents function: a direct invoke there can
send any event, a made-up function URL request for any tenant included. Once
the answer leaves the run waiting for nothing, the run is due, and the agents
function's sweep continues it (above).

**Which tenant an alert is for.** `registry/secretsmanager.Accounts` reads
the alert's KIFF account (its `tenant_id`) from one secret per account, named
`<accounts prefix><KIFF account>`:

```json
{"tenant": "t1", "secret": "<the alert target's signing secret>"}
```

- The account must be one name segment. Anything else, or an account with no
  secret, is `kiffalert.ErrUnknownAccount`, which the handler answers 401,
  the same as a bad signature. So an unsigned post can't reach another
  secret, or learn which accounts exist.
- Only the secret with that exact name answers. Secrets Manager can answer a
  name that ends in `-` and six characters with another secret, as part of an
  ARN.
- A secret that names no tenant (one segment) or no signing secret is a
  configuration error, and the handler answers 503. No error carries the
  signing secret.
- The accounts prefix must be outside the agents' key prefix, so no agent's
  key reference can name a signing secret. `kiffalerts` refuses to start if
  either prefix starts the other.

`cmd/kiffalerts` reads:

| Variable | What |
|---|---|
| `AGENTS_TABLE` | The DynamoDB table, for the runs |
| `KIFF_ACCOUNTS_PREFIX` | The name prefix of the KIFF accounts' secrets |
| `AGENTS_SECRETS_PREFIX` | The agents' key prefix, which `KIFF_ACCOUNTS_PREFIX` must stay apart from |

Its role may `secretsmanager:GetSecretValue` on `KIFF_ACCOUNTS_PREFIX` only,
and gets no `lambda:InvokeFunction`. On `AGENTS_TABLE` it reaches only the run
items an answer touches. The table also holds every tenant's agents,
sessions, notes and schedules, and a role that could reach them from the open
URL could read any tenant's conversations or rewrite an agent's definition.
The store's keys start with their kind, so IAM can bound it:

```json
{
  "Effect": "Allow",
  "Action": ["dynamodb:GetItem", "dynamodb:Query", "dynamodb:PutItem",
             "dynamodb:UpdateItem", "dynamodb:DeleteItem", "dynamodb:ConditionCheckItem"],
  "Resource": "arn:aws:dynamodb:<region>:<account>:table/<AGENTS_TABLE>",
  "Condition": {"ForAllValues:StringLike":
    {"dynamodb:LeadingKeys": ["R|*", "H|*", "L|*", "O|*", "W", "WP"]}}
}
```

- `R|`: the run and its turns.
- `H|`: its holds, which find a run by the exception KIFF names.
- `L|`, `O|`: its list items, by agent and by source.
- `W`, `WP`: the sweep's queues.

`TransactWriteItems` is authorized item by item under these actions. Check
the list at the first deploy, against a real answer.

Its reserved concurrency should be low: anyone can post to its URL, and each
post costs an invocation and a secret read, from the same Secrets Manager
quota the agents function reads its keys from.

### Configuration

Everything account-specific comes from the environment, never from code:

| Variable | What |
|---|---|
| `AGENTS_TABLE` | The DynamoDB table |
| `AGENTS_CALLERS` | Assumed-role ARN prefixes allowed to call, comma-separated |
| `AGENTS_GATEWAYS` | The gateways an agent may name; KIFF's by default |
| `SCHEDULE_GROUP`, `SCHEDULE_TARGET`, `SCHEDULE_ROLE` | EventBridge Scheduler's group, the function each firing invokes, and the role it invokes with |
| `AGENTS_SECRETS_PREFIX` | The name prefix of the agents' KIFF keys in Secrets Manager |
| `AGENTS_MODEL_KEY_SECRET` | The name of the secret holding the model provider's key; not under the prefix |
| `AWS_LAMBDA_FUNCTION_NAME` | Set by Lambda: where background runs are sent |

Setup names every missing variable at once. Both functions build for
`provided.al2023` on arm64.

**Before the first deploy**, IAM must hold this, because who may invoke the
function is the only guard on who the caller is:

- only `SCHEDULE_ROLE` and the function's own role may call
  `lambda:InvokeFunction` directly. The function's role needs it, on this
  function only, to send background runs. A direct invoke can send a run
  event, which only executes a stored run once, as a request would.
  `kiffalerts`' role must never get it;
- tafs cloud's role gets `lambda:InvokeFunctionUrl` (plus
  `lambda:InvokeFunction` only under `lambda:InvokedViaFunctionUrl`, if the
  account needs both);
- `iam:PassRole` on `SCHEDULE_ROLE` is limited to the operators who deploy;
- the function URL is created with `--invoke-mode RESPONSE_STREAM`;
- the function's asynchronous retries are off (`MaximumRetryAttempts` 0):
  the runner claims each run once, and the sweep ends what a crash leaves;
- the function's role may `secretsmanager:GetSecretValue` on secrets under
  `AGENTS_SECRETS_PREFIX` and on the model key's secret, nothing else;
- one EventBridge Scheduler schedule, `rate(1 minute)`, invokes the function
  with `{"sweep":{}}` through `SCHEDULE_ROLE`. Give it a name outside the
  agents' schedules (theirs are `s-` and a hash), for example `agents-sweep`.

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
| Setup wires the runner: agents' keys from Secrets Manager, the model keyed on first use, held calls collected through `kiff.Collect`, withdrawn through `kiff.Withdrawer`, the start route served | `TestSetupNeedsItsConfiguration` |
| A run streams through the function URL on Lambda | `TestARunStreamsOnLambda` |
| A background run is one asynchronous invoke naming only the run, executed by that invocation, once however often it's delivered | `TestABackgroundRunIsExecutedByAnotherInvocation` |
| A run that can't be queued is stored ERROR | `TestARunThatCantBeQueuedIsAnError` |
| A sweep event ends an unclaimed run and a crashed one, ends the crashed run's recording, gives both the agent's expiry, and leaves a run in its lease | `TestTheSweepEndsWhatNobodyElseWill` |
| A sweep that can't read fails its invocation | `TestAFailedSweepShows` |
| A sweep sends a resolved paused run to the background as a run event, and leaves one still waiting on KIFF alone | `TestTheSweepContinuesAResolvedPausedRun` |
| A firing starts its schedule's run, named by its key, and runs it to its stop; a repeated delivery starts and runs nothing more | `TestFiringsReachTheSchedule` |
| A repeated firing runs what the first left PENDING | `TestARepeatedFiringRunsWhatTheFirstLeftPending` |
| A repeated firing leaves a paused run alone: no claim, no collect | `TestARepeatedFiringLeavesAPausedRunToItsAnswer` |
| A named run starts once, even when two deliveries race | `TestANamedRunStartsOnce`, `TestANamedRunStartedMeanwhileIsReturned` (runner) |
| A schedule fired by hand is a background run of its agent, tagged with it, in the tenant only; one that can't go to the background is ERROR | `TestTriggerASchedule` |
| Without a Runner there is no trigger route | `TestNoRunnerNoStart` |
| A run event must name a run; a missing run doesn't fail the invocation | `TestARunEventMustNameARun` |
| The model key is read when a turn needs it, once, and again after a failed read | `TestTheModelKeyIsReadWhenATurnNeedsIt` |
| `kiffalerts` serves only `POST /kiff/alerts`, at that exact path; any other path is 404, any other event refused | `TestOnlyTheAlertsAreServed`, `TestOtherEventsAreRefused` (cmd/kiffalerts) |
| A signed answer is recorded and leaves the run PAUSED, due to the sweep; an unsigned, altered or unknown-account alert is 401 and changes nothing | `TestAnAnswerIsRecordedForTheSweep`, `TestAnAlertThatDoesntCheckChangesNothing` (cmd/kiffalerts) |
| Without `Continue`, the alerts handler only records, and the run is due to the sweep | `TestWithoutContinueTheSweepContinues` (server/kiffalert) |
| `kiffalerts` needs its configuration, with the accounts prefix apart from the agents' keys | `TestSetupNeedsItsConfiguration` (cmd/kiffalerts) |
| A KIFF account resolves to its tenant and signing secret, only under its prefix; an unknown or malformed account is ErrUnknownAccount; a bad secret is an error that carries no secret | `TestAnAccountIsItsTenantAndSecret`, `TestAnAccountIsOnlyOneUnderThePrefix`, `TestOnlyTheAccountAskedForAnswers`, `TestAnAccountSecretMustNameATenantAndASecret`, `TestAFailedAccountReadIsReported` (registry/secretsmanager) |
| The model key's secret can't be one an agent's key reference names | `TestTheModelKeyIsOutsideTheAgentsKeys`, `TestSetupNeedsItsConfiguration` |

Tests drive the routes against `store/memory`. No test touches AWS.

## Not delivered yet

- **An on-failure destination** for background invocations. Not built: a
  crash or a timeout leaves the run PENDING or RUNNING, and the sweep ends it
  (a PENDING run after 5 minutes, a RUNNING one once its lease runs out).
  A destination would only end it sooner.
- **429 at the tenant's concurrency cap.** The run store can't count a
  tenant's running runs yet.
- **The `continue` route and cancelling a paused run** are
  [#15](15-continue-on-hold.md)'s; `/evals`, `/usage` and `/ops/*` are
  [#14](14-console.md)'s.
- **The deploy:** both functions and their URLs, the table (in an EU
  region), the schedule group and role, the secrets, and the IAM above.
  Nothing has been created on AWS.
