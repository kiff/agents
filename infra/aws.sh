#!/usr/bin/env bash
# Creates kiff/agents on AWS, or brings it back in line. Safe to run again:
# each part is looked up first and only made or updated when it differs.
#
#   agents               DynamoDB: the one table (pk, sk), on-demand, TTL on "ttl",
#                        no indexes, no streams.
#   agents               Lambda (cmd/lambda, provided.al2023, arm64): the API behind
#                        an AWS_IAM function URL in RESPONSE_STREAM mode; background
#                        runs, schedule firings and the sweep. Async retries off.
#   agents-kiffalerts    Lambda (cmd/kiffalerts): only POST /kiff/alerts, behind a
#                        URL with no IAM auth (KIFF signs each alert), BUFFERED.
#   agents-lambda        the API function's role.
#   agents-kiffalerts    the alerts function's role: only the run items an answer
#                        touches, and the KIFF accounts' secrets.
#   agents-scheduler     the role EventBridge Scheduler invokes the API function with.
#   agents               the schedule group: the agents' schedules (s-<hash>) and
#                        agents-sweep, every minute.
#   kiff-agents-attachments  S3: the files people attach to chat messages
#                        (kiff/tafs#209), private, encrypted, TLS only. The sweep
#                        removes each when its message goes; a lifecycle rule
#                        removes anything left 38 days on (30 days' retention and
#                        the week a record outlives it).
#
# Secrets (made by a person or by tafs cloud, never by this script, except the
# model key below):
#   agents/<tenant>/<key_ref>      an agent's KIFF key            AGENTS_SECRETS_PREFIX
#   agents-accounts/<KIFF account> {"tenant","secret"} for alerts KIFF_ACCOUNTS_PREFIX
#   agents-drafts/<tenant>/<draft> a taf draft's claim secret     AGENTS_DRAFTS_PREFIX
#                                  (written by the API function, #91)
#   agents-sealing/<tenant>        the key a tenant's chats seal  AGENTS_SEALING_PREFIX
#                                  their kept reads with (#172; made by the
#                                  API function, the first time it's needed)
#   agents-model/anthropic         the model provider's key       AGENTS_MODEL_KEY_SECRET
#
# The model key's secret can be another one in the region, outside the agents'
# prefix: a replica of the key the caller already keeps, for example
# (AGENTS_MODEL_KEY_SECRET=<name>). Unset, an existing function keeps the one it
# already reads, and a new one uses agents-model/anthropic. When it doesn't
# exist, the key is read from
# stdin, so it never appears in a command line or a log:
#
#   some-command-printing-the-key | infra/aws.sh
#
# Needs an aws-cli recent enough for add-permission --invoked-via-function-url
# (2.28 is not); the deploy workflow uses the current one.
#
# Nothing account-specific is written here: the account comes from the
# caller's credentials, and ARNs are built from it at run time.
#
# No AGENTS_GATEWAYS or AGENTS_KIFF_API: KIFF's gateway and API are the
# defaults, and they come as a pair (cmd/lambda's kiffAPIFor).
#
# Usage: infra/aws.sh            (from the repository root)
# Config: AGENTS_REGION (eu-west-1), AGENTS_CALLER_ROLE (tafs-cloud-lambda), the
# IAM role whose sessions may call the API; AGENTS_PLANQUES
# (https://planque.kiff.dev), the planques a definition may name (#91);
# AGENTS_NOTIFY_URLS (as the function has it, else none), the URLs a run may be
# announced to when it ends
# or pauses (kiff/tafs#206); AGENTS_RUN_GRANTS (as the function has it, else
# unset), whether a member's run takes the account's run grant: unset, no;
# take; require.
set -euo pipefail

