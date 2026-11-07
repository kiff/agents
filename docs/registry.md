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
  "history": 20,
  "history_tokens": 30000
}
```

- `history_tokens` bounds what the earlier runs' messages take (estimated, four
  bytes a token). In a chat, what no longer fits is summarised, not dropped.
- `max_tool_result_bytes` bounds what one tool result puts in the model's
  context, after a result that is wholly JSON is compacted: 0 or left out is
  32 KB, up to 256 KB. A longer result is cut with a note to call again with a
  filter or a smaller limit. The turn that reads a result writes all of it to
  the cache, so a broad search paging through long answers costs more each
  turn than the thread does.
- `keep.kept_read`, `{ "Keep": 3, "Retention": <idle>, "Max": <at most> }`
  (`UntilIdle`), keeps what a chat read for its next messages, sealed. Left
  out, nothing is kept.
- Kinds a `keep` leaves out take another kind's rule (`session.Defaulted`):
  `component`, `trial_write`, `call_ref`, `summary`, `called`, `answered` and
  `own_refused` are kept like `agent_message`. `own_refused` is a call to the
  agent's own tool (a card, a note) that the tool refused, with the start of
  its input (512 bytes, which can hold the owner's words, as a card does) and
  the tool's reason.

- `price` is per million tokens, in millionths of a dollar. With both cache
  prices set, every turn uses the prompt cache; without them it doesn't, so
  the cost cap never counts cache tokens as free (#77). `CacheWrite` is the
  5-minute write price (1.25× input on Claude).
- `speed` (optional): `"fast"` runs the turns at the provider's fast output
  when someone waits on the run (an owner's message, a trial, an eval), never
  for a schedule or an event. `price_fast` is what those turns cost, and
  counts the cache as `price` does, so the cost cap counts them at their
  price. A conversation keeps the speed it began with, since the cache is per
  speed. A fast turn that the provider turns away for its fast limits or its
  load runs at once at standard speed, and the conversation stays at
  standard. Each turn's `usage` event records its `speed`.
- `thinking` (optional): `"summarized"` asks the model for a summary of its
  thinking as it writes it, streamed to whoever follows the run as `thinking`
  events, so someone waiting sees what it considers. `"updates"` keeps the
  reasoning hidden and streams only the short progress notes the model writes
  between tool calls ("Checking your last refunds…"), each whole, as an
  `update` event, under the API's beta `thinking-display-updates-2026-08-18`.
  On these models a line written before a tool call comes back as such a note,
  not as text: under `"summarized"` it is mixed with the reasoning's summary,
  and with no `thinking` it is hidden. Nothing records the events. Empty keeps
  the thinking hidden, the provider's default. The thinking is billed the same
  either way: it's taken only for a model that thinks whether asked or not
  (`registry.ThinkingModels`: Claude Opus 5.5, Sonnet 5.5, Fable 5 and Mythos
  5), since on another it would turn thinking on or be refused; `"updates"`
  only for one that writes progress notes (`registry.UpdatesModels`: not
  Mythos 5).
- `defer_vendor_tools` (optional): every gateway tool that isn't KIFF's own
  (`kiff_*`) goes to the model deferred, whatever its name. A Card that
  covers a vendor's tools mid-conversation (Setup's reads) then leaves the
  cached prefix as it was, instead of starting the cache over.
- `disclose` (optional): `{line, lines?, in: [{tool, fields}]}`. The runtime
  puts `line` ("Written by an AI agent for the company.") in each call of a
  named tool, a tool KIFF names with a connection's part before or after it
  too, so a message to someone outside the company says an AI agent wrote it,
  and for whom, whatever the model wrote (Eval 42193895936; AI Act Art.
  50(1)).
  - **It goes first, in each part:** at the top of each of `fields` (dotted
    paths) the call carries as text, or as the first field when the call
    carries its parent and none of them. Several fields can be renderings of
    one message (an email's `text` and its `html`) and a reader is shown one
    of them, so each starts with the line. Whoever reads the message reads it
    before anything else, as the Commission's guidelines show the label.
  - **In the message's language:** `lines` is the same sentence in other
    languages, by ISO 639-1 code (`{"es": "Escrito por un agente de IA para
    la empresa."}`), at most 12. The line that goes first is the one in the
    language the message is written in, told by the common words of all its
    fields read together (html tags left out): at least two, and more than
    any other language's. Otherwise, `line`. Every field gets the same one.
  - **Said once, wherever the model put it:** each of the lines, in any
    language and as html writes it (`&amp;` for `&`), is taken out of each
    field wherever it is, with the space around it, before the right one goes
    first. The rest of the text is as the model wrote it. A call that already
    reads that way is sent as it was, so a second pass changes nothing. One
    that carries none of the fields is sent as it was.
  - **In an html part** (a field whose name says html, such as `html` or
    `body_html`, or whose text is a whole page), the line is a paragraph of its
    own, escaped (`<p>Written by an AI agent for Smith &amp; Co.</p>`), so it
    never runs into the first sentence. It goes just inside the page's
    `<body>` when there is one, and never before its doctype.
  - **A quote of an earlier message keeps its line:** on lines that start with
    `>`, or inside an html `<blockquote>`, the earlier message is as it was
    sent.
  - What the call sends, its record and a hold's stored input carry the line;
    the model's own turn goes back to it as it wrote it.
- `show_types` (optional, with `show`): the cards the agent shows instead
  of Coach's set, from a taf's own: `ask_coach`, a line and an Ask Coach
  button that opens Coach on the taf with the owner's request written in.
  A taf shows it for a setting only Coach changes (a connection or its key,
  its Card, tools or schedule). It waits for nothing.
- `credit` (optional): `{line, after, in: [{tool, fields}]}`, checked as
  `disclose` is and kept, and **never put in a call**. Since 7 Nov no message
  carries a credit line ("Built with tafs."), on WhatsApp, SMS or email: the
  owner's call. A definition that still names one saves as before.
- `tenant` and `id` come from the verified caller and the path; a body that
  says otherwise is refused.
- `gateway` must be on the server's own list (`Server.Gateways`), so a
  definition can't send its key anywhere else.
- `key_ref` names the key; it is never the key.
- `workspace` (optional, off by default) gives each run a workspace: files
  and commands, with no credentials and no way out (#86). Only an agent that
  needs one sets it, such as kiff/tafs' setup agent.
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
	Connect: func(ctx context.Context, tenant, agent, gateway, key string) ([]tool.Tool, error) {
		return warm.Tools(ctx, gateway, tenant, agent, key) // warm := &kiff.Sessions{}
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
   reach today. `kiff.Sessions` keeps the gateway session (its handshake) for
   the agent's next run, for at most two minutes, per gateway, tenant, agent
   and key, and never across any of them: a new key replaces it, and any
   refusal or failure drops it. The tools are still listed each run, so a Card
   that changed shows at once;
5. adds the run's workspace tool when the definition asks for one; `BuildRun`
   names the run, and without a run or a server `Workspace` it refuses
   (`ErrNoWorkspace`) rather than build the agent without it;
6. returns the agent, checked, and the definition (its keep rules and history
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
