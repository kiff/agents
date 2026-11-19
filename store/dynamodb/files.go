package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/kiff/agents/files"
)

// Files is a files.Store on the table:
//
//	record  pk=DF|tenant  sk=id                 the File
//	due     pk=DFD        sk=expires|tenant|id  when the sweep removes it
//
// Every read is in the tenant's partition, so a guessed id never reaches
// another account's file. Removing a file is the sweep's job, since its bytes
// go too; a record still carries a TTL a week past its expiry, so a sweep
// that keeps failing leaves no record forever.
type Files struct {
	db    API
	table string
}

// NewFiles returns the delivered-files store on table.
func NewFiles(db API, table string) *Files { return &Files{db: db, table: table} }

var _ files.Store = (*Files)(nil)

const fileDuePK = "DFD"

func filePK(tenant string) string { return key("DF", tenant) }

func fileDueSK(expires time.Time, tenant, id string) string {
	return fmt.Sprintf("%020d|%s", expires.UnixNano(), key("", tenant, id))
}

func fileItem(f files.File) map[string]types.AttributeValue {
	item := pkey(filePK(f.Tenant), f.ID)
	item["agent"], item["run"], item["mt"], item["name"], item["sha"] = s(f.Agent), s(f.Run), s(f.MediaType), s(f.Name), s(f.SHA256)
	item["size"], item["created"], item["expires"] = n(f.Size), nanos(f.Created), nanos(f.Expires)
	if f.Session != "" {
		item["session"] = s(f.Session)
	}
	item["ttl"] = ttl(f.Expires.Add(attachmentGrace))
	return item
}

func decodeFile(tenant string, item map[string]types.AttributeValue) files.File {
	return files.File{ID: getS(item, "sk"), Tenant: tenant, Agent: getS(item, "agent"), Run: getS(item, "run"),
		Session: getS(item, "session"), Name: getS(item, "name"), MediaType: getS(item, "mt"), Size: getN(item, "size"),
		SHA256: getS(item, "sha"), Created: getTime(item, "created"), Expires: getTime(item, "expires")}
}

// Create stores f and its due entry, in one transaction.
func (st *Files) Create(ctx context.Context, f files.File) error {
	due := pkey(fileDuePK, fileDueSK(f.Expires, f.Tenant, f.ID))
	due["tenant"], due["id"] = s(f.Tenant), s(f.ID)
	_, err := st.db.TransactWriteItems(ctx, &ddb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Put: &types.Put{TableName: &st.table, Item: fileItem(f), ConditionExpression: aws.String("attribute_not_exists(pk)")}},
		{Put: &types.Put{TableName: &st.table, Item: due}},
	}})
	return err
}

// Get reads one, consistently.
func (st *Files) Get(ctx context.Context, tenant, id string) (files.File, error) {
	out, err := st.db.GetItem(ctx, &ddb.GetItemInput{TableName: &st.table, Key: pkey(filePK(tenant), id), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return files.File{}, err
	}
	if out.Item == nil {
		return files.File{}, files.ErrNotFound
	}
	return decodeFile(tenant, out.Item), nil
}

// List reads the tenant's partition.
func (st *Files) List(ctx context.Context, tenant string) ([]files.File, error) {
	var out []files.File
	var start map[string]types.AttributeValue
	for {
		page, err := st.db.Query(ctx, &ddb.QueryInput{TableName: &st.table, ConsistentRead: aws.Bool(true),
			KeyConditionExpression:    aws.String("pk = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(filePK(tenant))},
			ExclusiveStartKey:         start})
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			out = append(out, decodeFile(tenant, item))
		}
		if start = page.LastEvaluatedKey; start == nil {
			return out, nil
		}
	}
}

// Due lists what is due by now, oldest first.
func (st *Files) Due(ctx context.Context, now time.Time, limit int) ([]files.File, error) {
	out, err := st.db.Query(ctx, &ddb.QueryInput{TableName: &st.table,
		KeyConditionExpression:    aws.String("pk = :pk AND sk <= :now"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(fileDuePK), ":now": s(fmt.Sprintf("%020d|~", now.UnixNano()))},
		Limit:                     aws.Int32(int32(limit))})
	if err != nil {
		return nil, err
	}
	var due []files.File
	for _, item := range out.Items {
		f, err := st.Get(ctx, getS(item, "tenant"), getS(item, "id"))
		if errors.Is(err, files.ErrNotFound) {
			// The record is gone already: drop its stray entry.
			if _, err := st.db.DeleteItem(ctx, &ddb.DeleteItemInput{TableName: &st.table, Key: pkey(fileDuePK, getS(item, "sk"))}); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		due = append(due, f)
	}
	return due, nil
}

// Delete removes one and its due entry.
func (st *Files) Delete(ctx context.Context, tenant, id string) error {
	f, err := st.Get(ctx, tenant, id)
	if errors.Is(err, files.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = st.db.TransactWriteItems(ctx, &ddb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Delete: &types.Delete{TableName: &st.table, Key: pkey(filePK(tenant), id)}},
		{Delete: &types.Delete{TableName: &st.table, Key: pkey(fileDuePK, fileDueSK(f.Expires, tenant, id))}},
	}})
	return err
}