REGION=${AGENTS_REGION:-eu-west-1}
CALLER_ROLE=${AGENTS_CALLER_ROLE:-tafs-cloud-lambda}
TABLE=agents
FN=agents
ALERTS_FN=agents-kiffalerts
ROLE=agents-lambda
ALERTS_ROLE=agents-kiffalerts
SCHED_ROLE=agents-scheduler
GROUP=agents
SWEEP=agents-sweep
SECRETS_PREFIX=agents/
ACCOUNTS_PREFIX=agents-accounts/
# Claim secrets of taf drafts the setup agent submits (#91): outside the agents'
# key prefix, so no key reference can name one.
DRAFTS_PREFIX=agents-drafts/
# Each tenant's key for what its chats keep of their reads (#172): outside the
# agents' key prefix too.
SEALING_PREFIX=agents-sealing/
PLANQUES=${AGENTS_PLANQUES:-https://planque.kiff.dev}
NOTIFY_URLS=${AGENTS_NOTIFY_URLS:-}
MODEL_SECRET=${AGENTS_MODEL_KEY_SECRET:-agents-model/anthropic}
RUN_GRANTS=${AGENTS_RUN_GRANTS:-}
ATTACHMENTS=${AGENTS_ATTACHMENTS_BUCKET:-kiff-agents-attachments}

aws() { command aws --region "$REGION" --output text "$@"; }
say() { echo "• $*" >&2; }

# An existing function keeps the model key's secret it already reads: a rerun
# without AGENTS_MODEL_KEY_SECRET must never point it at another secret.
if [ -z "${AGENTS_MODEL_KEY_SECRET:-}" ]; then
  current=$(aws lambda get-function-configuration --function-name "$FN" \
    --query 'Environment.Variables.AGENTS_MODEL_KEY_SECRET' 2>/dev/null || true)
  if [ -n "$current" ] && [ "$current" != "None" ]; then MODEL_SECRET=$current; fi
fi
# And the URLs runs are announced to: a rerun without AGENTS_NOTIFY_URLS keeps
# them, and tafs cloud's public URL (its TAFS_PUBLIC_URL + /v1/runs/ended) must
# be among them, or every run Coach hands a taf is refused.
if [ -z "${AGENTS_NOTIFY_URLS+set}" ]; then
  current=$(aws lambda get-function-configuration --function-name "$FN" \
    --query 'Environment.Variables.AGENTS_NOTIFY_URLS' 2>/dev/null || true)
  if [ -n "$current" ] && [ "$current" != "None" ]; then NOTIFY_URLS=$current; fi
fi
# And whether it takes run grants: a rerun without AGENTS_RUN_GRANTS keeps it.
if [ -z "${AGENTS_RUN_GRANTS+set}" ]; then
  current=$(aws lambda get-function-configuration --function-name "$FN" \
    --query 'Environment.Variables.AGENTS_RUN_GRANTS' 2>/dev/null || true)
  if [ -n "$current" ] && [ "$current" != "None" ]; then RUN_GRANTS=$current; fi
fi

ACCOUNT=$(aws sts get-caller-identity --query Account)
ARN_FN="arn:aws:lambda:$REGION:$ACCOUNT:function:$FN"
ARN_TABLE="arn:aws:dynamodb:$REGION:$ACCOUNT:table/$TABLE"
ARN_SECRET="arn:aws:secretsmanager:$REGION:$ACCOUNT:secret"

# --- The table -------------------------------------------------------------

if ! aws dynamodb describe-table --table-name "$TABLE" >/dev/null 2>&1; then
  aws dynamodb create-table --table-name "$TABLE" --billing-mode PAY_PER_REQUEST \
    --attribute-definitions AttributeName=pk,AttributeType=S AttributeName=sk,AttributeType=S \
    --key-schema AttributeName=pk,KeyType=HASH AttributeName=sk,KeyType=RANGE >/dev/null
  aws dynamodb wait table-exists --table-name "$TABLE"
  say "created table $TABLE"
fi
if [ "$(aws dynamodb describe-time-to-live --table-name "$TABLE" --query TimeToLiveDescription.TimeToLiveStatus)" != "ENABLED" ]; then
  aws dynamodb update-time-to-live --table-name "$TABLE" --time-to-live-specification Enabled=true,AttributeName=ttl >/dev/null
  say "turned on TTL for $TABLE"
fi

# --- Attached files (kiff/tafs#209) ------------------------------------------

# The app uploads and downloads through presigned URLs only: nothing in the
# bucket is public, and S3 refuses any request not over TLS.
if ! aws s3api head-bucket --bucket "$ATTACHMENTS" >/dev/null 2>&1; then
  aws s3api create-bucket --bucket "$ATTACHMENTS" --create-bucket-configuration LocationConstraint="$REGION" >/dev/null
  say "created bucket $ATTACHMENTS"
fi
# People's files stay in the region (eu-west-1 unless AGENTS_REGION says): a
# bucket of that name anywhere else stops here.
where=$(aws s3api get-bucket-location --bucket "$ATTACHMENTS" --query LocationConstraint)
[ "$where" = "$REGION" ] || { echo "bucket $ATTACHMENTS is in $where, not $REGION" >&2; exit 1; }
aws s3api put-public-access-block --bucket "$ATTACHMENTS" --public-access-block-configuration \
  BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
aws s3api put-bucket-ownership-controls --bucket "$ATTACHMENTS" --ownership-controls 'Rules=[{ObjectOwnership=BucketOwnerEnforced}]'
aws s3api put-bucket-encryption --bucket "$ATTACHMENTS" --server-side-encryption-configuration \
  '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'
aws s3api put-bucket-lifecycle-configuration --bucket "$ATTACHMENTS" --lifecycle-configuration \
  '{"Rules":[{"ID":"leftovers-38-days","Status":"Enabled","Filter":{"Prefix":""},"Expiration":{"Days":38},"AbortIncompleteMultipartUpload":{"DaysAfterInitiation":1}}]}'
aws s3api put-bucket-policy --bucket "$ATTACHMENTS" --policy '{"Version":"2012-10-17","Statement":[{"Sid":"tls-only","Effect":"Deny","Principal":"*","Action":"s3:*",
 "Resource":["arn:aws:s3:::'"$ATTACHMENTS"'","arn:aws:s3:::'"$ATTACHMENTS"'/*"],"Condition":{"Bool":{"aws:SecureTransport":"false"}}}]}'

