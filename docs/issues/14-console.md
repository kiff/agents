# The console: what #14 delivers

**Issue:** [#14](https://github.com/kiff/agents/issues/14) · **Status:** in progress. The operators' run routes are built; the rest of `/ops`, the console pages, stored eval runs and operator sign-in are to come.
**PRs:** #65 (runs across tenants, the `/ops` run routes)

## What it's for

Step 3. Operators (the people building and running tafs, not a business
owner) need to see what agents do: every run, one run's timeline as it
happens, evals and cost. The owner's surface is the phone; this is the
operators'.

## Delivered

**Operators read runs across tenants (#65).** Served only when
`Server.Operator` is set: it verifies an operator, and the routes never take a
tenant's caller (a tenant's request is 401 there). They change nothing.

```go
srv := &server.Server{ /* … */
	Operator: func(r *http.Request) error { /* KIFF's sign-in, operators only */ },
}
```

```http
GET /ops/runs?limit=20             every run, newest first
GET /ops/runs/paused?limit=20      PAUSED runs, newest first, with what KIFF holds
GET /ops/tenants/t1/runs/5c4c…     one run
GET /ops/tenants/t1/runs/5c4c…/events

200 {"runs": [{"tenant": "t1", "id": "5c4c…", "agent": "rae", "status": "PAUSED",
               "requirements": [{"call_id": "toolu_01", "tool": "stripe_refund",
                                 "exception": "exc_123", "expires": "…"}], …}]}
```

- A run is named by its tenant in the path: run IDs are a tenant's own.
  (ARCHITECTURE.md planned `/ops/runs/{id}`.)
- A run shows what the tenant routes show, plus `tenant`; never its paused
  turn, claim, lease or version. Events are the run's as kept: what the keep
  rules dropped is gone.
- `Runs.ListAll` and `Runs.ListPaused` are the port's two lists across
  tenants, for these routes only. DynamoDB keeps them as partitions `A` (every
  run, with the run's TTL) and `PA` (written when a run pauses, deleted when
  it's claimed again or deleted; an entry left behind is never listed).

| Rule | Guarded by |
|---|---|
| all runs newest first, across tenants; paused ones only while paused; deleted and expired runs gone | `runtest` `OperatorsListAcrossTenants` (memory, DynamoDB) |
| the paused entry goes on a claim and on delete; one left behind lists nothing | `store/dynamodb` `TestPausedListItems` |
| operators list across tenants, paused apart, with a bounded `limit`; no one else, a tenant's caller included, gets in | `server` `TestOperatorsReadRunsAcrossTenants` |
| one run with its tenant, never its paused turn or claim; its kept events; another tenant's path is 404 | `TestOperatorsReadARun` |
| no `Operator`, no `/ops` | `TestNoOperatorNoOps` |

## Still to deliver

- **The rest of the operator routes**, read-only, across tenants: a run's
  events as SSE while it runs, `/ops/sessions/{id}`, `/ops/agents`,
  `/ops/agents/{id}/memories`, `/ops/schedules`, `/ops/evals`, `/ops/usage`. They are separate from the
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
| Listing runs across tenants | Built: `Runs.ListAll`, `Runs.ListPaused` (#65) |
| Usage per agent per day | Not built: needs an aggregate the stores don't keep |
| Operator sign-in through KIFF | Not built |
