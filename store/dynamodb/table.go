package dynamodb

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// TableAPI is what CreateTable and DeleteTable need.
type TableAPI interface {
	CreateTable(context.Context, *ddb.CreateTableInput, ...func(*ddb.Options)) (*ddb.CreateTableOutput, error)
	DescribeTable(context.Context, *ddb.DescribeTableInput, ...func(*ddb.Options)) (*ddb.DescribeTableOutput, error)
	UpdateTimeToLive(context.Context, *ddb.UpdateTimeToLiveInput, ...func(*ddb.Options)) (*ddb.UpdateTimeToLiveOutput, error)
	DeleteTable(context.Context, *ddb.DeleteTableInput, ...func(*ddb.Options)) (*ddb.DeleteTableOutput, error)
}

// CreateTable makes the store's table: on-demand, so an idle table costs only
// what it holds; keys pk and sk; TTL on "ttl", so expired items are deleted
// for free. No secondary indexes, backups or streams.
func CreateTable(ctx context.Context, db TableAPI, name string) error {
	_, err := db.CreateTable(ctx, &ddb.CreateTableInput{
		TableName:   &name,
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
		},
	})
	if err != nil {
		return err
	}
	if err := ddb.NewTableExistsWaiter(db).Wait(ctx, &ddb.DescribeTableInput{TableName: &name}, 2*time.Minute); err != nil {
		return err
	}
	_, err = db.UpdateTimeToLive(ctx, &ddb.UpdateTimeToLiveInput{
		TableName:               &name,
		TimeToLiveSpecification: &types.TimeToLiveSpecification{AttributeName: aws.String("ttl"), Enabled: aws.Bool(true)},
	})
	return err
}