# --- The model key ---------------------------------------------------------

if ! aws secretsmanager describe-secret --secret-id "$MODEL_SECRET" >/dev/null 2>&1; then
  if [ -t 0 ]; then
    echo "the model key's secret $MODEL_SECRET doesn't exist: pipe the key into this script" >&2
    exit 1
  fi
  KEY=$(tr -d '[:space:]')
  [ -n "$KEY" ] || { echo "no model key on stdin" >&2; exit 1; }
  # printf is a builtin: the key reaches the CLI on its stdin, never in an argv.
  printf %s "$KEY" | aws secretsmanager create-secret --name "$MODEL_SECRET" --secret-string file:///dev/stdin >/dev/null
  unset KEY
  say "stored the model key in $MODEL_SECRET"
fi

# --- Roles -----------------------------------------------------------------

role() { # name, trust policy JSON
  if ! aws iam get-role --role-name "$1" >/dev/null 2>&1; then
    aws iam create-role --role-name "$1" --assume-role-policy-document "$2" >/dev/null
    say "created role $1"
  else
    aws iam update-assume-role-policy --role-name "$1" --policy-document "$2"
  fi
}

LAMBDA_TRUST='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}'
SCHED_TRUST=$(cat <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"scheduler.amazonaws.com"},"Action":"sts:AssumeRole",
 "Condition":{"StringEquals":{"aws:SourceAccount":"$ACCOUNT"}}}]}
