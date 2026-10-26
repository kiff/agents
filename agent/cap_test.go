package agent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestALongToolResultIsCutAndSaysSo(t *testing.T) {
	long := strings.Repeat("é", MaxToolResult) // 2 bytes a rune: twice the cap
	got := capResult(long, MaxToolResult)
	if !utf8.ValidString(got) {
		t.Fatal("cut inside a rune")
	}
	if !strings.HasPrefix(got, long[:MaxToolResult]) || !strings.Contains(got, "cut at 32 KB of 64 KB") || !strings.Contains(got, "Call again for less") {
		t.Fatalf("got …%q", got[len(got)-200:])
	}
	if short := "ok"; capResult(short, MaxToolResult) != short {
		t.Fatal("a short result was changed")
	}
}
