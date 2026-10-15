package dynamodb

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/kiff/agents/registry"
)

// Agents is a registry.Agents on the store's table. A definition is one item,
// with no TTL: it lives until the agent is removed.
//
//	agent  pk=G|tenant  sk=id  the Definition, as JSON in "def"
type Agents struct {
	db    API
	table string
}

// NewAgents returns the registry on table.
func NewAgents(db API, table string) *Agents { return &Agents{db: db, table: table} }

var _ registry.Agents = (*Agents)(nil)

func agentsPK(tenant string) string { return key("G", tenant) }

func (a *Agents) Put(ctx context.Context, d registry.Definition) error {
	b, err := registry.Encode(d)
	if err != nil {
		return err
	}
	item := pkey(agentsPK(d.Tenant), d.ID)
	item["def"] = s(string(b))
	_, err = a.db.PutItem(ctx, &ddb.PutItemInput{TableName: &a.table, Item: item})
	return err
}

func (a *Agents) Get(ctx context.Context, tenant, id string) (registry.Definition, error) {
	out, err := a.db.GetItem(ctx, &ddb.GetItemInput{TableName: &a.table, Key: pkey(agentsPK(tenant), id), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return registry.Definition{}, err
	}
	if out.Item == nil {
		return registry.Definition{}, registry.ErrNotFound
	}
	return decodeAgent(out.Item)
}

// List reads the tenant's partition, in ID order.
func (a *Agents) List(ctx context.Context, tenant string) ([]registry.Definition, error) {
	var out []registry.Definition
	in := &ddb.QueryInput{
		TableName:                 &a.table,
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(agentsPK(tenant))},
		ConsistentRead:            aws.Bool(true),
	}
	for {
		page, err := a.db.Query(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			d, err := decodeAgent(item)
			if err != nil {
				return nil, err
			}
			out = append(out, d)
		}
		if page.LastEvaluatedKey == nil {
			return out, nil
		}
		in.ExclusiveStartKey = page.LastEvaluatedKey
	}
}

func (a *Agents) Delete(ctx context.Context, tenant, id string) error {
	_, err := a.db.DeleteItem(ctx, &ddb.DeleteItemInput{TableName: &a.table, Key: pkey(agentsPK(tenant), id)})
	return err
}

func decodeAgent(item map[string]types.AttributeValue) (registry.Definition, error) {
	var d registry.Definition
	if err := json.Unmarshal([]byte(getS(item, "def")), &d); err != nil {
		return registry.Definition{}, fmt.Errorf("dynamodb: agent %q: %w", getS(item, "sk"), err)
	}
	return d, nil
}