EOF
)
role "$ROLE" "$LAMBDA_TRUST"
role "$ALERTS_ROLE" "$LAMBDA_TRUST"
role "$SCHED_ROLE" "$SCHED_TRUST"

LOGS='{"Effect":"Allow","Action":["logs:CreateLogStream","logs:PutLogEvents"],"Resource":"arn:aws:logs:'"$REGION:$ACCOUNT"':log-group:/aws/lambda/agents*:*"}'
ITEMS='"dynamodb:GetItem","dynamodb:BatchGetItem","dynamodb:Query","dynamodb:PutItem","dynamodb:UpdateItem","dynamodb:DeleteItem","dynamodb:BatchWriteItem","dynamodb:ConditionCheckItem"'

# The API function: every item of its table (no scans, no table changes), the
# agents' keys and the model key, invoking itself for background runs, and the
# schedules of its own group, made with the scheduler's role. For taf drafts
# (#91) it also creates an agent's key when it claims one, never over an
# existing secret, and keeps the drafts' claim secrets under their own prefix.
# It makes and reads each tenant's sealing key (#172), never replacing one.
# It reads, writes and deletes attached files, and lists them to remove an
# agent's or a tenant's (kiff/tafs#209).
aws iam put-role-policy --role-name "$ROLE" --policy-name agents --policy-document "$(cat <<EOF
{"Version":"2012-10-17","Statement":[
 $LOGS,
 {"Effect":"Allow","Action":[$ITEMS],"Resource":"$ARN_TABLE"},
 {"Effect":"Allow","Action":"secretsmanager:GetSecretValue","Resource":["$ARN_SECRET:${SECRETS_PREFIX}*","$ARN_SECRET:${MODEL_SECRET}-??????","$ARN_SECRET:${DRAFTS_PREFIX}*","$ARN_SECRET:${SEALING_PREFIX}*"]},
 {"Effect":"Allow","Action":"secretsmanager:CreateSecret","Resource":["$ARN_SECRET:${SECRETS_PREFIX}*","$ARN_SECRET:${DRAFTS_PREFIX}*","$ARN_SECRET:${SEALING_PREFIX}*"]},
 {"Effect":"Allow","Action":"secretsmanager:PutSecretValue","Resource":"$ARN_SECRET:${DRAFTS_PREFIX}*"},
 {"Effect":"Allow","Action":"secretsmanager:DeleteSecret","Resource":["$ARN_SECRET:${DRAFTS_PREFIX}*","$ARN_SECRET:${SEALING_PREFIX}*"]},
 {"Effect":"Allow","Action":"secretsmanager:ListSecrets","Resource":"*"},
 {"Effect":"Allow","Action":"lambda:InvokeFunction","Resource":"$ARN_FN"},
 {"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":"arn:aws:s3:::$ATTACHMENTS/*"},
 {"Effect":"Allow","Action":"s3:ListBucket","Resource":"arn:aws:s3:::$ATTACHMENTS"},
 {"Effect":"Allow","Action":["scheduler:CreateSchedule","scheduler:UpdateSchedule","scheduler:DeleteSchedule","scheduler:GetSchedule"],
  "Resource":"arn:aws:scheduler:$REGION:$ACCOUNT:schedule/$GROUP/*"},
 {"Effect":"Allow","Action":"iam:PassRole","Resource":"arn:aws:iam::$ACCOUNT:role/$SCHED_ROLE",
  "Condition":{"StringEquals":{"iam:PassedToService":"scheduler.amazonaws.com"}}}
]}
EOF
)"

