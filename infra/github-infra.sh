#!/usr/bin/env bash
# The role the Infra workflow (.github/workflows/infra.yml) runs infra/aws.sh
# with, in place of a person's long-lived key. Run once by a person allowed to
# change IAM; after that, infra/aws.sh runs only through the workflow.
#
# Trusted only from this repository's GitHub environment "infra": main only, and
# a run waits for one of its two reviewers, never the one who started it, so every
# run is a reviewed one. Its policy is what
# infra/aws.sh does, on agents' own names alone (infra/github-infra-policy.sh):
# the six roles infra/aws.sh makes, by name, each written only while it
# carries the permissions boundary agents-boundary, which this script alone
# writes; the two functions, the table, the schedule group, the two buckets,
# the model key's secret, the functions' log groups. It may never change its
# own role, a role's trust or a boundary, so no run can give any role more
# than the boundary. It makes no KMS key: a missing one is made by a person,
# as before. It reads no secret's value.
#
#   infra/github-infra.sh
set -euo pipefail

REGION=${AGENTS_REGION:-eu-west-1}
REPO=kiff/agents
INFRA_ROLE=agents-github-infra
REPORTS=${AGENTS_EVAL_BUCKET:-kiff-agents-eval-reports}
ATTACHMENTS=${AGENTS_ATTACHMENTS_BUCKET:-kiff-agents-attachments}
# The aws binary by its path, not `command aws`: macOS's bash 3.2 exits under
# set -e when a `command` call fails inside a function, even before `|| true`.
AWS_BIN=$(type -P aws)
aws() { "$AWS_BIN" --region "$REGION" --output text "$@"; }
# shellcheck source=infra/github-infra-policy.sh
. "$(dirname "$0")/github-infra-policy.sh"

ACCOUNT=$(aws sts get-caller-identity --query Account)
OIDC="arn:aws:iam::$ACCOUNT:oidc-provider/token.actions.githubusercontent.com"
# GitHub's immutable subject (owner and repository by id), as for the deploy role.
SUB=$(gh api "repos/$REPO/actions/oidc/customization/sub" --jq .sub_claim_prefix)
TRUST=$(cat <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Federated":"$OIDC"},"Action":"sts:AssumeRoleWithWebIdentity",
 "Condition":{"StringEquals":{"token.actions.githubusercontent.com:aud":"sts.amazonaws.com","token.actions.githubusercontent.com:sub":"$SUB:environment:infra"}}}]}
EOF
)
if aws iam get-role --role-name "$INFRA_ROLE" >/dev/null 2>&1; then
  aws iam update-assume-role-policy --role-name "$INFRA_ROLE" --policy-document "$TRUST"
else
  aws iam create-role --role-name "$INFRA_ROLE" --assume-role-policy-document "$TRUST" --max-session-duration 3600 >/dev/null
  echo "created role $INFRA_ROLE" >&2
fi

# The boundary first: every role infra/aws.sh makes carries it, and the
# Infra role may write a role only while it does. Only this script, run by a
# person, writes it; a new version replaces the default one, and the oldest
# one goes when IAM's five are kept.
BOUNDARY="arn:aws:iam::$ACCOUNT:policy/agents-boundary"
BOUNDARY_DOC=$(agents_boundary_policy "$REGION" "$ACCOUNT" "$REPORTS" "$ATTACHMENTS")
if current=$(aws iam get-policy --policy-arn "$BOUNDARY" --query Policy.DefaultVersionId 2>/dev/null); then
  live=$(aws iam get-policy-version --policy-arn "$BOUNDARY" --version-id "$current" \
    --query PolicyVersion.Document --output json)
  if [ "$(jq -S -c . <<<"$live")" != "$(jq -S -c . <<<"$BOUNDARY_DOC")" ]; then
    if [ "$(aws iam list-policy-versions --policy-arn "$BOUNDARY" --query 'length(Versions)')" -ge 5 ]; then
      aws iam delete-policy-version --policy-arn "$BOUNDARY" \
        --version-id "$(aws iam list-policy-versions --policy-arn "$BOUNDARY" --query 'sort_by(Versions[?!IsDefaultVersion], &CreateDate)[0].VersionId')"
    fi
    aws iam create-policy-version --policy-arn "$BOUNDARY" --policy-document "$BOUNDARY_DOC" --set-as-default >/dev/null
    echo "updated the boundary agents-boundary" >&2
  fi
else
  aws iam create-policy --policy-name agents-boundary --policy-document "$BOUNDARY_DOC" \
    --description "The most any of agents' roles may do (infra/github-infra-policy.sh)" >/dev/null
  echo "created the boundary agents-boundary" >&2
fi
# The roles infra/aws.sh made before the boundary get it now, so the Infra
# role can write their policies.
for r in agents-lambda agents-kiffalerts agents-scheduler agents-github-deploy agents-github-eval agents-github-eval-branches; do
  if have=$(aws iam get-role --role-name "$r" --query Role.PermissionsBoundary.PermissionsBoundaryArn 2>/dev/null) && [ "$have" != "$BOUNDARY" ]; then
    aws iam put-role-permissions-boundary --role-name "$r" --permissions-boundary "$BOUNDARY"
    echo "role $r: boundary agents-boundary" >&2
  fi
done

aws iam put-role-policy --role-name "$INFRA_ROLE" --policy-name infra-agents \
  --policy-document "$(infra_role_policy "$REGION" "$ACCOUNT" "$REPORTS" "$ATTACHMENTS")"
echo "infra role: set it as the infra environment's secret AWS_INFRA_ROLE:" >&2
echo "  aws iam get-role --role-name $INFRA_ROLE --query Role.Arn --output text | gh secret set AWS_INFRA_ROLE --repo $REPO --env infra" >&2
