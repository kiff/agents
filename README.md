# agents

Runs AI agents under KIFF Cards, with an API to watch what they do.
[tafs](https://github.com/kiff/tafs) is built on it: each taf is one agent here.

For developers working on tafs, or running agents that KIFF governs.

Status: early. What each issue has delivered, with the tests that guard it:
[docs/](docs/README.md).

## Try it

```bash
go test ./...
```

How it fits together and the order of work: [ARCHITECTURE.md](ARCHITECTURE.md).
More: [runs, by example](docs/runs.md) and [schedules](docs/schedules.md).

## What it holds to

- **Fast.** Starting an agent or recording a session adds nothing noticeable
  next to the model call. A benchmark in CI keeps it that way.
- **Any model.** Agents talk to one model interface. Another provider, or your
  own inference, is another adapter, not a rewrite.
- **You decide what's recorded.** Each kind of event (a message, a tool call, a
  tool result, a report, a memory note) is kept, kept until the run ends, or
  never written, with its own retention. Removing an agent deletes its sessions
  and memory. You choose where each store lives.
- **KIFF decides, the agent proposes.** Cards, decisions, held calls and the
  owner's answers live in KIFF Cloud, never here. A tool that changes something
  (money, access, data, sending outside the company, deleting) is reached only
  through KIFF's gateway (`mcp.kiff.dev`), which checks every call against the
  agent's Card first. When KIFF holds a call, the agent stops and says what it
  waits for. It never retries around the hold.
- **Evals gate changes.** What a good agent does is written down as cases that
  run in CI. A change that lowers the score doesn't merge.

## Licence

[FSL-1.1-ALv2](LICENSE.md), the Functional Source License: source-available,
not open source. You may use, change and share it for anything except a
product that competes with it. Each version becomes Apache 2.0 two years after
its release.
