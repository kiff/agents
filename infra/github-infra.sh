#!/usr/bin/env bash
# The role the Infra workflow (.github/workflows/infra.yml) runs infra/aws.sh
# with, in place of a person's long-lived key. Run once by a person allowed to
# change IAM; after that, infra/aws.sh runs only through the workflow.
#
# Trusted only from this repository's GitHub environment "infra", which takes
# two required reviewers, so every run is a reviewed one. Its policy is what
# infra/aws.sh does, on agents' own names alone (roles agents-*, the two
# functions, the table, the schedule group, the two buckets, the model key's
# secret, the functions' log groups), and it may never change its own role,
# so it can't widen itself. It makes no KMS key: a missing one is made by a
# person, as before. It reads no secret's value.
#
#   infra/github-infra.sh
set -euo pipefail

REGION=${AGENTS_REGION:-eu-west-1}
REPO=kiff/agents
INFRA_ROLE=agents-github-infra
REPORTS=${AGENTS_EVAL_BUCKET:-kiff-agents-eval-reports}
ATTACHMENTS=${AGENTS_ATTACHMENTS_BUCKET:-kiff-agents-attachments}
aws() { command aws --region "$REGION" --output text "$@"; }

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

ARN_ROLES="arn:aws:iam::$ACCOUNT:role/agents-*"
ARN_SELF="arn:aws:iam::$ACCOUNT:role/$INFRA_ROLE"
FNS="\"arn:aws:lambda:$REGION:$ACCOUNT:function:agents\",\"arn:aws:lambda:$REGION:$ACCOUNT:function:agents-kiffalerts\""
aws iam put-role-policy --role-name "$INFRA_ROLE" --policy-name infra-agents --policy-document "$(cat <<EOF
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["iam:GetRole","iam:CreateRole","iam:UpdateAssumeRolePolicy","iam:PutRolePolicy"],"Resource":"$ARN_ROLES"},
 {"Effect":"Allow","Action":"iam:PassRole","Resource":"$ARN_ROLES",
  "Condition":{"StringEquals":{"iam:PassedToService":["lambda.amazonaws.com","scheduler.amazonaws.com"]}}},
 {"Effect":"Deny","Action":"iam:*","Resource":"$ARN_SELF"},
 {"Effect":"Allow","Action":["lambda:GetFunction","lambda:GetFunctionConfiguration","lambda:CreateFunction","lambda:UpdateFunctionCode",
   "lambda:UpdateFunctionConfiguration","lambda:GetFunctionUrlConfig","lambda:CreateFunctionUrlConfig","lambda:UpdateFunctionUrlConfig",
   "lambda:AddPermission","lambda:GetPolicy","lambda:PutFunctionEventInvokeConfig","lambda:PutFunctionConcurrency"],"Resource":[$FNS]},
 {"Effect":"Allow","Action":["dynamodb:DescribeTable","dynamodb:CreateTable","dynamodb:UpdateTable","dynamodb:DescribeTimeToLive",
   "dynamodb:UpdateTimeToLive","dynamodb:DescribeContinuousBackups","dynamodb:UpdateContinuousBackups"],
  "Resource":"arn:aws:dynamodb:$REGION:$ACCOUNT:table/agents"},
 {"Effect":"Allow","Action":"kms:DescribeKey","Resource":"*"},
 {"Effect":"Allow","Action":"secretsmanager:DescribeSecret","Resource":["arn:aws:secretsmanager:$REGION:$ACCOUNT:secret:agents-??????",
   "arn:aws:secretsmanager:$REGION:$ACCOUNT:secret:agents-model/*"]},
 {"Effect":"Allow","Action":["scheduler:GetScheduleGroup","scheduler:CreateScheduleGroup"],"Resource":"arn:aws:scheduler:$REGION:$ACCOUNT:schedule-group/agents"},
 {"Effect":"Allow","Action":["scheduler:GetSchedule","scheduler:CreateSchedule","scheduler:UpdateSchedule"],"Resource":"arn:aws:scheduler:$REGION:$ACCOUNT:schedule/agents/*"},
 {"Effect":"Allow","Action":["s3:CreateBucket","s3:ListBucket","s3:GetBucketLocation","s3:PutBucketPublicAccessBlock","s3:PutBucketOwnershipControls",
   "s3:PutEncryptionConfiguration","s3:PutLifecycleConfiguration","s3:PutBucketPolicy"],
  "Resource":["arn:aws:s3:::$REPORTS","arn:aws:s3:::$ATTACHMENTS"]},
 {"Effect":"Allow","Action":["logs:CreateLogGroup","logs:PutRetentionPolicy"],"Resource":"arn:aws:logs:$REGION:$ACCOUNT:log-group:/aws/lambda/agents*"}
]}
EOF
)"
echo "infra role: set it as the infra environment's secret AWS_INFRA_ROLE:" >&2
echo "  aws iam get-role --role-name $INFRA_ROLE --query Role.Arn --output text | gh secret set AWS_INFRA_ROLE --repo $REPO --env infra" >&2
