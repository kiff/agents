# The documents infra/eval-codebuild.sh writes for the Eval's backup runner on
# AWS CodeBuild: a full Eval of main, started by hand when GitHub won't run
# eval.yml. Sourced, not run; each function prints one JSON document.
#
#   codebuild_trust REGION ACCOUNT PROJECT
#       who may act as the build's role: CodeBuild, for this one project in
#       this account only (aws:SourceArn and aws:SourceAccount), so another
#       project, or another account's CodeBuild, can't take it.
#   codebuild_policy REGION ACCOUNT PROJECT BUCKET SECRET...
#       what the build may do, beside Bedrock (infra/bedrock-policy.sh, the
#       same document main's Eval role has): put its reports under
#       reports/codebuild/ (never over main's GitHub runs' reports, no Get,
#       List or Delete), read the Eval's secrets by their exact names
#       (agents-eval/<SECRET>), and write its own log group.
#   gate_trust ACCOUNT
#       who may act as the gate's read-only role: kiff/tafs' runs on main, in
#       the environment eval-gate (kiff/tafs' Upgrade job), by GitHub's OIDC.
#   gate_policy REGION ACCOUNT PROJECT
#       what the gate may read: this project's definition and builds, and its
#       log group's events. Nothing else, and nothing it may change.
CODEBUILD_PROJECT=agents-eval

codebuild_trust() {
  local region=$1 account=$2 project=$3
  cat <<POLICY
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Principal":{"Service":"codebuild.amazonaws.com"},"Action":"sts:AssumeRole",
  "Condition":{"StringEquals":{"aws:SourceAccount":"$account","aws:SourceArn":"arn:aws:codebuild:$region:$account:project/$project"}}}
]}
POLICY
}

codebuild_policy() {
  local region=$1 account=$2 project=$3 bucket=$4 secrets="" s
  shift 4
  for s in "$@"; do
    # Secrets Manager adds six random characters to a secret's ARN.
    secrets="$secrets\"arn:aws:secretsmanager:$region:$account:secret:agents-eval/$s-??????\","
  done
  secrets=${secrets%,}
  cat <<POLICY
{"Version":"2012-10-17","Statement":[
 {"Sid":"Reports","Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::$bucket/reports/codebuild/*"},
 {"Sid":"Secrets","Effect":"Allow","Action":"secretsmanager:GetSecretValue","Resource":[$secrets]},
 {"Sid":"Logs","Effect":"Allow","Action":["logs:CreateLogStream","logs:PutLogEvents"],
  "Resource":"arn:aws:logs:$region:$account:log-group:/aws/codebuild/$project:*"}
]}
POLICY
}

gate_trust() {
  local account=$1
  cat <<POLICY
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Principal":{"Federated":"arn:aws:iam::$account:oidc-provider/token.actions.githubusercontent.com"},
  "Action":"sts:AssumeRoleWithWebIdentity",
  "Condition":{"StringEquals":{"token.actions.githubusercontent.com:aud":"sts.amazonaws.com",
                               "token.actions.githubusercontent.com:sub":"repo:kiff/tafs:environment:eval-gate"}}}
]}
POLICY
}

gate_policy() {
  local region=$1 account=$2 project=$3
  cat <<POLICY
{"Version":"2012-10-17","Statement":[
 {"Sid":"Builds","Effect":"Allow","Action":["codebuild:BatchGetProjects","codebuild:BatchGetBuilds","codebuild:ListBuildsForProject"],
  "Resource":"arn:aws:codebuild:$region:$account:project/$project"},
 {"Sid":"Log","Effect":"Allow","Action":"logs:GetLogEvents",
  "Resource":"arn:aws:logs:$region:$account:log-group:/aws/codebuild/$project:log-stream:*"}
]}
POLICY
}
