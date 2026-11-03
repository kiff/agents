package dynamodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/kiff/agents/schedule"
)

// Schedules is a schedule.Schedules on the store's table. A schedule is one
// item, with no TTL: it lives until it, or its agent, is removed.
//
//	schedule  pk=C|tenant         sk=id         the Schedule, as JSON in "sched"
//	firing    pk=SF|tenant|id     sk=due (ns)   a Fired, as JSON in "fired", kept FiredKept
//	guard     pk=CF|tenant        sk=agent      "v", bumped with each follow-up the agent sets
type Schedules struct {
	db    API
	table string
}

// NewSchedules returns the schedule store on table.
func NewSchedules(db API, table string) *Schedules { return &Schedules{db: db, table: table} }

var (
	_ schedule.Schedules = (*Schedules)(nil)
	_ schedule.History   = (*Schedules)(nil)
)

// FiredKept is how long a schedule's firing is kept in its history.
const FiredKept = 90 * 24 * time.Hour

func firedPK(tenant, id string) string { return key("SF", tenant, id) }

// Add keeps f, by its due time: the same firing kept again replaces it.
func (st *Schedules) Add(ctx context.Context, tenant, id string, f schedule.Fired) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	item := pkey(firedPK(tenant, id), fmt.Sprintf("%020d", f.Due.UnixNano()))
	item["fired"] = &types.AttributeValueMemberS{Value: string(b)}
	item["ttl"] = ttl(f.At.Add(FiredKept))
	_, err = st.db.PutItem(ctx, &ddb.PutItemInput{TableName: &st.table, Item: item})
	return err
}

// Firings reads the schedule's history, newest first.
func (st *Schedules) Firings(ctx context.Context, tenant, id string, limit int) ([]schedule.Fired, error) {
	if limit <= 0 || limit > schedule.MaxHistory {
		limit = schedule.MaxHistory
	}
	out, err := st.db.Query(ctx, &ddb.QueryInput{
		TableName:                 &st.table,
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": &types.AttributeValueMemberS{Value: firedPK(tenant, id)}},
		ScanIndexForward:          aws.Bool(false),
		Limit:                     aws.Int32(int32(limit)),
	})
	if err != nil {
		return nil, err
	}
	fired := make([]schedule.Fired, 0, len(out.Items))
	for _, item := range out.Items {
		v, _ := item["fired"].(*types.AttributeValueMemberS)
		if v == nil {
			continue
		}
		var f schedule.Fired
		if err := json.Unmarshal([]byte(v.Value), &f); err != nil {
			return nil, fmt.Errorf("dynamodb: a firing: %w", err)
		}
		fired = append(fired, f)
	}
	return fired, nil
}

func schedulesPK(tenant string) string { return key("C", tenant) }

func (st *Schedules) Put(ctx context.Context, s schedule.Schedule) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	item := pkey(schedulesPK(s.Tenant), s.ID)
	item["sched"] = &types.AttributeValueMemberS{Value: string(b)}
	_, err = st.db.PutItem(ctx, &ddb.PutItemInput{TableName: &st.table, Item: item})
	return err
}

func (st *Schedules) Get(ctx context.Context, tenant, id string) (schedule.Schedule, error) {
	out, err := st.db.GetItem(ctx, &ddb.GetItemInput{TableName: &st.table, Key: pkey(schedulesPK(tenant), id), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return schedule.Schedule{}, err
	}
	if out.Item == nil {
		return schedule.Schedule{}, schedule.ErrNotFound
	}
	return decodeSchedule(out.Item)
}

// List reads the tenant's partition, in ID order.
func (st *Schedules) List(ctx context.Context, tenant string) ([]schedule.Schedule, error) {
	var out []schedule.Schedule
	in := &ddb.QueryInput{
		TableName:                 &st.table,
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": &types.AttributeValueMemberS{Value: schedulesPK(tenant)}},
		ConsistentRead:            aws.Bool(true),
	}
	for {
		page, err := st.db.Query(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			s, err := decodeSchedule(item)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		if page.LastEvaluatedKey == nil {
			return out, nil
		}
		in.ExclusiveStartKey = page.LastEvaluatedKey
	}
}

// followUpTries bounds how often PutFollowUp reads again when another
// follow-up of the agent was set between its read and its write.
const followUpTries = 5

// PutFollowUp counts the agent's open follow-ups and stores s in one step:
// the write bumps the agent's guard from the version read, together with s,
// so a follow-up set concurrently makes it read and count again.
func (st *Schedules) PutFollowUp(ctx context.Context, sc schedule.Schedule, max int) error {
	b, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	guard := pkey(key("CF", sc.Tenant), sc.Agent)
	for range followUpTries {
		got, err := st.db.GetItem(ctx, &ddb.GetItemInput{TableName: &st.table, Key: guard, ConsistentRead: aws.Bool(true)})
		if err != nil {
			return err
		}
		var v int64
		if a, ok := got.Item["v"].(*types.AttributeValueMemberN); ok {
			if v, err = strconv.ParseInt(a.Value, 10, 64); err != nil {
				return fmt.Errorf("dynamodb: a follow-up guard: %w", err)
			}
		}
		all, err := st.List(ctx, sc.Tenant)
		if err != nil {
			return err
		}
		open := 0
		for _, o := range all {
			if schedule.OpenFollowUp(o, sc) {
				open++
			}
		}
		if open >= max {
			return schedule.ErrFollowUpLimit
		}
		item := pkey(schedulesPK(sc.Tenant), sc.ID)
		item["sched"] = &types.AttributeValueMemberS{Value: string(b)}
		cond, vals := "#v = :v", map[string]types.AttributeValue{":v": n(v), ":n": n(v + 1)}
		if v == 0 {
			cond = "attribute_not_exists(#v) OR #v = :v"
		}
		_, err = st.db.TransactWriteItems(ctx, &ddb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
			{Update: &types.Update{TableName: &st.table, Key: guard, UpdateExpression: aws.String("SET #v = :n"),
				ConditionExpression: aws.String(cond), ExpressionAttributeNames: map[string]string{"#v": "v"}, ExpressionAttributeValues: vals}},
			{Put: &types.Put{TableName: &st.table, Item: item}},
		}})
		var cancelled *types.TransactionCanceledException
		if errors.As(err, &cancelled) && len(cancelled.CancellationReasons) > 0 && aws.ToString(cancelled.CancellationReasons[0].Code) == "ConditionalCheckFailed" {
			continue
		}
		return err
	}
	return fmt.Errorf("dynamodb: the agent's follow-ups changed %d times while one was set; try again", followUpTries)
}

func (st *Schedules) Delete(ctx context.Context, tenant, id string) error {
	_, err := st.db.DeleteItem(ctx, &ddb.DeleteItemInput{TableName: &st.table, Key: pkey(schedulesPK(tenant), id)})
	return err
}

func decodeSchedule(item map[string]types.AttributeValue) (schedule.Schedule, error) {
	var s schedule.Schedule
	v, _ := item["sched"].(*types.AttributeValueMemberS)
	if v == nil {
		return schedule.Schedule{}, fmt.Errorf("dynamodb: a schedule item has no body")
	}
	if err := json.Unmarshal([]byte(v.Value), &s); err != nil {
		return schedule.Schedule{}, fmt.Errorf("dynamodb: schedule: %w", err)
	}
	return s, nil
}
