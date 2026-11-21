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
# is cmd/evalsuites (eval/suites.json's paths, where anything unmapped
# touches every suite) over:
#   - kiff/tafs, every path from that run's eval/tafs.ref to this one's, and
#     every path changed on tafs' main since that run started: tafs cloud
#     deploys from main, not from the ref, and the setups call it live;
#   - kiff/agents, from that run's commit to this one;
#   - kiff/planque, every path changed on its main since that run started
#     (its deploys follow main): its guides touch Coach's suites, its docs
#     none, the rest every planque suite. When the paths can't be read (no
#     commit before that run, or GitHub's 300-file cap), every planque suite.
# Only runs of the last 7 days carry, which bounds what no diff sees (KIFF,
# the model provider).
set -euo pipefail
[ $# -eq 1 ] || { echo "usage: $0 <,suites,>" >&2; exit 2; }
list=$1
ref=$(tr -d '[:space:]' < eval/tafs.ref)
bin=${RUNNER_TEMP:-/tmp}/evalsuites
go build -o "$bin" ./cmd/evalsuites

# Earlier runs on main of the last 7 days, newest first: never this one, an
# override, or a run at another repeat than the bar's (their titles say so,
# eval.yml's run-name).
runs=$(gh api "repos/${GITHUB_REPOSITORY}/actions/workflows/eval.yml/runs?branch=main&status=completed&per_page=20" \
  --jq ".workflow_runs[] | select(.id != ${GITHUB_RUN_ID}) | select((.created_at | fromdateiso8601) > (now - 7 * 86400))
    | select(.display_title | (contains(\", override \") or contains(\", repeat \")) | not) | \"\(.id) \(.head_sha) \(.created_at)\"")

earlier='[]'
while read -r id sha started; do
  [ -n "$id" ] || continue
  if git cat-file -e "$sha^{commit}" 2>/dev/null && their=$(git show "$sha:eval/tafs.ref" 2>/dev/null | tr -d '[:space:]') \
    && git -C tafs cat-file -e "$their^{commit}" 2>/dev/null; then
    planque=$(gh api "repos/kiff/planque/commits?sha=main&since=$started&per_page=1" --jq length)
    planque_paths=main
    if [ "$planque" != 0 ]; then
      before=$(gh api "repos/kiff/planque/commits?sha=main&until=$started&per_page=1" --jq '.[0].sha // empty')
      if [ -n "$before" ]; then
        changed=$(gh api "repos/kiff/planque/compare/$before...main" --jq '.files | if length < 300 then .[].filename else "main" end')
        [ -z "$changed" ] || planque_paths=$changed
      fi
    fi
    touched=$({ { git -C tafs diff --name-only "$their" "$ref"
        git -C tafs log --format= --name-only --since="$started" origin/main; } | sort -u | "$bin" -repo kiff/tafs
      git diff --name-only "$sha" HEAD | "$bin" -repo kiff/agents
      # "main" maps as any planque path: what can't be read touches every planque suite.
      [ "$planque" = 0 ] || echo "$planque_paths" | "$bin" -repo kiff/planque; } | tr ',' '\n' | sed '/^$/d' | jq -R . | jq -s .)
  else
    touched='["all"]' # its commit or its tafs ref can't be read: nothing carries from it
  fi
  # A step name in two jobs counts as its worst: any other conclusion over
  # success, success over skipped.
  steps=$(gh api "repos/${GITHUB_REPOSITORY}/actions/runs/$id/jobs" --jq '[.jobs[].steps[] | {name, c: (.conclusion // "")}]
    | group_by(.name) | map({(.[0].name): ([.[].c] | if any(. != "success" and . != "skipped" and . != "") then (map(select(. != "success" and . != "skipped" and . != ""))[0]) elif any(. == "success") then "success" else "skipped" end)}) | add // {}')
  earlier=$(jq -c --argjson id "$id" --argjson steps "$steps" --argjson touched "$touched" '. + [{id: $id, steps: $steps, touched: $touched}]' <<<"$earlier")
done <<<"$runs"

echo "$earlier" | "$bin" -carry -asked "$list" | tee -a "$GITHUB_STEP_SUMMARY"
