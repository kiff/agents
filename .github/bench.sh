#!/usr/bin/env bash
# bench.sh BASE runs the benchmarks of BASE (a git ref) and of the working
# tree in turns, on one machine, and fails when one is clearly slower in the
# working tree: its time per op up by more than THRESHOLD percent (default
# 20), and benchstat's test says the change is real (p < 0.05). Benchmarks
# only one side has are left out. Needs benchstat on PATH.
set -euo pipefail

base=${1:?usage: bench.sh BASE}
rounds=${ROUNDS:-8}
threshold=${THRESHOLD:-20}

repo=$(pwd)
dir=$(mktemp -d)
git worktree add --quiet --detach "$dir/base" "$base"
trap 'git -C "$repo" worktree remove --force "$dir/base"; rm -rf "$dir"' EXIT

bench() { go test -run '^$' -bench . -benchtime "${BENCHTIME:-200ms}" -count 1 ./...; }

# In turns, so a machine that speeds up or slows down part way through
# weighs on both sides alike.
for _ in $(seq "$rounds"); do
	(cd "$dir/base" && bench) >>"$dir/base.txt"
	bench >>"$dir/head.txt"
done

cd "$dir"
benchstat base.txt head.txt

# In the CSV each unit has a table of its own. In a sec/op row, the sixth
# field is the change: "+25.00%", or "~" when the test finds none.
benchstat -format csv base.txt head.txt 2>/dev/null | awk -F, -v limit="$threshold" -v base="$base" '
	$2 == "sec/op" { timing = 1; next }
	$1 == "" { timing = 0 }
	timing && $1 != "geomean" && $6 ~ /^\+[0-9.]+%$/ && substr($6, 2) + 0 > limit {
		printf "%s is %s slower than on %s\n", $1, $6, base
		slow = 1
	}
	END { exit slow }
'
