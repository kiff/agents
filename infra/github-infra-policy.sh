# The two documents infra/github-infra.sh writes, kept apart so a test can
# read them (infra/policy_test.go). Sourced, not run.
#
# agents_boundary_policy REGION ACCOUNT REPORTS ATTACHMENTS prints the
# permissions boundary every role infra/aws.sh makes carries: the most any of
# them may ever do, whatever its own policy says, and only on agents' own
# resources. It's what infra/aws.sh's roles need between them: the
# functions' logs, the table's items (no scans, no table changes), agents'
# secrets, KMS only under agents' encryption contexts, the two functions, the
# two buckets' objects, the schedules of agents' group, the scheduler's role
# passed to the scheduler, and the Bedrock backup's EU profiles. Only a person
# changes it, through infra/github-infra.sh.
agents_boundary_policy() {
  local region=$1 account=$2 reports=$3 attachments=$4
  local secret="arn:aws:secretsmanager:$region:$account:secret"
  local fn="arn:aws:lambda:$region:$account:function"
  cat <<POLICY
{"Version":"2012-10-17","Statement":[
 {"Sid":"Logs","Effect":"Allow","Action":["logs:CreateLogStream","logs:PutLogEvents"],
  "Resource":"arn:aws:logs:$region:$account:log-group:/aws/lambda/agents*:*"},
 {"Sid":"TableItems","Effect":"Allow","Action":["dynamodb:GetItem","dynamodb:BatchGetItem","dynamodb:Query","dynamodb:PutItem",
   "dynamodb:UpdateItem","dynamodb:DeleteItem","dynamodb:BatchWriteItem","dynamodb:ConditionCheckItem"],
  "Resource":"arn:aws:dynamodb:$region:$account:table/agents"},
 {"Sid":"ReadSecrets","Effect":"Allow","Action":"secretsmanager:GetSecretValue",
  "Resource":["$secret:agents-??????","$secret:agents/*","$secret:agents-model/*","$secret:agents-drafts/*",
   "$secret:agents-sealing/*","$secret:agents-accounts/*"]},
 {"Sid":"KeepSecrets","Effect":"Allow","Action":["secretsmanager:CreateSecret","secretsmanager:DeleteSecret","secretsmanager:RestoreSecret"],
  "Resource":["$secret:agents/*","$secret:agents-drafts/*","$secret:agents-sealing/*"]},
 {"Sid":"ListSecrets","Effect":"Allow","Action":"secretsmanager:ListSecrets","Resource":"*"},
 {"Sid":"SealUnderAgentsContexts","Effect":"Allow","Action":["kms:Encrypt","kms:Decrypt"],"Resource":"arn:aws:kms:$region:$account:key/*",
  "Condition":{"StringLike":{"kms:EncryptionContext:purpose":"kiff/agents *"}}},
 {"Sid":"InvokeTheApi","Effect":"Allow","Action":"lambda:InvokeFunction","Resource":"$fn:agents"},
 {"Sid":"DeployTheFunctions","Effect":"Allow","Action":["lambda:UpdateFunctionCode","lambda:GetFunction","lambda:GetFunctionConfiguration"],
  "Resource":["$fn:agents","$fn:agents-kiffalerts"]},
 {"Sid":"Attachments","Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":"arn:aws:s3:::$attachments/*"},
 {"Sid":"ListAttachments","Effect":"Allow","Action":"s3:ListBucket","Resource":"arn:aws:s3:::$attachments"},
 {"Sid":"EvalReports","Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::$reports/reports/*"},
 {"Sid":"Schedules","Effect":"Allow","Action":["scheduler:CreateSchedule","scheduler:UpdateSchedule","scheduler:DeleteSchedule","scheduler:GetSchedule"],
  "Resource":"arn:aws:scheduler:$region:$account:schedule/agents/*"},
 {"Sid":"PassTheSchedulerRole","Effect":"Allow","Action":"iam:PassRole","Resource":"arn:aws:iam::$account:role/agents-scheduler",
  "Condition":{"StringEquals":{"iam:PassedToService":"scheduler.amazonaws.com"}}},
 {"Sid":"BedrockBackup","Effect":"Allow","Action":["bedrock:InvokeModel","bedrock:InvokeModelWithResponseStream"],
  "Resource":["arn:aws:bedrock:$region:$account:inference-profile/eu.anthropic.claude-*","arn:aws:bedrock:eu-*::foundation-model/anthropic.claude-*"]}
]}
POLICY
}

