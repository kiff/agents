package dynamodb

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/kiff/agents/usage"
)

// Usage is a usage.Store on the table:
//
//	period  pk=U|tenant  sk=period  "spent" (micro USD, a number), "ttl"
//
// Adding is one UpdateItem with ADD, so concurrent runs never lose a cost.
type Usage struct {
	db    API
	table string
}

// NewUsage returns the usage store on table.
func NewUsage(db API, table string) *Usage { return &Usage{db: db, table: table} }

var _ usage.Store = (*Usage)(nil)

func usagePK(tenant string) string { return key("U", tenant) }

// Add adds microUSD to the period, creating it at zero first.
func (u *Usage) Add(ctx context.Context, tenant, period string, microUSD int64, until time.Time) error {
	_, err := u.db.UpdateItem(ctx, &ddb.UpdateItemInput{TableName: &u.table, Key: pkey(usagePK(tenant), period),
		UpdateExpression:          aws.String("ADD spent :c SET #ttl = :ttl"),
		ExpressionAttributeNames:  map[string]string{"#ttl": "ttl"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":c": n(microUSD), ":ttl": ttl(until)}})
	return err
}

// Get reads the period, consistently.
func (u *Usage) Get(ctx context.Context, tenant, period string) (usage.Period, error) {
	out, err := u.db.GetItem(ctx, &ddb.GetItemInput{TableName: &u.table, Key: pkey(usagePK(tenant), period), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return usage.Period{}, err
	}
	return decodePeriod(out.Item), nil
}

// EraseTenant deletes the tenant's partition.
func (u *Usage) EraseTenant(ctx context.Context, tenant string) error {
	return (&Store{db: u.db, table: u.table}).erasePartition(ctx, usagePK(tenant))
}

func decodePeriod(item map[string]types.AttributeValue) usage.Period {
	if item == nil {
		return usage.Period{}
	}
	return usage.Period{Spent: getN(item, "spent")}
}
