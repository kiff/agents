# bedrock_policy REGION ACCOUNT [MODEL...] prints the one Bedrock policy
# document the backup's callers get (infra/aws.sh, infra/eval-bedrock.sh): to
# call Claude through the EU inference profiles, and nothing broader.
#   - InvokeModel and InvokeModelWithResponseStream only;
#   - on the eu. profiles of MODEL (claude-sonnet-5-5 and claude-opus-5-5
#     when none is named) in REGION, and on those models in the EU regions the
#     profiles route to, only through one of those profiles (a model can't be
#     called directly in another region).
# Sourced, not run.
bedrock_policy() {
  local region=$1 account=$2 models="" profiles="" r m
  shift 2
  [ $# -gt 0 ] || set -- claude-sonnet-5-5 claude-opus-5-5
  for m in "$@"; do
    profiles="$profiles\"arn:aws:bedrock:$region:$account:inference-profile/eu.anthropic.$m\","
    for r in eu-central-1 eu-north-1 eu-south-1 eu-south-2 eu-west-1 eu-west-3; do
      models="$models\"arn:aws:bedrock:$r::foundation-model/anthropic.$m\","
    done
  done
  models=${models%,} profiles=${profiles%,}
  cat <<POLICY
{"Version":"2012-10-17","Statement":[
 {"Sid":"Profiles","Effect":"Allow","Action":["bedrock:InvokeModel","bedrock:InvokeModelWithResponseStream"],
  "Resource":[$profiles]},
 {"Sid":"ModelsThroughAProfile","Effect":"Allow","Action":["bedrock:InvokeModel","bedrock:InvokeModelWithResponseStream"],
  "Resource":[$models],
  "Condition":{"StringLike":{"bedrock:InferenceProfileArn":[$profiles]}}}
]}
POLICY
}
