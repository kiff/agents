# AGENTS.md

For AI assistants working on this repository. Read this first, then the README
and ARCHITECTURE.md, which says where everything goes and in what order.

## Rules

- **This repository is public, and so are its Actions logs.** Never write
  people's emails or phones, account, key or team IDs, ARNs, or any secret into
  code, commits, PRs, issues or workflow inputs. Use GitHub secrets or read
  values from the environment.
- **KIFF is used like any customer uses it**: the public API (`api.kiff.dev`)
  and the gateway (`mcp.kiff.dev`). This repo never holds approvals, Card logic,
  or a back door into KIFF Cloud. If something is missing in KIFF, open an issue
  on kiff/kiff-cloud and link it from the issue you're working on.
- **Built for tafs first.** Add what tafs needs now, not what a general agent
  platform might need. If no tafs issue asks for it, it waits.
- **Docs describe what this is and does.** They don't describe what inspired it.
- **Small and fast.** Prefer the standard library. Add a dependency only when it
  saves real work, and pin it exactly. Hot paths (the run loop, recording an
  event) keep a benchmark.

## Layout

Ports and adapters. The core packages hold types, the interfaces they need
(ports) and the logic that uses them. They never import an adapter, an SDK or
a database driver; adapters import the core.

```
session/            sessions (threads of runs), events, keep rules, Recorder; ports Store, Notes
session/sessiontest the checks every Store must pass
run/                runs: statuses, the transition table, the lease, the sweep; port Runs
run/runtest         the checks every Runs must pass
store/memory        adapter: in memory, for tests and local runs
store/<name>        one package per store (sqlite, dynamodb)
model/              the model port: one streamed turn, Price
model/anthropic     adapter: Claude API, through the official Go SDK
tool/               the tool port; Check refuses a write tool outside KIFF's gateway
tool/kiff           adapter: MCP client to KIFF's gateway, with the agent's own key
tool/memory         adapter: the agent's notes, on session.Notes
tool/workspace      adapter: the workspace tool, on workspace.Workspaces (an Own tool)
workspace/          a run's workspace: files and commands, no credentials, no way out; port Workspaces
workspace/workspacetest the checks every Workspaces must pass
agent/              the run loop: model → tools → model, recorded and streamed; pauses on a KIFF hold
chat/               the owner talking to an agent: each message a run in the conversation's session
runner/             executes runs, as the server's Runner: start in a session, claim, build, record, run, store the stop; continue a paused run
registry/           stored agent definitions, built per request for their tenant; ports Agents, Secrets
registry/registrytest the checks every Agents store must pass
registry/secretsmanager adapter: Secrets on AWS Secrets Manager, each key under its tenant's name
schedule/           schedules: cron in a time zone, kept in step with what fires them; ports Schedules, Scheduler
schedule/scheduletest the checks every Schedules store must pass
schedule/eventbridge adapter: the Scheduler on EventBridge Scheduler
server/             the HTTP API, scoped by the verified caller; port Runner
server/lambdaurl    adapter: a Lambda function URL with AWS_IAM auth, streaming
eval/               eval cases, checks, the judge; grading runs
cmd/chat            a terminal chat against KIFF, for test mode
cmd/eval            grades runs against the eval cases
cmd/lambda          the server and schedule firings on Lambda
docs/issues/        what each issue delivers, with the tests that guard it
```

Model, tools, the run loop and evals follow the same shape: a core package
with its port, and one package per adapter (for example `model/anthropic`).
Wiring happens in `cmd/`. ARCHITECTURE.md lists the packages still to build
(`console`, `hook`) and where each fits;
today chat still opens one session per message, until step 1 of its order of
work lands.

Goroutines where they save waiting: one writer per recording, so the run loop
never waits on the store, and tool calls in parallel. Every goroutine has an
owner that ends it (`Recording.End`), and every error it hits reaches a caller.

## How sessions work together

Several assistants build this in parallel, and they don't talk to each other.
Issues and PRs are the shared record.

- Take one issue at a time. Comment on it when you start, so the others see it.
- A PR that delivers part of an issue updates that issue's page in
  `docs/issues/` in the same PR: what it adds, the tests that guard it, and
  what is left.
- The shared types are the contract between issues: sessions, events, the model
  interface and tools. Change them in a PR of their own, and comment on the
  issues that use them.
- If a finding touches another repo (kiff/tafs, kiff/kiff-cloud), leave it as a
  comment on that repo's issue.

## Working

```bash
gofmt -l . && go vet ./... && go test ./...
```

On a pull request, CI also runs the benchmarks against the base branch's and
fails on a clear slowdown; `.github/bench.sh main` does the same locally (it
needs `benchstat`).

New behaviour gets tests. Work on feature branches only; never push to `main`.
Merge only when the PR is approved at its current head.
