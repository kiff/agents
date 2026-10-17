# The setup agent: what #91 delivers

**Issue:** [#91](https://github.com/kiff/agents/issues/91) · **Status:** in progress (the `show` components, merged in #92, and the `planque` tool; the draft tool and its claim, and the nightly simulated-owner test, are next).

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
- **Opt-in:** a definition turns it on with `"show": true`; it is off unless set (`TestShowIsOptIn`).

**A new event kind, `component`.**
- **Recording:** when the `show` tool's result carries a component (`tool.Result.Component`, kept out of the tool result's own event), the loop records it as a `component` event. Only a tool that implements `tool.Shower` can put a component in the chat; `show` is the only one.
- **Two shapes, by where the app reads it:**
  - **the live stream** carries an `agent.Event`: `{"kind":"component","call_id":"…","component":{…}}`;
  - **`GET /sessions/{id}`** carries a `session.Event`: `{"kind":"component","seq":12,"time":"…","body":{…}}`.
- **Keeping:** a component is kept like the agent's messages. A keep policy may leave components out (`session.Defaulted`); they then take `agent_message`'s rule, so every definition stored before this keeps working. A rule of their own is checked like any other.
- **History:** the model sees what it showed. `chat.Conversation` brings each component back as `[shown] {…}`, so an answer such as `[approval connect-stripe] approved` can be followed up with the grant it named.

**Tests:**
- `tool/show`: `TestEveryComponentIsShown`, `TestWhatDoesNotFitIsRefused`;
- `agent`: `TestAShownComponentIsKept` (kept after the run, while the call that showed it isn't; streamed; a refused component records nothing), `TestOnlyShowPutsAComponentInTheChat`;
- `session`: `TestAComponentDefaultsToTheMessagesRule`;
- `chat`: `TestHistoryKeepsWhatWasShown`.

Turning off each guard fails its test.

**The `planque` tool (contract 2, on kiff/planque#1).** The agent asks planque for access to a vendor account for a taf, and follows the request.

- **`request {server, agent, tools}`:** `POST /v1/accounts/{tenant}/grants` with the agent's own KIFF key, which planque checks with KIFF. Each tool may be narrowed with `fixed_arguments` and `arguments`, as planque takes them.
- **`status {grant}`:** `GET …/grants/{id}`. It is `pending`, `approved`, `signed_in`, `connected`, `failed` (with a reason) or `disconnected`.
- **What the model reads:** the grant's `id`, `status`, `text` (what the owner will read), `tools` and `reason`; not its digest, request or audience.
- **Refusals:** planque's errors come back as readable results, for example `409` before the owner's phone is enrolled. A malformed ask, or a grant id with `/`, `?` or `#`, never leaves the tool.
- **Access:** asking grants nothing, so it is an `Own` tool; the agent never sees a vendor token.

**Opt-in per definition: `planque`.** A definition names planque's URL (https, no path). The server allows it exactly, as it does gateways (`AGENTS_PLANQUES` on Lambda; none by default), and checks it before the agent's key is looked up, so a key is never sent to a planque the server doesn't allow.

**Tests:**
- `tool/planque`: `TestRequestAndStatus`, `TestRefusalsAreReadable`;
- `registry`: `TestBuildGivesThePlanqueToolOnlyForAnAllowedPlanque`, and `TestCheckRefuses` for an http planque and a planque with a path.

Turning off each guard fails its test: the allowlist, the URL check, the agent's own key, the trimmed view, the grant id's path, and readable refusals.

## Left

- **A draft tool:** submit the taf's KIFF agent draft, keep its claim secret from the model, and claim the taf's key into Secrets Manager when tafs cloud asks (agreed on kiff/tafs#50, option b).
- **The setup agent's definition** (`setupdef.ts`, in kiff/tafs#53) and its Card.
- **The simulated-owner end-to-end test:** nightly against planque's fake vendor, and on demand against Stripe in test mode.

**Out of #91:** the workspace host. It served the relay that planque replaced; #86 stays open for when a scenario needs it.
