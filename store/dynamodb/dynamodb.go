// Package dynamodb is a session store on one DynamoDB table.
//
// The table is on-demand, has only a string partition key "pk" and sort key
// "sk", and expires items through TTL on "ttl" (see CreateTable). There are no
// secondary indexes, backups or streams: nothing is paid for that a session
// doesn't need. Every item whose data expires carries a TTL, so DynamoDB
// deletes it for free, and Purge does nothing.
//
// Items, in the partition each lives in:
//
//	session  pk=S|tenant|id     sk=M         the Session, and the last Seq
//	event    pk=S|tenant|id     sk=E|seq     one Event
//	index    pk=A|tenant|agent  sk=id        lists an agent's sessions
//	note     pk=N|tenant|agent  sk=name      one memory Note
package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/kiff/agents/session"
)

// API is the part of the DynamoDB client the store uses.
type API interface {
	GetItem(context.Context, *ddb.GetItemInput, ...func(*ddb.Options)) (*ddb.GetItemOutput, error)
	PutItem(context.Context, *ddb.PutItemInput, ...func(*ddb.Options)) (*ddb.PutItemOutput, error)
	UpdateItem(context.Context, *ddb.UpdateItemInput, ...func(*ddb.Options)) (*ddb.UpdateItemOutput, error)
	DeleteItem(context.Context, *ddb.DeleteItemInput, ...func(*ddb.Options)) (*ddb.DeleteItemOutput, error)
	Query(context.Context, *ddb.QueryInput, ...func(*ddb.Options)) (*ddb.QueryOutput, error)
	BatchGetItem(context.Context, *ddb.BatchGetItemInput, ...func(*ddb.Options)) (*ddb.BatchGetItemOutput, error)
	BatchWriteItem(context.Context, *ddb.BatchWriteItemInput, ...func(*ddb.Options)) (*ddb.BatchWriteItemOutput, error)
	TransactWriteItems(context.Context, *ddb.TransactWriteItemsInput, ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error)
}

// UntilEndTTL is how long an event kept only until its run ends may stay if
// the run never ends (a crash before End): a safety net so what a tool
// returned can't stay in the table for good.
const UntilEndTTL = 7 * 24 * time.Hour

// Store is a session.Store and session.Notes on one table.
type Store struct {
	db    API
	table string
	now   func() time.Time
}

// New returns a store on table, on the system clock.
func New(db API, table string) *Store { return NewAt(db, table, time.Now) }

// NewAt returns a store whose clock decides what has expired.
func NewAt(db API, table string, now func() time.Time) *Store {
	return &Store{db: db, table: table, now: now}
}

var (
	_ session.Store = (*Store)(nil)
	_ session.Notes = (*Store)(nil)
)

// key joins parts, each prefixed with its length, so no tenant, agent or name
// can make one key look like another.
func key(kind string, parts ...string) string {
	var b strings.Builder
	b.WriteString(kind)
	for _, p := range parts {
		fmt.Fprintf(&b, "|%d:%s", len(p), p)
	}
	return b.String()
}

func sessionPK(tenant, id string) string  { return key("S", tenant, id) }
func indexPK(tenant, agent string) string { return key("A", tenant, agent) }
func notesPK(tenant, agent string) string { return key("N", tenant, agent) }
func eventSK(seq int64) string            { return fmt.Sprintf("E|%020d", seq) }

const metaSK = "M"

