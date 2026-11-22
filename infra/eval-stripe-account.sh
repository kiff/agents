#!/usr/bin/env bash
# Moves the evals to their own Stripe test account (kiff/tafs#236, item 11). The
# Chargebacks case seeds disputed payments each run, and Stripe never deletes a
# payment: in an account people also test in, they pile up in front of theirs.
#
# Make a sandbox for the evals in Stripe's dashboard first, then run this. It
# asks for the sandbox's secret key without showing it, checks it's a test-mode
# key of another account than the one it replaces, and puts it in both places
# the evals read it: the eval taf's Stripe tools (infra/eval-stripemcp.sh) and
# this repository's STRIPE_API_KEY secret. It records the account's id (not a
# secret) as the EVAL_STRIPE_ACCOUNT variable, which the seed checks before it
# writes anything.
#
#   infra/eval-stripe-account.sh
set -euo pipefail
REPO=kiff/agents
REGION=${AGENTS_REGION:-eu-west-1}
FN=agents-eval-stripemcp
say() { echo "eval-stripe-account: $*" >&2; }

read -rsp "The eval sandbox's secret key (sk_test_…): " STRIPE_API_KEY; echo >&2
case "$STRIPE_API_KEY" in sk_test_*|rk_test_*) ;; *) say "that isn't a test-mode key"; exit 1 ;; esac

# The key goes to curl on stdin, never on its command line.
account() { printf 'user = "%s:"\n' "$1" | curl -fsS -K - https://api.stripe.com/v1/account | sed -n 's/.*"id": *"\(acct_[A-Za-z0-9]*\)".*/\1/p' | head -1; }
NEW=$(account "$STRIPE_API_KEY")
[ -n "$NEW" ] || { say "Stripe didn't accept that key"; exit 1; }
OLD=$(gh variable get EVAL_STRIPE_ACCOUNT -R "$REPO" 2>/dev/null || true)
# The tools' current key and token: from their one secret, or from the
# function's environment where an older deploy left them.
current() {
  local v
  v=$(command aws --region "$REGION" --output text secretsmanager get-secret-value --secret-id "$FN" \
    --query SecretString 2>/dev/null | sed -n 's/.*"'"$1"'": *"\([A-Za-z0-9_]*\)".*/\1/p')
  [ -n "$v" ] || v=$(command aws --region "$REGION" --output text lambda get-function-configuration --function-name "$FN" \
    --query "Environment.Variables.$2" 2>/dev/null || true)
  [ "$v" != None ] && printf '%s' "$v"
}
CURRENT=$(current stripe_api_key STRIPE_API_KEY || true)
[ -n "$CURRENT" ] && OLD=${OLD:-$(account "$CURRENT")}
if [ -n "$OLD" ] && [ "$OLD" = "$NEW" ]; then
  say "that key is the evals' current account ($NEW): make a sandbox for them and use its key"
  exit 1
fi
say "evals move to $NEW${OLD:+ (from $OLD)}"

TOKEN=$(current token STRIPEMCP_TOKEN || true)
[ -n "$TOKEN" ] || { say "no $FN to update: run infra/eval-stripemcp.sh up first"; exit 1; }
STRIPE_API_KEY=$STRIPE_API_KEY STRIPEMCP_TOKEN=$TOKEN AGENTS_REGION=$REGION "$(dirname "$0")/eval-stripemcp.sh" up
printf '%s' "$STRIPE_API_KEY" | gh secret set STRIPE_API_KEY -R "$REPO"
gh variable set EVAL_STRIPE_ACCOUNT -R "$REPO" --body "$NEW"
say "done: the next eval seeds $NEW only"