# infra_role_policy REGION ACCOUNT REPORTS ATTACHMENTS prints the Infra
# workflow's own policy: what infra/aws.sh does, on agents' own names alone.
#
# Its IAM is the six roles infra/aws.sh makes, by exact name, and each one
# made, or its policy written, only while it carries the boundary above; the
# boundary itself can only be read. A run can't change a role's trust (a
# person does: infra/aws.sh leaves an unchanged trust alone), remove or
# replace a boundary, attach a managed policy, or touch its own role. It may
# pass only the two functions' roles to Lambda and the scheduler's to the
# scheduler. So no run can give any role more than the boundary. Nor can it make
# either bucket public: it may never change a bucket's public access block or
# delete its policy, and a policy it writes is refused while the block is on.
infra_role_policy() {
  local region=$1 account=$2 reports=$3 attachments=$4
  local role="arn:aws:iam::$account:role"
  local roles="\"$role/agents-lambda\",\"$role/agents-kiffalerts\",\"$role/agents-scheduler\",\"$role/agents-github-deploy\",\"$role/agents-github-eval\",\"$role/agents-github-eval-branches\""
  local boundary="arn:aws:iam::$account:policy/agents-boundary"
  local fns="\"arn:aws:lambda:$region:$account:function:agents\",\"arn:aws:lambda:$region:$account:function:agents-kiffalerts\""
  cat <<POLICY
{"Version":"2012-10-17","Statement":[
 {"Sid":"ReadItsRolesAndTheBoundary","Effect":"Allow","Action":["iam:GetRole","iam:GetPolicy"],"Resource":[$roles,"$boundary"]},
 {"Sid":"RolesOnlyUnderTheBoundary","Effect":"Allow","Action":["iam:CreateRole","iam:PutRolePolicy","iam:PutRolePermissionsBoundary"],
  "Resource":[$roles],"Condition":{"StringEquals":{"iam:PermissionsBoundary":"$boundary"}}},
 {"Sid":"PassTheFunctionRoles","Effect":"Allow","Action":"iam:PassRole","Resource":["$role/agents-lambda","$role/agents-kiffalerts"],
  "Condition":{"StringEquals":{"iam:PassedToService":"lambda.amazonaws.com"}}},
 {"Sid":"PassTheSchedulerRole","Effect":"Allow","Action":"iam:PassRole","Resource":"$role/agents-scheduler",
  "Condition":{"StringEquals":{"iam:PassedToService":"scheduler.amazonaws.com"}}},
 {"Sid":"NeverItsOwnRole","Effect":"Deny","Action":"iam:*","Resource":"$role/agents-github-infra"},
 {"Sid":"TheBoundaryStays","Effect":"Deny","Action":["iam:CreatePolicyVersion","iam:SetDefaultPolicyVersion","iam:DeletePolicy",
   "iam:DeletePolicyVersion","iam:TagPolicy","iam:UntagPolicy"],"Resource":"$boundary"},
 {"Sid":"NoWayAround","Effect":"Deny","Action":["iam:DeleteRolePermissionsBoundary","iam:UpdateAssumeRolePolicy","iam:AttachRolePolicy",
   "iam:DetachRolePolicy","iam:DeleteRolePolicy","iam:CreatePolicy","iam:CreatePolicyVersion"],"Resource":"*"},
 {"Effect":"Allow","Action":["lambda:GetFunction","lambda:GetFunctionConfiguration","lambda:CreateFunction","lambda:UpdateFunctionCode",
   "lambda:UpdateFunctionConfiguration","lambda:GetFunctionUrlConfig","lambda:CreateFunctionUrlConfig","lambda:UpdateFunctionUrlConfig",
   "lambda:AddPermission","lambda:GetPolicy","lambda:PutFunctionEventInvokeConfig","lambda:PutFunctionConcurrency"],"Resource":[$fns]},
 {"Effect":"Allow","Action":["dynamodb:DescribeTable","dynamodb:CreateTable","dynamodb:UpdateTable","dynamodb:DescribeTimeToLive",
   "dynamodb:UpdateTimeToLive","dynamodb:DescribeContinuousBackups","dynamodb:UpdateContinuousBackups"],
  "Resource":"arn:aws:dynamodb:$region:$account:table/agents"},
 {"Effect":"Allow","Action":"kms:DescribeKey","Resource":"*"},
 {"Effect":"Allow","Action":"secretsmanager:DescribeSecret","Resource":["arn:aws:secretsmanager:$region:$account:secret:agents-??????",
   "arn:aws:secretsmanager:$region:$account:secret:agents-model/*"]},
 {"Effect":"Allow","Action":["scheduler:GetScheduleGroup","scheduler:CreateScheduleGroup"],"Resource":"arn:aws:scheduler:$region:$account:schedule-group/agents"},
 {"Effect":"Allow","Action":["scheduler:GetSchedule","scheduler:CreateSchedule","scheduler:UpdateSchedule"],"Resource":"arn:aws:scheduler:$region:$account:schedule/agents/*"},
 {"Effect":"Allow","Action":["s3:CreateBucket","s3:ListBucket","s3:GetBucketLocation","s3:GetBucketPublicAccessBlock","s3:PutBucketOwnershipControls",
   "s3:PutEncryptionConfiguration","s3:PutLifecycleConfiguration","s3:PutBucketPolicy"],
  "Resource":["arn:aws:s3:::$reports","arn:aws:s3:::$attachments"]},
 {"Sid":"BucketsStayPrivate","Effect":"Deny","Action":["s3:PutBucketPublicAccessBlock","s3:DeleteBucketPolicy"],
  "Resource":["arn:aws:s3:::$reports","arn:aws:s3:::$attachments"]},
 {"Effect":"Allow","Action":["logs:CreateLogGroup","logs:PutRetentionPolicy"],"Resource":"arn:aws:logs:$region:$account:log-group:/aws/lambda/agents*"}
]}
POLICY
}
