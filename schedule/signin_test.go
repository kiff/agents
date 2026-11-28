package schedule_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kiff/agents/schedule"
	"github.com/kiff/agents/schedule/scheduletest"
)

// Browser batch 2: a firing of a taf on a hosted-browser connection checks its
// saved sign-in first, with the taf's own key. A sign-in that stopped working
// holds the run before it starts; working, unclear or unreadable, it runs.
func TestASignInThatStoppedHoldsTheRunBeforeItStarts(t *testing.T) {
	for _, c := range []struct {
		name    string
		stopped *schedule.SignInStopped
		err     error
		runs    bool
	}{
		{"stopped", &schedule.SignInStopped{Site: "supplier-portal.example"}, nil, false},
		{"working or unknown", nil, nil, true},
		{"planque down", nil, errors.New("planque answered 503"), true},
	} {
		f := newFixture()
		var asked []string
		f.sv.SignedIn = func(_ context.Context, tenant, agent, grant string) (*schedule.SignInStopped, error) {
			asked = append(asked, tenant+"/"+agent+"/"+grant)
			return c.stopped, c.err
		}
		s := scheduletest.Schedule("t1", "morning")
		s.SignIn = &schedule.SignIn{Grant: "grt_browser1"}
		if err := f.sv.Put(ctx, s); err != nil {
			t.Fatal(err)
		}
		if err := f.sv.Fire(ctx, "t1", "morning", due); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(asked) != 1 || asked[0] != "t1/payouts/grt_browser1" {
			t.Fatalf("%s: asked %v", c.name, asked)
		}
		if (len(f.started) == 1) != c.runs {
			t.Fatalf("%s: started %d runs", c.name, len(f.started))
		}
		h, err := f.history.Firings(ctx, "t1", "morning", 1)
		if err != nil || len(h) != 1 {
			t.Fatalf("%s: history %v %v", c.name, h, err)
		}
		if !c.runs {
			if h[0].Outcome != schedule.FiredHeldSignIn || h[0].Grant != "grt_browser1" || h[0].Site != "supplier-portal.example" {
				t.Fatalf("held as %+v", h[0])
			}
			// Held, not failed: the schedule's failing is untouched.
			stored, _ := f.store.Get(ctx, "t1", "morning")
			if stored.Failing != nil {
				t.Fatalf("a held firing counted as failing: %+v", stored.Failing)
			}
		}
	}
}

// A schedule without sign_in never asks; one with it names a grant, and a
// follow-up checks none.
func TestASignInIsAskedOnlyWhenSetAndNamesAGrant(t *testing.T) {
	f := newFixture()
	f.sv.SignedIn = func(context.Context, string, string, string) (*schedule.SignInStopped, error) {
		t.Fatal("asked without sign_in")
		return nil, nil
	}
	s := scheduletest.Schedule("t1", "morning")
	_ = f.sv.Put(ctx, s)
	if err := f.sv.Fire(ctx, "t1", "morning", due); err != nil || len(f.started) != 1 {
		t.Fatalf("%v, %d runs", err, len(f.started))
	}
	for _, g := range []string{"", "grt with space", "grt/../x"} {
		bad := scheduletest.Schedule("t1", "bad")
		bad.SignIn = &schedule.SignIn{Grant: g}
		if bad.Check() == nil {
			t.Errorf("grant %q passed", g)
		}
	}
}
