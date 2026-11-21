#!/usr/bin/env bash
# The Eval on Claude in Amazon Bedrock: lets main's Eval role
# (agents-github-eval, the one eval.yml assumes on main) call Claude through
# the EU inference profiles, with the same document infra/aws.sh gives the
# function under AGENTS_BEDROCK=on (infra/bedrock-policy.sh). Nothing else: not
# the function's role, not the branches' Eval role, no setting. The Eval goes
# there only once the repository variable EVAL_MODEL_SOURCE is bedrock.
#
#   infra/eval-bedrock.sh          # grant
#   infra/eval-bedrock.sh down     # take it back
#
# Config: AGENTS_REGION (eu-west-1).
set -euo pipefail
trap 'echo "eval-bedrock: failed at line $LINENO" >&2' ERR
cd "$(dirname "$0")/.."
. infra/bedrock-policy.sh

REGION=${AGENTS_REGION:-eu-west-1}
ROLE=agents-github-eval
aws() ( command aws --region "$REGION" --output text "$@" )

if [ "${1:-}" = down ]; then
  aws iam delete-role-policy --role-name "$ROLE" --policy-name bedrock-backup 2>/dev/null || true
  echo "eval-bedrock: $ROLE may no longer call Bedrock" >&2
  exit 0
fi
ACCOUNT=$(aws sts get-caller-identity --query Account)
aws iam put-role-policy --role-name "$ROLE" --policy-name bedrock-backup --policy-document "$(bedrock_policy "$REGION" "$ACCOUNT")"
echo "eval-bedrock: $ROLE may call the eu. Claude profiles; set EVAL_MODEL_SOURCE=bedrock to run the Eval there" >&2
