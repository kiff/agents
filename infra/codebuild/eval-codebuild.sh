#!/usr/bin/env bash
# The Eval's backup runner on AWS CodeBuild, for when GitHub won't start
# eval.yml (26 Nov: dispatches sat queued with no jobs). It runs the same
# full Eval (infra/codebuild/run.sh) of a commit on main, on Bedrock, and
# kiff/tafs' Upgrade gate can read its result from CodeBuild's own record,
# through a read-only role (infra/codebuild/policy.sh). Every step here is an
# AWS change: run it on the owner's typed yes, from an admin session.
#
#   eval-codebuild.sh secrets   # the nine Eval secrets into Secrets Manager,
#                               # read from the terminal, never echoed
#   eval-codebuild.sh setup     # the build's role, its log group, the project,
#                               # and the gate's read-only role
#   eval-codebuild.sh start SHA # one full Eval of SHA, a kiff/agents commit
#                               # on main; refuses while the test account is
#                               # in use on GitHub
#   eval-codebuild.sh down      # removes the project and both roles (the
#                               # secrets stay, for `secrets` to replace)
#
# There's no way to start a build with other suites, another repeat, a model
# or effort, or another buildspec: none would count for the gate, which
# compares a build's settings with the project's.
#
# Config: AGENTS_REGION (eu-west-1), AGENTS_EVAL_BUCKET (kiff-agents-eval-reports).
set -euo pipefail
trap 'echo "eval-codebuild: failed at line $LINENO" >&2' ERR
cd "$(dirname "$0")/../.."
. infra/bedrock-policy.sh
. infra/codebuild/policy.sh

REGION=${AGENTS_REGION:-eu-west-1}
BUCKET=${AGENTS_EVAL_BUCKET:-kiff-agents-eval-reports}
PROJECT=$CODEBUILD_PROJECT
BUILD_ROLE=agents-eval-codebuild
GATE_ROLE=agents-eval-codebuild-gate
LOGS=/aws/codebuild/$PROJECT
SECRETS="ANTHROPIC_API_KEY KIFF_AGENT_KEY STRIPE_API_KEY E2E_KIFF_SESSION E2E_SETUP_KEY E2E_PHONE_KEY E2E_VENDOR E2E_VENDOR_TOKEN E2E_DESK_KEY"
aws() ( command aws --region "$REGION" --output text "$@" )
say() { echo "eval-codebuild: $*" >&2; }

secrets() {
  local s v
  for s in $SECRETS; do
    read -rsp "$s (empty: keep the stored one): " v
    echo >&2
    [ -n "$v" ] || continue
    if aws secretsmanager describe-secret --secret-id "agents-eval/$s" >/dev/null 2>&1; then
      aws secretsmanager put-secret-value --secret-id "agents-eval/$s" --secret-string "$v" >/dev/null
    else
      aws secretsmanager create-secret --name "agents-eval/$s" --secret-string "$v" >/dev/null
    fi
    say "stored agents-eval/$s"
  done
}

role() { # role NAME TRUST: creates it, or replaces its trust
  if aws iam get-role --role-name "$1" >/dev/null 2>&1; then
    aws iam update-assume-role-policy --role-name "$1" --policy-document "$2"
  else
    aws iam create-role --role-name "$1" --assume-role-policy-document "$2" >/dev/null
  fi
}

