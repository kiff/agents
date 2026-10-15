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
type Agents struct{ st *Store }

// NewAgents returns the registry on table.
func NewAgents(db API, table string) *Agents { return &Agents{st: New(db, table)} }

var _ registry.Agents = (*Agents)(nil)

func agentsPK(tenant string) string { return key("G", tenant) }

func (a *Agents) Put(ctx context.Context, d registry.Definition) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	item := pkey(agentsPK(d.Tenant), d.ID)
	item["def"] = s(string(b))
	_, err = a.st.db.PutItem(ctx, &ddb.PutItemInput{TableName: &a.st.table, Item: item})
	return err
}

func (a *Agents) Get(ctx context.Context, tenant, id string) (registry.Definition, error) {
	out, err := a.st.db.GetItem(ctx, &ddb.GetItemInput{TableName: &a.st.table, Key: pkey(agentsPK(tenant), id), ConsistentRead: aws.Bool(true)})
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
	items, err := a.st.partition(ctx, agentsPK(tenant), nil)
	if err != nil {
		return nil, err
	}
	out := make([]registry.Definition, 0, len(items))
	for _, item := range items {
		d, err := decodeAgent(item)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (a *Agents) Delete(ctx context.Context, tenant, id string) error {
	_, err := a.st.db.DeleteItem(ctx, &ddb.DeleteItemInput{TableName: &a.st.table, Key: pkey(agentsPK(tenant), id)})
	return err
}

func decodeAgent(item map[string]types.AttributeValue) (registry.Definition, error) {
	var d registry.Definition
	if err := json.Unmarshal([]byte(getS(item, "def")), &d); err != nil {
		return registry.Definition{}, fmt.Errorf("dynamodb: agent %q: %w", getS(item, "sk"), err)
	}
	return d, nil
}
