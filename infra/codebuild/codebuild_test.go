package codebuild

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The backup run (run.sh) is eval.yml's full run outside GitHub: the same
// suites, in the same order, each with the same command and limit. A suite
// changed in one and not the other fails here, so the two can't drift.

type suiteStep struct {
	name    string
	minutes string
	command string
	guard   string
}

var spaces = regexp.MustCompile(`\s+`)

// command joins a shell command's continued lines and reads the repeat as
// the bar's 3, which is eval.yml's default and run.sh's only value.
func command(lines []string) string {
	c := strings.Join(lines, " ")
	c = strings.ReplaceAll(c, "\\ ", " ")
	c = strings.ReplaceAll(c, `"${{ inputs.repeat || '3' }}"`, `"3"`)
	c = strings.ReplaceAll(c, `"'"$REPEAT"'"`, `"3"`)
	return strings.TrimSpace(spaces.ReplaceAllString(c, " "))
}

// commandIn finds the go run line in a step's body, with its continuations.
func commandIn(body string) (cmd, guard string) {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "run:"))
		if strings.HasPrefix(t, "if [ ! -f ") || strings.HasPrefix(t, "if ! jq ") {
			guard = command([]string{t})
		}
		if !strings.HasPrefix(t, "go run ./cmd/eval -run") && !strings.HasPrefix(t, "go run ./cmd/e2e $(") {
			continue
		}
		var parts []string
		for j := i; j < len(lines); j++ {
			p := strings.TrimSpace(lines[j])
			if j == i {
				p = t
			}
			parts = append(parts, p)
			if !strings.HasSuffix(p, "\\") {
				break
			}
		}
		return command(parts), guard
	}
	return "", guard
}

func evalSuites(t *testing.T) []suiteStep {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "eval.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var out []suiteStep
	for _, part := range strings.Split(string(b), "\n      - ")[1:] {
		head, body, _ := strings.Cut(part, "\n")
		if !strings.HasPrefix(head, "name: ") {
			continue
		}
		cmd, guard := commandIn(body)
		if cmd == "" {
			continue
		}
		m := regexp.MustCompile(`\n        timeout-minutes: (\d+)`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("eval.yml's %q has no limit", head)
		}
		out = append(out, suiteStep{strings.TrimPrefix(head, "name: "), m[1], cmd, guard})
	}
	return out
}

func runSuites(t *testing.T) []suiteStep {
	t.Helper()
	b, err := os.ReadFile("run.sh")
	if err != nil {
		t.Fatal(err)
	}
	var out []suiteStep
	head := regexp.MustCompile(`^"([^"]+)" (\d+)\s`)
	for _, part := range strings.Split(string(b), "\nsuite ")[1:] {
		// Each suite's body is one single-quoted shell string: read it as the
		// shell does.
		// Its first line opens the quote; the first line after that ending
		// in a quote closes it.
		lines := strings.Split(part, "\n")
		for i := 1; i < len(lines); i++ {
			if strings.HasSuffix(lines[i], "'") {
				lines = append(lines[:i], strings.TrimSuffix(lines[i], "'"))
				break
			}
		}
		lines[0] = strings.TrimSuffix(lines[0], "'")
		part = strings.ReplaceAll(strings.Join(lines, "\n"), `'"'"'`, `'`)
		m := head.FindStringSubmatch(part)
		if m == nil {
			continue
		}
		cmd, guard := commandIn(part)
		out = append(out, suiteStep{m[1], m[2], cmd, guard})
	}
	return out
}

func TestTheBackupRunsEvalYmlsSuitesAsTheyAre(t *testing.T) {
	want, got := evalSuites(t), runSuites(t)
	if len(want) < 10 {
		t.Fatalf("read %d suite steps from eval.yml: the reader is wrong", len(want))
	}
	if len(got) != len(want) {
		t.Fatalf("run.sh has %d suites, eval.yml %d", len(got), len(want))
	}
	for i := range want {
		w, g := want[i], got[i]
		if g.name != w.name {
			t.Errorf("suite %d: run.sh's is %q, eval.yml's %q", i+1, g.name, w.name)
			continue
		}
		if g.minutes != w.minutes {
			t.Errorf("%q: run.sh's limit is %s minutes, eval.yml's %s", w.name, g.minutes, w.minutes)
		}
		if g.command != w.command {
			t.Errorf("%q runs another command:\n eval.yml: %s\n run.sh:   %s", w.name, w.command, g.command)
		}
		if g.guard != w.guard {
			t.Errorf("%q's not-measured check differs:\n eval.yml: %s\n run.sh:   %s", w.name, w.guard, g.guard)
		}
	}
}

