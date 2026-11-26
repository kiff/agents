package infra

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Every Bedrock setting the function reads (cmd/lambda/source.go) is kept by
// aws.sh across a rerun of Infra, as given or as the function has it. One it
// didn't keep was dropped by the next run: the store reviewers' slice and each
// account's share went back to their defaults, which are other limits.
func TestARerunKeepsEveryBedrockSetting(t *testing.T) {
	src, err := os.ReadFile("../cmd/lambda/source.go")
	if err != nil {
		t.Fatal(err)
	}
	var read []string
	for _, m := range regexp.MustCompile(`"(AGENTS_(?:MODEL|BEDROCK)_[A-Z_]+)"`).FindAllStringSubmatch(string(src), -1) {
		if !slices.Contains(read, m[1]) {
			read = append(read, m[1])
		}
	}
	if len(read) < 5 {
		t.Fatalf("read %d settings from source.go, want the source, fallback, cap, share and reserved ones: %v", len(read), read)
	}

	script, err := os.ReadFile("aws.sh")
	if err != nil {
		t.Fatal(err)
	}
	loop := regexp.MustCompile(`(?s)\nfor k in (AGENTS_MODEL_SOURCE.*?); do`).FindStringSubmatch(string(script))
	if loop == nil {
		t.Fatal("aws.sh's loop that keeps the Bedrock settings isn't there")
	}
	kept := strings.Fields(strings.ReplaceAll(loop[1], "\\", " "))
	for _, k := range read {
		if !slices.Contains(kept, k) {
			t.Errorf("%s is read by the function but not kept by aws.sh: a rerun of Infra would drop it", k)
		}
	}
}