func s(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
func n(v int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
}
func nanos(t time.Time) types.AttributeValue {
	if t.IsZero() {
		return n(0)
	}
	return n(t.UnixNano())
}

// ttl is the TTL attribute for an expiry: whole seconds, rounded up, so an
// item never goes before its time.
func ttl(t time.Time) types.AttributeValue {
	sec := t.Unix()
	if t.Nanosecond() > 0 {
		sec++
	}
	return n(sec)
}

func getS(item map[string]types.AttributeValue, name string) string {
	if v, ok := item[name].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func getN(item map[string]types.AttributeValue, name string) int64 {
	if v, ok := item[name].(*types.AttributeValueMemberN); ok {
		i, _ := strconv.ParseInt(v.Value, 10, 64)
		return i
	}
	return 0
}

func getTime(item map[string]types.AttributeValue, name string) time.Time {
	if v := getN(item, name); v != 0 {
		return time.Unix(0, v).UTC()
	}
	return time.Time{}
}

func expired(at, now time.Time) bool { return !at.IsZero() && at.Before(now) }

func pkey(pk, sk string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"pk": s(pk), "sk": s(sk)}
}

func metaItem(se session.Session, seq int64) map[string]types.AttributeValue {
	item := pkey(sessionPK(se.Tenant, se.ID), metaSK)
	item["id"] = s(se.ID)
	item["agent"] = s(se.Agent)
	item["kind"] = s(string(se.Kind))
	item["status"] = s(string(se.Status))
	item["started"] = nanos(se.Started)
	item["updated"] = nanos(se.Updated)
	item["expires"] = nanos(se.Expires)
	item["seq"] = n(seq)
	if !se.Expires.IsZero() {
		item["ttl"] = ttl(se.Expires)
	}
	return item
}

func sessionOf(tenant string, item map[string]types.AttributeValue) session.Session {
	return session.Session{
		ID: getS(item, "id"), Tenant: tenant,
		Agent:   getS(item, "agent"),
		Kind:    session.Kind(getS(item, "kind")),
		Status:  session.Status(getS(item, "status")),
		Started: getTime(item, "started"),
		Updated: getTime(item, "updated"),
		Expires: getTime(item, "expires"),
	}
}

func eventItem(tenant, id string, e session.Event) map[string]types.AttributeValue {
	item := pkey(sessionPK(tenant, id), eventSK(e.Seq))
	item["time"] = nanos(e.Time)
	item["kind"] = s(string(e.Kind))
	item["body"] = &types.AttributeValueMemberB{Value: e.Body} // the SDK copies it into the request
	item["expires"] = nanos(e.Expires)
	switch {
	case e.UntilEnd:
		item["until_end"] = &types.AttributeValueMemberBOOL{Value: true}
		item["ttl"] = ttl(e.Time.Add(UntilEndTTL))
	case !e.Expires.IsZero():
		item["ttl"] = ttl(e.Expires)
	}
	return item
}

func eventOf(id string, item map[string]types.AttributeValue) session.Event {
	seq, _ := strconv.ParseInt(strings.TrimPrefix(getS(item, "sk"), "E|"), 10, 64)
	e := session.Event{
		Session: id, Seq: seq,
		Time:    getTime(item, "time"),
		Kind:    session.EventKind(getS(item, "kind")),
		Expires: getTime(item, "expires"),
	}
	if b, ok := item["body"].(*types.AttributeValueMemberB); ok {
		e.Body = b.Value // a fresh slice from the response: the caller's own
	}
	if u, ok := item["until_end"].(*types.AttributeValueMemberBOOL); ok {
		e.UntilEnd = u.Value
	}
	return e
}

func (st *Store) Create(ctx context.Context, se session.Session) error {
	index := pkey(indexPK(se.Tenant, se.Agent), se.ID)
	if !se.Expires.IsZero() {
		index["ttl"] = ttl(se.Expires)
	}
	_, err := st.db.TransactWriteItems(ctx, &ddb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Put: &types.Put{TableName: &st.table, Item: metaItem(se, 0), ConditionExpression: aws.String("attribute_not_exists(pk)")}},
		{Put: &types.Put{TableName: &st.table, Item: index}},
	}})
	return err
}

// deleting is the status of a session being deleted: Append and End require
// a running session, so nothing new lands in it, and callers no longer find
// it. It stays in the table until its events are gone, so a retried Delete or
// DeleteAgent can finish the job.
const deleting session.Status = "deleting"

// meta reads the tenant's live session and its last Seq. A session being
// deleted is not found.
func (st *Store) meta(ctx context.Context, tenant, id string) (session.Session, int64, error) {
	se, seq, err := st.metaAny(ctx, tenant, id)
	if err == nil && se.Status == deleting {
		return session.Session{}, 0, session.ErrNotFound
	}
	return se, seq, err
}

