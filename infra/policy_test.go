package infra

import (
	"encoding/json"
	"os/exec"
	"path"
	"strings"
	"testing"
)

// The documents infra/github-infra.sh writes, as infra/github-infra-policy.sh
// renders them, read the way IAM does: an explicit Deny wins, then any Allow
// whose action, resource and conditions all match; nothing else is allowed.

const account = "123456789012"

type statement struct {
	Sid       string
	Effect    string
	Action    any
	NotAction any
	Resource  any
	Condition map[string]map[string]any
}

type policy struct{ Statement []statement }

func render(t *testing.T, fn string) policy {
	t.Helper()
	out, err := exec.Command("bash", "-c", `. ./github-infra-policy.sh && "$0" eu-west-1 `+account+` kiff-agents-eval-reports kiff-agents-attachments`, fn).Output()
	if err != nil {
		t.Fatalf("%s: %v", fn, err)
	}
	var p policy
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("%s isn't JSON: %v\n%s", fn, err, out)
	}
	return p
}

func list(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, len(v))
		for i, s := range v {
			out[i] = s.(string)
		}
		return out
	}
	return nil
}

// like is IAM's wildcard match: * any run, ? one character.
func like(pattern, s string) bool {
	ok, err := path.Match(strings.ReplaceAll(pattern, "/", "\x00"), strings.ReplaceAll(s, "/", "\x00"))
	return err == nil && ok
}

func anyLike(patterns []string, s string, fold bool) bool {
	for _, p := range patterns {
		if fold && like(strings.ToLower(p), strings.ToLower(s)) || !fold && like(p, s) {
			return true
		}
	}
	return false
}

func (s statement) matches(t *testing.T, action, resource string, ctx map[string]string) bool {
	t.Helper()
	if s.Action != nil && !anyLike(list(s.Action), action, true) || s.NotAction != nil && anyLike(list(s.NotAction), action, true) {
		return false
	}
	if !anyLike(list(s.Resource), resource, false) {
		return false
	}
	for op, keys := range s.Condition {
		for key, want := range keys {
			got, ok := ctx[key]
			switch op {
			case "StringEquals":
				if !ok || !slicesContains(list(want), got) {
					return false
				}
			case "StringLike":
				if !ok || !anyLike(list(want), got, false) {
					return false
				}
			default:
				t.Fatalf("statement %q: the test doesn't read the condition %s", s.Sid, op)
			}
		}
	}
	return true
}

func slicesContains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func (p policy) allows(t *testing.T, action, resource string, ctx map[string]string) bool {
	t.Helper()
	allowed := false
	for _, s := range p.Statement {
		if s.matches(t, action, resource, ctx) {
			if s.Effect == "Deny" {
				return false
			}
			allowed = true
		}
	}
	return allowed
}

type try struct {
	action, resource string
	ctx              map[string]string
	allow            bool
}

func check(t *testing.T, p policy, tries []try) {
	t.Helper()
	for _, c := range tries {
		if got := p.allows(t, c.action, c.resource, c.ctx); got != c.allow {
			t.Errorf("%s on %s with %v: allowed %v, want %v", c.action, c.resource, c.ctx, got, c.allow)
		}
	}
}

func role(name string) string { return "arn:aws:iam::" + account + ":role/" + name }

var boundary = "arn:aws:iam::" + account + ":policy/agents-boundary"

