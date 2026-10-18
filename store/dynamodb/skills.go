package dynamodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/kiff/agents/skill"
)

// Skills is a skill.Store on the store's table. A version is one item, with
// no TTL, written once: a version never changes (#101).
//
//	skill  pk=K|tenant  sk=name|version  the Package, as JSON in "pkg"
type Skills struct {
	db    API
	table string
}

// NewSkills returns the skill store on table.
func NewSkills(db API, table string) *Skills { return &Skills{db: db, table: table} }

var _ skill.Store = (*Skills)(nil)

func skillsPK(tenant string) string       { return key("K", tenant) }
func skillSK(name, version string) string { return name + "|" + version }

// Put writes p only if its version isn't stored; when it is, the stored
// content decides: the same is fine, other content is skill.ErrExists.
func (st *Skills) Put(ctx context.Context, p skill.Package) error {
	b, err := skill.Encode(p)
	if err != nil {
		return err
	}
	item := pkey(skillsPK(p.Tenant), skillSK(p.Name, p.Version))
	item["pkg"] = s(string(b))
	_, err = st.db.PutItem(ctx, &ddb.PutItemInput{TableName: &st.table, Item: item, ConditionExpression: aws.String("attribute_not_exists(pk)")})
	var exists *types.ConditionalCheckFailedException
	if !errors.As(err, &exists) {
		return err
	}
	old, err := st.Get(ctx, p.Tenant, p.Name, p.Version)
	if err != nil {
		return err
	}
	if !old.Same(p) {
		return skill.ErrExists
	}
	return nil
}

func (st *Skills) Get(ctx context.Context, tenant, name, version string) (skill.Package, error) {
	out, err := st.db.GetItem(ctx, &ddb.GetItemInput{TableName: &st.table, Key: pkey(skillsPK(tenant), skillSK(name, version)), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return skill.Package{}, err
	}
	if out.Item == nil {
		return skill.Package{}, skill.ErrNotFound
	}
	return decodeSkill(out.Item)
}

// List reads the tenant's partition, by name then version, without bodies
// and files.
func (st *Skills) List(ctx context.Context, tenant string) ([]skill.Package, error) {
	var out []skill.Package
	in := &ddb.QueryInput{
		TableName:                 &st.table,
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(skillsPK(tenant))},
		ConsistentRead:            aws.Bool(true),
	}
	for {
		page, err := st.db.Query(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			p, err := decodeSkill(item)
			if err != nil {
				return nil, err
			}
			p.Body, p.Files = "", nil
			out = append(out, p)
		}
		if page.LastEvaluatedKey == nil {
			return out, nil
		}
		in.ExclusiveStartKey = page.LastEvaluatedKey
	}
}

func decodeSkill(item map[string]types.AttributeValue) (skill.Package, error) {
	var p skill.Package
	if err := json.Unmarshal([]byte(getS(item, "pkg")), &p); err != nil {
		return skill.Package{}, fmt.Errorf("dynamodb: skill %q: %w", getS(item, "sk"), err)
	}
	return p, nil
}
