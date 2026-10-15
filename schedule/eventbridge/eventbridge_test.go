package eventbridge_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	sch "github.com/aws/aws-sdk-go-v2/service/scheduler"
	"github.com/aws/aws-sdk-go-v2/service/scheduler/types"

	"github.com/kiff/agents/schedule"
	"github.com/kiff/agents/schedule/eventbridge"
	"github.com/kiff/agents/schedule/scheduletest"
)

var ctx = context.Background()

// fake is EventBridge Scheduler as far as the adapter sees it: schedules by
// group and name, and its errors for one that is missing or already there.
type fake struct {
	schedules map[string]*sch.CreateScheduleInput // by group/name
	calls     []string
	// race makes the next CreateSchedule find the schedule created meanwhile.
	race bool
	fail error
}

func newFake() *fake { return &fake{schedules: map[string]*sch.CreateScheduleInput{}} }

func (f *fake) CreateSchedule(_ context.Context, in *sch.CreateScheduleInput, _ ...func(*sch.Options)) (*sch.CreateScheduleOutput, error) {
	f.calls = append(f.calls, "create")
	k := *in.GroupName + "/" + *in.Name
	if f.race {
		f.race = false
		f.schedules[k] = &sch.CreateScheduleInput{}
	}
	if _, ok := f.schedules[k]; ok {
		return nil, &types.ConflictException{Message: new(string)}
	}
	f.schedules[k] = in
	return &sch.CreateScheduleOutput{}, nil
}

func (f *fake) UpdateSchedule(_ context.Context, in *sch.UpdateScheduleInput, _ ...func(*sch.Options)) (*sch.UpdateScheduleOutput, error) {
	f.calls = append(f.calls, "update")
	if f.fail != nil {
		return nil, f.fail
	}
	k := *in.GroupName + "/" + *in.Name
	if _, ok := f.schedules[k]; !ok {
		return nil, &types.ResourceNotFoundException{Message: new(string)}
	}
	f.schedules[k] = &sch.CreateScheduleInput{Name: in.Name, GroupName: in.GroupName, ScheduleExpression: in.ScheduleExpression,
		ScheduleExpressionTimezone: in.ScheduleExpressionTimezone, State: in.State, FlexibleTimeWindow: in.FlexibleTimeWindow, Target: in.Target}
	return &sch.UpdateScheduleOutput{}, nil
}

func (f *fake) DeleteSchedule(_ context.Context, in *sch.DeleteScheduleInput, _ ...func(*sch.Options)) (*sch.DeleteScheduleOutput, error) {
	f.calls = append(f.calls, "delete")
	if f.fail != nil {
		return nil, f.fail
	}
	k := *in.GroupName + "/" + *in.Name
	if _, ok := f.schedules[k]; !ok {
		return nil, &types.ResourceNotFoundException{Message: new(string)}
	}
	delete(f.schedules, k)
	return &sch.DeleteScheduleOutput{}, nil
}

const (
	group  = "agents"
	target = "the-function"
	role   = "the-role"
)

func (f *fake) get(t *testing.T, tenant, id string) *sch.CreateScheduleInput {
	t.Helper()
	in := f.schedules[group+"/"+eventbridge.Name(tenant, id)]
	if in == nil {
		t.Fatalf("no EventBridge schedule for %s/%s", tenant, id)
	}
	return in
}

func TestSetCreatesTheSchedule(t *testing.T) {
	f := newFake()
	s := scheduletest.Schedule("t1", "morning") // 09:00 on weekdays, Paris
	if err := eventbridge.New(f, group, target, role).Set(ctx, s); err != nil {
		t.Fatal(err)
	}
	in := f.get(t, "t1", "morning")
	if *in.ScheduleExpression != "cron(0 9 ? * 2,3,4,5,6 *)" || *in.ScheduleExpressionTimezone != "Europe/Paris" ||
		in.State != types.ScheduleStateEnabled || in.FlexibleTimeWindow.Mode != types.FlexibleTimeWindowModeOff {
		t.Fatalf("created %s in %s, %s, window %s", *in.ScheduleExpression, *in.ScheduleExpressionTimezone, in.State, in.FlexibleTimeWindow.Mode)
	}
	if *in.Target.Arn != target || *in.Target.RoleArn != role {
		t.Fatalf("target %s with role %s", *in.Target.Arn, *in.Target.RoleArn)
	}
	if r := in.Target.RetryPolicy; r == nil || *r.MaximumRetryAttempts != 3 || *r.MaximumEventAgeInSeconds != 3600 {
		t.Fatalf("retry policy %+v", r)
	}
	// The input names the schedule, not its contents, and leaves the due time
	// to EventBridge, unescaped so EventBridge finds it.
	want := `{"tenant":"t1","schedule":"morning","due":"<aws.scheduler.scheduled-time>"}`
	if *in.Target.Input != want {
		t.Fatalf("input %s, want %s", *in.Target.Input, want)
	}
	if strings.Contains(*in.Target.Input, s.Input) {
		t.Fatal("the schedule's input reached EventBridge")
	}
}

