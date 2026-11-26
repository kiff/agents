#!/usr/bin/env bash
# The Eval on Claude in Amazon Bedrock: lets an Eval role call Claude through
# the EU inference profiles, with the document infra/aws.sh gives the function
# under AGENTS_BEDROCK=on (infra/bedrock-policy.sh). Nothing else: not the
# function's role, no setting.
#   - main's Eval role (agents-github-eval, the one eval.yml assumes on main):
#     Sonnet and Opus. The Eval goes there once the repository variable
#     EVAL_MODEL_SOURCE is bedrock.
#   - the branches' Eval role (agents-github-eval-branches, trusted from any
#     pushed branch of kiff/agents): Sonnet only, the judges' model, so a
#     branch's run on Opus is refused. A branch's Eval goes there once the
#     repository variable EVAL_BRANCH_MODEL_SOURCE is bedrock. Its spend has
#     no daily cap of ours (the Eval is exempt), so it is granted only with
#     an AWS budget on it in place (docs/runs.md).
#
#   infra/eval-bedrock.sh                 # grant main's role
#   infra/eval-bedrock.sh down            # take it back
#   infra/eval-bedrock.sh branches        # grant the branches' role
#   infra/eval-bedrock.sh branches down   # take it back
#
# Config: AGENTS_REGION (eu-west-1).
set -euo pipefail
trap 'echo "eval-bedrock: failed at line $LINENO" >&2' ERR
cd "$(dirname "$0")/.."
. infra/bedrock-policy.sh

REGION=${AGENTS_REGION:-eu-west-1}
ROLE=agents-github-eval
MODELS=()
VARIABLE=EVAL_MODEL_SOURCE
if [ "${1:-}" = branches ]; then
  ROLE=agents-github-eval-branches MODELS=(claude-sonnet-5-5) VARIABLE=EVAL_BRANCH_MODEL_SOURCE
  shift
fi
aws() ( command aws --region "$REGION" --output text "$@" )

if [ "${1:-}" = down ]; then
  aws iam delete-role-policy --role-name "$ROLE" --policy-name bedrock-backup 2>/dev/null || true
  echo "eval-bedrock: $ROLE may no longer call Bedrock" >&2
  exit 0
fi
ACCOUNT=$(aws sts get-caller-identity --query Account)
aws iam put-role-policy --role-name "$ROLE" --policy-name bedrock-backup --policy-document "$(bedrock_policy "$REGION" "$ACCOUNT" ${MODELS[@]+"${MODELS[@]}"})"
echo "eval-bedrock: $ROLE may call the eu. Claude profiles (${MODELS[*]:-Sonnet and Opus}); set $VARIABLE=bedrock to run there" >&2
