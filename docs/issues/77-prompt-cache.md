# Prompt caching and a context report: what #77 delivers

**Issue:** [#77](https://github.com/kiff/agents/issues/77) · **Status:** in progress (caching and the context report delivered; trimming is next).
**PRs:** #80, #83 (was #81)

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

**A context report per turn.** Each turn's Usage event carries `context`:
each part's size in bytes as the model reads it, the messages it held, and
the model's context window. The tokens are the usage's own: the whole input
is `input_tokens + cache_read_tokens + cache_write_tokens`
(`UsageBody.Prompt()`), and the room left is the window minus that
(`UsageBody.Room()`). `/ops/tenants/{t}/runs/{id}/events` returns it with
the run's events.

```json
{ "kind": "usage", "body": {
  "model": "claude-opus-5-5", "input_tokens": 2, "output_tokens": 95,
  "cache_read_tokens": 9085, "cache_write_tokens": 84, "cost_micro_usd": 4145,
  "context": { "system_bytes": 27790, "tools_bytes": 117, "messages_bytes": 210,
               "tool_results_bytes": 45, "messages": 3, "window_tokens": 1000000 } } }
```

- Bytes, not tokens, per part: the API counts tokens for the whole input
  only. Bytes show which part grows (tool results, most often).
- The window comes from the Models API (`max_input_tokens`), asked once per
  model, without retries and for at most 5 seconds. A failed ask leaves it
  out and fails nothing; a turn cancelled while it asks doesn't count, so
  the next turn asks again. On Lambda the model is made with its key on
  first use (`keyedModel`), and passes the ask through.
- In the eval, the skill is part of `system_bytes`.

**The evals read it.** Each result has `context`, and `eval` prints it:

```json
"context": { "turns": 9, "prompt_tokens": 412000, "cache_read_tokens": 371000,
  "cache_write_tokens": 38000, "cache_share": 0.9, "peak_prompt_tokens": 61000,
  "peak_tool_results_bytes": 88000, "least_room_tokens": 939000 }
```

```
chargebacks  4/6  $0.3100 a run, $0.0500 an item, judge $0.0200  (sesn_…)
  context: 9 turns, 412000 input tokens, 90% from the cache, peak turn 61000, least room 939000
```

`least_room_tokens` is -1 when no turn knew its window. (The numbers above
show the shape; the measured Chargebacks run goes on #77.)

## Next

- Trimming old tool results past a threshold, measured in the evals first.
- Chargebacks before and after, same score, lower cost.
