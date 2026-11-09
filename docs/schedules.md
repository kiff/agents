# Schedules

A schedule is when an agent runs by itself. Each firing starts an ordinary
background run, asked the schedule's `input`. A run that pauses on a KIFF hold
counts as paused, not failed. What is stored is the truth: a firing reads its
schedule as it is now, so a late or repeated firing never runs what was changed
or removed since.

Package: `schedule` (the type, its rules, the service, the store and scheduler
ports). The follow-up tool is `tool/followup`.

## A schedule, as stored

```json
{
  "tenant": "t1",
  "id": "refunds-morning",
  "agent": "rae",
  "kind": "cron",
  "cron": "0 9 * * 1-5",
  "time_zone": "Europe/Paris",
  "input": "Refund the late orders from yesterday.",
  "enabled": true,
  "missed": "within_1h",
  "busy": "skip"
}
```

## How it fires: `kind`

| `kind` | Fires | Fields |
|---|---|---|
| `cron` (the default) | as five-field cron says, in `time_zone` (IANA; empty is UTC). Daily, weekdays, chosen days, several times a day and every N minutes between set hours are all crons. | `cron`, `time_zone` |
| `once` | once, at `at`, to the minute; then it's gone | `at` |
| `monthly_last_day` | on the month's last day, at the time of its `cron`, written as a daily cron (`"M H * * *"`): cron can't say "the last day", so it fires daily and only the last day's firing runs | `cron`, `time_zone` |

A schedule fires at most every 15 minutes. The input is 1 to 32,000 bytes.
`Check` refuses anything else before it is stored, with the rule it broke.

## A firing that comes late, or while the agent works

- **`missed`**: a firing more than 5 minutes late was missed.
  - `within_1h` (the default) still runs it, once, if it is less than an hour
    late.
  - `skip` skips every missed firing.
- **`busy`**: a firing that comes while the agent is still at work (a run
  pending or running, whatever started it; one paused for its owner isn't).
  - `skip` (the default) skips it.
  - `latest` keeps it waiting, as the agent's one waiting firing. A later
    firing, from any of its schedules, replaces it. It runs once the agent is
    free, so a burst isn't lost, and two firings never run the agent side by
    side.

## Skipping coming firings: `skip`

`skip` lists coming firings the owner skipped, by their due time, at most 20.
Each starts nothing and is kept as skipped by the owner. A skip well past its
time is pruned when its schedule next fires.

## Checking an app first: `check`

A schedule with `check` makes each firing one read of a connected app first,
through the agent's own Card and without the model. The agent runs only when
the read returns items past the cursor, and those items go to the run as
untrusted data.

```json
"check": {"tool": "list_charges", "arguments": {"limit": 50},
          "items": "data", "cursor": "created", "id": "id"}
```

- `items` is the dotted path to the list in the read's result (empty: the
  result is the list); `cursor` the field compared with the last check's value
  (numbers as numbers, anything else as text); `id` names an item for good
  (`id` when empty).
- The first check only takes the cursor: nothing is new yet.
- One firing hands its run at most 50 new items, oldest first, as many as fit
  the run's input. The cursor moves only past items handed over; the rest come
  with the next check. An item too large for any run is counted, said in the
  firing's outcome, and not read again.
- **The cursor is the service's, like `failing`.** What the check kept
  (`last`, `due`, `partial`, `tries`) shows on every read, and a body that
  sets any of it is ignored.
- **An update keeps the cursor when the check still reads the same thing:**
  the same `tool`, `arguments` (as JSON, however written), `items`, `cursor`
  and `id`, for the same agent. So writing the same schedule again, or
  changing only its times or its input, never skips what came since the last
  check.
- **A check that reads something else starts anew:** an old cursor means
  nothing for another read, so its first firing only takes the cursor, as a
  new check's does.
- **A halted or switching agent's check reads nothing.** The firing is
  `skipped_halted` or `skipped_switching`, and the cursor stays, so the first
  firing afterwards hands over what came meanwhile.

## Follow-ups the agent sets itself

