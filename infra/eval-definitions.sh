#!/bin/sh
# Writes each tafs job's agent definition, as kiff/tafs builds it, for
# eval -run -definitions (#78):
#
#	infra/eval-definitions.sh <tafs checkout> <out dir>
#
# <out>/<job>.json is tafdef.ts' definition() for the job, with no owner
# notes; <out>/setup.json is setupdef.ts' setup agent; <out>/skills holds
# the skills those definitions may name (runtime/skills, kiff/tafs#218), when
# tafs has any; <out>/source.json names the tafs commit it came from. Check out
# tafs at eval/tafs.ref first, so the eval runs what that commit runs:
#
#	git clone https://github.com/kiff/tafs && git -C tafs checkout "$(cat eval/tafs.ref)"
set -eu
[ $# -eq 2 ] || { echo "usage: $0 <tafs checkout> <out dir>" >&2; exit 2; }
tafs=$(cd "$1" && pwd)
mkdir -p "$2"
out=$(cd "$2" && pwd)
commit=$(git -C "$tafs" rev-parse HEAD)
(cd "$tafs/runtime" && npm ci --ignore-scripts --no-audit --no-fund >/dev/null && npx tsc -p .)
node --input-type=module - "$tafs/runtime/dist" "$out" "$commit" <<'JS'
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
const [dist, out, commit] = process.argv.slice(2);
const { definition } = await import(pathToFileURL(join(dist, "cloud/tafdef.js")).href);
const { JOBS } = await import(pathToFileURL(join(dist, "jobs.js")).href);
for (const job of JOBS) {
  writeFileSync(join(out, `${job.id}.json`), JSON.stringify(definition(`eval-${job.id}`, job), null, 2) + "\n");
}
// Setup too (#78's setup case): its definition, for cmd/e2e -setup.
const { setupDefinition } = await import(pathToFileURL(join(dist, "cloud/setupdef.js")).href);
writeFileSync(join(out, "setup.json"), JSON.stringify(setupDefinition("eval-setup"), null, 2) + "\n");
writeFileSync(join(out, "source.json"), JSON.stringify({ repo: "kiff/tafs", commit, jobs: JOBS.map((j) => j.id) }, null, 2) + "\n");
console.log(`wrote ${JOBS.length} job definitions and Setup's from kiff/tafs@${commit.slice(0, 12)} to ${out}`);
JS
rm -rf "$out/skills"
if [ -d "$tafs/runtime/skills" ]; then
  cp -R "$tafs/runtime/skills" "$out/skills"
  echo "copied kiff/tafs' skills to $out/skills"
fi
