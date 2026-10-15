# agents

Agents that act under KIFF Cards: a runtime for running them, and an API for
watching what they do. [tafs](https://github.com/kiff/tafs) is the first product
built on it. Each taf is one agent here.

Status: early. Sessions, runs, the run loop, the runner, the registry,
schedules, evals and the server's routes are built; the server isn't deployed
yet, and a paused run can't continue until KIFF's outcomes are collected. How
it fits together and the order of work: [ARCHITECTURE.md](ARCHITECTURE.md).
What each issue delivers, with the tests that guard it: [docs/](docs/README.md).

## What it holds to

- **Fast.** Starting an agent or recording a session adds nothing noticeable
  next to the model call. A benchmark in CI keeps it that way.
- **Any model.** Agents talk to one model interface. Anthropic is the first
  adapter. Another provider, or your own inference, is another adapter, not a
  rewrite.
- **You decide what's recorded.** Each kind of event (a message, a tool call, a
  tool result, a report, a memory note) has a rule: kept, kept until the run
  ends, or never written. It also has a retention. Removing an agent deletes its
  sessions and memory. You choose where each store lives.
- **KIFF decides, the agent proposes.** Cards, decisions, held calls and the
  owner's answers live in KIFF Cloud, never here. A tool that changes something
  is reached only through KIFF's gateway (`mcp.kiff.dev`), which checks every
  call against the agent's Card first. That means changing money, access or
  data, sending to anyone outside the company, or deleting. When KIFF holds a
  call, the agent stops and says what it waits for. It never retries around the
  hold.
- **Evals gate changes.** What a good agent does is written down as cases that
  run in CI. A change that lowers the score doesn't merge.

## License

[FSL-1.1-ALv2](LICENSE.md), the Functional Source License. It is
source-available, not open source. You may use, change and share this for
anything except a product that competes with it. Each version becomes Apache 2.0
two years after its release.
