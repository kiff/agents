# Steer a run: what #102 delivers

**Issue:** [#102](https://github.com/kiff/agents/issues/102) · **Status:** built here. tafs cloud's route and the app follow in [kiff/tafs#64](https://github.com/kiff/tafs/issues/64).

## What it's for

The owner corrects an agent while it works ("only the €20 one") without
stopping it and starting over. The run takes the message at its next turn,
so the chat reads as talking to someone at work, not filling in a queue.

## Delivered

**A run keeps what the owner wrote while it goes** (`run.Run.Steering`).
`run.Steer` adds a message to a run that hasn't ended and wasn't asked to
stop: pending, running or paused. That's a compare-and-set, like a cancel, so
a running run's own writes keep it (`run.Save` reads the run again). A run
that ended, or was asked to stop, is `ErrTransition`. One that holds
`run.MaxSteering` (20) messages is `ErrFull`.

**The loop takes it at a checkpoint** (`agent.Agent.Steer`, the runner's
`steered`):
- **When:** before each model turn, after `Cancel`, and once more when the
  agent has answered. A message that came while it wrote its answer gets a
  turn of its own, if the caps leave one.
- **Marked taken first:** the runner marks each message taken on the run, under
  its claim, before the loop records it. So no message is taken twice.
- **Recorded and streamed:** each one taken is recorded as an `owner_message`
  under the keep rules, and streamed as a `delta` of kind `owner_message`.
- **Where the model reads it:** after what it read last, in the same user turn
  when that ends the conversation (a tool batch's results, or the input),
  opened by `agent.SteerNote`. Never inside a tool's result.
- **A paused run:** one that continues reads the messages it took before the
  pause after its input (`agent.Continue`), since it doesn't see the turns
  between them. A message written while it was paused is taken at its first
  checkpoint.

**Nothing is dropped silently.** A message the run never took, because it
ended first (a cap, a cancel, an error), stays on the run, not taken. The run
view lists it (`steering`, each with `taken`), and the caller sends it as the
next message.

**The route:** `POST /agents/{id}/runs/{run_id}/messages` with `{"text": …}`.
- **202** with the run, for a run that takes it.
- **409** for a run that has ended or was asked to stop (send it as a new run), and for one full of messages.
- **400** for empty text, or more than `MaxInput`.
- **404** for a run outside the tenant or the agent.

**KIFF still decides every call.** A message steers within the job, as a chat
message does. It can't widen the Card or answer a held call: only KIFF
resolves a requirement.

## The tests

| Rule | Test |
|---|---|
| Kept in order until taken; none on an ended run, one asked to stop, or past the cap | `run.TestSteerKeepsMessagesForARunThatGoes` |
| Taken at the next checkpoint: after the results, recorded, streamed; a failed checkpoint stops the run | `agent.TestAnOwnersMessageIsTakenAtTheNextCheckpoint` |
| One that came during the answer gets a turn; none asked for with no turn left | `agent.TestAMessageDuringTheAnswerGetsATurn` |
| A continued run reads what it took before the pause | `agent.TestAContinuedRunKnowsWhatTheOwnerWrote` |
| Through the store: marked taken, read by the model, recorded | `runner.TestTheOwnerSteersARunThatGoes` |
| Written while paused, taken when it continues | `runner.TestAPausedRunTakesWhatTheOwnerWroteWhenItContinues` |
| The route: 202, 409, 400, 404 | `server.TestSteerRun` |

## Left

- tafs cloud passes it through, and the app sends instead of queueing, and sends a message not taken as the next one ([kiff/tafs#64](https://github.com/kiff/tafs/issues/64)).
