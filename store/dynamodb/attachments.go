package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/kiff/agents/attach"
)

// Attachments is an attach.Store on the table:
//
//	record  pk=F|tenant  sk=id                 the Attachment, and "dsk", its due entry
//	due     pk=FD        sk=due|tenant|id      when the sweep removes it
//
// Every read is in the tenant's partition, so a guessed id never reaches
// another tenant's file. The due queue holds one entry per attachment that
// will go: an unsent one an hour after it was made, a sent one when its
// message does. Removing a file is the sweep's job, since its bytes go too;
// a record still carries a TTL a week past its due time, so a sweep that
// keeps failing leaves no record forever.
type Attachments struct {
	db    API
	table string
}

// NewAttachments returns the attachments store on table.
func NewAttachments(db API, table string) *Attachments { return &Attachments{db: db, table: table} }

var _ attach.Store = (*Attachments)(nil)

const attDuePK = "FD"

// attachmentGrace is how long past its due time a record stays if no sweep
// removes it.
const attachmentGrace = 7 * 24 * time.Hour

func attPK(tenant string) string { return key("F", tenant) }

func attDueSK(due time.Time, tenant, id string) string {
	return fmt.Sprintf("%020d|%s", due.UnixNano(), key("", tenant, id))
}

func attItem(a attach.Attachment) map[string]types.AttributeValue {
	item := pkey(attPK(a.Tenant), a.ID)
	item["agent"], item["owner"], item["mt"], item["name"] = s(a.Agent), s(a.Owner), s(a.MediaType), s(a.Name)
	item["size"], item["sha"], item["created"] = n(a.Size), s(a.SHA256), nanos(a.Created)
	if due := a.Due(); !due.IsZero() {
		item["dsk"] = s(attDueSK(due, a.Tenant, a.ID))
		item["ttl"] = ttl(due.Add(attachmentGrace))
	}
	return item
}

func decodeAttachment(tenant string, item map[string]types.AttributeValue) attach.Attachment {
	return attach.Attachment{ID: getS(item, "sk"), Tenant: tenant, Agent: getS(item, "agent"), Owner: getS(item, "owner"),
		MediaType: getS(item, "mt"), Name: getS(item, "name"), Size: getN(item, "size"), SHA256: getS(item, "sha"),
		Created: getTime(item, "created"), Checked: getBool(item, "checked"), Session: getS(item, "session"),
		Run: getS(item, "run"), Bound: getTime(item, "bound"), Expires: getTime(item, "expires")}
}

func dueItem(a attach.Attachment) map[string]types.AttributeValue {
	item := pkey(attDuePK, attDueSK(a.Due(), a.Tenant, a.ID))
	item["tenant"], item["id"] = s(a.Tenant), s(a.ID)
	return item
}

// Create stores a and its due entry, in one transaction.
func (st *Attachments) Create(ctx context.Context, a attach.Attachment) error {
	_, err := st.db.TransactWriteItems(ctx, &ddb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Put: &types.Put{TableName: &st.table, Item: attItem(a), ConditionExpression: aws.String("attribute_not_exists(pk)")}},
		{Put: &types.Put{TableName: &st.table, Item: dueItem(a)}},
	}})
	return err
}

