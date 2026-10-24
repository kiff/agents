# The setup agent: what #91 delivers

**Issue:** [#91](https://github.com/kiff/agents/issues/91) · **Status:** in progress (the `show` components, merged in #92; the `planque` tool, merged in #93; the draft tool and its claim, merged in #94; the nightly simulated-owner test, merged in #96; a run against Stripe in test mode is left).

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
| `result` | `taf`: `{agent, name, job, draft}` (`job` is one of tafs' jobs, such as `refunds`; `draft` is the KIFF draft the owner confirmed: what tafs cloud starts the taf from); `text`; `tools` |

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

**The draft tool and its claim (agreed on kiff/tafs#50, option b).** The setup agent submits a taf's KIFF draft (its agent and Cards). KIFF answers with the draft's claim secret, shown once, which mints the taf's key. The key never passes through the model, the phone or tafs cloud.

1. **Submit (`kiff_draft`, `submit {agent_id, label, cards}`):**
   - the tool posts `POST /v1/me/agent-drafts` with the setup agent's own key;
   - it keeps the claim secret sealed under the tenant (`drafts.Store`, on Secrets Manager under `AGENTS_DRAFTS_PREFIX`);
   - the model reads only the draft's `id`, `agent_id`, `status` and `card_ids`, and KIFF's refusals.
2. **Confirm:** the phone confirms the draft behind the approval's Face ID (contract 4, in the app).
3. **Claim (`POST /drafts/{draft_id}/claim {"key_ref"}`):** tafs cloud asks, with its verified tenant call. kiff/agents, in this order:
   - refuses a draft another tenant submitted (404);
   - answers the same on a retry with the same `key_ref`, and refuses another one (409);
   - refuses a `key_ref` that holds another agent's key **before** asking KIFF to mint (409);
   - claims the key with the sealed secret (`POST /v1/agent-drafts/{id}/claim`). KIFF's 409 (not confirmed yet, or already claimed or discarded) and 410 (expired) are answered 409 with what to do;
   - **holds the minted key in the draft's record at once,** in place of the claim secret, so a claim that fails after this point is finished by its retry: KIFF mints a key only once;
   - checks with `/v1/me` that the key is bound to the draft's agent on this tenant. If KIFF can't be asked (a timeout, a 5xx), the claim returns that error, keeps the key held, and the retry asks again. Only KIFF naming another tenant or agent is a mismatch: then the key is kept in the record, flagged and logged with the draft and key ids for the owner to revoke in KIFF, and it is never stored where a definition can name it;
   - creates the key's secret where the registry reads it, never replacing one;
   - drops the key from the record, and answers `{"agent_id"}`.

   A claim that stopped anywhere after the mint (before storing the key, or after storing it but before recording itself) is finished by its retry, without a second claim.
4. **Store:** tafs cloud stores the taf's definition with that `key_ref`.

**Opt-in:** `"drafts": true` on a definition. The server offers drafts only with `AGENTS_DRAFTS_PREFIX` set. That prefix must not overlap the agents' key prefix, so no key reference can name a claim secret. The function's role needs `secretsmanager:CreateSecret` and `PutSecretValue` on both prefixes.

**Tests:**
- `drafts`: `TestSubmitKeepsTheSecretFromTheModel`, `TestClaim`, `TestClaimNeverReplacesAnotherKey`, `TestARetryFinishesAnInterruptedClaim`, `TestARetryFinishesAfterTheKeyWasMinted`, `TestAKeyForAnotherAgentIsNotStored`, `TestAMismatchedKeyIsKeptForRevoking`, `TestKIFFFailingAfterTheClaimIsNotAMismatch`, `TestKIFFFailingOnAHeldRefIsNotTaken`;
- `drafts/kiffapi`: `TestClaim`;
- `tool/drafts`: `TestSubmit`;
- `server`: `TestClaimRoute`, `TestNoClaimRouteWithoutDrafts`;
- `registry/secretsmanager`: `TestDraftRecords`, `TestKeysAreCreatedNeverReplaced`, `TestARefBeingDeletedIsTaken` (a removed taf's key, scheduled for deletion, is taken: 409, not 500);
- `registry`: `TestDraftsIsOptIn`.

Turning off each guard fails its test.

**The setup agent itself (kiff/tafs#53).**
- **Its definition:** `setupdef.ts` in kiff/tafs: `show`, `planque` and `drafts` on, KIFF's gateway, and a cost cap per run.
- **Its key:** it holds no tools. Its key comes from a no-tools draft and carries no role, so it can call none of the account's tools. KIFF's gateway lists it only KIFF's own (`kiff_card`, `kiff_pending`). That replaces "its own Card allows setup only": no Card is needed for an agent that can call nothing.
- **What `kiff_draft` tells the model:** a Card as KIFF issues it (`count` or `sum(<argument>)`, the windows `calendar_day`, `rolling_1h`, `rolling_24h` and `lifetime`, at least one aggregate, `on_exceed`). Without it, the model guessed formats KIFF refuses.

**The simulated-owner end-to-end test (`cmd/e2e`, #96).** A scripted owner talks to the setup agent and ends with a Refunds taf. The runtime runs in the test's process, at the commit tested. The model, KIFF (a test account), planque and the vendor are the real services.
- **The flow:**
  - the owner's phone (a stable P-256 key) approves planque's grant;
  - it signs in at kiff/planque's fake vendor (an MCP server behind its own OAuth, which consents on its own and registers only planque's callback);
  - it confirms the sign-in, then the taf's draft;
  - the taf's key is claimed into the agents' keys.
- **It fails if:**
  - the agent shows a result without a confirmed draft (a skipped approval);
  - any key, session, token or claim secret appears in what either agent's runs recorded, by value or by KIFF's prefixes;
  - the vendor receives anything but the one refund inside the Card (300 cents). The 90000-cent one is refused by the Card.
- **Nightly:** `.github/workflows/e2e.yml` at 03:17 UTC, and on demand; never on pull requests. The log is public, so in CI it carries only the steps and the outcome, and every id the test learns is masked.
- **The one-time setup:** `-signin`. A person approves KIFF's device sign-in for the test phone (90 days), and the setup agent is made from a no-tools draft.

## Left

- **On demand against Stripe in test mode:** the same scenario at `mcp.stripe.com`, with a person completing Stripe's sign-in.

**Out of #91:** the workspace host. It served the relay that planque replaced; #86 stays open.
