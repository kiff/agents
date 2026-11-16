package eval

import "strings"

// Layer says where a missed check's cause sits, read from its reason as the
// transcript words it, never guessed (decided 16 Nov 2026): a refusal names
// the service that refused. Only a miss in the definitions (Coach's skills,
// a taf's instructions) is the Upgrade's to fix; the others are fixed and
// deployed where they sit, with their own tests.
func Layer(why string) string {
	w := strings.ToLower(why)
	switch {
	case strings.Contains(w, "planque answered"), strings.Contains(w, "planque refused"):
		return "connection server"
	case strings.Contains(w, "tafs cloud's check"), strings.Contains(w, "tafs cloud answered"):
		return "tafs cloud runtime"
	case strings.Contains(w, "kiff refused"), strings.Contains(w, "kiff answered"):
		return "KIFF"
	case strings.Contains(w, "the harness"), strings.Contains(w, "the fake vendor answered"):
		return "harness"
	}
	// show's refusals, Coach's words, the judge's reading: the definitions.
	return "definition"
}
