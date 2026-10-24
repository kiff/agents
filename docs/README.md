# What each issue delivers

One page per issue: what it's for, what it delivers (packages, rules,
storage, routes), the test that guards each rule, and what's still to do.
ARCHITECTURE.md is the plan; these pages are what was built against it.

**Keep them current.** A PR that delivers part of an issue updates that
issue's page in the same PR: what it adds, the tests that guard it, and what
is left.

As of `main` at 9c1ba7e (2026-10-16). By example: [runs.md](runs.md) (runs,
the runner, chat and evals) and [registry.md](registry.md).

| Issue | Step | Status | Page |
|---|---|---|---|
| [#1](https://github.com/kiff/agents/issues/1) Sessions | — | Delivered; SQLite deferred | [01-sessions.md](issues/01-sessions.md) |
| [#2](https://github.com/kiff/agents/issues/2) The model, the loop, KIFF's gateway as tools | — | Closed, delivered | [02-chat-loop.md](issues/02-chat-loop.md) |
| [#3](https://github.com/kiff/agents/issues/3) Evals | — | Harness, both cases, `eval -run` on this runtime and the cases' Stripe tools behind KIFF built; first Chargebacks numbers posted; Refunds tools, the baseline and the nightly run to do | [03-evals.md](issues/03-evals.md) |
| [#12](https://github.com/kiff/agents/issues/12) Runs | 1 | Delivered (chat on runs: #34) | [12-runs.md](issues/12-runs.md) |
| [#13](https://github.com/kiff/agents/issues/13) Server on Lambda | 2 | Built: routes, auth, streaming, runs, background runs, the sweep, firings, KIFF's alerts, the 429 cap. Not deployed (the tafs#42 session deploys) | [13-server.md](issues/13-server.md) |
| [#14](https://github.com/kiff/agents/issues/14) Console | 3 | In progress: `/ops` runs across tenants, operator sign-in; pages next | [14-console.md](issues/14-console.md) |
| [#15](https://github.com/kiff/agents/issues/15) Continue on a hold | 4 | Built: hold IDs, Collect, alerts, withdraw, expiry, cancel, continue, what a continued held call says (#72); `cmd/lambda` wiring with #13 | [15-continue-on-hold.md](issues/15-continue-on-hold.md) |
| [#16](https://github.com/kiff/agents/issues/16) Registry | 5 | Delivered | [16-registry.md](issues/16-registry.md) |
| [#17](https://github.com/kiff/agents/issues/17) Schedules | 6 | Delivered: firings, firing by hand, triggers (`/agents/{id}/events`) | [17-schedules.md](issues/17-schedules.md) |
| [#68](https://github.com/kiff/agents/issues/68) Opus 5.5 in the model adapter | — | Delivered: effort per agent, thinking kept, refusals | [68-opus55-model.md](issues/68-opus55-model.md) |
| [#91](https://github.com/kiff/agents/issues/91) The setup agent (kiff/tafs#50) | — | In progress: `show` (#92) and the `planque` tool (#93), merged; the draft tool and its claim; the nightly simulated-owner test next. The workspace is out (#86 stays open) | [91-setup-agent.md](issues/91-setup-agent.md) |
| [#102](https://github.com/kiff/agents/issues/102) Steer a run | — | Built: messages kept on a run, taken at checkpoints, the route; tafs' side in kiff/tafs#64 | [102-steer.md](issues/102-steer.md) |
| [kiff/tafs#42](https://github.com/kiff/tafs/issues/42) tafs switches over | 7 | Waits on #13, #15, #16 | — |

## What blocks what

```
#15 IDs, Collect, /kiff/alerts ──► paused runs continue and end ──► kiff/tafs#42
#3 grading this runtime: unblocked (runs execute on the runner, locally or on Lambda)
#14 console: needs a cross-tenant run listing and usage, which no store has yet
```

The shortest path to tafs switching over: #15, then the deploy. #14 is independent of that path.

## The older issues still open

- **#1 (Sessions).** Everything its "done when" asks is built and tested,
  except tafs reading a taf's thread from it, which is tafs' switch-over
  (kiff/tafs#42). What's left is SQLite, deferred since 2026-10-15 because
  nothing needs it. **Suggested:** close #1, and open SQLite as its own issue
  only when something needs it.
- **#3 (Evals).** Its "done when" isn't met: the baseline score and cost
  aren't posted, and the nightly run (#9) has had no review since
  2026-10-15. #10 (the Chargebacks seed) is a draft. The baseline waits on a
  Stripe test-mode run set up on the tafs side (kiff/tafs#22).
  **Suggested:** review #9; agree on #3 who grades this runtime from
  `cmd/eval`, now that the runner exists.
- **#12 (Runs).** Its "done when" holds: the transition table is tested
  against both stores, a paused run continues in a fresh `Runner`, and the
  benchmarks exist, and chat on runs merged (#34). **Suggested:** close #12.
  Continuing in production is #15's.
- **#16 (Registry)** and **#17 (Schedules)** are built up to what needs
  `cmd/lambda` wiring. Their remaining items move together with #13.
- **#14 and #15** have no owner.

## Facts that changed outside this repo

- **KIFF's withdraw call shipped.** ARCHITECTURE.md calls it "the likely
  bottleneck" for #15; it isn't any more. KIFF now has an agent-key call to
  withdraw a held call, and it tells alert targets when a hold expires. See
  [15-continue-on-hold.md](issues/15-continue-on-hold.md).