func TestTheInfraRoleWritesRolesOnlyUnderTheBoundary(t *testing.T) {
	p := render(t, "infra_role_policy")
	under := map[string]string{"iam:PermissionsBoundary": boundary}
	other := map[string]string{"iam:PermissionsBoundary": "arn:aws:iam::" + account + ":policy/wide-open"}
	check(t, p, []try{
		// Its own roles, carrying the boundary: what infra/aws.sh does.
		{"iam:CreateRole", role("agents-lambda"), under, true},
		{"iam:PutRolePolicy", role("agents-kiffalerts"), under, true},
		{"iam:PutRolePermissionsBoundary", role("agents-github-deploy"), under, true},
		{"iam:GetRole", role("agents-scheduler"), nil, true},
		{"iam:GetPolicy", boundary, nil, true},
		// A role made or written without the boundary, or with another one.
		{"iam:CreateRole", role("agents-lambda"), nil, false},
		{"iam:CreateRole", role("agents-lambda"), other, false},
		{"iam:PutRolePolicy", role("agents-lambda"), nil, false},
		{"iam:PutRolePolicy", role("agents-lambda"), other, false},
		{"iam:PutRolePermissionsBoundary", role("agents-lambda"), other, false},
		// A role by any other name, even an agents- one, even with the boundary.
		{"iam:CreateRole", role("agents-anything"), under, false},
		{"iam:PutRolePolicy", role("agents-anything"), under, false},
		{"iam:CreateRole", role("someone-else"), under, false},
		// Its own role, ever.
		{"iam:PutRolePolicy", role("agents-github-infra"), under, false},
		{"iam:UpdateAssumeRolePolicy", role("agents-github-infra"), nil, false},
		{"iam:PutRolePermissionsBoundary", role("agents-github-infra"), under, false},
		// No way around the boundary: removing it, rewriting it, a managed
		// policy, a trust, deleting a role's policy.
		{"iam:DeleteRolePermissionsBoundary", role("agents-lambda"), under, false},
		{"iam:CreatePolicyVersion", boundary, nil, false},
		{"iam:SetDefaultPolicyVersion", boundary, nil, false},
		{"iam:DeletePolicy", boundary, nil, false},
		{"iam:CreatePolicy", "arn:aws:iam::" + account + ":policy/wide-open", nil, false},
		{"iam:AttachRolePolicy", role("agents-lambda"), under, false},
		{"iam:UpdateAssumeRolePolicy", role("agents-github-deploy"), under, false},
		{"iam:DeleteRolePolicy", role("agents-lambda"), under, false},
		// PassRole: the two functions' roles to Lambda, the scheduler's to the
		// scheduler, nothing else.
		{"iam:PassRole", role("agents-lambda"), map[string]string{"iam:PassedToService": "lambda.amazonaws.com"}, true},
		{"iam:PassRole", role("agents-kiffalerts"), map[string]string{"iam:PassedToService": "lambda.amazonaws.com"}, true},
		{"iam:PassRole", role("agents-scheduler"), map[string]string{"iam:PassedToService": "scheduler.amazonaws.com"}, true},
		{"iam:PassRole", role("agents-github-deploy"), map[string]string{"iam:PassedToService": "lambda.amazonaws.com"}, false},
		{"iam:PassRole", role("agents-anything"), map[string]string{"iam:PassedToService": "lambda.amazonaws.com"}, false},
		{"iam:PassRole", role("agents-lambda"), map[string]string{"iam:PassedToService": "ec2.amazonaws.com"}, false},
		{"iam:PassRole", role("agents-lambda"), nil, false},
		{"iam:PassRole", role("agents-scheduler"), map[string]string{"iam:PassedToService": "lambda.amazonaws.com"}, false},
		// Its functions only.
		{"lambda:UpdateFunctionCode", "arn:aws:lambda:eu-west-1:" + account + ":function:agents-kiffalerts", nil, true},
		{"lambda:UpdateFunctionConfiguration", "arn:aws:lambda:eu-west-1:" + account + ":function:planque", nil, false},
		{"lambda:CreateFunction", "arn:aws:lambda:eu-west-1:" + account + ":function:agents-new", nil, false},
		// No secret's value.
		{"secretsmanager:GetSecretValue", "arn:aws:secretsmanager:eu-west-1:" + account + ":secret:agents-AbCdEf", nil, false},
	})
}

