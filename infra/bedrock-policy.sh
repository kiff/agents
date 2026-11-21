# bedrock_policy REGION ACCOUNT prints the one Bedrock policy document the
# backup's callers get (infra/aws.sh, infra/eval-bedrock.sh): to call Claude
# through the EU inference profiles, and nothing broader.
#   - InvokeModel and InvokeModelWithResponseStream only;
#   - on the two eu. profiles in REGION, and on the models in the EU regions
#     those profiles route to, only through a profile (a model can't be called
#     directly in another region).
# Sourced, not run.
bedrock_policy() {
  local region=$1 account=$2 models="" r m
  for r in eu-central-1 eu-north-1 eu-south-1 eu-south-2 eu-west-1 eu-west-3; do
    for m in anthropic.claude-sonnet-5-5 anthropic.claude-opus-5-5; do
      models="$models\"arn:aws:bedrock:$r::foundation-model/$m\","
    done
  done
  models=${models%,}
  cat <<POLICY
{"Version":"2012-10-17","Statement":[
 {"Sid":"Profiles","Effect":"Allow","Action":["bedrock:InvokeModel","bedrock:InvokeModelWithResponseStream"],
  "Resource":["arn:aws:bedrock:$region:$account:inference-profile/eu.anthropic.claude-sonnet-5-5",
              "arn:aws:bedrock:$region:$account:inference-profile/eu.anthropic.claude-opus-5-5"]},
 {"Sid":"ModelsThroughAProfile","Effect":"Allow","Action":["bedrock:InvokeModel","bedrock:InvokeModelWithResponseStream"],
  "Resource":[$models],
  "Condition":{"StringLike":{"bedrock:InferenceProfileArn":"arn:aws:bedrock:$region:$account:inference-profile/eu.anthropic.claude-*-5-5"}}}
]}
POLICY
}
