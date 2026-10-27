# The model, the loop and KIFF's gateway as tools: what #2 delivers

**Issue:** [#2](https://github.com/kiff/agents/issues/2) · **Status:** closed, delivered
**PRs:** #7; the loop extended by #28, #31, #32 and #36 (pause, continue, cancel; under [#12](12-runs.md))

## What it's for

An owner chats with a taf in the app. That chat, and every job a taf runs,
needs a loop that calls a model, runs its tool calls, records what happened
and streams it, and that can't write anything except through KIFF.

## What it delivers

| Package | What |
|---|---|
| `model` | The model port. `Turn` streams one turn and returns the reply, its tool calls and usage. `Price` turns usage into cost in millionths of a dollar. The model id is configuration, never a constant |
| `model/anthropic` | The Claude API adapter, on the official Go SDK (pinned) |
| `tool` | The tool port. Each tool says what it can change: `Read`, `Own` (its own memory) or `Write`. `tool.Check` refuses a `Write` tool that doesn't go through KIFF's gateway (`ErrUngoverned`) |
| `tool/kiff` | An MCP client to KIFF's gateway, with the agent's own KIFF key |
| `tool/memory` | The memory tool on `session.Notes`: list, write, forget |
| `agent` | The loop |
| `chat` | The owner talking to an agent |
| `cmd/chat` | A terminal chat against KIFF, for test mode |

### The loop

A run goes model → tool calls (run in parallel) → model, until the agent
answers or something stops it. A call that acts (`tool.Write`, and not
`tool.Reads`) never goes in a turn with a read: sent beside one, it's refused
back unmade with `agent.OneAtATime` (recorded `not_made`), and the reads in that
turn still run. Acts side by side go together. Every event is recorded to the session as it
happens and streamed to the caller.

`agent.Run` returns an `Outcome` whose `Stop` says why it stopped:

| Stop | Means |
|---|---|
| `answered` | The agent answered and called nothing more |
| `paused` | KIFF holds at least one call for the owner; the run waits for it |
| `cost_cap` | The run reached its cost cap |
| `turn_cap` | The run reached its turn limit (`DefaultMaxTurns` is 50) |
| `max_token` | The model ran out of room mid-answer |
| `cancelled` | The run was asked to stop, and stopped at a checkpoint |

- **Cost cap.** Checked before each model turn. `agent.Check` refuses an
  agent with no cap.
- **Held calls.** When KIFF holds a call, every other call in that turn still
  runs, then the run pauses with one requirement per held call and the turn
  it paused in (#28). The model then gets one more turn to say what it waits
  for, with its tools offered but calling them forbidden (`NoToolUse`, sent
  as `tool_choice: none`, #31 and #32), so it can't retry around the hold.
  A cap never skips a pause.
- **Continue.** `agent.Continue` carries a paused run on from the run as
  stored, with one outcome per held call. It reads the paused turn from the
  run, never from events, so it doesn't depend on the keep rules. A missing,
  extra, repeated or still-held result, or one naming another tool, is
  refused (`ErrResults`) before anything is recorded.
- **Refused calls** come back as an error result the agent reads; the run
  carries on.
- **Unknown outcome.** A tool call that errors becomes an error result saying
  whether it took effect is unknown. As KIFF's skill says, such a call is
  never repeated.
- **Cancel.** `Agent.Cancel` is asked at each checkpoint, before a model turn
  (so after each tool batch).

### Tools through KIFF

- `tool/kiff` lists the gateway's tools with the agent's own key. A tool is
  `Read` when the gateway marks it `readOnlyHint`, otherwise `Write`.
- Held and refused calls are read from the result's
  `_meta["dev.kiff/call"].state`, never by parsing text.
- **The key goes only in the Authorization header.** It is never logged,
  recorded or put in an error. A test checks it stays out of errors.
- KIFF's skill goes in the agent's instructions.

### Trying it

```sh
KIFF_AGENT_KEY=… ANTHROPIC_API_KEY=… go run ./cmd/chat -model <model id> -price 5000000,25000000
```

Keys come from the environment only. Each line you type is one message.

## What holds, and the test that guards it

| Rule | Test |
|---|---|
| A full turn records events in order, streamed and costed | `TestAFullTurnIsRecordedAndStreamed` |
| A held call pauses the run; the last turn can't call tools | `TestAHeldCallPausesTheRun`, `TestNoToolUseSendsTheToolsAndChoiceNone` |
| A cap never skips a pause; caps hold across pauses | `TestACapNeverSkipsAPause`, `TestCapsHoldAcrossPauses` |
| A paused run continues with exactly its held calls' outcomes | `TestAPausedRunContinues`, `TestContinueTakesExactlyTheHeldCalls` |
| A refused call is read and the run carries on | `TestARefusedCallIsReadNotAStop` |
| The cost cap stops the run | `TestTheCostCapStopsTheRun` |
| An ungoverned write tool doesn't start | `TestAnUngovernedWriteToolIsRefused` |
| An unknown tool is an error result, not a crash | `TestAnUnknownToolIsAnErrorResult` |
| A cancel stops the run at a checkpoint | `TestCancelStopsAtACheckpoint` |
| Tools are listed and called through the gateway; the KIFF key stays out of errors | `TestListsAndCallsToolsThroughTheGateway`, `TestTheKeyStaysOutOfErrors`, `TestTheKeyStaysOutOfGatewayErrors` |
| A real tool round trip works on the Claude API | `TestLiveTurn`, only with `ANTHROPIC_API_KEY` and `AGENTS_LIVE_MODEL` set, never in CI |

**Benchmarks:** `BenchmarkTurn` (about 48 µs and 64 allocations per turn,
the session's start and end included, with an instant model and tool) and
`BenchmarkPauseAndContinue`. CI compares both with `main` on every PR (#27).

## Where it differs from the issue

- **Chat continuation.** The issue said a message joins the latest chat if it
  was active in the last 30 minutes. #7 shipped one session per message,
  with the 30-minute window carrying the conversation. #34 (under
  [#12](12-runs.md)) replaced that: one session per conversation, each
  message a run through the runner, with history from the conversation's
  finished runs.
- **Holds pause, they don't stop.** The issue said a held call stops the run.
  Since #28 it pauses, so the run can continue once KIFF resolves it.

## Left elsewhere

- `tool/kiff` recording KIFF's IDs for each call, and collecting a held
  call's outcome: [#15](15-continue-on-hold.md).
- tafs serving `POST /v1/tafs/{id}/messages` from this:
  [kiff/tafs#42](https://github.com/kiff/tafs/issues/42).