// metaAny is meta, including a session being deleted.
func (st *Store) metaAny(ctx context.Context, tenant, id string) (session.Session, int64, error) {
	out, err := st.db.GetItem(ctx, &ddb.GetItemInput{TableName: &st.table, Key: pkey(sessionPK(tenant, id), metaSK), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return session.Session{}, 0, err
	}
	if out.Item == nil {
		return session.Session{}, 0, session.ErrNotFound
	}
	se := sessionOf(tenant, out.Item)
	if expired(se.Expires, st.now()) {
		return session.Session{}, 0, session.ErrNotFound
	}
	return se, getN(out.Item, "seq"), nil
}

func (st *Store) Append(ctx context.Context, tenant, id string, events []session.Event) error {
	se, seq, err := st.meta(ctx, tenant, id)
	if err != nil {
		return err
	}
	if se.Status != session.Running {
		return session.ErrEnded
	}
	next, updated, expires := seq, se.Updated, se.Expires
	var add []session.Event
	for _, e := range events {
		if e.Seq <= seq {
			continue // stored by an earlier attempt
		}
		if e.Seq != next+1 {
			return session.ErrGap
		}
		next, updated = e.Seq, e.Time
		if !expires.IsZero() && e.Expires.After(expires) {
			expires = e.Expires
		}
		add = append(add, e)
	}
	if len(add) == 0 {
		return nil
	}

	update := "SET seq = :next, updated = :updated"
	values := map[string]types.AttributeValue{
		":next": n(next), ":seq": n(seq), ":updated": nanos(updated), ":running": s(string(session.Running)),
	}
	moved := !expires.Equal(se.Expires)
	if moved {
		update += ", expires = :expires, #ttl = :ttl"
		values[":expires"], values[":ttl"] = nanos(expires), ttl(expires)
	}
	names := map[string]string{"#status": "status"}
	if moved {
		names["#ttl"] = "ttl"
	}
	items := []types.TransactWriteItem{{Update: &types.Update{
		TableName: &st.table, Key: pkey(sessionPK(tenant, id), metaSK),
		UpdateExpression:          &update,
		ConditionExpression:       aws.String("seq = :seq AND #status = :running"),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	}}}
	for _, e := range add {
		items = append(items, types.TransactWriteItem{Put: &types.Put{
			TableName: &st.table, Item: eventItem(tenant, id, e), ConditionExpression: aws.String("attribute_not_exists(pk)"),
		}})
	}
	if moved {
		items = append(items, types.TransactWriteItem{Update: &types.Update{
			TableName: &st.table, Key: pkey(indexPK(tenant, se.Agent), id),
			UpdateExpression:          aws.String("SET #ttl = :ttl"),
			ExpressionAttributeNames:  map[string]string{"#ttl": "ttl"},
			ExpressionAttributeValues: map[string]types.AttributeValue{":ttl": ttl(expires)},
		}})
	}
	_, err = st.db.TransactWriteItems(ctx, &ddb.TransactWriteItemsInput{TransactItems: items})
	var cancelled *types.TransactionCanceledException
	if errors.As(err, &cancelled) {
		// Someone got there first: the session ended, or an earlier attempt
		// of this very append landed. Say which.
		now, nowSeq, rerr := st.meta(ctx, tenant, id)
		switch {
		case rerr != nil:
			return rerr
		case now.Status != session.Running:
			return session.ErrEnded
		case nowSeq >= next:
			return nil
		}
	}
	return err
}

// partition reads every item in a partition, in sort key order.
func (st *Store) partition(ctx context.Context, pk string, filter func(map[string]types.AttributeValue) bool) ([]map[string]types.AttributeValue, error) {
	var out []map[string]types.AttributeValue
	in := &ddb.QueryInput{
		TableName:                 &st.table,
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(pk)},
		ConsistentRead:            aws.Bool(true),
	}
	for {
		page, err := st.db.Query(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if filter == nil || filter(item) {
				out = append(out, item)
			}
		}
		if page.LastEvaluatedKey == nil {
			return out, nil
		}
		in.ExclusiveStartKey = page.LastEvaluatedKey
	}
}

func (st *Store) Read(ctx context.Context, tenant, id string) (session.Session, []session.Event, error) {
	items, err := st.partition(ctx, sessionPK(tenant, id), nil)
	if err != nil {
		return session.Session{}, nil, err
	}
	now := st.now()
	var se session.Session
	found := false
	var events []session.Event
	for _, item := range items {
		if getS(item, "sk") == metaSK {
			se, found = sessionOf(tenant, item), true
			continue
		}
		if e := eventOf(id, item); !expired(e.Expires, now) {
			events = append(events, e)
		}
	}
	if !found || se.Status == deleting || expired(se.Expires, now) {
		return session.Session{}, nil, session.ErrNotFound
	}
	return se, events, nil
}