# The alerts function sits behind an open URL: only the run items an answer
# touches (docs/issues/13-server.md), and only the KIFF accounts' secrets. It
# may not invoke anything.
aws iam put-role-policy --role-name "$ALERTS_ROLE" --policy-name agents-kiffalerts --policy-document "$(cat <<EOF
{"Version":"2012-10-17","Statement":[
 $LOGS,
 {"Effect":"Allow","Action":["dynamodb:GetItem","dynamodb:Query","dynamodb:PutItem","dynamodb:UpdateItem","dynamodb:DeleteItem","dynamodb:ConditionCheckItem"],
  "Resource":"$ARN_TABLE","Condition":{"ForAllValues:StringLike":{"dynamodb:LeadingKeys":["R|*","H|*","L|*","O|*","W","WP"]}}},
 {"Effect":"Allow","Action":"secretsmanager:GetSecretValue","Resource":"$ARN_SECRET:${ACCOUNTS_PREFIX}*"}
]}
EOF
)"

# EventBridge Scheduler invokes only the API function.
aws iam put-role-policy --role-name "$SCHED_ROLE" --policy-name agents-scheduler --policy-document \
  '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"lambda:InvokeFunction","Resource":"'"$ARN_FN"'"}]}'

# --- Code ------------------------------------------------------------------

BUILD=$(mktemp -d)
trap 'rm -rf "$BUILD"' EXIT
for cmd in lambda kiffalerts; do
  mkdir -p "$BUILD/$cmd"
  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -tags lambda.norpc -ldflags=-s -o "$BUILD/$cmd/bootstrap" "./cmd/$cmd"
  (cd "$BUILD/$cmd" && zip -q -X ../$cmd.zip bootstrap)
done
say "built cmd/lambda and cmd/kiffalerts at $(git rev-parse --short HEAD)"

# A role made a moment ago can't be assumed by Lambda yet.
sleep_for_new_role() { for _ in 1 2 3 4 5 6; do "$@" && return 0; sleep 10; done; "$@"; }

ENV_FN=$(cat <<EOF
{"Variables":{"AGENTS_TABLE":"$TABLE","AGENTS_CALLERS":"arn:aws:sts::$ACCOUNT:assumed-role/$CALLER_ROLE/",
 "SCHEDULE_GROUP":"$GROUP","SCHEDULE_TARGET":"$ARN_FN","SCHEDULE_ROLE":"arn:aws:iam::$ACCOUNT:role/$SCHED_ROLE",
 "AGENTS_SECRETS_PREFIX":"$SECRETS_PREFIX","AGENTS_MODEL_KEY_SECRET":"$MODEL_SECRET",
 "AGENTS_PLANQUES":"$PLANQUES","AGENTS_NOTIFY_URLS":"$NOTIFY_URLS","AGENTS_DRAFTS_PREFIX":"$DRAFTS_PREFIX","AGENTS_SEALING_PREFIX":"$SEALING_PREFIX",
 "AGENTS_RUN_GRANTS":"$RUN_GRANTS","AGENTS_ATTACHMENTS_BUCKET":"$ATTACHMENTS"}}
EOF
)
ENV_ALERTS='{"Variables":{"AGENTS_TABLE":"'"$TABLE"'","KIFF_ACCOUNTS_PREFIX":"'"$ACCOUNTS_PREFIX"'","AGENTS_SECRETS_PREFIX":"'"$SECRETS_PREFIX"'"}}'

function_up() { # name, role, zip, timeout, memory, environment
  if ! aws lambda get-function --function-name "$1" >/dev/null 2>&1; then
    sleep_for_new_role aws lambda create-function --function-name "$1" --runtime provided.al2023 --architectures arm64 \
      --handler bootstrap --role "arn:aws:iam::$ACCOUNT:role/$2" --zip-file "fileb://$3" \
      --timeout "$4" --memory-size "$5" --environment "$6" >/dev/null
    say "created function $1"
  else
    aws lambda update-function-configuration --function-name "$1" --role "arn:aws:iam::$ACCOUNT:role/$2" \
      --timeout "$4" --memory-size "$5" --environment "$6" >/dev/null
    aws lambda wait function-updated-v2 --function-name "$1"
    aws lambda update-function-code --function-name "$1" --zip-file "fileb://$3" >/dev/null
  fi
  aws lambda wait function-updated-v2 --function-name "$1"
  aws logs create-log-group --log-group-name "/aws/lambda/$1" 2>/dev/null || true
  aws logs put-retention-policy --log-group-name "/aws/lambda/$1" --retention-in-days 30
}

