# The registry: what #16 delivers

**Issue:** [#16](https://github.com/kiff/agents/issues/16) · **Status:** mostly delivered. Definitions, building per request, the routes and the Secrets Manager adapter are built. Still to do: withdrawing an agent's holds when it's removed.
**PRs:** #19; the routes came in #24 and #30 (under [#13](13-server.md)); the Secrets Manager adapter in this page's PR. By example: [docs/registry.md](../registry.md)

## What it's for

Step 5. A taf is an agent stored here, not code. Each request builds it
afresh for the verified tenant, with its own KIFF key, so one tenant's agent
can never run with another's key, and a key is never stored with the
definition.

## What it delivers

| Package | What |
|---|---|
| `registry` | `Definition` and its `Check`; port `Agents`; port `Secrets`; `Builder.Build` |
| `registry/registrytest` | The checks every `Agents` store must pass |
| `registry/secretsmanager` | `Secrets` on AWS Secrets Manager: a key is the secret named prefix + tenant + `/` + its reference |
| `store/memory`, `store/dynamodb` | `Agents` adapters. DynamoDB: `pk=G|tenant`, `sk` the agent ID, the definition as JSON, no TTL |

### A definition

| Field | What |
|---|---|
| `Instructions` | The system prompt, KIFF's skill included |
| `Model`, `Price`, `MaxTokens` | The provider's model id and what it charges |
| `Gateway` | KIFF's gateway URL, https, on the server's list |
| `KeyRef` | A **reference** to the agent's KIFF key in the tenant's secrets. Never the key |
| `Keep` | The keep rule for every event kind its runs record |
| `MaxCost`, `MaxTurns` | What one run may spend, and how many model turns (0 is 50) |
| `History` | How many of the session's finished runs the model gets |

`Check` refuses a definition that can't be stored or run safely: a bad ID,
no cost cap, a gateway that isn't https, a keep policy missing a kind, or a
definition over `MaxSize` (300,000 bytes) **as stored**. The bound is on the
whole encoded definition (`Encode`: every field, JSON escaping included), not
on the instructions alone, so it always fits one DynamoDB item. Long
instructions are what usually reach it.

### Building an agent

```go
a, def, err := builder.Build(ctx, tenant, agentID)
```

`Build` reads the tenant's definition, refuses a gateway that isn't on
`Builder.Gateways`, resolves the key through `Secrets`, lists the gateway's
tools with that key, adds the agent's own tools (its memory), and runs
`agent.Check`. It returns the definition too, since the runner applies its
keep rules and history size.

### Two guards on the key

- **The gateway must be on the server's list, checked before the key is
  looked up.** Otherwise a definition could point at any server, which would
  get the agent's KIFF key and count as "the gateway", write tools included.
- **A key reference resolves only within its tenant.** That's the `Secrets`
  contract. `Build` also refuses a record whose tenant or ID differs from
  what was asked, before looking up any key.

### Keys on Secrets Manager

```go
secrets := secretsmanager.New(client, "agents/")
key, err := secrets.Key(ctx, "t1", "kiff/rae") // the secret "agents/t1/kiff/rae"
```

- **The tenant is one name segment, the reference a path of them.** Each
  segment is letters, digits and `_+=.@-`; no segment is empty, `.` or `..`.
  So the name always starts with the tenant's own segment: no reference
  reaches another tenant's key, and a tenant such as `t1/kiff` can't pass
  as `t1`. Anything else is refused before Secrets Manager is asked.
- **Only the secret asked for is a key.** Secrets Manager may read a name
  ending in `-` and six characters as part of an ARN and answer with another
  secret; an answer whose name differs is `ErrNotFound`.
- A missing secret is `ErrNotFound`. An empty or binary secret is refused.
  The prefix is configuration, never a constant.

## What holds, and the test that guards it

| Rule | Test |
|---|---|
| A definition that can't run safely is refused | `TestCheckRefuses`, `TestBuildRefusesWhatCantRun` |
| An unknown gateway is refused before the key is looked up | `TestBuildRefusesAnUnknownGatewayBeforeTheKey` |
| Build finds only the tenant's agent, and refuses another tenant's record | `TestBuildFindsOnlyTheTenantsAgent`, `TestBuildRefusesAnotherTenantsRecord` |
| Build makes the agent from its definition | `TestBuildMakesTheAgentFromItsDefinition` |
| A key reference resolves only within its tenant: same reference, another tenant, another secret | `TestAKeyIsTheTenantsOwn` (`registry/secretsmanager`) |
| No tenant or reference names a secret outside the tenant's own, and none reaches Secrets Manager | `TestNoReferenceLeavesItsTenant` |
| Only the secret asked for is a key (a name read as part of an ARN) | `TestOnlyTheSecretAskedForIsAKey` |
| An empty or binary secret is refused; a failed read is reported, not taken as missing | `TestASecretWithoutAKeyIsRefused`, `TestAFailedReadIsReported` |
| Stores: put replaces, tenants stay apart, the largest definition fits, delete is safe to repeat, what's stored is a copy | `registrytest`: `PutReplaces`, `TenantsAreApart`, `TheLargestDefinitionFits`, `DeleteIsSafeToRepeat`, `StoredIsACopy`, and others |

**Benchmark:** `BenchmarkBuild`, the per-request build including the gateway
round trip.

## Not delivered yet

- **`cmd/lambda` builds agents with the adapter.** It's wired with the
  runner there ([#13](13-server.md)): `secretsmanager.New` on the function's
  Secrets Manager client, with the name prefix from its environment. The
  function's role reads only secrets under that prefix.
- **Removing an agent withdraws its holds in KIFF** before deleting what's
  kept for it. `DELETE /agents/{id}` already deletes schedules, runs,
  sessions and notes; withdrawing is [#15](15-continue-on-hold.md)'s.