func (st *Store) List(ctx context.Context, tenant, agent string, limit int) ([]session.Session, error) {
	index, err := st.partition(ctx, indexPK(tenant, agent), nil)
	if err != nil {
		return nil, err
	}
	keys := make([]map[string]types.AttributeValue, 0, len(index))
	for _, item := range index {
		keys = append(keys, pkey(sessionPK(tenant, getS(item, "sk")), metaSK))
	}
	metas, err := st.batchGet(ctx, keys)
	if err != nil {
		return nil, err
	}
	now := st.now()
	var out []session.Session
	for _, item := range metas {
		if se := sessionOf(tenant, item); se.Status != deleting && !expired(se.Expires, now) {
			out = append(out, se)
		}
	}
	sortNewestFirst(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// batchGet reads items by key, 100 to a request, a few requests at a time.
func (st *Store) batchGet(ctx context.Context, keys []map[string]types.AttributeValue) ([]map[string]types.AttributeValue, error) {
	var (
		mu  sync.Mutex
		out []map[string]types.AttributeValue
	)
	err := chunks(ctx, keys, 100, func(ctx context.Context, chunk []map[string]types.AttributeValue) error {
		req := map[string]types.KeysAndAttributes{st.table: {Keys: chunk, ConsistentRead: aws.Bool(true)}}
		for try := 0; len(req) > 0; try++ {
			if try > 0 {
				if err := backoff(ctx, try); err != nil {
					return err
				}
			}
			page, err := st.db.BatchGetItem(ctx, &ddb.BatchGetItemInput{RequestItems: req})
			if err != nil {
				return err
			}
			mu.Lock()
			out = append(out, page.Responses[st.table]...)
			mu.Unlock()
			req = page.UnprocessedKeys
		}
		return nil
	})
	return out, err
}

// deleteKeys deletes items by key, 25 to a request, a few requests at a time.
func (st *Store) deleteKeys(ctx context.Context, keys []map[string]types.AttributeValue) error {
	return chunks(ctx, keys, 25, func(ctx context.Context, chunk []map[string]types.AttributeValue) error {
		writes := make([]types.WriteRequest, len(chunk))
		for i, k := range chunk {
			writes[i] = types.WriteRequest{DeleteRequest: &types.DeleteRequest{Key: k}}
		}
		req := map[string][]types.WriteRequest{st.table: writes}
		for try := 0; len(req) > 0; try++ {
			if try > 0 {
				if err := backoff(ctx, try); err != nil {
					return err
				}
			}
			out, err := st.db.BatchWriteItem(ctx, &ddb.BatchWriteItemInput{RequestItems: req})
			if err != nil {
				return err
			}
			req = out.UnprocessedItems
		}
		return nil
	})
}

// keysOf returns the primary keys of items.
func keysOf(items []map[string]types.AttributeValue) []map[string]types.AttributeValue {
	keys := make([]map[string]types.AttributeValue, len(items))
	for i, item := range items {
		keys[i] = map[string]types.AttributeValue{"pk": item["pk"], "sk": item["sk"]}
	}
	return keys
}

func (st *Store) End(ctx context.Context, tenant, id string, status session.Status) error {
	se, _, err := st.meta(ctx, tenant, id)
	if err != nil {
		return err
	}
	if se.Status != session.Running {
		return session.ErrEnded
	}
	// Drop what was kept only until the end first: if this fails, the session
	// is still running and End is retried, so nothing is left behind.
	untilEnd, err := st.partition(ctx, sessionPK(tenant, id), func(item map[string]types.AttributeValue) bool {
		u, ok := item["until_end"].(*types.AttributeValueMemberBOOL)
		return ok && u.Value
	})
	if err != nil {
		return err
	}
	if err := st.deleteKeys(ctx, keysOf(untilEnd)); err != nil {
		return err
	}
	_, err = st.db.UpdateItem(ctx, &ddb.UpdateItemInput{
		TableName: &st.table, Key: pkey(sessionPK(tenant, id), metaSK),
		UpdateExpression:          aws.String("SET #status = :status"),
		ConditionExpression:       aws.String("#status = :running"),
		ExpressionAttributeNames:  map[string]string{"#status": "status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":status": s(string(status)), ":running": s(string(session.Running))},
	})
	var failed *types.ConditionalCheckFailedException
	if errors.As(err, &failed) {
		return session.ErrEnded
	}
	return err
}

func (st *Store) Delete(ctx context.Context, tenant, id string) error {
	se, _, err := st.metaAny(ctx, tenant, id) // a session already being deleted: finish the job
	if err != nil {
		return err
	}
	return st.deleteSession(ctx, tenant, se.Agent, id)
}

// deleteSession marks the session deleting, then deletes its events, then
// the session, then its index entry. Once marked, nothing more can be
// appended, so no event is left behind; and whichever step fails, a retry
// still finds the session, through Delete (the session item) or DeleteAgent
// (the index entry), and carries on.
func (st *Store) deleteSession(ctx context.Context, tenant, agent, id string) error {
	_, err := st.db.UpdateItem(ctx, &ddb.UpdateItemInput{
		TableName: &st.table, Key: pkey(sessionPK(tenant, id), metaSK),
		UpdateExpression:          aws.String("SET #status = :deleting"),
		ConditionExpression:       aws.String("attribute_exists(pk)"),
		ExpressionAttributeNames:  map[string]string{"#status": "status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":deleting": s(string(deleting))},
	})
	var gone *types.ConditionalCheckFailedException
	if err != nil && !errors.As(err, &gone) { // gone: an earlier attempt deleted the session item
		return err
	}
	items, err := st.partition(ctx, sessionPK(tenant, id), func(item map[string]types.AttributeValue) bool {
		return getS(item, "sk") != metaSK
	})
	if err != nil {
		return err
	}
	if err := st.deleteKeys(ctx, keysOf(items)); err != nil {
		return err
	}
	if _, err := st.db.DeleteItem(ctx, &ddb.DeleteItemInput{TableName: &st.table, Key: pkey(sessionPK(tenant, id), metaSK)}); err != nil {
		return err
	}
	return st.deleteKeys(ctx, []map[string]types.AttributeValue{pkey(indexPK(tenant, agent), id)})
}

func (st *Store) DeleteAgent(ctx context.Context, tenant, agent string) error {
	index, err := st.partition(ctx, indexPK(tenant, agent), nil)
	if err != nil {
		return err
	}
	return chunks(ctx, index, 1, func(ctx context.Context, one []map[string]types.AttributeValue) error {
		return st.deleteSession(ctx, tenant, agent, getS(one[0], "sk"))
	})
}

// Purge does nothing: every item that expires carries a TTL, and DynamoDB
// deletes it for free. Until it does, Read and List leave it out.
func (st *Store) Purge(context.Context, time.Time) (int, error) { return 0, nil }

func (st *Store) ListNotes(ctx context.Context, tenant, agent string) ([]session.Note, error) {
	items, err := st.partition(ctx, notesPK(tenant, agent), nil)
	if err != nil {
		return nil, err
	}
	out := make([]session.Note, len(items))
	for i, item := range items {
		out[i] = session.Note{Tenant: tenant, Agent: agent, Name: getS(item, "sk"), Text: getS(item, "text"), Updated: getTime(item, "updated")}
	}
	return out, nil // in sort key order: by name
}

func (st *Store) PutNote(ctx context.Context, note session.Note) error {
	item := pkey(notesPK(note.Tenant, note.Agent), note.Name)
	item["text"] = s(note.Text)
	item["updated"] = nanos(note.Updated)
	_, err := st.db.PutItem(ctx, &ddb.PutItemInput{TableName: &st.table, Item: item})
	return err
}

func (st *Store) DeleteNote(ctx context.Context, tenant, agent, name string) error {
	_, err := st.db.DeleteItem(ctx, &ddb.DeleteItemInput{TableName: &st.table, Key: pkey(notesPK(tenant, agent), name)})
	return err
}

func (st *Store) DeleteNotes(ctx context.Context, tenant, agent string) error {
	items, err := st.partition(ctx, notesPK(tenant, agent), nil)
	if err != nil {
		return err
	}
	return st.deleteKeys(ctx, keysOf(items))
}
