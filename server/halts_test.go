package server_test

import (
	"slices"
	"testing"
	"time"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
)

// A halt is stored, then cancels every active run of
// the agent as a cancel does: a pending one now, a running one at its next
// checkpoint, a paused one with its held calls withdrawn in KIFF. An ended
// run is left as it is. A resume keeps when the halt began.
func TestHaltCancelsTheAgentsRuns(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	w := &withdrawals{answers: map[string]string{"exc_1": "invalidated"}}
	f.server.Withdraw = w.withdraw
	f.restart()
	pending, running, done := f.stored(run.Pending), f.stored(run.Running), f.stored(run.Completed)
	paused := f.heldOn("exc_1")

	code, body := f.do("t2", "POST", "/agents/payouts/halt", nil)
	f.want(404, code, body, "halting another tenant's agent")
	code, body = f.do("t1", "POST", "/agents/payouts/halt", nil)
	f.want(200, code, body, "halt")
	if body["halted"] != true || body["own"] != true || body["tenant"] != false || body["stopped"] != float64(3) || body["halted_at"] == nil {
		t.Fatalf("halt answered %v", body)
	}
	for _, c := range []struct {
		r      run.Run
		status run.Status
		cancel bool
	}{{pending, run.Cancelled, true}, {running, run.Running, true}, {paused, run.Paused, true}, {done, run.Completed, false}} {
		got, _ := f.runs.Get(ctx, "t1", c.r.ID)
		if got.Status != c.status || got.Cancel != c.cancel {
			t.Errorf("%s run after the halt: %s, cancel %v; want %s, cancel %v", c.r.Status, got.Status, got.Cancel, c.status, c.cancel)
		}
	}
	if want := []string{"t1/" + paused.ID + "/exc_1"}; !slices.Equal(w.asked, want) {
		t.Fatalf("withdrew %v, want %v", w.asked, want)
	}

	code, body = f.do("t1", "GET", "/agents/payouts/halt", nil)
	f.want(200, code, body, "read the halt")
	if body["halted"] != true {
		t.Fatalf("the halt reads %v", body)
	}
	code, body = f.do("t1", "POST", "/agents/payouts/resume", nil)
	f.want(200, code, body, "resume")
	if body["halted"] != false || body["halted_at"] == nil {
		t.Fatalf("resume answered %v; want resumed, the halt's time kept", body)
	}
	code, body = f.do("t1", "POST", "/agents/nobody/halt", nil)
	f.want(404, code, body, "halting an agent that isn't stored")
}

// A paused run whose held call KIFF gave no ID to withdraw by can't be
// cancelled here; the halt still marks it to stop, and says so: KIFF refuses
// its held calls while halted, and it ends at its first checkpoint after.
func TestHaltMarksAPausedRunItCantWithdraw(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	paused := f.stored(run.Paused) // its requirement has no exception ID

	code, body := f.do("t1", "POST", "/agents/payouts/halt", nil)
	f.want(200, code, body, "halt")
	if body["stopped"] != float64(0) || body["unstopped"] != float64(1) {
		t.Fatalf("halt answered %v; want one run marked, not stopped", body)
	}
	if got, _ := f.runs.Get(ctx, "t1", paused.ID); got.Status != run.Paused || !got.Cancel {
		t.Fatalf("the paused run after the halt: %+v", got)
	}
}

// Halt all halts every agent of the tenant, those it makes meanwhile too,
// and cancels their runs. Resuming one agent doesn't release it while the
// tenant is halted; resuming the tenant leaves an agent halted on its own
// halted. Another tenant is never touched.
func TestHaltAll(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.putAgent("t1", "refunds")
	f.putAgent("t2", "payouts")
	pending := f.stored(run.Pending)

	code, body := f.do("t1", "POST", "/halt", nil)
	f.want(200, code, body, "halt all")
	if body["halted"] != true || body["tenant"] != true || body["stopped"] != float64(1) {
		t.Fatalf("halt all answered %v", body)
	}
	if got, _ := f.runs.Get(ctx, "t1", pending.ID); got.Status != run.Cancelled {
		t.Fatalf("a pending run after halt all: %s", got.Status)
	}
	f.putAgent("t1", "made-meanwhile")
	for _, id := range []string{"payouts", "refunds", "made-meanwhile"} {
		code, body = f.do("t1", "GET", "/agents/"+id+"/halt", nil)
		if code != 200 || body["halted"] != true || body["tenant"] != true || body["own"] != false {
			t.Errorf("%s under halt all: %d %v", id, code, body)
		}
	}
	if code, body = f.do("t2", "GET", "/agents/payouts/halt", nil); body["halted"] != false {
		t.Fatalf("another tenant's agent: %d %v", code, body)
	}

	f.do("t1", "POST", "/agents/refunds/halt", nil)
	code, body = f.do("t1", "POST", "/agents/payouts/resume", nil)
	if code != 200 || body["halted"] != true {
		t.Fatalf("resuming one agent under halt all: %d %v; want it still halted", code, body)
	}
	code, body = f.do("t1", "POST", "/resume", nil)
	f.want(200, code, body, "resume all")
	for id, halted := range map[string]bool{"payouts": false, "made-meanwhile": false, "refunds": true} {
		if _, body = f.do("t1", "GET", "/agents/"+id+"/halt", nil); body["halted"] != halted {
			t.Errorf("%s after resume all: %v; want halted %v", id, body, halted)
		}
	}
}

// What came before a halt never runs once resumed: the halt's time stays, and
// a run created at or before it is refused.
func TestAHaltOutlivesItsResume(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	at := now.Add(-time.Minute)
	if _, err := f.agents.Halt(ctx, "t1", "payouts", at); err != nil {
		t.Fatal(err)
	}
	if _, err := f.agents.Resume(ctx, "t1", "payouts"); err != nil {
		t.Fatal(err)
	}
	h, err := registry.Halts(ctx, f.agents, "t1", "payouts")
	if err != nil || h.On || !h.Before(at) || !h.Before(at.Add(-time.Second)) || h.Before(now) {
		t.Fatalf("Halts after the resume = %+v, %v", h, err)
	}
}
