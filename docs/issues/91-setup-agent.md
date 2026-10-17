# The setup agent: what #91 delivers

**Issue:** [#91](https://github.com/kiff/agents/issues/91) · **Status:** in progress (the `show` components, merged in #92; the `planque` tool in #93; the draft tool and its claim; the nightly simulated-owner test is next).

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
   - checks with `/v1/me` that the key is bound to the draft's agent on this tenant. If it isn't, the key is kept in the record, flagged and logged with the draft and key ids for the owner to revoke in KIFF, and it is never stored where a definition can name it;
   - creates the key's secret where the registry reads it, never replacing one;
   - drops the key from the record, and answers `{"agent_id"}`.

   A claim that stopped anywhere after the mint (before storing the key, or after storing it but before recording itself) is finished by its retry, without a second claim.
4. **Store:** tafs cloud stores the taf's definition with that `key_ref`.

**Opt-in:** `"drafts": true` on a definition. The server offers drafts only with `AGENTS_DRAFTS_PREFIX` set. That prefix must not overlap the agents' key prefix, so no key reference can name a claim secret. The function's role needs `secretsmanager:CreateSecret` and `PutSecretValue` on both prefixes.

**Tests:**
- `drafts`: `TestSubmitKeepsTheSecretFromTheModel`, `TestClaim`, `TestClaimNeverReplacesAnotherKey`, `TestARetryFinishesAnInterruptedClaim`, `TestARetryFinishesAfterTheKeyWasMinted`, `TestAKeyForAnotherAgentIsNotStored`, `TestAMismatchedKeyIsKeptForRevoking`;
- `drafts/kiffapi`: `TestClaim`;
- `tool/drafts`: `TestSubmit`;
- `server`: `TestClaimRoute`, `TestNoClaimRouteWithoutDrafts`;
- `registry/secretsmanager`: `TestDraftRecords`, `TestKeysAreCreatedNeverReplaced`;
- `registry`: `TestDraftsIsOptIn`.

Turning off each guard fails its test.

## Left

- **The `planque` tool:** ask for a grant and follow it, with the agent's own KIFF key.
- **The setup agent's definition** (`setupdef.ts` in kiff/tafs) and its Card.
- **The simulated-owner end-to-end test:** nightly against planque's fake vendor, and on demand against Stripe in test mode.
- **The setup agent's own Card:** a KIFF change for a Card with no account tools (asked on #91).

**Out of #91:** the workspace host. It served the relay that planque replaced; #86 stays open.