// The backup holds what can count for the Upgrade gate only: all suites,
// the bar's repeat, no override, on Bedrock, ending red on a failed suite or
// a mixed run, with the same sweeps.
func TestTheBackupRunsOnlyAFullRunAndFailsAsEvalYmlDoes(t *testing.T) {
	b, err := os.ReadFile("run.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"REPEAT=3\n",
		"export AGENTS_MODEL_SOURCE=bedrock\n",
		"git merge-base --is-ancestor \"$head\" FETCH_HEAD",
		"infra/eval-definitions.sh tafs defs",
		"go run ./cmd/e2e -sweep",
		"go run ./cmd/eval seed -close-all chargebacks",
		"infra/eval-deployed.sh compare deployed-start.txt deployed-end.txt",
		`key="reports/codebuild/`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("run.sh doesn't hold %q", want)
		}
	}
	for _, never := range []string{"inputs.", "${1", "eval-override.sh", "MODEL=", "EFFORT="} {
		if strings.Contains(strings.SplitN(s, "\nsuite ", 2)[0], never) {
			t.Errorf("run.sh takes %q: a backup run has no inputs or overrides", never)
		}
	}
}

// The buildspec's secrets are eval.yml's suites' secrets, and the script's
// list (the build's role may read those alone).
func TestTheBackupsSecretsAreEvalYmlsAndNoMore(t *testing.T) {
	yml, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "eval.yml"))
	if err != nil {
		t.Fatal(err)
	}
	inEval := map[string]bool{}
	for _, m := range regexp.MustCompile(`secrets\.([A-Z0-9_]+)`).FindAllStringSubmatch(string(yml), -1) {
		if !strings.HasPrefix(m[1], "AWS_") {
			inEval[m[1]] = true
		}
	}
	spec, err := os.ReadFile("buildspec.yml")
	if err != nil {
		t.Fatal(err)
	}
	inSpec := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^    ([A-Z0-9_]+): agents-eval/([A-Z0-9_]+)$`).FindAllStringSubmatch(string(spec), -1) {
		if m[1] != m[2] {
			t.Errorf("buildspec maps %s to agents-eval/%s", m[1], m[2])
		}
		inSpec[m[1]] = true
	}
	script, err := os.ReadFile("eval-codebuild.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^SECRETS="([^"]+)"$`).FindStringSubmatch(string(script))
	if m == nil {
		t.Fatal("eval-codebuild.sh has no SECRETS list")
	}
	inScript := map[string]bool{}
	for _, s := range strings.Fields(m[1]) {
		inScript[s] = true
	}
	keys := func(m map[string]bool) string {
		var k []string
		for s := range m {
			k = append(k, s)
		}
		sort.Strings(k)
		return strings.Join(k, " ")
	}
	if keys(inSpec) != keys(inEval) || keys(inScript) != keys(inEval) {
		t.Fatalf("secrets differ:\n eval.yml:          %s\n buildspec:         %s\n eval-codebuild.sh: %s", keys(inEval), keys(inSpec), keys(inScript))
	}
}

type statement struct {
	Sid       string
	Effect    string
	Principal map[string]any
	Action    any
	Resource  any
	Condition map[string]map[string]any
}

type policy struct{ Statement []statement }

