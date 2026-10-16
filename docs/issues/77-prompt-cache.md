# Prompt caching and a context report: what #77 delivers

**Issue:** [#77](https://github.com/kiff/agents/issues/77) · **Status:** in progress (caching delivered; the context report and trimming are next).
**PRs:** #80

## What it's for

Every turn of a run resends the system prompt, the tool definitions and the
conversation so far. Without `cache_control`, each turn paid full input
price for all of it.

## Delivered

**Caching, when the price counts it.** `model.Request.Cache` asks the
adapter to cache. The agent sets it when its price has both cache prices
(`model.Price.Caches()`). A price without them caches nothing, so the cost
cap never counts cache tokens as free. Existing definitions keep working
unchanged; they start caching once their price has cache prices.

```json
PUT /agents/rae
{ "model": "claude-opus-5-5",
  "price": { "Input": 4000000, "Output": 20000000, "CacheRead": 200000, "CacheWrite": 5000000 }, … }
```

**Two breakpoints.** The prompt renders tools, then system, then messages;
a breakpoint caches everything before it.

- the system prompt's block (the last tool's when there's no system prompt):
  the prefix every turn shares;
- the last block of the last message that can take one (thinking blocks
  can't): the conversation, which the next turn reads back and extends.

```json
POST /v1/messages
{ "tools": [ … ],
  "system": [ { "type": "text", "text": "…", "cache_control": { "type": "ephemeral" } } ],
  "messages": [ …,
    { "role": "user", "content": [
      { "type": "tool_result", "tool_use_id": "toolu_1", "content": "…",
        "cache_control": { "type": "ephemeral" } } ] } ] }
```

The judge in `eval` doesn't cache: it asks once per run, so a cache write
would only cost more.

**Measured** on `claude-opus-5-5` (`TestLiveCache`, a ~9k-token prompt):

| Turn | input | cache write | cache read |
|---|---:|---:|---:|
| 1 | 4 | 9,085 | 0 |
| 2 | 2 | 84 | 9,085 |

```sh
ANTHROPIC_API_KEY=… AGENTS_LIVE_MODEL=claude-opus-5-5 go test -run LiveCache ./model/anthropic/
```

**Prices on the command line.** `cmd/chat` and `cmd/eval -run` take
`-price in,out` or `-price in,out,cache-read,cache-write`. Only the second
caches. Opus 5.5:

```sh
eval -run -model claude-opus-5-5 -price 4000000,20000000,200000,5000000 …
```

## Next

- A context report per turn: what the input was made of (system, tools,
  skills, messages), what the cache served, the room left. Recorded with the
  run's usage, shown in `/ops`, read by the evals.
- Trimming old tool results past a threshold, measured in the evals first.
- Chargebacks before and after, same score, lower cost.
