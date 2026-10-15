# AGENTS.md

For AI assistants working on this repository. Read this first, then the README.

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

## How sessions work together

Several assistants build this in parallel, and they don't talk to each other.
Issues and PRs are the shared record.

- Take one issue at a time. Comment on it when you start, so the others see it.
- The shared types are the contract between issues: sessions, events, the model
  interface and tools. Change them in a PR of their own, and comment on the
  issues that use them.
- If a finding touches another repo (kiff/tafs, kiff/kiff-cloud), leave it as a
  comment on that repo's issue.

## Working

```bash
gofmt -l . && go vet ./... && go test ./...
```

New behaviour gets tests. Work on feature branches only; never push to `main`.
Merge only when the PR is approved at its current head.
