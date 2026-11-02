package followup_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/schedule"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/followup"
)

var ctx = context.Background()

type setter struct{ set map[string]schedule.Schedule }

func (s *setter) Set(_ context.Context, sc schedule.Schedule) error { s.set[sc.ID] = sc; return nil }
func (s *setter) Remove(_ context.Context, _, id string) error     { delete(s.set, id); return nil }

func call(t *testing.T, tl *followup.Tool, c context.Context, in string) tool.Result {
	t.Helper()
	r, err := tl.Call(c, json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A taf sets, lists and removes its own follow-ups; the bounds are the
// server's, and a follow-up's own run can't set another.
func TestFollowUps(t *testing.T) {
	now := time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	store, runs := memory.NewSchedules(), memory.NewRuns()
	sv := &schedule.Service{Store: store, Scheduler: &setter{set: map[string]schedule.Schedule{}}, History: store,
		Live: func(context.Context, string, string) (bool, error) { return true, nil }, Now: func() time.Time { return now }}
	tl := followup.New(sv, runs, "t1", "refunds", 3)
	if tl.Access() != tool.Own || tl.Gateway() {
		t.Fatal("a follow-up changes only the agent's own schedules")
	}

	r := call(t, tl, ctx, `{"action":"set","at":"2026-11-03T09:00:00+01:00","check":"Is refund re_1 paid out?"}`)
	if r.IsError || !strings.HasPrefix(r.Text, "Set fu-") {
		t.Fatalf("set: %+v", r)
	}
	id := strings.Fields(r.Text)[1]
	id = strings.TrimSuffix(id, ",")
	s, err := store.Get(ctx, "t1", id)
	if err != nil || !s.FollowUp || s.Version != 3 || !s.At.Equal(time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)) || s.Agent != "refunds" {
		t.Fatalf("stored %+v, %v", s, err)
	}
	if r := call(t, tl, ctx, `{"action":"list"}`); !strings.Contains(r.Text, id) || !strings.Contains(r.Text, "re_1") {
		t.Fatalf("list: %+v", r)
	}

	for in, want := range map[string]string{
		`{"action":"set","at":"2026-11-20T09:00:00Z","check":"x"}`: "within 7 days",
		`{"action":"set","at":"2026-11-01T09:00:00Z","check":"x"}`: "within 7 days",
		`{"action":"set","at":"tomorrow","check":"x"}`:             "RFC 3339",
		`{"action":"set","at":"2026-11-03T09:00:00Z"}`:             "RFC 3339",
	} {
		if r := call(t, tl, ctx, in); !r.IsError || !strings.Contains(r.Text, want) {
			t.Errorf("%s: %+v", in, r)
		}
	}

	// A run a follow-up started can't set another.
	_ = runs.Create(ctx, run.Run{ID: "r9", Tenant: "t1", Agent: "refunds", Session: "s", Trigger: run.BySchedule, Source: id, Status: run.Running})
	inRun := run.WithRef(ctx, run.Ref{Session: "s", Run: "r9"})
	if r := call(t, tl, inRun, `{"action":"set","at":"2026-11-04T09:00:00Z","check":"again"}`); !r.IsError || !strings.Contains(r.Text, "can't set another") {
		t.Fatalf("chained: %+v", r)
	}

	// Another agent's schedule isn't one to remove.
	other := followup.New(sv, runs, "t1", "payouts", 1)
	if r := call(t, other, ctx, `{"action":"remove","id":"`+id+`"}`); !r.IsError {
		t.Fatalf("another agent removed it: %+v", r)
	}
	if r := call(t, tl, ctx, `{"action":"remove","id":"`+id+`"}`); r.IsError {
		t.Fatalf("remove: %+v", r)
	}
	if r := call(t, tl, ctx, `{"action":"list"}`); r.Text != "No follow-ups waiting." {
		t.Fatalf("after remove: %+v", r)
	}
}
