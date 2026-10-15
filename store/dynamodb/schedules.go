package dynamodb

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/kiff/agents/schedule"
)

// Schedules is a schedule.Schedules on the store's table. A schedule is one
// item, with no TTL: it lives until it, or its agent, is removed.
//
//	schedule  pk=C|tenant  sk=id  the Schedule, as JSON in "sched"
type Schedules struct {
	db    API
	table string
}

// NewSchedules returns the schedule store on table.
func NewSchedules(db API, table string) *Schedules { return &Schedules{db: db, table: table} }

var _ schedule.Schedules = (*Schedules)(nil)

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