# Runs must fit Lambda's 15 minutes; the lease ends a minute after that.
function_up "$FN" "$ROLE" "$BUILD/lambda.zip" 900 512 "$ENV_FN"
function_up "$ALERTS_FN" "$ALERTS_ROLE" "$BUILD/kiffalerts.zip" 15 128 "$ENV_ALERTS"

# The runner claims each run once, and the sweep ends what a crash leaves.
aws lambda put-function-event-invoke-config --function-name "$FN" --maximum-retry-attempts 0 >/dev/null
# Anyone can post to the alerts URL: each post costs an invocation and a secret read.
aws lambda put-function-concurrency --function-name "$ALERTS_FN" --reserved-concurrent-executions 5 >/dev/null

url_up() { # function, auth type, invoke mode
  if ! aws lambda get-function-url-config --function-name "$1" >/dev/null 2>&1; then
    aws lambda create-function-url-config --function-name "$1" --auth-type "$2" --invoke-mode "$3" >/dev/null
    say "created the URL of $1"
  else
    aws lambda update-function-url-config --function-name "$1" --auth-type "$2" --invoke-mode "$3" >/dev/null
  fi
}
url_up "$FN" AWS_IAM RESPONSE_STREAM
url_up "$ALERTS_FN" NONE BUFFERED

# The alerts URL is public: KIFF's signature is its check. Nobody may invoke
# the API function except through IAM: no resource policy grants it.
# Both are needed: invoking through the URL, and invoking only when it comes
# through the URL, so nobody can invoke the function directly.
POLICY=$(aws lambda get-policy --function-name "$ALERTS_FN" --query Policy 2>/dev/null || true)
case "$POLICY" in *'"Sid":"public-url"'*) ;; *)
  aws lambda add-permission --function-name "$ALERTS_FN" --statement-id public-url \
    --action lambda:InvokeFunctionUrl --principal '*' --function-url-auth-type NONE >/dev/null ;;
esac
case "$POLICY" in *'"Sid":"public-url-invoke"'*) ;; *)
  aws lambda add-permission --function-name "$ALERTS_FN" --statement-id public-url-invoke \
    --action lambda:InvokeFunction --principal '*' --invoked-via-function-url >/dev/null ;;
esac

# --- Schedules -------------------------------------------------------------

aws scheduler get-schedule-group --name "$GROUP" >/dev/null 2>&1 || {
  aws scheduler create-schedule-group --name "$GROUP" >/dev/null
  say "created schedule group $GROUP"
}
SWEEP_TARGET='{"Arn":"'"$ARN_FN"'","RoleArn":"arn:aws:iam::'"$ACCOUNT"':role/'"$SCHED_ROLE"'","Input":"{\"sweep\":{}}","RetryPolicy":{"MaximumRetryAttempts":0}}'
if aws scheduler get-schedule --group-name "$GROUP" --name "$SWEEP" >/dev/null 2>&1; then
  aws scheduler update-schedule --group-name "$GROUP" --name "$SWEEP" --schedule-expression "rate(1 minute)" \
    --flexible-time-window Mode=OFF --target "$SWEEP_TARGET" --state ENABLED >/dev/null
else
  sleep_for_new_role aws scheduler create-schedule --group-name "$GROUP" --name "$SWEEP" --schedule-expression "rate(1 minute)" \
    --flexible-time-window Mode=OFF --target "$SWEEP_TARGET" --state ENABLED >/dev/null
  say "created the sweep, every minute"
fi

# --- The deploy role, for this repository's main only ------------------------

