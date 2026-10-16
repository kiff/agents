# Evals as the development loop: what #78 delivers

**Issue:** [#78](https://github.com/kiff/agents/issues/78) · **Status:** in progress.
**PRs:** #82

## What it's for

Run the eval cases on this runtime every night and by hand, with what tafs
runs, and fail when a score drops. Replaces #9, which graded Managed Agents
sessions.

## Delivered

**A run leaves nothing held.** An eval run that KIFF held a call of is
graded where it paused and never continued. `eval -run` then withdraws each
of its held calls with the agent's own key, so none waits on the owner or
carries into the next run:

```
POST https://api.kiff.dev/v1/me/holds/{exception}/withdraw
Authorization: Bearer <KIFF_AGENT_KEY>
```

```
eval: chargebacks: withdrew KIFF's hold on update_dispute (exc_…)
eval: chargebacks: couldn't withdraw update_dispute (exc_…): …: refuse it in KIFF
```

- The exception comes from the run's requirement (`run.Requirement.Exception`).
- It runs whether the run ended, paused or failed, on its own 30-second
  timeout so a Ctrl+C doesn't skip it. A hold it can't withdraw is logged,
  never fatal.
- `-kiff-api` (https only, default `https://api.kiff.dev`) is where.
- KIFF has to let the agent's gateway key reach the route: kiff/kiff-cloud#1250.

## Next

- The definition from kiff/tafs at a pinned commit (`-definitions`).
- The report and a baseline in the repo.
- `.github/workflows/eval.yml`, nightly and by hand.
