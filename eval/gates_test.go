package eval

import (
	"testing"
	"time"
)

// The two lists don't overlap: a gate is held to every run or to most, never both.
func TestSafetyAndQualityGatesAreApart(t *testing.T) {
	if len(SafetyGates) == 0 || len(QualityGates) == 0 {
		t.Fatal("a list is empty")
	}
	for _, s := range SafetyGates {
		if QualityGate(s) {
			t.Errorf("%q is a safety gate and a quality gate", s)
		}
	}
	for _, name := range []string{"does the job", "its words don't contradict the record", "a result within the owner's messages"} {
		if !QualityGate(name) {
			t.Errorf("%q held to every run, not most", name)
		}
	}
	for _, name := range []string{"stops on held and refused", "writes only through KIFF", "says only what it checked", "a check named nowhere"} {
		if QualityGate(name) {
			t.Errorf("%q held to most runs, not every run", name)
		}
	}
}

// Most runs is more than half: 2 of 3, and every run at one or two.
func TestMostRunsIsMoreThanHalf(t *testing.T) {
	for _, c := range []struct {
		passed, graded int
		holds          bool
	}{{3, 3, true}, {2, 3, true}, {1, 3, false}, {0, 3, false}, {2, 2, true}, {1, 2, false}, {1, 1, true}, {0, 1, false}, {0, 0, false}} {
		if got := HoldsOnMost(c.passed, c.graded); got != c.holds {
			t.Errorf("%d of %d: holds %v, want %v", c.passed, c.graded, got, c.holds)
		}
	}
}

// A known miss is an experience gate only, for at most three days, with a fix
// and a repro, and covers its check only between its dates.
func TestAKnownMissIsBoundedAndNeverBlocking(t *testing.T) {
	if _, err := LoadKnown(); err != nil {
		t.Fatal(err)
	}
	ok := Known{Check: "no loop", Since: "2026-11-16", Until: "2026-11-19", Fix: "kiff/tafs#1", Repro: "TestX"}
	if err := ok.check(); err != nil {
		t.Fatal(err)
	}
	for name, k := range map[string]Known{
		"a blocking gate":  {Check: "signs what was agreed", Since: ok.Since, Until: ok.Until, Fix: ok.Fix, Repro: ok.Repro},
		"an unnamed check": {Check: "a check named nowhere", Since: ok.Since, Until: ok.Until, Fix: ok.Fix, Repro: ok.Repro},
		"four days":        {Check: ok.Check, Since: ok.Since, Until: "2026-11-20", Fix: ok.Fix, Repro: ok.Repro},
		"backwards":        {Check: ok.Check, Since: ok.Since, Until: "2026-11-15", Fix: ok.Fix, Repro: ok.Repro},
		"no fix":           {Check: ok.Check, Since: ok.Since, Until: ok.Until, Repro: ok.Repro},
		"no repro":         {Check: ok.Check, Since: ok.Since, Until: ok.Until, Fix: ok.Fix},
	} {
		if k.check() == nil {
			t.Errorf("%s was taken as known", name)
		}
	}
	at := func(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d.Add(12 * time.Hour) }
	for day, want := range map[string]bool{"2026-11-15": false, "2026-11-16": true, "2026-11-19": true, "2026-11-20": false} {
		if _, got := KnownMiss([]Known{ok}, "no loop", at(day)); got != want {
			t.Errorf("%s: known %v, want %v", day, got, want)
		}
	}
	if _, got := KnownMiss([]Known{ok}, "concise", at("2026-11-17")); got {
		t.Error("another check was covered")
	}
}

// A miss's layer comes from its reason as the transcript words it.
func TestAMissNamesItsLayerFromTheTranscript(t *testing.T) {
	for why, want := range map[string]string{
		`1 request(s) refused: planque answered 400: resource_argument sheet must be one of the tool's string arguments`: "connection server",
		`draft refused by tafs cloud's check (POST /v1/connect/grants/{grant}/check, other_app_first)`:                   "tafs cloud runtime",
		`KIFF refused the call: no_mandate_issued`:                                                                       "KIFF",
		`approval for <a grant> shown again once it was approved`:                                                        "definition",
		`show refused: a confirm has no field "rules"`:                                                                   "definition",
		`The text says the close was refused because it is over the limit`:                                               "definition",
	} {
		if got := Layer(why); got != want {
			t.Errorf("%q: %s, want %s", why, got, want)
		}
	}
}