func TestSetReplacesAndSwitchesOff(t *testing.T) {
	f := newFake()
	es := eventbridge.New(f, group, target, role)
	s := scheduletest.Schedule("t1", "morning")
	_ = es.Set(ctx, s)
	s.Cron, s.TimeZone, s.Enabled = "*/15 * * * *", "", false
	if err := es.Set(ctx, s); err != nil {
		t.Fatal(err)
	}
	in := f.get(t, "t1", "morning")
	if *in.ScheduleExpression != "cron(0,15,30,45 * * * ? *)" || *in.ScheduleExpressionTimezone != "UTC" || in.State != types.ScheduleStateDisabled {
		t.Fatalf("replaced with %s in %s, %s", *in.ScheduleExpression, *in.ScheduleExpressionTimezone, in.State)
	}
	if len(f.schedules) != 1 {
		t.Fatalf("%d schedules, want the one replaced", len(f.schedules))
	}
}

// Two Sets of a new schedule at once: the one whose create comes second
// replaces what the first created.
func TestSetThatRacesACreateReplaces(t *testing.T) {
	f := newFake()
	f.race = true
	s := scheduletest.Schedule("t1", "morning")
	if err := eventbridge.New(f, group, target, role).Set(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls, ","); got != "update,create,update" {
		t.Fatalf("calls %s, want update,create,update", got)
	}
	if in := f.get(t, "t1", "morning"); in.ScheduleExpression == nil || *in.ScheduleExpression != "cron(0 9 ? * 2,3,4,5,6 *)" {
		t.Fatal("the schedule created meanwhile wasn't replaced")
	}
}

func TestSetReportsOtherErrors(t *testing.T) {
	f := newFake()
	f.fail = errors.New("throttled")
	if err := eventbridge.New(f, group, target, role).Set(ctx, scheduletest.Schedule("t1", "morning")); err == nil {
		t.Fatal("a failed update reported success")
	}
	if len(f.schedules) != 0 || strings.Contains(strings.Join(f.calls, ","), "create") {
		t.Fatal("created a schedule after an update failed for another reason")
	}
}

func TestRemoveIsSafeToRepeat(t *testing.T) {
	f := newFake()
	es := eventbridge.New(f, group, target, role)
	_ = es.Set(ctx, scheduletest.Schedule("t1", "morning"))
	_ = es.Set(ctx, scheduletest.Schedule("t2", "morning"))
	for i := range 2 {
		if err := es.Remove(ctx, "t1", "morning"); err != nil {
			t.Fatalf("remove %d: %v", i+1, err)
		}
	}
	f.get(t, "t2", "morning") // another tenant's stays
	if len(f.schedules) != 1 {
		t.Fatalf("%d schedules left, want t2's", len(f.schedules))
	}
	f.fail = errors.New("throttled")
	if err := es.Remove(ctx, "t2", "morning"); err == nil {
		t.Fatal("a failed delete reported success")
	}
}

func TestNameFitsEventBridgeAndSaysNothing(t *testing.T) {
	valid := regexp.MustCompile(`^[0-9a-zA-Z_.-]{1,64}$`)
	long := strings.Repeat("t", 256)
	names := map[string]bool{}
	for _, p := range [][2]string{{"t1", "morning"}, {"t2", "morning"}, {"t1", "evening"}, {long, strings.Repeat("i", 128)}, {"t1|5:m", "orning"}} {
		n := eventbridge.Name(p[0], p[1])
		if !valid.MatchString(n) {
			t.Fatalf("%q is not an EventBridge schedule name", n)
		}
		if strings.Contains(n, "morning") || strings.Contains(n, "t1") {
			t.Fatalf("%q shows the tenant or the schedule", n)
		}
		if names[n] {
			t.Fatalf("two schedules share the name %q", n)
		}
		names[n] = true
	}
	if eventbridge.Name("t1", "morning") != eventbridge.Name("t1", "morning") {
		t.Fatal("a name isn't stable")
	}
}

// The event a firing invokes the target with reads back as the schedule and
// its due time, ready for Service.Fire.
func TestParseEvent(t *testing.T) {
	f := newFake()
	_ = eventbridge.New(f, group, target, role).Set(ctx, scheduletest.Schedule("t<1>", "morning"))
	in := *f.get(t, "t<1>", "morning").Target.Input
	// EventBridge replaces the attribute with the due time.
	fired := strings.Replace(in, "<aws.scheduler.scheduled-time>", "2026-10-19T07:00:00Z", 1)
	tenant, id, due, err := eventbridge.ParseEvent([]byte(fired))
	if err != nil || tenant != "t<1>" || id != "morning" || !due.Equal(time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %q %q %v (err %v)", tenant, id, due, err)
	}
	for _, bad := range []string{`{`, `{"tenant":"t1","schedule":"m"}`, `{"tenant":"t1","due":"2026-10-19T07:00:00Z"}`,
		`{"schedule":"m","due":"2026-10-19T07:00:00Z"}`, string(must(json.Marshal(eventbridge.Event{Tenant: "t1", Schedule: "m", Due: "<aws.scheduler.scheduled-time>"})))} {
		if _, _, _, err := eventbridge.ParseEvent([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

var _ schedule.Scheduler = (*eventbridge.Scheduler)(nil)