// Get reads one, consistently.
func (st *Attachments) Get(ctx context.Context, tenant, id string) (attach.Attachment, error) {
	out, err := st.db.GetItem(ctx, &ddb.GetItemInput{TableName: &st.table, Key: pkey(attPK(tenant), id), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return attach.Attachment{}, err
	}
	if out.Item == nil {
		return attach.Attachment{}, attach.ErrNotFound
	}
	return decodeAttachment(tenant, out.Item), nil
}

// Check marks it checked.
func (st *Attachments) Check(ctx context.Context, tenant, id string) error {
	_, err := st.db.UpdateItem(ctx, &ddb.UpdateItemInput{TableName: &st.table, Key: pkey(attPK(tenant), id),
		UpdateExpression:          aws.String("SET checked = :y"),
		ConditionExpression:       aws.String("attribute_exists(pk)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":y": yes}})
	var failed *types.ConditionalCheckFailedException
	if errors.As(err, &failed) {
		return attach.ErrNotFound
	}
	return err
}

// Bind ties an unbound one to its message, and moves its due entry from an
// hour after it was made to when its message goes (none: with its session).
func (st *Attachments) Bind(ctx context.Context, tenant, id, session, run string, at, expires time.Time) error {
	a, err := st.Get(ctx, tenant, id)
	if err != nil {
		return err
	}
	if a.Session != "" {
		return attach.ErrBound
	}
	old := a
	a.Session, a.Run, a.Bound, a.Expires = session, run, at, expires
	vals := map[string]types.AttributeValue{":s": s(session), ":r": s(run), ":b": nanos(at), ":e": nanos(expires)}
	update := "SET #s = :s, #r = :r, bound = :b, expires = :e"
	if due := a.Due(); !due.IsZero() {
		update += ", dsk = :d, #ttl = :t"
		vals[":d"], vals[":t"] = s(attDueSK(due, tenant, id)), ttl(due.Add(attachmentGrace))
	} else {
		update += " REMOVE dsk, #ttl"
	}
	items := []types.TransactWriteItem{
		{Update: &types.Update{TableName: &st.table, Key: pkey(attPK(tenant), id), UpdateExpression: aws.String(update),
			ConditionExpression:       aws.String("attribute_exists(pk) AND attribute_not_exists(#s)"),
			ExpressionAttributeNames:  map[string]string{"#s": "session", "#r": "run", "#ttl": "ttl"},
			ExpressionAttributeValues: vals}},
		{Delete: &types.Delete{TableName: &st.table, Key: pkey(attDuePK, attDueSK(old.Due(), tenant, id))}},
	}
	if !a.Due().IsZero() {
		items = append(items, types.TransactWriteItem{Put: &types.Put{TableName: &st.table, Item: dueItem(a)}})
	}
	_, err = st.db.TransactWriteItems(ctx, &ddb.TransactWriteItemsInput{TransactItems: items})
	var cancelled *types.TransactionCanceledException
	if errors.As(err, &cancelled) {
		return attach.ErrBound // sent meanwhile, or deleted: either way, not this message's
	}
	return err
}

// list reads the tenant's attachments that keep says to.
func (st *Attachments) list(ctx context.Context, tenant string, keep func(attach.Attachment) bool) ([]attach.Attachment, error) {
	var out []attach.Attachment
	var start map[string]types.AttributeValue
	for {
		page, err := st.db.Query(ctx, &ddb.QueryInput{TableName: &st.table, ConsistentRead: aws.Bool(true),
			KeyConditionExpression:    aws.String("pk = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(attPK(tenant))},
			ExclusiveStartKey:         start})
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if a := decodeAttachment(tenant, item); keep(a) {
				out = append(out, a)
			}
		}
		if start = page.LastEvaluatedKey; start == nil {
			return out, nil
		}
	}
}

// Session lists a session's.
func (st *Attachments) Session(ctx context.Context, tenant, session string) ([]attach.Attachment, error) {
	return st.list(ctx, tenant, func(a attach.Attachment) bool { return a.Session == session })
}

// Agent lists an agent's.
func (st *Attachments) Agent(ctx context.Context, tenant, agent string) ([]attach.Attachment, error) {
	return st.list(ctx, tenant, func(a attach.Attachment) bool { return a.Agent == agent })
}

// Due lists what is due by now, oldest first.
func (st *Attachments) Due(ctx context.Context, now time.Time, limit int) ([]attach.Attachment, error) {
	out, err := st.db.Query(ctx, &ddb.QueryInput{TableName: &st.table,
		KeyConditionExpression:    aws.String("pk = :pk AND sk <= :now"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(attDuePK), ":now": s(fmt.Sprintf("%020d|~", now.UnixNano()))},
		Limit:                     aws.Int32(int32(limit))})
	if err != nil {
		return nil, err
	}
	var due []attach.Attachment
	for _, item := range out.Items {
		tenant, id := getS(item, "tenant"), getS(item, "id")
		a, err := st.Get(ctx, tenant, id)
		if errors.Is(err, attach.ErrNotFound) {
			// The record is gone already: drop its stray entry.
			if _, err := st.db.DeleteItem(ctx, &ddb.DeleteItemInput{TableName: &st.table, Key: pkey(attDuePK, getS(item, "sk"))}); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		due = append(due, a)
	}
	return due, nil
}

// Delete removes one and its due entry.
func (st *Attachments) Delete(ctx context.Context, tenant, id string) error {
	a, err := st.Get(ctx, tenant, id)
	if errors.Is(err, attach.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	items := []types.TransactWriteItem{{Delete: &types.Delete{TableName: &st.table, Key: pkey(attPK(tenant), id)}}}
	if due := a.Due(); !due.IsZero() {
		items = append(items, types.TransactWriteItem{Delete: &types.Delete{TableName: &st.table, Key: pkey(attDuePK, attDueSK(due, tenant, id))}})
	}
	_, err = st.db.TransactWriteItems(ctx, &ddb.TransactWriteItemsInput{TransactItems: items})
	return err
}
