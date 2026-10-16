# Continue on a hold: what #15 delivers

**Issue:** [#15](https://github.com/kiff/agents/issues/15) · **Status:** in progress. Its outside dependency, [kiff/kiff-cloud#1245](https://github.com/kiff/kiff-cloud/issues/1245), has shipped.
**PRs:** [#44](https://github.com/kiff/agents/pull/44) (hold IDs), [#45](https://github.com/kiff/agents/pull/45) (Collect), [#49](https://github.com/kiff/agents/pull/49) (runs by hold, answers), [#50](https://github.com/kiff/agents/pull/50) (the alerts handler), [#51](https://github.com/kiff/agents/pull/51) (withdraw), [#52](https://github.com/kiff/agents/pull/52) (expiry in the sweep)

## What it's for

Step 4. When KIFF holds one of a run's calls, the run pauses
([#12](12-runs.md)). This step lets it continue once the owner answers on the
phone, or once the hold expires, and lets a cancel withdraw what KIFF still
holds. Until it lands, a paused run waits and never continues, and tafs
can't switch over ([kiff/tafs#42](https://github.com/kiff/tafs/issues/42)).

## Delivered

**A held call keeps KIFF's handle on it (#44).** The gateway reports a held
call in its result's `_meta`:

```json
"dev.kiff/call": {
  "state": "held",
  "operation_id": "op-refund-42",
  "exception_id": "exc_123",
  "hold_expires_at": "2026-10-16T09:10:00Z",
  "retry": "same_call"
}
```

`tool/kiff` reads it into `tool.Result.Hold`, and the loop copies it onto the
requirement:

```json
{ "call_id": "toolu_01", "tool": "stripe_refund", "input": {"order": 42},
  "operation": "op-refund-42", "exception": "exc_123",
  "expires": "2026-10-16T09:10:00Z" }
```

- `operation`: the call's `kiff_operation_id`. Repeating the identical call
  with it collects the outcome. For a call the model made without one, it is
  KIFF's own key (`invocation`, `tc-…`), which the gateway accepts in its
  place.
- `exception`: the owner's decision. Alerts name it; withdraw takes it.
- `expires`: when the hold stops waiting. With it, a requirement resolves on
  expiry as well as on an answer.
- Only `state` decides whether a call is held. A hold field in another shape
  is left empty, and an empty expiry fails closed, so the run still pauses.
- Other tools' `_meta` namespaces are never decoded with KIFF's schema.

| Rule | Guarded by |
|---|---|
| a held result's IDs and expiry reach `Hold` | `tool/kiff` `TestListsAndCallsToolsThroughTheGateway` (JSON, SSE) |
| a read result has no `Hold` | the same |
| another namespace's fields don't break a call | the same (`example.com/audit` with a numeric `hold_expires_at`) |
| a hold field in another shape keeps the call held | `TestAHoldStaysHeldWhenAFieldDoesntParse` |
| the loop copies operation, exception and expiry | `agent` `TestAHeldCallPausesTheRun` |

**Collecting a held call's outcome (#45).** `kiff.Collect` is
`Runner.Collect`: for each requirement it repeats the call as the model made
it, with its operation ID.

```json
{"name": "stripe_refund",
 "arguments": {"order": 42, "kiff_operation_id": "op-refund-42"}}
```

The gateway answers by what the owner decided. An approved call is sent once
and its result comes back:

```json
{"content": [{"type": "text", "text": "refunded re_1"}],
 "_meta": {"dev.kiff/call": {"state": "forwarded", "sent": "yes"}}}
```

A refused, expired or withdrawn one comes back refused. Repeating a call
already collected returns the same outcome without sending it again, so a
retry is safe. A call with no final outcome yet is `kiff.ErrNotFinal`, and
the runner puts the run back to PAUSED, its requirement intact and no model
turn taken. `Collect` reads the gateway's `"retry": "same_call"`, which today
it sets for these states:

```json
{"state": "held"}        the owner hasn't answered
{"state": "deciding"}    KIFF is still checking the call
{"state": "forwarding"}  KIFF is still sending it
```

```go
rn := &runner.Runner{Runs: runs, Sessions: sessions, Builder: builder, Collect: kiff.Collect}
r, err := rn.Execute(ctx, "t1", pausedRunID, emit) // collects, then continues
```

| Rule | Guarded by |
|---|---|
| each call is repeated with its operation ID and its own arguments | `tool/kiff` `TestCollectRepeatsEachCallByItsOperation` |
| an approved call's result and an expired hold's refusal come back in order | the same |
| `retry: same_call` (held, deciding, forwarding) is `ErrNotFinal`, and nothing is returned | `TestCollectLeavesACallWithNoOutcome` |
| a call made without `kiff_operation_id` is collected by KIFF's `tc-…` key | `TestACallMadeWithoutAnOperationIsCollectedByItsKey` |
| a call still being decided leaves the run paused, no model turn; collected later | `TestACallStillBeingDecidedLeavesTheRunPaused` |
| no operation ID, or a tool the agent lacks, is refused before the gateway | `TestCollectRefuses` |
| held, answered, collected and continued, end to end through the gateway | `TestAHeldRunContinuesThroughTheGateway` |

**A run is found by its hold, and KIFF's answer is recorded on it (#49).**
`Runs.ByHold(ctx, tenant, exception)` finds the run that paused on the call
KIFF holds as `exception`, for as long as the run exists: after it ended too.
`run.Answer` records what KIFF answered on that requirement:

```go
r, err := run.Answer(ctx, runs, "t1", "exc_123", "approved", time.Now())
```

```json
{ "call_id": "toolu_01", "tool": "stripe_refund", "operation": "op-refund-42",
  "exception": "exc_123", "expires": "2026-10-16T09:10:00Z",
  "answered": "2026-10-16T09:03:12Z", "answer": "approved" }
```

| Case | Result |
|---|---|
| first answer | recorded (`answered`, `answer`); the requirement is resolved |
| the same answer again, or another | nothing changes |
| the run is no longer paused (went on, ended, swept) | nothing changes, no error |
| no run has paused on that hold yet | `run.ErrNotFound`: the alert must come again |

The run keeps every hold it has paused on, over all its pauses, in
`holds` (`Change` only adds to it). DynamoDB keeps `H|tenant|exception → run
id` for each, written in the run's transaction; all of them take the run's
TTL when it ends, and go when it is deleted.

| Rule | Guarded by |
|---|---|
| found by hold, in its tenant only; gone with the run | `run/runtest` `ARunIsFoundByItsHold` (both stores) |
| an answer is recorded once; before the pause it's `ErrNotFound` | `AnAnswerIsRecordedOnce` |
| an ended or swept run is still found, and an answer changes nothing | `AnAnswerOutlivesTheRunsEnd` |
| a run paused twice is found by both holds; an answer on a running run changes nothing | `ARunPausedTwiceKeepsBothHolds` |
| every hold item, both pauses', takes the run's TTL and is deleted with it | `store/dynamodb` `TestHoldItems` |

**`POST /kiff/alerts`: the handler (#50).** `server/kiffalert.Handler` takes
KIFF's `hold_answered` alert, as KIFF's dispatcher sends it:

```http
POST /kiff/alerts
Content-Type: application/json
KIFF-Signature: t=1760605800,v1=626c49dffc5af165d8bf32513062d95528a2cf37b7904511c5e5bdedaa803211

{"schema_version":"1","tenant_id":"kiff-acct-1","outcome":"hold_answered",
 "hold":{"id":"exc_1","status":"approved"},"event_id":"evt_1","decided_at":"2026-10-16T09:03:12Z"}
```

`v1` is the hex HMAC-SHA256 of `"<t>.<body>"` with the alert target's secret;
`t` must be within 5 minutes. It records the answer (`run.Answer`), and when
the paused run waits for nothing more, calls `Continue`.

```go
h := &kiffalert.Handler{
	Accounts: accounts, // KIFF account -> tenant here, and the target's secret
	Runs:     runs,
	Continue: func(ctx context.Context, tenant, runID string) error { /* execute in the background */ },
}
mux.Handle("POST /kiff/alerts", h) // its own URL: KIFF can't sign with AWS
```

KIFF delivers again until it gets a 2xx:

| Response | When |
|---|---|
| 200 | answer recorded (or nothing to record it on: the run went on or ended; a repeat); another outcome, ignored |
| 404 | no run has paused on that hold yet, and `decided_at` is under 10 min old (`PauseWindow`): deliver again |
| 200 | no run has paused on it, and `decided_at` is older, missing or unreadable: not ours (another client on the account, a removed agent's run); logged |
| 401 | no signature, a wrong one, too old or new, an unknown account, an account with no secret; one body for all, `the alert doesn't check` |
| 503 | the account couldn't be read, or continuing the run failed (the answer stays recorded) |
| 400, 405, 413 | not a KIFF alert, not POST, body over 64 KB |

| Rule | Guarded by |
|---|---|
| each answer recorded; the run continues once all are, not before | `server/kiffalert` `TestAnswersAreRecordedAndTheRunContinuesOnceAllAre` |
| what doesn't check is 401, one body for all, and records nothing | `TestAnAlertThatDoesntCheckIsRefused` |
| KIFF's own signature checks, byte for byte | `TestKIFFsOwnSignatureChecks` (vector from `alert.SignatureHeader`) |
| an account with no secret is refused | `TestAnAccountWithNoSecretIsRefused` |
| 404 before the pause while recent; 200 once older or with no time, so KIFF never switches the target off; 200 for a run that went on | `TestTheStatusSaysWhetherKIFFShouldDeliverAgain` |
| a failed continue is 503, then continued on redelivery | `TestAFailedContinueIsDeliveredAgain` |

On Lambda it runs as its own function, `cmd/kiffalerts`, behind a URL with no
IAM auth that serves this route only. `Accounts` reads Secrets Manager. It
sets no `Continue`, so it only records the answer: the function anyone can
post to holds no power to execute anything, and the sweep continues the run
about a minute later (#13: [KIFF's alerts](13-server.md#kiffs-alerts)).
`TestWithoutContinueTheSweepContinues` covers the handler without one.

**The sweep continues a resolved run (#52).** A PAUSED run is due one
minute (`run.ResolvedGrace`) after its last requirement is answered or
expires. The sweep continues it then, in case no alert did: KIFF's expiry
alert was lost, or the alert's own continue failed.

```go
sw := run.Sweeper{Runs: runs, Ended: rn.Ended, Expires: keep,
	Continue: func(ctx context.Context, tenant, id string) error {
		return invokeAsync(ctx, tenant, id) // Execute in the background
	}}
n, err := sw.Sweep(ctx, time.Now(), 100) // marked or continued
```

```text
requirement c1  expires 09:10            c2  answered 09:04
run due         09:11  (latest resolution + 1 min)
sweep 09:12     retries 1, retry 09:14; Continue("t1", run)
still paused    looked at again 09:14, 09:18, 09:26 … then hourly
```

- Paused runs are a queue of their own (`Runs.DuePaused`). Each sweep reads
  up to `limit` RUNNING and PENDING runs first, then up to `limit` paused
  ones, so paused runs never hold up crash recovery.
- Before each continue the sweep stores `retries` and `retry`: the next look
  waits 2, 4, 8 … minutes, up to an hour (`run.MaxRetryWait`). That's about
  30 looks a day for a run that stays paused (KIFF says not final, its agent
  is gone). It backs off with `Continue` nil too. A new pause starts from
  zero; a pause after a failed collect keeps it.
- A requirement with no expiry and no answer keeps its run off the sweep:
  only an answer resolves it.
- A day after it resolved (`run.PausedGiveUp`), a run still paused is
  claimed by the sweep (`run.SweepClaim`), its recording is ended, and it is
  marked ERROR, `"it could not continue once KIFF resolved its held calls"`,
  or CANCELLED if it was asked to stop. The claim is a compare-and-set on
  the run the sweep read, so a continue that claimed it first wins, and its
  recording is never ended under it.

| Rule | Guarded by |
|---|---|
| due at the latest answer or expiry plus the grace, or at `retry` once later; never, with an open requirement that doesn't expire | `run` `TestDue` |
| the sweep continues a resolved paused run, then backs off (2, 4 … min); a failed continue is an error; a day later ERROR, recording ended | `runtest` `SweepContinuesAResolvedPausedRun` (memory, DynamoDB) |
| more resolved paused runs than `limit` don't stop a lost lease or an unclaimed run being marked; they back off with no `Continue` | `runtest` `PausedRunsNeverHoldUpCrashRecovery` |
| a continue that claims the run before the give-up wins; no recording ended | `runtest` `AGiveUpNeverEndsAContinuedRun` |
| a pause after a failed collect keeps the backoff; a new held call resets it | `runner` `TestANewPauseResetsTheSweepsBackoff` |
| an expired hold, swept: collected, continued, COMPLETED; KIFF down for a day: ERROR | `runner` `TestTheSweepContinuesARunWhoseHoldExpired` |

## Still to deliver

- **`POST /agents/{id}/runs/{run_id}/continue`**: a repeat of a continue
  already applied gets 200; any other continue on a run that isn't PAUSED
  gets 409. A held call is never collected twice.
- **Cancelling a paused run** withdraws its held calls in KIFF first, so an
  answer that comes later forwards nothing. The cancel route answers 409 for
  a paused run until then. Removing an agent does the same before deleting
  anything.
  - **The client is built** ([#51](https://github.com/kiff/agents/pull/51)):
    `kiff.Withdraw(ctx, hc, kiff.APIURL, key, exception)`.

    ```http
    POST https://api.kiff.dev/v1/me/holds/exc_123/withdraw
    Authorization: Bearer <the agent's KIFF key>
    ```

    | KIFF answers | `Withdraw` returns |
    |---|---|
    | 200 (and 200 again on a repeat) | `Withdrawal{Withdrawn: true, Status: "invalidated"}` |
    | 409 `already_answered`, with `hold.status` | `Withdrawal{Withdrawn: false, Status: "approved"}` (or refused, expired): what KIFF did |
    | 409 with another `error`, or no status | an error: KIFF didn't say what became of the call |
    | 404 | `kiff.ErrNoHold` |
    | anything else | an error, never carrying the key |

    Guarded by `tool/kiff` `TestWithdrawAHeldCall`,
    `TestACallAlreadyAnsweredIsNotWithdrawn`, `TestWithdrawFailures`.
  - **Left:** the cancel route calls it for each requirement's `exception`,
    then sets `Cancel`. KIFF then announces each hold `invalidated`; the
    alerts handler records it, the run is claimed again, collects (refused,
    `mandate_withdrawn`) and ends CANCELLED at its first checkpoint (#36,
    #38). A call KIFF already sent is recorded when collected.

## What KIFF now offers (from kiff/kiff-cloud#1245)

- **Withdraw:** `POST /v1/me/holds/{exception_id}/withdraw`, with the
  agent's own key. 200 for the agent's own held call, and again for a
  repeat (no second alert). An approved call not yet collected is withdrawn
  too, and nothing is sent. 409 `already_answered` for a call already sent,
  refused or expired. 404 for another agent's call. A withdrawn call stays
  withdrawn on retry.
- **Expiry is told as it happens.** KIFF sweeps every minute and tells the
  alert targets once per expired hold, as `hold_answered` with
  `hold.status` `expired`. A withdrawn hold is told the same way, with
  status `invalidated`. So `/kiff/alerts` sees one event kind, and reads the
  status.
- Holds expire after the Card's hold expiry: 10 minutes by default, 1 minute
  to 7 days. Expiry means refused.

## What's ready for it

| Needs | State |
|---|---|
| `Requirement` with `Operation`, `Exception`, `Expires`, `Answered` | Built; filled from the gateway (#44) |
| Requirements fail closed when they carry no expiry | Built (#32) |
| `Runner.Collect` hook, and pausing again when it fails | Built (#36) |
| A run cancelled while held ends CANCELLED once resolved | Built (#36, #38) |
| Withdraw and expiry alerts in KIFF | Shipped in KIFF (kiff/kiff-cloud#1245) |
| Signed HMAC alerts, alert targets | Exist in KIFF |

## Which IDs a requirement stores

Both (#44). The outcome is collected by `operation` (the gateway's
`kiff_operation_id`); alerts and withdraw go by `exception`
(kiff/kiff-cloud#1247). ARCHITECTURE.md says so.
