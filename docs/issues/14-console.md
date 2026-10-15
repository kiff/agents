# The console: what #14 will deliver

**Issue:** [#14](https://github.com/kiff/agents/issues/14) · **Status:** not started. Nobody has taken it.
**PRs:** none

## What it's for

Step 3. Operators (the people building and running tafs, not a business
owner) need to see what agents do: every run, one run's timeline as it
happens, evals and cost. The owner's surface is the phone; this is the
operators'.

## What it will deliver

- **Operator routes**, read-only, across tenants: `GET /ops/runs`,
  `/ops/runs/{id}`, `/ops/runs/{id}/events` (SSE while it runs),
  `/ops/sessions/{id}`, `/ops/agents`, `/ops/agents/{id}/memories`,
  `/ops/schedules`, `/ops/evals`, `/ops/usage`. They are separate from the
  tenant-scoped routes tafs cloud calls, so the tenant rule there holds
  unchanged.
- **`console`** at `/console`, in Go, templ, htmx and plain CSS, with no
  Node. First pages:
  - **Runs:** every run, paused runs first, with what KIFF holds.
  - **A run:** its live timeline, with KIFF's decisions linked to
    app.kiff.dev, not copied.
  - **Evals.**
- **Stored eval runs.** From here on each eval case is an ordinary run in an
  eval session, and `GET /evals`, `GET /evals/{id}` and
  `GET /usage?agent_id=&from=&to=` (runs, tokens and cost per agent per day)
  join the API.

**Rules it keeps:** sign-in is KIFF's, limited to operators. The console
changes nothing; switching a schedule or an agent goes through tafs. It never
shows what the keep rules dropped.

## What's ready for it

| Needs | State |
|---|---|
| Runs with a status, requirements and cost | Built ([#12](12-runs.md)) |
| Sessions and events per run | Built ([#1](01-sessions.md), [#12](12-runs.md)) |
| Following a run's events live | Built for tenants (`resume`, [#13](13-server.md)); the `/ops` version reuses it |
| Grading a run of this runtime | Built (`eval.Load`, [#3](03-evals.md)) |
| Listing runs across tenants | Not built: the `Runs` port lists by agent, by source, and due runs only |
| Usage per agent per day | Not built: needs an aggregate the stores don't keep |
| Operator sign-in through KIFF | Not built |
