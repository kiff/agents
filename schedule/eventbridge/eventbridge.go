// Package eventbridge is the schedule.Scheduler on EventBridge Scheduler: one
// EventBridge schedule per schedule, in one group, that invokes the server's
// function with the schedule's tenant and ID and the time the firing was due.
//
// The function and the role EventBridge invokes it with are configuration,
// never constants here. Nothing about a schedule but its cron, time zone and
// state reaches EventBridge: its name is a hash, and its input is read from
// the store when it fires.
package eventbridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sch "github.com/aws/aws-sdk-go-v2/service/scheduler"
	"github.com/aws/aws-sdk-go-v2/service/scheduler/types"

	"github.com/kiff/agents/schedule"
)

// API is the part of the EventBridge Scheduler client the adapter uses.
type API interface {
	CreateSchedule(context.Context, *sch.CreateScheduleInput, ...func(*sch.Options)) (*sch.CreateScheduleOutput, error)
	UpdateSchedule(context.Context, *sch.UpdateScheduleInput, ...func(*sch.Options)) (*sch.UpdateScheduleOutput, error)
	DeleteSchedule(context.Context, *sch.DeleteScheduleInput, ...func(*sch.Options)) (*sch.DeleteScheduleOutput, error)
}

// Retries: EventBridge tries again, for up to an hour, to deliver a firing it
// couldn't hand to the target. That covers delivery only: an error the
// target's Service.Fire returns is retried, or not, by the target's own
// asynchronous invocation settings, set with the function. Service.Fire is
// safe to repeat (Firing.Key), so no retry starts a second run.
const (
	maxAttempts = 3
	maxAge      = time.Hour
)

// Scheduler is a schedule.Scheduler on EventBridge Scheduler.
type Scheduler struct {
	api API
	// Group is the schedule group every schedule goes in.
	Group string
	// Target is the function each firing invokes, and Role what EventBridge
	// assumes to invoke it.
	Target, Role string
}

// New returns a Scheduler that puts schedules in group, invoking target with
// role.
func New(api API, group, target, role string) *Scheduler {
	return &Scheduler{api: api, Group: group, Target: target, Role: role}
}

var _ schedule.Scheduler = (*Scheduler)(nil)

// Name is the EventBridge schedule's name for the tenant's schedule: a hash,
// so it fits EventBridge's 64 characters and says nothing about either.
func Name(tenant, id string) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%d:%s|%d:%s", len(tenant), tenant, len(id), id))
	return "s-" + hex.EncodeToString(h[:20])
}

// Event is what a firing invokes the target with.
type Event struct {
	Tenant   string `json:"tenant"`
	Schedule string `json:"schedule"`
	// Due is when the firing was due, filled in by EventBridge.
	Due string `json:"due"`
}

// dueAttribute is the context attribute EventBridge replaces with when a
// firing was due.
const dueAttribute = "<aws.scheduler.scheduled-time>"

// input is the target's input for a schedule: written without escaping, so
// EventBridge finds its context attribute.
func input(tenant, id string) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(Event{Tenant: tenant, Schedule: id, Due: dueAttribute}); err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(buf.Bytes())), nil
}

// ParseEvent reads a firing's event: the tenant, the schedule and when it was
// due, for schedule.Service.Fire.
func ParseEvent(b []byte) (tenant, id string, due time.Time, err error) {
	var e Event
	if err := json.Unmarshal(b, &e); err != nil {
		return "", "", time.Time{}, fmt.Errorf("eventbridge: a firing's event: %w", err)
	}
	due, err = time.Parse(time.RFC3339, e.Due)
	if err != nil || e.Tenant == "" || e.Schedule == "" {
		return "", "", time.Time{}, errors.New("eventbridge: a firing's event needs a tenant, a schedule and when it was due")
	}
	return e.Tenant, e.Schedule, due, nil
}

// Set creates or replaces the EventBridge schedule for s, enabled or
// disabled as s is.
func (sc *Scheduler) Set(ctx context.Context, s schedule.Schedule) error {
	c, err := schedule.ParseCron(s.Cron)
	if err != nil {
		return err
	}
	in, err := input(s.Tenant, s.ID)
	if err != nil {
		return err
	}
	zone := s.TimeZone
	if zone == "" {
		zone = "UTC"
	}
	state := types.ScheduleStateDisabled
	if s.Enabled {
		state = types.ScheduleStateEnabled
	}
	name := Name(s.Tenant, s.ID)
	target := &types.Target{Arn: &sc.Target, RoleArn: &sc.Role, Input: &in,
		RetryPolicy: &types.RetryPolicy{MaximumRetryAttempts: aws.Int32(maxAttempts), MaximumEventAgeInSeconds: aws.Int32(int32(maxAge.Seconds()))}}
	window := &types.FlexibleTimeWindow{Mode: types.FlexibleTimeWindowModeOff}
	expr := c.EventBridge()

	update := func() error {
		_, err := sc.api.UpdateSchedule(ctx, &sch.UpdateScheduleInput{Name: &name, GroupName: &sc.Group, ScheduleExpression: &expr,
			ScheduleExpressionTimezone: &zone, State: state, FlexibleTimeWindow: window, Target: target})
		return err
	}
	err = update()
	var missing *types.ResourceNotFoundException
	if !errors.As(err, &missing) {
		return err
	}
	_, err = sc.api.CreateSchedule(ctx, &sch.CreateScheduleInput{Name: &name, GroupName: &sc.Group, ScheduleExpression: &expr,
		ScheduleExpressionTimezone: &zone, State: state, FlexibleTimeWindow: window, Target: target})
	var conflict *types.ConflictException
	if errors.As(err, &conflict) {
		return update() // created meanwhile by another Set: replace it
	}
	return err
}

// Remove deletes the EventBridge schedule. One that isn't there is not an
// error.
func (sc *Scheduler) Remove(ctx context.Context, tenant, id string) error {
	_, err := sc.api.DeleteSchedule(ctx, &sch.DeleteScheduleInput{Name: aws.String(Name(tenant, id)), GroupName: &sc.Group})
	var missing *types.ResourceNotFoundException
	if errors.As(err, &missing) {
		return nil
	}
	return err
}
