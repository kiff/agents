# Schedules: what #17 delivers

**Issue:** [#17](https://github.com/kiff/agents/issues/17) · **Status:** delivered up to triggers. Schedules, EventBridge Scheduler, the routes, firings starting runs and firing by hand are built. Triggers (work starting runs) are still to come.
**PRs:** #21 (core), #22 (EventBridge adapter); the routes came in #24 and #30 (under [#13](13-server.md)); a run's source in #26 (under [#12](12-runs.md))

## What it's for

Step 6. A taf runs by itself on a schedule, as tafs does today, and later
when work arrives (a Stripe event, an e-invoice, an answered hold). Each
firing is an ordinary run.

## What it delivers

| Package | What |
|---|---|
| `schedule` | `Cron`, `Schedule` and its `Check`; ports `Schedules` (stored) and `Scheduler` (what fires them); `Service`, which keeps the two in step; `Firing` |
| `schedule/scheduletest` | The checks every `Schedules` store must pass |
| `schedule/eventbridge` | The `Scheduler` on EventBridge Scheduler, and `ParseEvent` for its firings |
| `store/memory`, `store/dynamodb` | `Schedules` adapters. DynamoDB: `pk=C|tenant`, `sk` the schedule ID, no TTL |

### A schedule

- **Cron:** five-field POSIX, as tafs uses: `*`, lists, ranges and steps; 0
  or 7 is Sunday. One of the two day fields must be `*`, because POSIX fires
  on days that match either, and EventBridge can't express "either".
- **Time zone:** IANA, empty means UTC. The zone database is embedded, so it
  loads the same on a bare Lambda image.
- **Input** (at most 32 KB) and **on or off**.
- **At most every 15 minutes** (`MinGap`, the same rule as tafs'
  `calmEnough`).
- `Cron.Next(t, loc)` gives the next firing in the schedule's zone, which the
  app shows and EventBridge doesn't report. A local time a DST change skips
  doesn't fire.

### Keeping the store and EventBridge in step

- **Put** checks the schedule, refuses it unless its agent is live, stores
  it, then sets it to fire. If setting fails, the stored copy stays (it is
  the truth) and a retry sets it.
- **Delete** stops the schedule first, then forgets it. **DeleteAgent** does
  that for every schedule of an agent being removed. Both are safe to repeat.
- **Fire** reads the schedule as it is now. A schedule removed or switched
  off, or whose agent is gone, starts nothing, and that isn't an error, so
  EventBridge doesn't retry. A firing that disagrees with the store heals
  EventBridge.
- **`Firing.Key()`** depends only on the tenant, the schedule and when the
  firing was due. EventBridge delivers at least once, so starting one run
  per key makes a repeated firing start one run.

### On EventBridge Scheduler

- One EventBridge schedule per schedule, in one group, invoking the function
  with a role. The group, the function and the role are configuration; no
  ARN is in code or tests.
- **Nothing but timing reaches EventBridge.** The name is a hash of tenant
  and ID (`s-` and 40 hex characters). The target's input is only
  `{"tenant", "schedule", "due"}`; the schedule's own input is read from the
  store when it fires.
- A retry policy of 3 attempts within an hour.

### Routes

`POST /schedules`, `GET /schedules?agent_id=`, `GET`/`PUT`/`DELETE
/schedules/{id}`, `POST /schedules/{id}/enable` and `/disable`, and
`GET /schedules/{id}/runs` (the runs it started, newest first), and
`POST /schedules/{id}/trigger` (fire it now, by hand, as a background run).
See [#13](13-server.md).

### A firing starts a run

On Lambda, a firing starts an ordinary run of the schedule's agent with its
input (`Trigger: run.BySchedule`, `Source` the schedule's ID) and executes it
in the firing's own invocation. The run's ID is `Firing.Key()`, so however
often EventBridge delivers a firing, it starts one run, and runs it once. A
run that pauses counts as paused, not failed. How: [#13](13-server.md).

## What holds, and the test that guards it

| Rule | Test |
|---|---|
| A cron EventBridge can't run exactly is refused | `TestParseCronRefuses`, `TestCheckRefuses` |
| The EventBridge expression and the next firing match the cron, in its zone | `TestEventBridge`, `TestNext`, `TestNextMatchesEveryMinute`, `TestNextIsInTheSchedulesZoneAndOnlyWhenOn` |
| The 15-minute rule counts the gap across the hour | `TestMinGap` |
| Put stores, then sets; a failed set can be retried; a bad schedule is refused before it's stored | `TestPutStoresThenSets`, `TestPutThatFailsToSetCanBeRetried`, `TestPutRefusesBeforeStoring` |
| Delete stops, then forgets; DeleteAgent deletes only that agent's | `TestDeleteStopsThenForgets`, `TestDeleteAgentDeletesOnlyItsSchedules` |
| A firing starts the schedule as it is now, heals EventBridge, and leaves other tenants alone | `TestFireStartsTheScheduleAsItIsNow`, `TestFireHealsWhatTheSchedulerGotWrong`, `TestFireLeavesOtherTenantsAlone`, `TestFireReportsWhatShouldBeRetried` |
| A firing's key depends only on tenant, schedule and due time | `TestFiringKey` |
| The EventBridge name fits and says nothing; set and remove are safe to race and repeat | `TestNameFitsEventBridgeAndSaysNothing`, `TestSetThatRacesACreateReplaces`, `TestRemoveIsSafeToRepeat` |
| A firing reads back exactly | `TestParseEvent` |
| A firing starts one run of the schedule through `cmd/lambda`, named by its key, and runs it once however often it's delivered | `TestFiringsReachTheSchedule`, `TestARepeatedFiringRunsWhatTheFirstLeftPending` |
| A schedule fired by hand is a background run tagged with it | `TestTriggerASchedule` |

## Not delivered yet

- **The deploy:** the schedule group, and the IAM role that lets
  `scheduler.amazonaws.com` invoke the function only.
- **Triggers:** work (a Stripe event, an e-invoice, an answered hold) starts
  or continues a run the same way, and the clock becomes the safety net.
