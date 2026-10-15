# The registry: what #16 delivers

**Issue:** [#16](https://github.com/kiff/agents/issues/16) · **Status:** mostly delivered. Definitions, building per request and the routes are built. Still to do: the Secrets Manager adapter, and withdrawing an agent's holds when it's removed.
**PRs:** #19; the routes came in #24 and #30 (under [#13](13-server.md))

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
| `store/memory`, `store/dynamodb` | `Agents` adapters. DynamoDB: `pk=G|tenant`, `sk` the agent ID, the definition as JSON, no TTL |

### A definition

| Field | What |
|---|---|
| `Instructions` | The system prompt, KIFF's skill included (at most 200 KB) |
| `Model`, `Price`, `MaxTokens` | The provider's model id and what it charges |
| `Gateway` | KIFF's gateway URL, https, on the server's list |
| `KeyRef` | A **reference** to the agent's KIFF key in the tenant's secrets. Never the key |
| `Keep` | The keep rule for every event kind its runs record |
| `MaxCost`, `MaxTurns` | What one run may spend, and how many model turns (0 is 50) |
| `History` | How many of the session's finished runs the model gets |

`Check` refuses a definition that can't be stored or run safely: a bad ID,
instructions too long, no cost cap, a gateway that isn't https, a keep policy
missing a kind.

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

## What holds, and the test that guards it

| Rule | Test |
|---|---|
| A definition that can't run safely is refused | `TestCheckRefuses`, `TestBuildRefusesWhatCantRun` |
| An unknown gateway is refused before the key is looked up | `TestBuildRefusesAnUnknownGatewayBeforeTheKey` |
| Build finds only the tenant's agent, and refuses another tenant's record | `TestBuildFindsOnlyTheTenantsAgent`, `TestBuildRefusesAnotherTenantsRecord` |
| Build makes the agent from its definition | `TestBuildMakesTheAgentFromItsDefinition` |
| Stores: put replaces, tenants stay apart, the largest definition fits, delete is safe to repeat, what's stored is a copy | `registrytest`: `PutReplaces`, `TenantsAreApart`, `TheLargestDefinitionFits`, `DeleteIsSafeToRepeat`, `StoredIsACopy`, and others |

**Benchmark:** `BenchmarkBuild`, the per-request build including the gateway
round trip.

## Not delivered yet

- **The Secrets Manager adapter** for `Secrets`, resolving a key reference
  only within its tenant. `cmd/lambda` can't build agents, and so can't run
  them, without it ([#13](13-server.md)).
- **Removing an agent withdraws its holds in KIFF** before deleting what's
  kept for it. `DELETE /agents/{id}` already deletes schedules, runs,
  sessions and notes; withdrawing is [#15](15-continue-on-hold.md)'s.
