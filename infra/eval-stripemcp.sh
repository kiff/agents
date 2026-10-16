#!/usr/bin/env bash
# The eval cases' Stripe tools (cmd/stripemcp) on AWS: one small Lambda
# behind a public function URL, for KIFF's gateway to reach. Its bearer
# token is its check. It holds a Stripe test-mode key only.
#
#   STRIPE_API_KEY=sk_test_… STRIPEMCP_TOKEN=… infra/eval-stripemcp.sh up
#   infra/eval-stripemcp.sh down     # removes the function, its URL and role
#
# Config: AGENTS_REGION (eu-west-1). Prints the URL to register in KIFF.
# Needs an aws-cli recent enough for add-permission --invoked-via-function-url.
set -euo pipefail
trap 'echo "eval-stripemcp: failed at line $LINENO" >&2' ERR
cd "$(dirname "$0")/.."

REGION=${AGENTS_REGION:-eu-west-1}
FN=agents-eval-stripemcp
ROLE=agents-eval-stripemcp
# Each call runs in a subshell: the AWS CLI can take its parent shell down
# when it fails, so "|| true" and "if !" wouldn't get to run otherwise.
aws() ( command aws --region "$REGION" --output text "$@" )
say() { echo "eval-stripemcp: $*" >&2; }
ACCOUNT=$(aws sts get-caller-identity --query Account)

if [ "${1:-}" = down ]; then
  aws lambda delete-function-url-config --function-name "$FN" 2>/dev/null || true
  aws lambda delete-function --function-name "$FN" 2>/dev/null || true
  aws iam delete-role-policy --role-name "$ROLE" --policy-name logs 2>/dev/null || true
  aws iam delete-role --role-name "$ROLE" 2>/dev/null || true
  aws logs delete-log-group --log-group-name "/aws/lambda/$FN" 2>/dev/null || true
  say "removed"
  exit 0
fi
[ "${1:-}" = up ] || { echo "usage: $0 up|down" >&2; exit 2; }
case "${STRIPE_API_KEY:-}" in sk_test_*|rk_test_*) ;; *) echo "STRIPE_API_KEY must be a test-mode key" >&2; exit 1 ;; esac
[ "${#STRIPEMCP_TOKEN}" -ge 32 ] || { echo "STRIPEMCP_TOKEN must be 32 characters or more" >&2; exit 1; }

BUILD=$(mktemp -d)
trap 'rm -rf "$BUILD"' EXIT
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags lambda.norpc -trimpath -o "$BUILD/bootstrap" ./cmd/stripemcp
(cd "$BUILD" && zip -q stripemcp.zip bootstrap)

if ! aws iam get-role --role-name "$ROLE" >/dev/null 2>&1; then
  aws iam create-role --role-name "$ROLE" --assume-role-policy-document \
    '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}' >/dev/null
  say "created role $ROLE"
  sleep 10 # a new role takes a moment to be assumable
fi
aws iam put-role-policy --role-name "$ROLE" --policy-name logs --policy-document \
  '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["logs:CreateLogStream","logs:PutLogEvents"],"Resource":"arn:aws:logs:'"$REGION:$ACCOUNT"':log-group:/aws/lambda/'"$FN"':*"}]}'

# The secrets go to the AWS CLI in a private file, never on its command
# line, where any process could read them. Both are plain tokens (checked
# above and here), so they need no JSON escaping.
case "$STRIPE_API_KEY$STRIPEMCP_TOKEN" in *[!A-Za-z0-9_]*) echo "the key and token must be letters, digits and underscores" >&2; exit 1 ;; esac
ENV="$BUILD/env.json"
(umask 077; printf '{"Variables":{"STRIPE_API_KEY":"%s","STRIPEMCP_TOKEN":"%s"}}' "$STRIPE_API_KEY" "$STRIPEMCP_TOKEN" > "$ENV")
if ! aws lambda get-function --function-name "$FN" >/dev/null 2>&1; then
  aws lambda create-function --function-name "$FN" --runtime provided.al2023 --architectures arm64 --handler bootstrap \
    --role "arn:aws:iam::$ACCOUNT:role/$ROLE" --zip-file "fileb://$BUILD/stripemcp.zip" \
    --timeout 30 --memory-size 256 --environment "file://$ENV" >/dev/null
  say "created function $FN"
else
  aws lambda update-function-configuration --function-name "$FN" --timeout 30 --memory-size 256 --environment "file://$ENV" >/dev/null
  aws lambda wait function-updated-v2 --function-name "$FN"
  aws lambda update-function-code --function-name "$FN" --zip-file "fileb://$BUILD/stripemcp.zip" >/dev/null
fi
aws lambda wait function-updated-v2 --function-name "$FN"
aws logs create-log-group --log-group-name "/aws/lambda/$FN" 2>/dev/null || true
aws logs put-retention-policy --log-group-name "/aws/lambda/$FN" --retention-in-days 7
# Anyone can post to the URL: the token is checked first. An agent sends a
# turn's calls at once and KIFF forwards each, so it takes ten at a time.
aws lambda put-function-concurrency --function-name "$FN" --reserved-concurrent-executions 10 >/dev/null

if ! aws lambda get-function-url-config --function-name "$FN" >/dev/null 2>&1; then
  aws lambda create-function-url-config --function-name "$FN" --auth-type NONE --invoke-mode RESPONSE_STREAM >/dev/null
  say "created the URL"
fi
POLICY=$(aws lambda get-policy --function-name "$FN" --query Policy 2>/dev/null || true)
case "$POLICY" in *'"Sid":"public-url"'*) ;; *)
  aws lambda add-permission --function-name "$FN" --statement-id public-url \
    --action lambda:InvokeFunctionUrl --principal '*' --function-url-auth-type NONE >/dev/null ;;
esac
case "$POLICY" in *'"Sid":"public-url-invoke"'*) ;; *)
  aws lambda add-permission --function-name "$FN" --statement-id public-url-invoke \
    --action lambda:InvokeFunction --principal '*' --invoked-via-function-url >/dev/null ;;
esac
aws lambda get-function-url-config --function-name "$FN" --query FunctionUrl
