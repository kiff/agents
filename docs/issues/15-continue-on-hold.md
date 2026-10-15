# Continue on a hold: what #15 will deliver

**Issue:** [#15](https://github.com/kiff/agents/issues/15) · **Status:** not started. Nobody has taken it. Its outside dependency, [kiff/kiff-cloud#1245](https://github.com/kiff/kiff-cloud/issues/1245), **has shipped**, so nothing outside this repo blocks it now.
**PRs:** none

## What it's for

Step 4. When KIFF holds one of a run's calls, the run pauses
([#12](12-runs.md)). This step lets it continue once the owner answers on the
phone, or once the hold expires, and lets a cancel withdraw what KIFF still
holds. Until it lands, a paused run waits and never continues, and tafs
can't switch over ([kiff/tafs#42](https://github.com/kiff/tafs/issues/42)).

## What it will deliver

- **`tool/kiff` records KIFF's IDs and the hold's expiry for each call.**
  Today a result keeps only KIFF's `state` (`held`, refused). Each
  requirement needs:
  - the ID that withdraws the hold: KIFF's `exception_id`, which the gateway
    already puts in a held result's `_meta` (`dev.kiff/call`);
  - the hold's expiry, `hold_expires_at`, from the same `_meta`, into
    `Requirement.Expires`;
  - the ID the call's outcome is collected by (`Requirement.Operation`).
- **`Runner.Collect`**: one outcome per requirement, none still held. An
  approved call is forwarded by KIFF once; a refused or expired one comes
  back refused. The runner already calls it
  (`func(ctx, agent.Agent, []run.Requirement) ([]agent.ToolResultBody, error)`)
  and already handles its failure by pausing again.
- **`POST /kiff/alerts`**: verifies KIFF's HMAC signature, records the answer
  on the requirement, and answers 200 once it's recorded, whatever the run's
  state, so KIFF stops redelivering. When every requirement is resolved, the
  run continues in the background under the same ID. KIFF can't sign with
  AWS, so this needs its own URL, not the IAM function URL
  ([#13](13-server.md)).
- **`POST /agents/{id}/runs/{run_id}/continue`**: a repeat of a continue
  already applied gets 200; any other continue on a run that isn't PAUSED
  gets 409. A held call is never collected twice.
- **Expiry in the sweep:** a requirement whose hold expired with no answer
  recorded is asked of KIFF, and the run continues with the outcome.
- **Cancelling a paused run** withdraws its held calls in KIFF first, so an
  answer that comes later forwards nothing. The cancel route answers 409 for
  a paused run until then. Removing an agent does the same before deleting
  anything.

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
| `Requirement` with `Operation`, `Expires`, `Answered` | Built; `Operation` and `Expires` are empty until this step |
| Requirements fail closed when they carry no expiry | Built (#32) |
| `Runner.Collect` hook, and pausing again when it fails | Built (#36) |
| A run cancelled while held ends CANCELLED once resolved | Built (#36, #38) |
| Withdraw and expiry alerts in KIFF | Shipped in KIFF (kiff/kiff-cloud#1245) |
| Signed HMAC alerts, alert targets | Exist in KIFF |

## Open question for whoever takes it

ARCHITECTURE.md says outcomes are collected and holds withdrawn "by
operation ID". KIFF's withdraw goes by `exception_id`. Decide whether a
requirement stores both, or whether `Operation` holds the exception ID, and
update ARCHITECTURE.md to match.
