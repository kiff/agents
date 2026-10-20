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

	"github.com/kiff/agents/registry"
)

// Agents is a registry.Agents on the store's table. A definition is one item,
// with no TTL: it lives until the agent is removed. Its versions
// (kiff/tafs#107) are kept beside it, one item each, numbered from 1; the
// live one is a copy in the agent's item, so Get and List read no versions.
//
//	agent    pk=G|tenant     sk=id        the live Definition, as JSON in "def"
//	version  pk=V|tenant|id  sk=%010d n   its Definition in "def", its record in "ver"
type Agents struct {
	db    API
	table string
	// now stamps a version's creation.
	now func() time.Time
}

// NewAgents returns the registry on table.
func NewAgents(db API, table string) *Agents {
	return &Agents{db: db, table: table, now: func() time.Time { return time.Now().UTC() }}
}

var _ registry.Agents = (*Agents)(nil)

func agentsPK(tenant string) string { return key("G", tenant) }

func versionsPK(tenant, id string) string { return key("V", tenant, id) }

func versionSK(n int) string { return fmt.Sprintf("%010d", n) }

// Put stores d as the live definition. An agent with versions gets it as its
// next version, live at once (an upgrade is part of its history); one
// without stays unversioned.
func (a *Agents) Put(ctx context.Context, d registry.Definition) error {
	d.Version = 0
	last, err := a.lastVersion(ctx, d.Tenant, d.ID)
	if err != nil {
		return err
	}
	if last > 0 {
		d.Version = last + 1
		if err := a.putVersion(ctx, d, registry.Version{N: d.Version, Note: "put"}); err != nil {
			return err
		}
	}
	return a.putLive(ctx, d)
}

func (a *Agents) putLive(ctx context.Context, d registry.Definition) error {
	b, err := registry.Encode(d)
	if err != nil {
		return err
	}
	item := pkey(agentsPK(d.Tenant), d.ID)
	item["def"] = s(string(b))
	_, err = a.db.PutItem(ctx, &ddb.PutItemInput{TableName: &a.table, Item: item})
	return err
}

// putVersion stores d as version v.N, once: a second writer of the same
// number gets ErrVersionTaken, never a silent overwrite.
func (a *Agents) putVersion(ctx context.Context, d registry.Definition, v registry.Version) error {
	dg, err := registry.Digest(d)
	if err != nil {
		return err
	}
	d.Version = v.N
	v.Digest, v.Created, v.Live = dg, a.now(), false
	def, err := registry.Encode(d)
	if err != nil {
		return err
	}
	ver, err := json.Marshal(v)
	if err != nil {
		return err
	}
	item := pkey(versionsPK(d.Tenant, d.ID), versionSK(v.N))
	item["def"], item["ver"] = s(string(def)), s(string(ver))
	_, err = a.db.PutItem(ctx, &ddb.PutItemInput{TableName: &a.table, Item: item,
		ConditionExpression: aws.String("attribute_not_exists(pk)")})
	var taken *types.ConditionalCheckFailedException
	if errors.As(err, &taken) {
		return ErrVersionTaken
	}
	return err
}

// ErrVersionTaken is a version number another writer stored first: read the
// versions again and retry.
var ErrVersionTaken = errors.New("dynamodb: that version was just stored by another writer")

// lastVersion is the agent's highest version number; 0 when it has none.
func (a *Agents) lastVersion(ctx context.Context, tenant, id string) (int, error) {
	out, err := a.db.Query(ctx, &ddb.QueryInput{
		TableName:                 &a.table,
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(versionsPK(tenant, id))},
		ScanIndexForward:          aws.Bool(false),
		Limit:                     aws.Int32(1),
		ConsistentRead:            aws.Bool(true),
	})
	if err != nil || len(out.Items) == 0 {
		return 0, err
	}
	n, err := strconv.Atoi(getS(out.Items[0], "sk"))
	if err != nil {
		return 0, fmt.Errorf("dynamodb: version key %q: %w", getS(out.Items[0], "sk"), err)
	}
	return n, nil
}

func (a *Agents) PutVersion(ctx context.Context, d registry.Definition, v registry.Version) (int, error) {
	live, err := a.Get(ctx, d.Tenant, d.ID)
	if err != nil {
		return 0, err
	}
	last, err := a.lastVersion(ctx, d.Tenant, d.ID)
	if err != nil {
		return 0, err
	}
	if last == 0 { // stored before versions: what's live is version 1
		if err := a.putVersion(ctx, live, registry.Version{N: 1, Note: "as it was before versions"}); err != nil && !errors.Is(err, ErrVersionTaken) {
			return 0, err
		}
		live.Version = 1
		if err := a.putLive(ctx, live); err != nil {
			return 0, err
		}
		last = 1
	}
	v.N = last + 1
	if err := a.putVersion(ctx, d, v); err != nil {
		return 0, err
	}
	return v.N, nil
}

func (a *Agents) Versions(ctx context.Context, tenant, id string) ([]registry.Version, error) {
	live, err := a.Get(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	var out []registry.Version
	in := &ddb.QueryInput{
		TableName:                 &a.table,
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(versionsPK(tenant, id))},
		ProjectionExpression:      aws.String("pk, sk, ver"),
		ConsistentRead:            aws.Bool(true),
	}
	for {
		page, err := a.db.Query(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			var v registry.Version
			if err := json.Unmarshal([]byte(getS(item, "ver")), &v); err != nil {
				return nil, fmt.Errorf("dynamodb: version %q: %w", getS(item, "sk"), err)
			}
			v.Live = v.N == live.Version
			out = append(out, v)
		}
		if page.LastEvaluatedKey == nil {
			return out, nil
		}
		in.ExclusiveStartKey = page.LastEvaluatedKey
	}
}

func (a *Agents) GetVersion(ctx context.Context, tenant, id string, n int) (registry.Definition, error) {
	if n < 1 {
		return registry.Definition{}, registry.ErrNoVersion
	}
	out, err := a.db.GetItem(ctx, &ddb.GetItemInput{TableName: &a.table, Key: pkey(versionsPK(tenant, id), versionSK(n)), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return registry.Definition{}, err
	}
	if out.Item == nil {
		return registry.Definition{}, registry.ErrNoVersion
	}
	return decodeAgent(out.Item)
}

func (a *Agents) Switch(ctx context.Context, tenant, id string, n int) error {
	if _, err := a.Get(ctx, tenant, id); err != nil {
		return err
	}
	d, err := a.GetVersion(ctx, tenant, id, n)
	if err != nil {
		return err
	}
	return a.putLive(ctx, d)
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

// Delete removes the agent and its versions, versions first, so a delete
// that fails half way leaves the agent to be deleted again.
func (a *Agents) Delete(ctx context.Context, tenant, id string) error {
	in := &ddb.QueryInput{
		TableName:                 &a.table,
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(versionsPK(tenant, id))},
		ProjectionExpression:      aws.String("pk, sk"),
	}
	var keys []map[string]types.AttributeValue
	for {
		page, err := a.db.Query(ctx, in)
		if err != nil {
			return err
		}
		keys = append(keys, keysOf(page.Items)...)
		if page.LastEvaluatedKey == nil {
			break
		}
		in.ExclusiveStartKey = page.LastEvaluatedKey
	}
	if len(keys) > 0 {
		if err := (&Store{db: a.db, table: a.table}).deleteKeys(ctx, keys); err != nil {
			return err
		}
	}
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
