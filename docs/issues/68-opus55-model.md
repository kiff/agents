# Opus 5.5 in the model adapter: what #68 delivers

**Issue:** [#68](https://github.com/kiff/agents/issues/68) · **Status:** delivered.
**PRs:** #74

## What it's for

tafs switches over on `claude-opus-5-5` (kiff/tafs#44). Three gaps in
`model/anthropic` mattered there: effort quietly dropped to medium,
thinking was dropped between turns, and refusals ended a run with no reason.

## Delivered

**Effort per agent.** `registry.Definition.Effort` (`low`, `medium`, `high`,
`xhigh`, `max`; empty is the API's default, medium on Opus 5.5) goes on every
turn as `output_config.effort`.

```json
PUT /agents/rae
{ "model": "claude-opus-5-5", "effort": "high", "max_tokens": 8192, … }
```

```json
POST /v1/messages
{ "model": "claude-opus-5-5", "output_config": { "effort": "high" }, … }
```

**Thinking goes back in place.** A thinking block (`model.Thinking`: its text
and `signature`) and a redacted one (`model.RedactedThinking`: `data`) are
kept opaque and replayed exactly as they came, before the tool call they
came with. Dropping them "breaks the turn" on Opus 5.5. The paused turn
(`run.Turn`) stores them, so a continued run sends them back too.

```json
{ "role": "assistant", "content": [
  { "type": "thinking", "thinking": "The refund is under the Card.", "signature": "sig-abc" },
  { "type": "tool_use", "id": "toolu_1", "name": "refund", "input": { … } } ] }
```

**Refusals are their own stop.** `stop_reason: "refusal"` is `model.Refused`
with `stop_details.category`. The run ends COMPLETED with `stop: "refused"`
and `refusal`: the category (`cyber`, `bio`, `reasoning_extraction`, …), or
empty when the API names none.

```json
GET /agents/rae/runs/5c4c…
{ "status": "COMPLETED", "stop": "refused", "refusal": "cyber", … }
```

| Rule | Guarded by |
|---|---|
| thinking and redacted thinking come back in order, and go back unchanged, in place | `model/anthropic` `TestThinkingIsKeptAndSentBackInPlace` |
| effort is sent as `output_config.effort`; none sends nothing | `TestEffortIsSent` |
| a refusal is `Refused` with its category | `TestARefusalIsItsOwnStop` |
| the loop replays the thinking with its tool call | `agent` `TestThinkingGoesBackInPlace` |
| a paused turn keeps its thinking through the run store | `TestAPausedTurnKeepsItsThinking` |
| a refused turn stops the run as refused, with its category; effort on every turn | `TestARefusedTurnStopsTheRun`, `TestEffortGoesOnEveryTurn` |
| effort is empty or one of the levels; the built agent has it | `registry` `TestCheckRefuses`, `TestBuildMakesTheAgentFromItsDefinition` |
| the stored run and the API show `stop: refused` and `refusal` | `runner` `TestARefusedRunRecordsWhy`, `server` `TestARefusedRunShowsWhy` |

**Live:** `TestLiveTurn` on `claude-opus-5-5` passes at effort `high` and
`max`. At `max` the first turn thought, and its thinking went back with the
tool call:

```sh
ANTHROPIC_API_KEY=… AGENTS_LIVE_MODEL=claude-opus-5-5 AGENTS_LIVE_EFFORT=max go test -run Live ./model/anthropic/
```

## Not delivered

- **The server-side fallback** (`server-side-fallback-2026-07-01`,
  `fallbacks: "default"`), which the issue listed as optional. A refused
  turn ends the run as refused; the owner sees why.