With the `follow_up` tool, an agent sets its own one-off follow-up ("check
this refund again tomorrow at 9"). The schedule service keeps the bounds,
never the model's words:

- a follow-up fires once, in the future, within 7 days;
- at most 3 wait for an agent at a time;
- a follow-up's own run can't set another;
- it checks nothing first, and it fires only under the agent version it was set
  under: a firing under another version starts nothing.

The tool changes only the agent's own schedules, so it doesn't go through
KIFF's gateway, and each follow-up's run works inside the same Card.

## What came of a firing

Each schedule keeps its last 100 firings, newest first: `due`, `at` (when it
came), `outcome`, and `run` when it started one. A firing is kept once by its
schedule and due time, so a repeated delivery replaces it.

| `outcome` | Meaning |
|---|---|
| `run` | it started `run` |
| `skipped_missed` | it came too late (`missed`) |
| `skipped_busy` | the agent was at work (`busy: skip`) |
| `waiting` | it waits for the agent to be free (`busy: latest`) |
| `skipped_replaced` | a later firing took its place while it waited |
| `skipped_dropped` | its schedule changed or went before the agent was free |
| `skipped_by_owner` | the owner skipped this one firing |
| `quiet` | its check found nothing new |
| `skipped_too_large` | its check found only items too large for a run |
| `failed_unworked` | it started `run`, which failed before any turn and did nothing |
| `failed_retrying` | its check's run did some work, then failed or was cut short, and changed nothing: the cursor stays, and the next firing hands the same items to a new run; `failing` stays as it was |
| `failed_unfinished` | its check's run didn't finish, and its items aren't handed again (a call of it may have changed something, a turn of it was declined, or three runs were handed them): the cursor moves past them, and it counts toward `failing` |
| `skipped_halted` | its agent, or the whole account, was halted: no run, and the firing isn't tried again; a check reads nothing and its cursor stays, so what came meanwhile goes to the first firing after the halt; `failing` stays as it was |
| `skipped_switching` | its agent was switching versions: no run, nothing used up, so a check's cursor stays and the next firing hands the same items to the new version; `failing` stays as it was |
| `failed_check` | its check couldn't read the app: no run, a check's cursor stays, and it counts toward `failing` |
| `skipped_credits` | the account's tafs wait for credits (runs.md): no run, and what it was handed waits, as a check's cursor stays, for the next firing that runs; `failing` stays as it was, and `waited` says why |

A schedule keeps its last firing that waited for credits, kept by the service
and shown on every read: `waited: {due, why}`, why being KIFF's code
(`credits_used_up`, `limit_reached` or `no_plan`). A run that starts unsets it,
as it unsets `failing`. A once schedule (an agent's follow-up) is off after its
firing, as always: its work doesn't run again, and its `waited` says so. A body
that sets `waited` is ignored, and an update keeps it.

## A run that does nothing

A firing's run can fail before its first turn: the model refuses its first
call (for want of credits, say), or the run stops before its loop. It did
nothing, so the firing used up nothing:
- **its outcome is `failed_unworked`,** with its `run`;
- **a check's cursor stays where it was,** so the next firing hands the same
  items to a new run, with anything new since;
- **the schedule's `failing` says so,** kept by the service and shown on every
  read: `{since, firings, run, outcome, last}`, the first such firing's due
  time, how many in a row, and the last one's run, outcome and due time. A run
  that works ends it. A body that sets `failing` is ignored, and an update
  keeps it.

A check that couldn't read the app is the same kind of failure, one step
earlier: its outcome is `failed_check`, with no `run`, its cursor stays, and it
counts in `failing` as a run that did nothing does (`run` empty). A read that
fails because the agent was halted or was switching versions is that skip
instead (`skipped_halted`, `skipped_switching`), and counts nothing.

A run counts as doing nothing only when the runner itself stored its error
(it ended the run, or stopped it before its loop) with no turn done, and the
sweep never carried it on. A run whose invocation died is ended by the sweep,
and its turns may be behind what it did, so it never counts.

A repeated delivery of a firing whose run did nothing does nothing either,
says the same, and counts once in `failing`, and so does a late delivery of an
older firing that comes after a newer one was counted.

## A check's run that doesn't finish

A check's cursor moves only past items a run finished. A run finished what it
was handed when it ended with an answer. A run paused for its owner holds its
items and carries on from there. A run its owner stopped isn't started again,
and neither is one a halt stopped partway: a halt that lands during a run
drops that run's items (the cursor stays only for a firing that hadn't started
its run).

A run **didn't finish** when it did some work and then failed, or was cut
short: by its turn cap, by its cost cap, or by its reply running out of room
(a reply cut off before its next call looks just like that). What happens to
its items depends on one thing: whether anything it did can have changed
something outside it.

- **It changed nothing: the items go again.**
  - The firing's outcome is `failed_retrying`, with its `run`. The cursor
    stays, and `failing` stays as it was.
  - The next firing hands the same items to a new run, with anything new
    since. That run's input says an earlier run didn't finish them, and to
    check in the app what was already done for each before acting.
  - **Three runs at most** (`check.tries` counts them). After the third, the
    cursor moves past the items, the outcome is `failed_unfinished`, and it
    counts toward `failing`. `tries` stays at 3 until the check hands a run
    items again, so a batch no run can finish never costs a run at every
    firing for good.
- **It may have changed something: nothing is handed again, so nothing is
  done twice.** This is the rule for anything that moved money.
  - The outcome is `failed_unfinished`, with its `run`. The cursor moves past
    the items, as it always did.
  - It counts toward `failing`, so whoever shows the schedule tells the owner.
    The run's own record says what it did (`done`).
- **A turn of it was declined** (a safety classifier): the same as above. A
  new run with the same items would only be declined again, so they aren't
  handed again, the cursor moves, and it counts toward `failing`.

**"May have changed something" is decided from KIFF's word on each call that
acts, never from the call's error mark.** KIFF marks its own answers as
errors, among them a call it sent whose answer was lost, and that call may
have paid or refunded. A run keeps the count in its record (`run.Run.Sent`;
not in the API's view of a run):

| A call that acts | Counts as sent |
|---|---|
| went through | yes |
| sent, the answer lost (`unknown`), or not in yet (`forwarding`, `sending`) | yes |
| still being decided (`deciding`): not sent yet, and another attempt of the same call may still send it | yes |
| sent, and the vendor failed it | yes |
| any state this runtime doesn't know, or none (a tool outside KIFF) | yes |
| held for the owner, refused, or failed before sending | no |
| a trial's, or one refused back to the model unmade | no |

A read never counts. What a read is comes from the tool's mark, as its
connection gives it. A run the sweep carried on or ended counts as having
changed something, whatever it kept: its turns may be behind what it did.

A plain schedule hands its run nothing to give back. Its run that started and
worked is `run`, however it ended, as before.

A repeated delivery of a firing whose run didn't finish says what the first
delivery said, and counts once, in `tries` and in `failing`.

**A `once` schedule is off after its firing, whatever came of it,** this one
included. An agent's own follow-ups are `once` schedules. A follow-up whose run
did nothing doesn't run again, and its `failing` stays to say so (`firings`
1). What the owner changed while the run went is kept. One they moved to
another time meanwhile stays on, for that time. Trying a once schedule's work
again, later, isn't done today.

The service can't reach the owner: whoever shows the schedule tells them,
once for each run of failures, and at once for a `once` schedule, whose work
won't run (kiff/tafs alerts on `failing`).

## What a run is told of the last one

Each firing's run gets one more line after its input, from the runtime and not
from the agent's notes: when its schedule's last run that did any work ended,
and how (it finished, failed, was stopped by its owner, or was cut short by
its turn or cost limit or by running out of room), with the start of its last
answer (up to 400 bytes), marked as the run's own words, not the runtime's. A run that did work is the newest one of the
schedule's last 10, other than this one, that did at least one turn and has
ended; a run paused or still going isn't one. With none, the line says the
schedule has no earlier run that did any work. An agent that keeps notes on
its progress can check them against it.

The scheduler may deliver a firing more than once. Every delivery of one
firing carries the same key, so it starts one run however often it comes.

## Routes

| Route | What |
|---|---|
| `POST /schedules` | store a schedule and set it to fire; its agent must exist |
| `GET /schedules`, `GET /schedules/{id}` | the tenant's schedules, one schedule |
| `PUT /schedules/{id}` | change it; safe to repeat if setting it to fire failed |
| `DELETE /schedules/{id}` | remove it |
| `POST /schedules/{id}/enable`, `/disable` | turn it on or off |
| `GET /schedules/{id}/runs` | the runs it started |
| `GET /schedules/{id}/firings` | its firings and what came of each |
| `POST /schedules/{id}/trigger` | fire it by hand, where runs can be executed |

## How tafs uses them

A taf's dial in [tafs](https://github.com/kiff/tafs) writes its When ring as
schedules here: daily, weekdays, weekly, several times a day and every N
minutes become `cron`; once becomes `once`; the month's last day becomes
`monthly_last_day`, all in the account's time zone. Its If busy stop is `busy`
(`skip` or `latest`), an app check is `check`, and a follow-up the taf set
itself shows on the dial until it fires. A dial change becomes schedules only
when the owner makes that version live. Pausing a taf turns off every one of
its schedules, its follow-ups included.
