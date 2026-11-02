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

// Waitings is a schedule.Waitings on the store's table: each agent's one
// waiting firing, all in one partition the sweep reads. There are few: one
// per busy agent, at most.
//
//	waiting  pk=SW  sk=tenant|agent  the Waiting, as JSON in "waiting"
type Waitings struct {
	db    API
	table string
}

// NewWaitings returns the store of waiting firings on table.
func NewWaitings(db API, table string) *Waitings { return &Waitings{db: db, table: table} }

var _ schedule.Waitings = (*Waitings)(nil)

const waitingsPK = "SW"

func waitingSK(tenant, agent string) string { return key("W", tenant, agent) }

// Swap puts w in its agent's place and returns what was there.
func (st *Waitings) Swap(ctx context.Context, w schedule.Waiting) (schedule.Waiting, bool, error) {
	b, err := json.Marshal(w)
	if err != nil {
		return schedule.Waiting{}, false, err
	}
	item := pkey(waitingsPK, waitingSK(w.Tenant, w.Agent))
	item["waiting"] = s(string(b))
	out, err := st.db.PutItem(ctx, &ddb.PutItemInput{TableName: &st.table, Item: item, ReturnValues: types.ReturnValueAllOld})
	if err != nil {
		return schedule.Waiting{}, false, err
	}
	return decodeWaiting(out.Attributes)
}

// Take deletes the agent's waiting firing and returns it.
func (st *Waitings) Take(ctx context.Context, tenant, agent string) (schedule.Waiting, bool, error) {
	out, err := st.db.DeleteItem(ctx, &ddb.DeleteItemInput{TableName: &st.table, Key: pkey(waitingsPK, waitingSK(tenant, agent)),
		ReturnValues: types.ReturnValueAllOld})
	if err != nil {
		return schedule.Waiting{}, false, err
	}
	return decodeWaiting(out.Attributes)
}

// List reads up to limit waiting firings, across tenants.
func (st *Waitings) List(ctx context.Context, limit int) ([]schedule.Waiting, error) {
	out, err := st.db.Query(ctx, &ddb.QueryInput{
		TableName:                 &st.table,
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(waitingsPK)},
		Limit:                     aws.Int32(int32(limit)),
	})
	if err != nil {
		return nil, err
	}
	var all []schedule.Waiting
	for _, item := range out.Items {
		w, ok, err := decodeWaiting(item)
		if err != nil {
			return nil, err
		}
		if ok {
			all = append(all, w)
		}
	}
	return all, nil
}

func decodeWaiting(item map[string]types.AttributeValue) (schedule.Waiting, bool, error) {
	v, _ := item["waiting"].(*types.AttributeValueMemberS)
	if v == nil {
		return schedule.Waiting{}, false, nil
	}
	var w schedule.Waiting
	if err := json.Unmarshal([]byte(v.Value), &w); err != nil {
		return schedule.Waiting{}, false, fmt.Errorf("dynamodb: a waiting firing: %w", err)
	}
	return w, true, nil
}
