# The setup agent: what #91 delivers

**Issue:** [#91](https://github.com/kiff/agents/issues/91) · **Status:** in progress (the `show` components; the planque and draft tools, the workspace host and the simulated-owner test are next).

## What it's for

In the tafs setup chat (kiff/tafs#50), the owner says what to connect, and a
setup agent does it: plans, asks planque for a grant, waits for the owner's
Face ID and vendor sign-in, checks it works, and hands over a working taf.
The app draws what the agent shows; the agent never takes the app's word for
what was approved.

## Delivered

**The `show` tool (contract 1, written on #91).** One component per call:

| `type` | Fields |
|---|---|
| `steps` | `title`; `steps`: `[{label, status}]`, status `pending`, `active`, `done`, `failed` or `skipped` |
| `notice` | `tone` (`info`, `success`, `warning`, `error`), `title`, `text` |
| `confirm` | `text`; `options`: `[{value, label}]`, 2 to 4 |
| `approval` | `grant` (planque), `draft` (KIFF, optional) |
| `sign_in` | `grant`, `service` |
| `result` | `taf`: `{agent, name}`; `text`; `tools` |

- **Checks:** every component has a `type` and an `id` (1 to 64 of `a-z 0-9 - _`). The tool refuses anything else, as a result the model can correct: a field another type uses, an unknown field, a text over 2,000 characters, more than 20 steps, or outside 2 to 4 options.
- **Updates:** showing an `id` again replaces that component in the app.
- **Access:** it is an `Own` tool, so `tool.Check` is unchanged.

**A new event kind, `component`.**
- **Recording:** when a tool's result carries a component (`tool.Result.Component`, kept out of the tool result's own event), the loop records it as a `component` event and streams it as `{"kind":"component","component":{…}}`. The app reads it from the stream and from `GET /sessions/{id}`.
- **Keeping:** a component is kept like the agent's messages. A keep policy may leave components out (`session.Defaulted`); they then take `agent_message`'s rule, so every definition stored before this keeps working. A rule of their own is checked like any other.
- **History:** the model sees what it showed. `chat.Conversation` brings each component back as `[shown] {…}`, so an answer such as `[approval connect-stripe] approved` can be followed up with the grant it named.

**Tests:**
- `tool/show`: `TestEveryComponentIsShown`, `TestWhatDoesNotFitIsRefused`;
- `agent`: `TestAShownComponentIsKept` (kept after the run, while the call that showed it isn't; streamed; a refused component records nothing);
- `session`: `TestAComponentDefaultsToTheMessagesRule`;
- `chat`: `TestHistoryKeepsWhatWasShown`.

Turning off each guard fails its test.

## Left

- **The `planque` tool:** ask for a grant and follow it, with the agent's own KIFF key.
- **A draft tool:** submit and follow the taf's KIFF agent draft. KIFF's gateway has no draft tool, and a draft grants nothing until the phone confirms it.
- **The setup agent's definition** (`setupdef.ts` in kiff/tafs) and its Card.
- **The workspace host:** the AWS adapter, which closes #86.
- **The simulated-owner end-to-end test:** Stripe in test mode through planque.