func TestTheBoundaryHoldsAgentsOwnResourcesOnly(t *testing.T) {
	p := render(t, "agents_boundary_policy")
	fn := "arn:aws:lambda:eu-west-1:" + account + ":function:"
	secret := "arn:aws:secretsmanager:eu-west-1:" + account + ":secret:"
	key := "arn:aws:kms:eu-west-1:" + account + ":key/0000"
	check(t, p, []try{
		// What infra/aws.sh's roles need between them.
		{"logs:PutLogEvents", "arn:aws:logs:eu-west-1:" + account + ":log-group:/aws/lambda/agents-kiffalerts:log-stream:x", nil, true},
		{"dynamodb:Query", "arn:aws:dynamodb:eu-west-1:" + account + ":table/agents", nil, true},
		{"dynamodb:BatchWriteItem", "arn:aws:dynamodb:eu-west-1:" + account + ":table/agents", nil, true},
		{"secretsmanager:GetSecretValue", secret + "agents-AbCdEf", nil, true},
		{"secretsmanager:GetSecretValue", secret + "agents-model/anthropic-AbCdEf", nil, true},
		{"secretsmanager:GetSecretValue", secret + "agents-accounts/tnt-AbCdEf", nil, true},
		{"secretsmanager:CreateSecret", secret + "agents-sealing/tnt-AbCdEf", nil, true},
		{"secretsmanager:DeleteSecret", secret + "agents-drafts/d-AbCdEf", nil, true},
		{"secretsmanager:RestoreSecret", secret + "agents/k-AbCdEf", nil, true},
		{"secretsmanager:ListSecrets", "*", nil, true},
		{"kms:Decrypt", key, map[string]string{"kms:EncryptionContext:purpose": "kiff/agents alert secret v1"}, true},
		{"kms:Encrypt", key, map[string]string{"kms:EncryptionContext:purpose": "kiff/agents draft record v1"}, true},
		{"lambda:InvokeFunction", fn + "agents", nil, true},
		{"lambda:UpdateFunctionCode", fn + "agents-kiffalerts", nil, true},
		{"s3:PutObject", "arn:aws:s3:::kiff-agents-attachments/t/a", nil, true},
		{"s3:ListBucket", "arn:aws:s3:::kiff-agents-attachments", nil, true},
		{"s3:PutObject", "arn:aws:s3:::kiff-agents-eval-reports/reports/branches/r.json", nil, true},
		{"scheduler:CreateSchedule", "arn:aws:scheduler:eu-west-1:" + account + ":schedule/agents/s", nil, true},
		{"iam:PassRole", role("agents-scheduler"), map[string]string{"iam:PassedToService": "scheduler.amazonaws.com"}, true},
		{"bedrock:InvokeModel", "arn:aws:bedrock:eu-west-1:" + account + ":inference-profile/eu.anthropic.claude-sonnet-5-5", nil, true},
		// More than that, whatever a role's own policy says.
		{"iam:CreateRole", role("agents-lambda"), nil, false},
		{"iam:PutRolePolicy", role("agents-lambda"), nil, false},
		{"iam:PassRole", role("agents-lambda"), map[string]string{"iam:PassedToService": "lambda.amazonaws.com"}, false},
		{"iam:PassRole", role("agents-scheduler"), map[string]string{"iam:PassedToService": "lambda.amazonaws.com"}, false},
		{"sts:AssumeRole", "arn:aws:iam::" + account + ":role/OrganizationAccountAccessRole", nil, false},
		{"secretsmanager:GetSecretValue", secret + "kiff/cloud/all-AbCdEf", nil, false},
		{"secretsmanager:GetSecretValue", secret + "planque-AbCdEf", nil, false},
		{"secretsmanager:GetSecretValue", secret + "agents-more-than-six-AbCdEf", nil, false},
		{"secretsmanager:PutSecretValue", secret + "agents-AbCdEf", nil, false},
		{"kms:Decrypt", key, map[string]string{"kms:EncryptionContext:purpose": "kiff/planque grant v1"}, false},
		{"kms:Decrypt", key, nil, false},
		{"kms:CreateGrant", key, map[string]string{"kms:EncryptionContext:purpose": "kiff/agents taf key v1"}, false},
		{"dynamodb:Scan", "arn:aws:dynamodb:eu-west-1:" + account + ":table/agents", nil, false},
		{"dynamodb:GetItem", "arn:aws:dynamodb:eu-west-1:" + account + ":table/planque", nil, false},
		{"dynamodb:DeleteTable", "arn:aws:dynamodb:eu-west-1:" + account + ":table/agents", nil, false},
		{"lambda:UpdateFunctionCode", fn + "planque", nil, false},
		{"lambda:InvokeFunction", fn + "tafs-cloud", nil, false},
		{"s3:GetObject", "arn:aws:s3:::kiff-agents-eval-reports/reports/r.json", nil, false},
		{"s3:GetObject", "arn:aws:s3:::some-other-bucket/x", nil, false},
		{"bedrock:InvokeModel", "arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-opus-5-5", nil, false},
	})
}
