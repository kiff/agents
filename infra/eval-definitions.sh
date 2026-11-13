#!/bin/sh
# Writes each tafs job's agent definition, as kiff/tafs builds it, for
# eval -run -definitions (#78):
#
#	infra/eval-definitions.sh <tafs checkout> <out dir>
#
# <out>/<job>.json is tafdef.ts' definition() for the job, with no owner
# notes; <out>/setup.json is setupdef.ts' setup agent; <out>/skills holds
# the skills those definitions may name (runtime/skills, kiff/tafs#218), when
# tafs has any; <out>/chargebacks-handoff.json and <out>/handoff-task.json are
# a hand-off's taf and task, from a commit that hands work on (eval/cases,
# handoff.go); <out>/recipe-<id>.json is each recipe's taf as Coach would draft
# it from the recipe's words (eval/cases, tickets.go); <out>/source.json names
# the tafs commit it came from. Check out
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
// A taf that writes to people outside names who it writes for: tafs cloud
// takes the account's company name, and Coach asks for it. The Eval's
// account is a test shop, so each such job writes for one, and its
// disclosure reads as a real taf's ("on behalf of …"), not the bare line.
const owned = (job) => (job.disclose?.length && !job.writesFor ? { ...job, writesFor: "Acme Store" } : job);
for (const job of JOBS) {
  writeFileSync(join(out, `${job.id}.json`), JSON.stringify(definition(`eval-${job.id}`, owned(job)), null, 2) + "\n");
}
// Setup too (#78's setup case): its definition, for cmd/e2e -setup.
const { setupDefinition } = await import(pathToFileURL(join(dist, "cloud/setupdef.js")).href);
writeFileSync(join(out, "setup.json"), JSON.stringify(setupDefinition("eval-setup"), null, 2) + "\n");
// A hand-off between tafs (eval/cases, handoff.go), from a tafs commit whose
// tafs can say they have nothing to hand over: the chargebacks taf with a taf
// place, Books, which takes each accepted dispute, and tafs' own task for a
// hand-off, with marks where the sender's name, its hand-off and the fence go.
// An earlier commit writes neither, and the Eval's step says it measured none.
const places = await import(pathToFileURL(join(dist, "cloud/places.js")).href);
const delivery = await import(pathToFileURL(join(dist, "cloud/delivery.js")).href);
const handoffs = Boolean(places.NOTHING_TO_HAND && delivery.DELIVERY_WORDS?.handoffTask);
if (handoffs) {
  const books = { place: "taf", taf: "eval-books", name: "Books", fill: { disputes: "each dispute you accepted, by its reference and amount" } };
  const chargebacks = owned(JOBS.find((j) => j.id === "chargebacks"));
  writeFileSync(join(out, "chargebacks-handoff.json"), JSON.stringify(definition("eval-chargebacks-handoff", chargebacks, { actionResults: [books] }), null, 2) + "\n");
  writeFileSync(join(out, "handoff-task.json"), JSON.stringify({ task: delivery.DELIVERY_WORDS.handoffTask("@@FROM@@", "@@HANDOFF@@", "@@FENCE@@") }, null, 2) + "\n");
}
// Each recipe's taf (eval/cases, tickets.go), as Coach drafts a job of its own
// from the owner's words alone: its name, its line, and the recipe's words as
// how it works. A commit with no recipes writes none.
let recipes = [];
try {
  const { RECIPES } = await import(pathToFileURL(join(dist, "recipes.js")).href);
  const { ownJob } = await import(pathToFileURL(join(dist, "jobs.js")).href);
  for (const r of RECIPES ?? []) {
    const job = ownJob({ name: r.title, summary: r.line, kickoff: "Do the job now, as set out.", instructions: r.say });
    writeFileSync(join(out, `recipe-${r.id}.json`), JSON.stringify(definition(`eval-recipe-${r.id}`, job), null, 2) + "\n");
    recipes.push(r.id);
  }
} catch (e) {
  if (e?.code !== "ERR_MODULE_NOT_FOUND") throw e;
}
writeFileSync(join(out, "source.json"), JSON.stringify({ repo: "kiff/tafs", commit, jobs: JOBS.map((j) => j.id), recipes }, null, 2) + "\n");
console.log(`wrote ${JOBS.length} job definitions, ${recipes.length} recipes' and Setup's${handoffs ? ", and a hand-off's taf and task," : ""} from kiff/tafs@${commit.slice(0, 12)} to ${out}`);
JS
rm -rf "$out/skills"
# Every skill package, wherever kiff/tafs keeps it (Coach's sit at
# runtime/skills/coach/<name>/<version>/, kiff/tafs#525), as -skills reads them.
"$(dirname "$0")/copy-skill-packages.sh" "$tafs/runtime/skills" "$out/skills"