func render(t *testing.T, call string) policy {
	t.Helper()
	out, err := exec.Command("bash", "-c", ". ./policy.sh && "+call).Output()
	if err != nil {
		t.Fatalf("%s: %v", call, err)
	}
	var p policy
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("%s isn't JSON: %v\n%s", call, err, out)
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

const account = "123456789012"

func TestOnlyThisProjectsBuildsTakeTheBuildRole(t *testing.T) {
	p := render(t, "codebuild_trust eu-west-1 "+account+" agents-eval")
	if len(p.Statement) != 1 {
		t.Fatalf("%d statements", len(p.Statement))
	}
	s := p.Statement[0]
	if s.Principal["Service"] != "codebuild.amazonaws.com" || list(s.Action)[0] != "sts:AssumeRole" {
		t.Fatalf("trusts %v for %v", s.Principal, s.Action)
	}
	eq := s.Condition["StringEquals"]
	if eq["aws:SourceAccount"] != account || eq["aws:SourceArn"] != "arn:aws:codebuild:eu-west-1:"+account+":project/agents-eval" {
		t.Fatalf("not pinned to the project: %v", s.Condition)
	}
}

func TestTheBuildRoleWritesReportsReadsNineSecretsAndItsLog(t *testing.T) {
	secrets := "ANTHROPIC_API_KEY KIFF_AGENT_KEY"
	p := render(t, "codebuild_policy eu-west-1 "+account+" agents-eval kiff-agents-eval-reports "+secrets)
	got := map[string][]string{}
	for _, s := range p.Statement {
		if s.Effect != "Allow" {
			t.Fatalf("%s: %s", s.Sid, s.Effect)
		}
		for _, a := range list(s.Action) {
			got[a] = append(got[a], list(s.Resource)...)
		}
	}
	want := map[string][]string{
		"s3:PutObject": {"arn:aws:s3:::kiff-agents-eval-reports/reports/codebuild/*"},
		"secretsmanager:GetSecretValue": {
			"arn:aws:secretsmanager:eu-west-1:" + account + ":secret:agents-eval/ANTHROPIC_API_KEY-??????",
			"arn:aws:secretsmanager:eu-west-1:" + account + ":secret:agents-eval/KIFF_AGENT_KEY-??????",
		},
		"logs:CreateLogStream": {"arn:aws:logs:eu-west-1:" + account + ":log-group:/aws/codebuild/agents-eval:*"},
		"logs:PutLogEvents":    {"arn:aws:logs:eu-west-1:" + account + ":log-group:/aws/codebuild/agents-eval:*"},
	}
	if len(got) != len(want) {
		t.Fatalf("actions %v, want %v", got, want)
	}
	for a, res := range want {
		if strings.Join(got[a], " ") != strings.Join(res, " ") {
			t.Errorf("%s on %v, want %v", a, got[a], res)
		}
	}
}

func TestOnlyTafsUpgradeOnMainTakesTheGateRoleAndItOnlyReads(t *testing.T) {
	trust := render(t, "gate_trust "+account)
	if len(trust.Statement) != 1 {
		t.Fatalf("%d statements", len(trust.Statement))
	}
	eq := trust.Statement[0].Condition["StringEquals"]
	if eq["token.actions.githubusercontent.com:sub"] != "repo:kiff/tafs:environment:eval-gate" ||
		eq["token.actions.githubusercontent.com:aud"] != "sts.amazonaws.com" {
		t.Fatalf("gate trust: %v", trust.Statement[0].Condition)
	}
	if len(trust.Statement[0].Condition) != 1 {
		t.Fatalf("a condition beside StringEquals (a wildcard?): %v", trust.Statement[0].Condition)
	}
	p := render(t, "gate_policy eu-west-1 "+account+" agents-eval")
	for _, s := range p.Statement {
		for _, a := range list(s.Action) {
			switch a {
			case "codebuild:BatchGetProjects", "codebuild:BatchGetBuilds", "codebuild:ListBuildsForProject":
				if r := list(s.Resource); len(r) != 1 || r[0] != "arn:aws:codebuild:eu-west-1:"+account+":project/agents-eval" {
					t.Errorf("%s on %v", a, r)
				}
			case "logs:GetLogEvents":
				if r := list(s.Resource); len(r) != 1 || r[0] != "arn:aws:logs:eu-west-1:"+account+":log-group:/aws/codebuild/agents-eval:log-stream:*" {
					t.Errorf("%s on %v", a, r)
				}
			default:
				t.Errorf("the gate may %s: it only reads", a)
			}
		}
	}
}
