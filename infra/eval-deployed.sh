#!/usr/bin/env bash
# What tafs cloud, planque and agents run live, for an Eval's report: each
# one's newest deploy job on main that succeeded, read from GitHub's API (the
# three repositories are public, so the run's own token reads them). A deploy
# that fails puts the code before it back, so the newest success is what runs.
# eval.yml takes it at the start and at the end: a deploy between the two
# makes the run mixed, which counts for kiff/tafs' Upgrade gate only once
# judged (its rule-B check). The log and the report carry commits and times
# only, never anything a function holds.
#
#	infra/eval-deployed.sh                   one line per service:
#	    <service> <commit> <finished, UTC> <run id>, and "<service> deploying <run id>"
#	    while a newer deploy of it is under way
#	infra/eval-deployed.sh compare START END the report's block on stdout;
#	    exit 0 when nothing changed, 3 when the run is mixed (or a side is
#	    missing, which proves nothing)
#
# Needs GH_TOKEN for the first form.
set -euo pipefail

# service repository workflow deploy-job
SERVICES="tafs-cloud kiff/tafs runtime.yml deploy
planque kiff/planque ci.yml deploy
agents kiff/agents deploy.yml deploy"

api() { # api <path> [gh api's flags]: three tries, GitHub's API answers a 502 now and then
	local i
	for i in 1 2 3; do
		gh api "$@" </dev/null && return 0
		sleep $((i * 5))
	done
	return 1
}

snapshot() {
	local service repo workflow job runs id jobs line found
	while read -r service repo workflow job; do
		runs=$(api "repos/$repo/actions/workflows/$workflow/runs?branch=main&per_page=15" \
			--jq '.workflow_runs[] | select(.event == "push" or .event == "workflow_dispatch" or .event == "workflow_run") | "\(.id) \(.head_sha)"')
		found=""
		while read -r id sha; do
			[ -n "$id" ] || continue
			jobs=$(api "repos/$repo/actions/runs/$id/jobs" \
				--jq ".jobs[] | select(.name == \"$job\") | \"\\(.status) \\(.conclusion) \\(.completed_at)\"")
			[ -n "$jobs" ] || continue
			read -r status conclusion at <<<"$jobs"
			if [ "$status" != completed ]; then
				echo "$service deploying $id"
				continue
			fi
			if [ "$conclusion" = success ]; then
				echo "$service $sha $at $id"
				found=1
				break
			fi
		done <<<"$runs"
		[ -n "$found" ] || echo "$service unknown"
	done <<<"$SERVICES"
}

side() { # side <service> <snapshot>: its lines there, "commit at run" shortened, joined with "; "
	grep -E "^$1 " "$2" 2>/dev/null | cut -d' ' -f2- |
		awk '$1 ~ /^[0-9a-f]{40}$/ { $1 = substr($1, 1, 12) } { print }' | paste -sd';' - | sed 's/;/; /g' || true
}

compare() { # compare <start> <end>
	local start=$1 end=$2 mixed="" service a b
	echo "## Deployed"
	echo
	echo "Each service's newest successful deploy on main, from GitHub's API, at the run's start and end."
	echo
	echo "| service | at the start | at the end |"
	echo "|---|---|---|"
	while read -r service _; do
		a=$(side "$service" "$start")
		b=$(side "$service" "$end")
		echo "| $service | ${a:-not read} | ${b:-not read} |"
		# Changed, under way at either end, or not read: the run can't show it ran on one version.
		if [ -z "$a" ] || [ "$a" != "$b" ] || [[ "$a" == *deploying* ]] || [[ "$b" == *deploying* ]] || [[ "$a" == *unknown* ]]; then
			mixed="$mixed $service"
		fi
	done <<<"$SERVICES"
	echo
	if [ -n "$mixed" ]; then
		echo "deployed: mixed:$mixed"
		return 3
	fi
	echo "deployed: unchanged"
}

case "${1:-}" in
"") snapshot ;;
compare)
	[ $# -eq 3 ] || { echo "usage: $0 [compare <start> <end>]" >&2; exit 2; }
	compare "$2" "$3"
	;;
*) echo "usage: $0 [compare <start> <end>]" >&2; exit 2 ;;
esac
