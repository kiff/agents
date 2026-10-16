# The registry

An agent is stored here, not written as code: a definition per tenant, built
afresh for each request with its own KIFF key. The key is never stored with
the definition; the definition names it, and `Secrets` looks it up within
the tenant.

Packages: `registry` (the definition, `Build`, the ports `Agents` and
`Secrets`), `registry/secretsmanager` (`Secrets` on AWS Secrets Manager),
`store/memory` and `store/dynamodb` (`Agents`).

## A definition, as stored

`PUT /agents/rae` for tenant `t1` stores this (JSON; `keep` shortened to two
of its eight kinds):

```json
{
  "tenant": "t1",
  "id": "rae",
  "instructions": "You refund orders. Every write goes through KIFF.",
  "model": "claude-opus-5-5",
  "price": { "Input": 4000000, "Output": 20000000, "CacheRead": 200000, "CacheWrite": 5000000 },
  "max_tokens": 8192,
  "effort": "high",
  "gateway": "https://mcp.kiff.dev/mcp",
  "key_ref": "kiff/rae",
  "keep": {
    "owner_message": { "Keep": 2, "Retention": 2592000000000000 },
    "tool_result": { "Keep": 1, "Retention": 0 }
  },
  "max_cost_micro_usd": 500000,
  "history": 20
}
```

- `price` is per million tokens, in millionths of a dollar. With both cache
  prices set, every turn uses the prompt cache; without them it doesn't, so
  the cost cap never counts cache tokens as free (#77). `CacheWrite` is the
  5-minute write price (1.25× input on Claude).
- `tenant` and `id` come from the verified caller and the path; a body that
  says otherwise is refused.
- `gateway` must be on the server's own list (`Server.Gateways`), so a
  definition can't send its key anywhere else.
- `key_ref` names the key; it is never the key.
- `keep` has a rule for every event kind (`session.DefaultPolicy` gives one).
  `Retention` is in nanoseconds: 30 days here.
- `Check` refuses what can't run safely: a bad ID, no cost cap, a gateway
  that isn't https, a keep policy missing a kind, more than `MaxSize` bytes
  as stored.

`DELETE /agents/rae` removes its schedules, runs, sessions and notes, then
the definition, so a retry after a failure carries on.

## Building an agent

```go
b := &registry.Builder{
	Agents:   agents,
	Secrets:  secretsmanager.New(sm.NewFromConfig(cfg), "agents/"),
	Model:    anthropic.New(),
	Gateways: []string{kiff.URL},
	Connect: func(ctx context.Context, gateway, key string) ([]tool.Tool, error) {
		return kiff.New(gateway, key, nil).Tools(ctx)
	},
}
a, def, err := b.Build(ctx, "t1", "rae")
```

`Build`, once per request:

1. reads t1's definition of `rae`, and refuses a record of another tenant or
   ID;
2. refuses a gateway not on `Gateways`, **before** looking up any key;
3. looks up the key with `Secrets.Key(ctx, "t1", "kiff/rae")`;
4. lists the gateway's tools with that key, so a run sees what its Cards
   reach today;
5. returns the agent, checked, and the definition (its keep rules and history
   size are the runner's to apply).

## Keys on Secrets Manager

A key is the secret named prefix + tenant + `/` + its reference:

| Prefix | Tenant | `key_ref` | Secret |
|---|---|---|---|
| `agents/` | `t1` | `kiff/rae` | `agents/t1/kiff/rae` |
| `agents/` | `t2` | `kiff/rae` | `agents/t2/kiff/rae` |

```go
secrets := secretsmanager.New(client, "agents/")
key, err := secrets.Key(ctx, "t1", "kiff/rae") // the string in agents/t1/kiff/rae
```

- **A reference stays in its tenant.** The tenant is one name segment and the
  reference a path of them, each of letters, digits and `_+=.@-`, none empty,
  `.` or `..`. The name always starts with the tenant's own segment, so
  `../t1/kiff/rae` from t2 is refused, and so is a tenant `t1/kiff`. Nothing
  refused reaches Secrets Manager.
- **Only the secret asked for is a key.** Secrets Manager may read a name
  ending in `-` and six characters as part of an ARN and answer with another
  secret; an answer under another name is `secretsmanager.ErrNotFound`.
- **A missing secret** is `secretsmanager.ErrNotFound`. An empty or binary
  secret is refused. The secret's string is the key, spaces trimmed.

To give t1's agent `rae` its key, store the KIFF agent key as a plain string
secret named `<prefix>t1/kiff/rae`, and give the function's role
`secretsmanager:GetSecretValue` on secrets under the prefix only. The prefix
is configuration; it is never a constant in code.

Runnable: [`registry/secretsmanager/example_test.go`](../registry/secretsmanager/example_test.go).