# GitHub's immutable subject (owner and repository by id), so a renamed or
# recreated repository can't take the role over. The OIDC provider is the
# account's one for token.actions.githubusercontent.com.
DEPLOY_ROLE=agents-github-deploy
SUB=$(gh api repos/kiff/agents/actions/oidc/customization/sub --jq .sub_claim_prefix)
OIDC="arn:aws:iam::$ACCOUNT:oidc-provider/token.actions.githubusercontent.com"
role "$DEPLOY_ROLE" "$(cat <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Federated":"$OIDC"},"Action":"sts:AssumeRoleWithWebIdentity",
 "Condition":{"StringEquals":{"token.actions.githubusercontent.com:aud":"sts.amazonaws.com","token.actions.githubusercontent.com:sub":"$SUB:ref:refs/heads/main"}}}]}
EOF
)"
aws iam put-role-policy --role-name "$DEPLOY_ROLE" --policy-name deploy-agents --policy-document \
  '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["lambda:UpdateFunctionCode","lambda:GetFunction","lambda:GetFunctionConfiguration"],
    "Resource":["'"$ARN_FN"'","arn:aws:lambda:'"$REGION:$ACCOUNT"':function:'"$ALERTS_FN"'"]}]}'
# The workflow reads the role from the secret AWS_DEPLOY_ROLE, never from code.
echo "deploy role: set it as the secret AWS_DEPLOY_ROLE: aws iam get-role --role-name $DEPLOY_ROLE --query Role.Arn --output text | gh secret set AWS_DEPLOY_ROLE --repo kiff/agents" >&2

# --- The eval's reports, private (#78) --------------------------------------

# The nightly eval's log is public, so its full report (each run's checks and
# why, with the run's ids) goes to this bucket instead: private, encrypted,
# kept 90 days. Its role may only write reports, from main's workflow runs.
REPORTS=${AGENTS_EVAL_BUCKET:-kiff-agents-eval-reports}
EVAL_ROLE=agents-github-eval
if ! aws s3api head-bucket --bucket "$REPORTS" >/dev/null 2>&1; then
  aws s3api create-bucket --bucket "$REPORTS" --create-bucket-configuration LocationConstraint="$REGION" >/dev/null
  say "created bucket $REPORTS"
fi
aws s3api put-public-access-block --bucket "$REPORTS" --public-access-block-configuration \
  BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
aws s3api put-bucket-ownership-controls --bucket "$REPORTS" --ownership-controls 'Rules=[{ObjectOwnership=BucketOwnerEnforced}]'
aws s3api put-bucket-encryption --bucket "$REPORTS" --server-side-encryption-configuration \
  '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'
aws s3api put-bucket-lifecycle-configuration --bucket "$REPORTS" --lifecycle-configuration \
  '{"Rules":[{"ID":"reports-90-days","Status":"Enabled","Filter":{"Prefix":""},"Expiration":{"Days":90}}]}'
role "$EVAL_ROLE" "$(cat <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Federated":"$OIDC"},"Action":"sts:AssumeRoleWithWebIdentity",
 "Condition":{"StringEquals":{"token.actions.githubusercontent.com:aud":"sts.amazonaws.com","token.actions.githubusercontent.com:sub":"$SUB:ref:refs/heads/main"}}}]}
EOF
)"
aws iam put-role-policy --role-name "$EVAL_ROLE" --policy-name eval-reports --policy-document \
  '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::'"$REPORTS"'/reports/*"}]}'
echo "eval role: set it as the secret AWS_EVAL_ROLE: aws iam get-role --role-name $EVAL_ROLE --query Role.Arn --output text | gh secret set AWS_EVAL_ROLE --repo kiff/agents" >&2

echo "api:    $(aws lambda get-function-url-config --function-name "$FN" --query FunctionUrl)"
echo "alerts: $(aws lambda get-function-url-config --function-name "$ALERTS_FN" --query FunctionUrl)kiff/alerts"
