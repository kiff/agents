package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Eval says what tafs cloud, planque and agents ran: each one's newest
// successful deploy on main, read at the start and at the end
// (infra/eval-deployed.sh). A deploy between the two, one under way, or a side
// not read makes the run mixed, and a mixed run ends red: kiff/tafs' Upgrade
// gate takes it only once it's judged.
func TestTheEvalSaysWhatWasDeployedAndAMixedRunEndsRed(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "eval.yml"))
	if err != nil {
		t.Fatal(err)
	}
	steps := map[string]string{}
	var order []string
	for _, part := range strings.Split(string(b), "\n      - ")[1:] {
		name, _, _ := strings.Cut(part, "\n")
		name = strings.TrimPrefix(name, "name: ")
		steps[name] = part
		order = append(order, name)
	}
	at := func(name string) int {
		for i, n := range order {
			if n == name {
				return i
			}
		}
		t.Fatalf("no step %q", name)
		return -1
	}
	start, end, gate := "What's deployed, at the start", "What's deployed, at the end", "A run with a deploy in the middle counts only once judged"
	// The start is read before the first suite, the end after the sweep and
	// before the report goes up, whatever the suites did.
	if at(start) > at("Chargebacks, two seeded runs") {
		t.Errorf("%q comes after the first suite", start)
	}
	if at(end) < at("The sweep, the test account's leftover tafs") || at(end) > at("The full report, to the private bucket") {
		t.Errorf("%q isn't between the sweep and the report", end)
	}
	for _, want := range []string{"always()", "id: deployed", "infra/eval-deployed.sh compare deployed-start.txt deployed-end.txt > deployed.md", `echo "mixed=$mixed" >> "$GITHUB_OUTPUT"`} {
		if !strings.Contains(steps[end], want) {
			t.Errorf("%q doesn't hold %s", end, want)
		}
	}
	if !strings.Contains(steps["The full report, to the private bucket"], " deployed.md; do") {
		t.Error("the report doesn't carry deployed.md")
	}
	if !strings.Contains(steps[gate], "if: always() && steps.deployed.outputs.mixed == 'true'") || !strings.Contains(steps[gate], "exit 1") {
		t.Errorf("%q doesn't end a mixed run red", gate)
	}

	// The comparison, on the script itself.
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	same := "tafs-cloud 46b97dd192121ff5175af9c7e0c6b00f3072a619 2026-11-17T13:18:38Z 1\n" +
		"planque 80265693b9b05ab1f206c9d3164030c5d1dadc22 2026-11-17T11:35:01Z 2\n" +
		"agents 7577749b1c7098d785a41bf47c5632d06b1985e9 2026-11-17T11:48:12Z 3\n"
	a := write("a", same)
	for _, c := range []struct {
		name, end, want string
		mixed           bool
	}{
		{"nothing changed", same, "deployed: unchanged", false},
		{"planque deployed in between", strings.Replace(same, "80265693b9b05ab1f206c9d3164030c5d1dadc22 2026-11-17T11:35:01Z 2", "1111111111111111111111111111111111111111 2026-11-17T12:40:00Z 9", 1), "deployed: mixed: planque", true},
		{"agents deploying at the end", "agents deploying 9\n" + same, "deployed: mixed: agents", true},
		{"tafs cloud not read at the end", strings.SplitN(same, "\n", 2)[1], "deployed: mixed: tafs-cloud", true},
		{"tafs cloud's deploy not found", strings.Replace(same, "tafs-cloud 46b97dd192121ff5175af9c7e0c6b00f3072a619 2026-11-17T13:18:38Z 1", "tafs-cloud unknown", 1), "deployed: mixed: tafs-cloud", true},
	} {
		out, err := exec.Command("bash", "../../infra/eval-deployed.sh", "compare", a, write("end", c.end)).Output()
		mixed := err != nil
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() != 3 {
			t.Errorf("%s: exit %d, want 3", c.name, ee.ExitCode())
		}
		if mixed != c.mixed || !strings.Contains(string(out), c.want) {
			t.Errorf("%s: mixed %v, want %v; got\n%s", c.name, mixed, c.mixed, out)
		}
	}
	// A start that wasn't read proves nothing either.
	out, err := exec.Command("bash", "../../infra/eval-deployed.sh", "compare", filepath.Join(dir, "none"), a).Output()
	if err == nil || !strings.Contains(string(out), "deployed: mixed: tafs-cloud planque agents") {
		t.Errorf("no start: %v\n%s", err, out)
	}
}
