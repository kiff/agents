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
//	switch   pk=W|tenant     sk=id        a switch under way: its version in "n", its mark in "sw"
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

func switchPK(tenant string) string { return key("W", tenant) }

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
		if err := a.putVersion(ctx, d, registry.Version{N: d.Version, Note: registry.NotePut}); err != nil {
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
		if err := a.putVersion(ctx, live, registry.Version{N: 1, Note: registry.NoteFirst}); err != nil && !errors.Is(err, ErrVersionTaken) {
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
	if err := a.EndSwitch(ctx, tenant, id); err != nil {
		return err
	}
	_, err := a.db.DeleteItem(ctx, &ddb.DeleteItemInput{TableName: &a.table, Key: pkey(agentsPK(tenant), id)})
	return err
}

// BeginSwitch writes the mark only when none is stored, in one conditional
// write, so two switches can't both begin; one for the same version is kept.
func (a *Agents) BeginSwitch(ctx context.Context, tenant, id string, sw registry.Switching) (registry.Switching, error) {
	if _, err := a.Get(ctx, tenant, id); err != nil {
		return registry.Switching{}, err
	}
	sw.At = sw.At.UTC()
	b, err := json.Marshal(sw)
	if err != nil {
		return registry.Switching{}, err
	}
	item := pkey(switchPK(tenant), id)
	item["n"], item["sw"] = n(int64(sw.N)), s(string(b))
	_, err = a.db.PutItem(ctx, &ddb.PutItemInput{TableName: &a.table, Item: item,
		ConditionExpression: aws.String("attribute_not_exists(pk)")})
	var failed *types.ConditionalCheckFailedException
	if !errors.As(err, &failed) {
		return sw, err
	}
	was, ok, err := a.Switching(ctx, tenant, id)
	switch {
	case err != nil:
		return registry.Switching{}, err
	case !ok: // ended meanwhile: begin again
		return a.BeginSwitch(ctx, tenant, id, sw)
	case was.N != sw.N:
		return was, registry.ErrSwitching
	}
	return was, nil
}

// SwitchDraft rewrites the mark with the new draft only over the mark it
// read, in one conditional write; a mark that changed meanwhile is read again.
func (a *Agents) SwitchDraft(ctx context.Context, tenant, id string, n int, draft string) (registry.Switching, error) {
	for try := 0; try < 3; try++ {
		out, err := a.db.GetItem(ctx, &ddb.GetItemInput{TableName: &a.table, Key: pkey(switchPK(tenant), id), ConsistentRead: aws.Bool(true)})
		if err != nil {
			return registry.Switching{}, err
		}
		if out.Item == nil {
			return registry.Switching{}, registry.ErrNoSwitch
		}
		was := getS(out.Item, "sw")
		var sw registry.Switching
		if err := json.Unmarshal([]byte(was), &sw); err != nil {
			return registry.Switching{}, fmt.Errorf("dynamodb: switch of %q: %w", id, err)
		}
		if sw.N != n {
			return registry.Switching{}, registry.ErrNoSwitch
		}
		sw.Draft = draft
		b, err := json.Marshal(sw)
		if err != nil {
			return registry.Switching{}, err
		}
		item := pkey(switchPK(tenant), id)
		item["n"], item["sw"] = out.Item["n"], s(string(b))
		_, err = a.db.PutItem(ctx, &ddb.PutItemInput{TableName: &a.table, Item: item,
			ConditionExpression:       aws.String("sw = :was"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":was": s(was)}})
		var failed *types.ConditionalCheckFailedException
		if !errors.As(err, &failed) {
			return sw, err
		}
	}
	return registry.Switching{}, registry.ErrSwitching
}

func (a *Agents) Switching(ctx context.Context, tenant, id string) (registry.Switching, bool, error) {
	out, err := a.db.GetItem(ctx, &ddb.GetItemInput{TableName: &a.table, Key: pkey(switchPK(tenant), id), ConsistentRead: aws.Bool(true)})
	if err != nil || out.Item == nil {
		return registry.Switching{}, false, err
	}
	var sw registry.Switching
	if err := json.Unmarshal([]byte(getS(out.Item, "sw")), &sw); err != nil {
		return registry.Switching{}, false, fmt.Errorf("dynamodb: switch of %q: %w", id, err)
	}
	return sw, true, nil
}

func (a *Agents) EndSwitch(ctx context.Context, tenant, id string) error {
	_, err := a.db.DeleteItem(ctx, &ddb.DeleteItemInput{TableName: &a.table, Key: pkey(switchPK(tenant), id)})
	return err
}

func decodeAgent(item map[string]types.AttributeValue) (registry.Definition, error) {
	var d registry.Definition
	if err := json.Unmarshal([]byte(getS(item, "def")), &d); err != nil {
		return registry.Definition{}, fmt.Errorf("dynamodb: agent %q: %w", getS(item, "sk"), err)
	}
	return d, nil
}

// SetCards rewrites version n's record with its Cards, leaving its
// definition as stored; only a version that exists.
func (a *Agents) SetCards(ctx context.Context, tenant, id string, n int, cards []registry.CardRevision) error {
	if n < 1 {
		return registry.ErrNoVersion
	}
	key := pkey(versionsPK(tenant, id), versionSK(n))
	out, err := a.db.GetItem(ctx, &ddb.GetItemInput{TableName: &a.table, Key: key, ConsistentRead: aws.Bool(true), ProjectionExpression: aws.String("ver")})
	if err != nil {
		return err
	}
	if out.Item == nil {
		return registry.ErrNoVersion
	}
	var v registry.Version
	if err := json.Unmarshal([]byte(getS(out.Item, "ver")), &v); err != nil {
		return fmt.Errorf("dynamodb: version %d of %q: %w", n, id, err)
	}
	v.Cards = cards
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = a.db.UpdateItem(ctx, &ddb.UpdateItemInput{TableName: &a.table, Key: key,
		UpdateExpression:          aws.String("SET ver = :v"),
		ConditionExpression:       aws.String("attribute_exists(pk)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":v": s(string(b))}})
	var gone *types.ConditionalCheckFailedException
	if errors.As(err, &gone) {
		return registry.ErrNoVersion
	}
	return err
}
