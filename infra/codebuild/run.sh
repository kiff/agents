#!/usr/bin/env bash
# The Eval's backup run, inside AWS CodeBuild (infra/codebuild/buildspec.yml): the
# same suites, with the same commands, limits and checks, as eval.yml's full
# run on main, for when GitHub won't start eval.yml. A test
# (infra/codebuild/codebuild_test.go) fails when a suite's command here differs from
# eval.yml's, so the two can't drift apart.
#
# It runs only what can count for kiff/tafs' Upgrade gate: every suite, at
# the bar's repeat (3), on the shipped definitions (no model or effort
# override), on Bedrock. There are no inputs: a build started with other
# settings than the project's own doesn't count (the gate compares them).
#
# Each suite runs whatever the ones before it did, as eval.yml's steps do
# (always()), within its own limit; the build fails when any suite failed, or
# when a deploy landed during the run (mixed), as eval.yml's run does.
set -uo pipefail
cd "$(dirname "$0")/../.."

REPEAT=3
export REPORTS=${REPORTS:-kiff-agents-eval-reports}
export E2E_MADE=e2e-made.txt
export AGENTS_MODEL_SOURCE=bedrock
# eval.yml's steps write their notes to the run's summary; here it's a file,
# printed at the end.
export GITHUB_STEP_SUMMARY=$PWD/summary.md
: > "$GITHUB_STEP_SUMMARY"
failed=""

# suite NAME MINUTES COMMAND: runs COMMAND within MINUTES, as eval.yml's
# step of that name does within its timeout-minutes, and notes a failure.
suite() {
  local name=$1 minutes=$2
  shift 2
  echo "::group::$name"
  if timeout --kill-after=60 "${minutes}m" bash -c "$1"; then
    echo "$name: passed"
  else
    echo "$name: FAILED"
    failed="$failed\n- $name"
  fi
  echo "::endgroup::"
}

# The commit: main's, never a branch's. CodeBuild checked out the source
# version it was started with; it must be on main.
git fetch --quiet https://github.com/kiff/agents main
head=$(git rev-parse HEAD)
if ! git merge-base --is-ancestor "$head" FETCH_HEAD; then
  echo "eval-codebuild: $head isn't on kiff/agents' main: not run" >&2
  exit 1
fi
echo "kiff/agents at $head, kiff/tafs at $(cat eval/tafs.ref), suites all, repeat $REPEAT, on Bedrock"

# tafs' definitions, at eval/tafs.ref, as eval.yml builds them.
git clone --quiet https://github.com/kiff/tafs tafs
git -C tafs checkout --quiet "$(cat eval/tafs.ref)"
infra/eval-definitions.sh tafs defs || { echo "eval-codebuild: the definitions didn't build" >&2; exit 1; }
echo "Definitions built with TAFS_YOUR_STEP=${TAFS_YOUR_STEP:-off}." >> "$GITHUB_STEP_SUMMARY"

infra/eval-deployed.sh > deployed-start.txt || true

suite "Chargebacks, two seeded runs" 30 '
          go run ./cmd/eval -run -public -definitions defs $([ -d defs/skills ] && echo -skills defs/skills) -seed -repeat "'"$REPEAT"'" \
            -baseline eval/baseline.json -gate \
            -judge-model claude-sonnet-5-5 -judge-price 2000000,10000000 \
            -out results.json -report report.md chargebacks'

suite "Hand-offs between tafs, a sender and a receiver" 60 '
          if [ ! -f defs/handoff-task.json ] || [ ! -f defs/chargebacks-handoff.json ]; then
            echo "Hand-offs between tafs: **not measured**. kiff/tafs at eval/tafs.ref writes no hand-off definitions." >> "$GITHUB_STEP_SUMMARY"
            exit 0
          fi
          go run ./cmd/eval -run -public -definitions defs $([ -d defs/skills ] && echo -skills defs/skills) -seed -gate -repeat "'"$REPEAT"'" \
            -judge-model claude-sonnet-5-5 -judge-price 2000000,10000000 \
            -out results-handoff.json -report report-handoff.md handoff-sender handoff-nothing handoff-nothing-fr handoff-receiver'

suite "API doors, a request from another application" 30 '
          if [ ! -f defs/door-accept.json ] || [ ! -f defs/door-evidence.json ]; then
            echo "API doors: **not measured**. kiff/tafs at eval/tafs.ref writes no door requests." >> "$GITHUB_STEP_SUMMARY"
            exit 0
          fi
          go run ./cmd/eval -run -public -definitions defs $([ -d defs/skills ] && echo -skills defs/skills) -seed -gate -repeat "'"$REPEAT"'" \
            -judge-model claude-sonnet-5-5 -judge-price 2000000,10000000 \
            -out results-doors.json -report report-doors.md door-accept door-evidence'

suite "Tickets, a recipe's taf on the fake desk" 10 '
          if [ ! -f defs/recipe-acknowledge-every-ticket.json ]; then
            echo "Tickets: **not measured**. kiff/tafs at eval/tafs.ref has no recipe acknowledge-every-ticket." >> "$GITHUB_STEP_SUMMARY"
            exit 0
          fi
          go run ./cmd/eval -run -public -definitions defs $([ -d defs/skills ] && echo -skills defs/skills) -gate -repeat "'"$REPEAT"'" \
            -judge-model claude-sonnet-5-5 -judge-price 2000000,10000000 \
            -out results-tickets.json -report report-tickets.md tickets'