setup() {
  local account project_json
  account=$(aws sts get-caller-identity --query Account)
  role "$BUILD_ROLE" "$(codebuild_trust "$REGION" "$account" "$PROJECT")"
  # shellcheck disable=SC2086
  aws iam put-role-policy --role-name "$BUILD_ROLE" --policy-name eval-codebuild \
    --policy-document "$(codebuild_policy "$REGION" "$account" "$PROJECT" "$BUCKET" $SECRETS)"
  aws iam put-role-policy --role-name "$BUILD_ROLE" --policy-name bedrock-backup \
    --policy-document "$(bedrock_policy "$REGION" "$account")"
  aws logs create-log-group --log-group-name "$LOGS" 2>/dev/null || true
  aws logs put-retention-policy --log-group-name "$LOGS" --retention-in-days 90
  # The repository's variables the Eval reads, as plain settings: neither is
  # a secret. Run setup again when either changes.
  local stripe_account your_step
  stripe_account=$(gh variable get EVAL_STRIPE_ACCOUNT -R kiff/agents)
  your_step=$(gh variable get TAFS_YOUR_STEP -R kiff/agents 2>/dev/null || true)
  project_json=$(cat <<JSON
{"name":"$PROJECT",
 "description":"The Eval's backup runner: a full Eval of a kiff/agents commit on main (infra/codebuild)",
 "source":{"type":"GITHUB","location":"https://github.com/kiff/agents.git","gitCloneDepth":0,
           "buildspec":"infra/codebuild/buildspec.yml","reportBuildStatus":false},
 "artifacts":{"type":"NO_ARTIFACTS"},
 "environment":{"type":"LINUX_CONTAINER","image":"aws/codebuild/standard:7.0","computeType":"BUILD_GENERAL1_MEDIUM",
   "privilegedMode":false,
   "environmentVariables":[
     {"name":"EVAL_STRIPE_ACCOUNT","value":"$stripe_account","type":"PLAINTEXT"},
     {"name":"TAFS_YOUR_STEP","value":"$your_step","type":"PLAINTEXT"},
     {"name":"REPORTS","value":"$BUCKET","type":"PLAINTEXT"},
     {"name":"AWS_REGION","value":"$REGION","type":"PLAINTEXT"}]},
 "serviceRole":"arn:aws:iam::$account:role/$BUILD_ROLE",
 "timeoutInMinutes":285,
 "queuedTimeoutInMinutes":60,
 "concurrentBuildLimit":1,
 "logsConfig":{"cloudWatchLogs":{"status":"ENABLED","groupName":"$LOGS"}}}
JSON
)
  if aws codebuild batch-get-projects --names "$PROJECT" --query 'projects[0].name' | grep -qx "$PROJECT"; then
    aws codebuild update-project --cli-input-json "$project_json" >/dev/null
  else
    # A new role takes a few seconds before CodeBuild may use it.
    sleep 10
    aws codebuild create-project --cli-input-json "$project_json" >/dev/null
  fi
  role "$GATE_ROLE" "$(gate_trust "$account")"
  aws iam put-role-policy --role-name "$GATE_ROLE" --policy-name eval-codebuild-gate \
    --policy-document "$(gate_policy "$REGION" "$account" "$PROJECT")"
  say "project $PROJECT ready; no webhook, no schedule: it runs only from 'start'"
  say "gate role: set it as kiff/tafs' eval-gate environment secret AWS_EVAL_GATE_ROLE: aws iam get-role --role-name $GATE_ROLE --query Role.Arn --output text"
}

start() {
  local sha=${1:?start SHA: a kiff/agents commit on main} status busy id
  [[ "$sha" =~ ^[0-9a-f]{40}$ ]] || { say "give the full 40-character commit"; exit 2; }
  # On main: main is the commit or after it.
  status=$(gh api "repos/kiff/agents/compare/$sha...main" --jq .status)
  case "$status" in identical|ahead) ;; *) say "$sha isn't on main ($status): not started"; exit 1 ;; esac
  # The test account is GitHub's E2E's and Eval's too: two runs at once take
  # each other's tools away. Any of their runs in progress (with a job
  # running) holds this back; runs queued with no job (26 Nov) don't.
  busy=$(gh api "repos/kiff/agents/actions/runs?status=in_progress&per_page=50" \
    --jq '[.workflow_runs[] | select(.path | test("^\\.github/workflows/(eval|e2e[a-z-]*|proofs|eval-recipe)\\.yml$")) | .id] | join(" ")')
  if [ -n "$busy" ]; then
    say "the test account is in use on GitHub (runs $busy): not started"
    exit 1
  fi
  if [ -n "$(aws codebuild list-builds-for-project --project-name "$PROJECT" --query 'ids[0]')" ] &&
     [ "$(aws codebuild batch-get-builds --ids "$(aws codebuild list-builds-for-project --project-name "$PROJECT" --query 'ids[0]')" --query 'builds[0].buildStatus')" = IN_PROGRESS ]; then
    say "a build of $PROJECT is already running: not started"
    exit 1
  fi
  id=$(aws codebuild start-build --project-name "$PROJECT" --source-version "$sha" --query build.id)
  say "started $id (kiff/agents $sha): aws codebuild batch-get-builds --ids $id"
  echo "$id"
}

down() {
  aws codebuild delete-project --name "$PROJECT" 2>/dev/null || true
  for r in "$BUILD_ROLE" "$GATE_ROLE"; do
    for p in $(aws iam list-role-policies --role-name "$r" --query 'PolicyNames' 2>/dev/null || true); do
      aws iam delete-role-policy --role-name "$r" --policy-name "$p"
    done
    aws iam delete-role --role-name "$r" 2>/dev/null || true
  done
  say "removed $PROJECT, $BUILD_ROLE and $GATE_ROLE (secrets kept under agents-eval/)"
}

case "${1:-}" in
  secrets) secrets ;;
  setup) setup ;;
  start) shift; start "$@" ;;
  down) down ;;
  *) echo "usage: $0 secrets | setup | start SHA | down" >&2; exit 2 ;;
esac
