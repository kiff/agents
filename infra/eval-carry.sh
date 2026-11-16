#!/usr/bin/env bash
# Decides a run of some suites for kiff/tafs' Upgrade gate (eval.yml's last
# step): the run counts when every suite it didn't measure passed in the
# newest earlier run on main that measured it, and nothing since touched it
# (cmd/evalsuites -carry, eval.Suites.Carry). So a red suite is rerun alone
# and the rest of a full run's verdict stands, until a change touches it.
#
#	infra/eval-carry.sh <suites as eval.yml's steps read them, ",connect,">
#
# Needs GH_TOKEN, a full clone of kiff/tafs at ./tafs (eval.yml's
# definitions step) and this repository's history (fetch-depth 0). "Touched"
# is cmd/evalsuites over kiff/tafs' definition sources (the ones kiff/tafs'
# Upgrade gate compares) from that run's eval/tafs.ref to this one's, and
# over kiff/agents from that run's commit to this one. A change neither
# names (a planque deploy, tafs cloud's runtime) isn't seen here, as it
# isn't by a full run's verdict either.
set -euo pipefail
[ $# -eq 1 ] || { echo "usage: $0 <,suites,>" >&2; exit 2; }
list=$1
sources="runtime/src/jobs.ts runtime/src/kiff-skill.ts runtime/skills runtime/src/cloud/tafdef.ts runtime/src/cloud/setupdef.ts"
ref=$(tr -d '[:space:]' < eval/tafs.ref)
bin=${RUNNER_TEMP:-/tmp}/evalsuites
go build -o "$bin" ./cmd/evalsuites

# Earlier runs on main, newest first: never this one, an override, or a run
# at another repeat than the bar's (their titles say so, eval.yml's run-name).
runs=$(gh api "repos/${GITHUB_REPOSITORY}/actions/workflows/eval.yml/runs?branch=main&status=completed&per_page=20" \
  --jq ".workflow_runs[] | select(.id != ${GITHUB_RUN_ID}) | select(.display_title | (contains(\", override \") or contains(\", repeat \")) | not) | \"\(.id) \(.head_sha)\"")

earlier='[]'
while read -r id sha; do
  [ -n "$id" ] || continue
  if git cat-file -e "$sha^{commit}" 2>/dev/null && their=$(git show "$sha:eval/tafs.ref" 2>/dev/null | tr -d '[:space:]') \
    && git -C tafs cat-file -e "$their^{commit}" 2>/dev/null; then
    touched=$({ git -C tafs diff --name-only "$their" "$ref" -- $sources | "$bin" -repo kiff/tafs
      git diff --name-only "$sha" HEAD | "$bin" -repo kiff/agents; } | tr ',' '\n' | sed '/^$/d' | jq -R . | jq -s .)
  else
    touched='["all"]' # its commit or its tafs ref can't be read: nothing carries from it
  fi
  steps=$(gh api "repos/${GITHUB_REPOSITORY}/actions/runs/$id/jobs" --jq '[.jobs[].steps[] | {(.name): (.conclusion // "")}] | add // {}')
  earlier=$(jq -c --argjson id "$id" --argjson steps "$steps" --argjson touched "$touched" '. + [{id: $id, steps: $steps, touched: $touched}]' <<<"$earlier")
done <<<"$runs"

echo "$earlier" | "$bin" -carry -asked "$list" | tee -a "$GITHUB_STEP_SUMMARY"