suite "Setup with the taf, in English, graded" 45 '
          go run ./cmd/e2e $([ -d defs/skills ] && echo -skills defs/skills) -setup defs/setup.json -taf defs/refunds.json -owner en -grade -improve -max-cost 4 -owner-turns 14 -repeat "'"$REPEAT"'" -transcript setup.md -judge-model claude-sonnet-5-5 -judge-price 2000000,10000000'

suite "Setup on the second vendor, a desk by its key" 15 '
          go run ./cmd/e2e $([ -d defs/skills ] && echo -skills defs/skills) -setup defs/setup.json -desk -grade -max-cost 3 -owner-turns 14 -repeat "'"$REPEAT"'" -transcript desk.md -judge-model claude-sonnet-5-5 -judge-price 2000000,10000000'

suite "Setup on billing, invoices that stay drafts" 15 '
          go run ./cmd/e2e $([ -d defs/skills ] && echo -skills defs/skills) -setup defs/setup.json -invoices -grade -max-cost 3 -owner-turns 14 -repeat "'"$REPEAT"'" -transcript invoices.md -judge-model claude-sonnet-5-5 -judge-price 2000000,10000000'

suite "Replay, introductions from a list" 25 '
          go run ./cmd/e2e $([ -d defs/skills ] && echo -skills defs/skills) -setup defs/setup.json -replay intros -grade -max-cost 3 -owner-turns 14 -repeat "'"$REPEAT"'" -transcript replay-intros.md -judge-model claude-sonnet-5-5 -judge-price 2000000,10000000'

suite "Plan words, Google Sheets in English" 10 '
          go run ./cmd/e2e $([ -d defs/skills ] && echo -skills defs/skills) -setup defs/setup.json -plan sheets -owner en -grade -max-cost 2 -owner-turns 8 -repeat "'"$REPEAT"'" -transcript plan-sheets.md -judge-model claude-sonnet-5-5 -judge-price 2000000,10000000'

suite "Plan words, Google Sheets in French" 10 '
          go run ./cmd/e2e $([ -d defs/skills ] && echo -skills defs/skills) -setup defs/setup.json -plan sheets -owner fr -grade -max-cost 2 -owner-turns 8 -repeat "'"$REPEAT"'" -transcript plan-sheets-fr.md -judge-model claude-sonnet-5-5 -judge-price 2000000,10000000'

suite "Plan words, Gmail in English" 10 '
          go run ./cmd/e2e $([ -d defs/skills ] && echo -skills defs/skills) -setup defs/setup.json -plan gmail -owner en -grade -max-cost 2 -owner-turns 8 -repeat "'"$REPEAT"'" -transcript plan-gmail.md -judge-model claude-sonnet-5-5 -judge-price 2000000,10000000'

suite "Route, the watch's requests over ten tafs" 10 '
          if ! jq -e '"'"'[.. | objects | select(.name? == "ask-a-taf")] | length > 0'"'"' defs/setup.json >/dev/null 2>&1; then
            echo "Route: **not measured**. kiff/tafs at eval/tafs.ref has no ask-a-taf." >> "$GITHUB_STEP_SUMMARY"
            exit 0
          fi
          go run ./cmd/e2e $([ -d defs/skills ] && echo -skills defs/skills) -setup defs/setup.json -route -grade -repeat "'"$REPEAT"'" -transcript route.md -judge-model claude-sonnet-5-5 -judge-price 2000000,10000000'

# The sweeps never decide the run, as in eval.yml.
go run ./cmd/e2e -sweep || echo "the sweep of leftover tafs failed (reported, not the verdict)"
go run ./cmd/eval seed -close-all chargebacks || echo "the sweep of open disputes failed (reported, not the verdict)"

mixed=false
infra/eval-deployed.sh > deployed-end.txt || echo "the end's read failed: the run counts as mixed"
infra/eval-deployed.sh compare deployed-start.txt deployed-end.txt > deployed.md || mixed=true
cat deployed.md

# The full report, to the private bucket, under reports/codebuild/ (the
# build's role may write nowhere else).
key="reports/codebuild/$(date -u +%Y-%m-%d)-${CODEBUILD_BUILD_NUMBER:-0}"
for f in report.md results.json report-handoff.md results-handoff.json report-doors.md results-doors.json report-tickets.md results-tickets.json setup*.md desk*.md invoices*.md replay-intros*.md plan-*.md route*.md deployed.md summary.md; do
  [ -f "$f" ] && aws s3 cp --only-show-errors "$f" "s3://$REPORTS/$key/$f"
done
echo "report: s3://$REPORTS/$key/"

cat "$GITHUB_STEP_SUMMARY"
if [ -n "$failed" ]; then
  printf "Failed:%b\n" "$failed"
  exit 1
fi
if [ "$mixed" = true ]; then
  echo "Mixed: a deploy landed during this run, or a side couldn't be read. It doesn't count for the Upgrade gate."
  exit 1
fi
echo "Every suite passed."
