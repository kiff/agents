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
| `skipped_halted` | its agent, or the whole account, was halted: no run, and nothing from before a halt runs, so a check's cursor moves past what it found; `failing` stays as it was |
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
  read: `{since, firings, run, last}`, the first such firing's due time, how
  many in a row, and the last one's run and due time. A run that works ends it.
  A body that sets `failing` is ignored, and an update keeps it.

A run counts as doing nothing only when the runner itself stored its error
(it ended the run, or stopped it before its loop) with no turn done, and the
sweep never carried it on. A run whose invocation died is ended by the sweep,
and its turns may be behind what it did, so it never counts.

A run that failed after it acted keeps today's rule: what it was handed is
used up, so nothing is done twice. A repeated delivery of a firing whose run
did nothing does nothing either, says the same, and counts once in `failing`,
and so does a late delivery of an older firing that comes after a newer one
was counted.

**A `once` schedule is off after its firing, whatever came of it,** this one
included. An agent's own follow-ups are `once` schedules. A follow-up whose run
did nothing doesn't run again, and its `failing` stays to say so (`firings`
1). What the owner changed while the run went is kept. One they moved to
another time meanwhile stays on, for that time. Trying a once schedule's work
again, later, isn't done today.

The service can't reach the owner: whoever shows the schedule tells them,
once for each run of failures, and at once for a `once` schedule, whose work
won't run (kiff/tafs alerts on `failing`).

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
