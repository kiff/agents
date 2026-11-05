#!/usr/bin/env bash
# eval-override.sh <defs dir> <model> <effort>
#
# An Eval comparing models or efforts (eval.yml's model and effort inputs):
# after infra/eval-definitions.sh wrote kiff/tafs' definitions, sets every
# one, Coach's (setup.json) and each taf's, to run on <model> at <effort>,
# with <model>'s prices, so the runs' costs and cost caps count what that
# model charges. An empty <model> or <effort> leaves that field as kiff/tafs
# defines it. A model this table doesn't price is refused: its cost caps
# would count another model's prices. source.json records what was set.
#
# Prices are millionths of a US dollar per million tokens, as the
# definitions carry them. Sonnet 5.5's input and output are the Eval judge's
# price (eval.yml's -judge-price); its cache prices keep Opus 5.5's ratios
# to input (a read 1/20, a write 5/4, an hour's write 2x), and the fast
# output costs twice the standard, as Opus's does.
set -euo pipefail
[ $# -eq 3 ] || { echo "usage: $0 <defs dir> <model> <effort>" >&2; exit 2; }
defs=$1 model=$2 effort=$3

price='' fast=''
case "$model" in
  "") ;;
  claude-opus-5-5)
    price='{"Input":4000000,"Output":20000000,"CacheRead":200000,"CacheWrite":5000000,"CacheWrite1h":8000000}'
    fast='{"Input":8000000,"Output":40000000,"CacheRead":400000,"CacheWrite":10000000,"CacheWrite1h":16000000}' ;;
  claude-sonnet-5-5)
    price='{"Input":2000000,"Output":10000000,"CacheRead":100000,"CacheWrite":2500000,"CacheWrite1h":4000000}'
    fast='{"Input":4000000,"Output":20000000,"CacheRead":200000,"CacheWrite":5000000,"CacheWrite1h":8000000}' ;;
  *) echo "eval-override: no prices for model \"$model\": add them here first" >&2; exit 2 ;;
esac
case "$effort" in
  ""|low|medium|high|xhigh|max) ;;
  *) echo "eval-override: effort is low, medium, high, xhigh or max, not \"$effort\"" >&2; exit 2 ;;
esac
[ -n "$model$effort" ] || { echo "eval-override: nothing to set"; exit 0; }

n=0
for f in "$defs"/*.json; do
  [ "$(basename "$f")" = source.json ] && continue
  jq --arg model "$model" --arg effort "$effort" --argjson price "${price:-null}" --argjson fast "${fast:-null}" '
    (if $model != "" then .model = $model | .price = $price else . end)
    | (if $model != "" and has("price_fast") then .price_fast = $fast else . end)
    | (if $effort != "" then .effort = $effort else . end)' "$f" > "$f.tmp"
  mv "$f.tmp" "$f"
  n=$((n + 1))
done
jq --arg model "$model" --arg effort "$effort" '.override = {model: $model, effort: $effort}' "$defs/source.json" > "$defs/source.json.tmp"
mv "$defs/source.json.tmp" "$defs/source.json"
echo "eval-override: ${n} definitions on ${model:-their own model}, effort ${effort:-their own}"
