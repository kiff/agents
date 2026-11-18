package main

import (
	"errors"
	"slices"
	"testing"

	"github.com/kiff/agents/cmd/internal/testaccount"
)

func grantOf(id, agent, status string) testaccount.Grant {
	var g testaccount.Grant
	g.ID, g.Status, g.Request.Agent = id, status, agent
	return g
}

// The sweep ends every unfinished grant of the tafs this job made (one a
// failed sign-in left approved, above all), and nothing else: not another
// taf's, not Setup's, not one planque already finished.
func TestTheSweepEndsOnlyItsTafsUnfinishedGrants(t *testing.T) {
	gs := []testaccount.Grant{
		grantOf("g-approved", "refunds-1", "approved"), // a sign-in that failed after the phone approved
		grantOf("g-pending", "refunds-1", "pending"),
		grantOf("g-signed", "refunds-1", "signed_in"),
		grantOf("g-connected", "desk-2", "connected"),
		grantOf("g-done", "refunds-1", "disconnected"),
		grantOf("g-failed", "desk-2", "failed"),
		grantOf("g-setup", "setup", "connected"),
		grantOf("g-other", "owners-taf", "approved"),
		grantOf("g-other-done", "owners-taf", "disconnected"),
	}
	var ended []string
	g := endTheirGrants(gs, theirs([]string{"refunds-1", "desk-2", "setup", ""}, "setup"), func(id string) error {
		ended = append(ended, id)
		return nil
	})
	if want := []string{"g-approved", "g-pending", "g-signed", "g-connected"}; !slices.Equal(ended, want) {
		t.Fatalf("ended %v, want %v", ended, want)
	}
	if g.ended != 4 || g.failed != 0 || g.others != 2 || g.err() != nil {
		t.Fatalf("counts: %+v", g)
	}
}

// One grant that can't be ended doesn't stop the others, and fails the sweep's step
// (which is continue-on-error: it never decides the run).
func TestASweepGrantThatWontEndIsCountedAndTheRestGoOn(t *testing.T) {
	gs := []testaccount.Grant{grantOf("a", "t", "approved"), grantOf("b", "t", "connected"), grantOf("c", "t", "pending")}
	var ended []string
	g := endTheirGrants(gs, theirs([]string{"t"}, ""), func(id string) error {
		if id == "b" {
			return errors.New("planque said no")
		}
		ended = append(ended, id)
		return nil
	})
	if !slices.Equal(ended, []string{"a", "c"}) || g.ended != 2 || g.failed != 1 || g.err() == nil {
		t.Fatalf("ended %v, counts %+v", ended, g)
	}
}

// Nothing is ended for a job that made no taf, and Setup's agent is never one of them.
func TestNoMadeTafsEndsNoGrant(t *testing.T) {
	gs := []testaccount.Grant{grantOf("s", "setup", "approved"), grantOf("x", "", "approved")}
	g := endTheirGrants(gs, theirs([]string{"setup"}, "setup"), func(string) error { t.Fatal("a grant was ended"); return nil })
	if g.ended != 0 || g.others != 2 {
		t.Fatalf("counts %+v", g)
	}
}
