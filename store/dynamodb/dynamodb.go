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
//	session  pk=S|tenant|id     sk=M             the Session, and its open runs' last Seq
//	ended    pk=S|tenant|id     sk=R|run         a run that has ended
//	event    pk=S|tenant|id     sk=E|run|seq     one Event
//	index    pk=A|tenant|agent  sk=id            lists an agent's sessions
//	note     pk=N|tenant|agent  sk=name          one memory Note
//
// A run that is open (begun and not ended, held or not) has its last Seq and
// its creation time in the session item's map "runs": every Append writes
// that item anyway, so an open run costs no item of its own, and its state
// can't expire apart from the session. Only an open run takes events or a
// hold, and that check is on the session item, so it lasts as long as the
// session. When the run ends it leaves the map, so the session item stays as
// small as the runs open in it, however long the thread. An "ended" item
// takes its place, which keeps Begin from opening the run again: it lives at
// least MaxStartDelay past the run's creation, after which Begin refuses the
// run anyway, and usually as long as the session. "held" is the string set of runs held in the session;
// while it has any, the session and its index entry carry no TTL.
package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
// the run never ends (a crash before End): a safety net so what a tool did
// can't stay in the table for good. It is a day past KIFF's longest hold (7
// days), so a run paused that long still has its timeline when it continues.
const UntilEndTTL = 8 * 24 * time.Hour

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

// eventPrefix starts the sort key of every event of run.
func eventPrefix(run string) string { return key("E", run) + "|" }

func eventSK(run string, seq int64) string { return fmt.Sprintf("%s%020d", eventPrefix(run), seq) }

func endedSK(run string) string { return key("R", run) }

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

var yes types.AttributeValue = &types.AttributeValueMemberBOOL{Value: true}

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

func getBool(item map[string]types.AttributeValue, name string) bool {
	v, ok := item[name].(*types.AttributeValueMemberBOOL)
	return ok && v.Value
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

func metaItem(se session.Session) map[string]types.AttributeValue {
	item := pkey(sessionPK(se.Tenant, se.ID), metaSK)
	item["id"] = s(se.ID)
	item["agent"] = s(se.Agent)
	item["kind"] = s(string(se.Kind))
	item["started"] = nanos(se.Started)
	item["updated"] = nanos(se.Updated)
	item["expires"] = nanos(se.Expires)
	item["runs"] = &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{}}
	if !se.Expires.IsZero() {
		item["ttl"] = ttl(se.Expires)
	}
	return item
}

// meta is a session item, read.
type meta struct {
	session.Session
	runs     map[string]types.AttributeValue
	deleting bool
}

func metaOf(tenant string, item map[string]types.AttributeValue) meta {
	m := meta{Session: session.Session{
		ID: getS(item, "id"), Tenant: tenant,
		Agent:   getS(item, "agent"),
		Kind:    session.Kind(getS(item, "kind")),
		Started: getTime(item, "started"),
		Updated: getTime(item, "updated"),
		Expires: getTime(item, "expires"),
	}, deleting: getBool(item, "deleting")}
	if v, ok := item["held"].(*types.AttributeValueMemberSS); ok {
		m.Held = slices.Sorted(slices.Values(v.Value))
	}
	if v, ok := item["runs"].(*types.AttributeValueMemberM); ok {
		m.runs = v.Value
	}
	return m
}

// run is what the session item says of one run: open, with its last Seq, or
// not.
func (m meta) run(id string) session.RunState {
	state := session.RunState{Held: slices.Contains(m.Held, id)}
	if v, ok := m.runs[id].(*types.AttributeValueMemberM); ok {
		state.Seq, state.Open = getN(v.Value, "seq"), true
	}
	return state
}

// created is when an open run was made.
func (m meta) created(id string) time.Time {
	if v, ok := m.runs[id].(*types.AttributeValueMemberM); ok {
		return getTime(v.Value, "created")
	}
	return time.Time{}
}

func eventItem(tenant, id, run string, e session.Event) map[string]types.AttributeValue {
	item := pkey(sessionPK(tenant, id), eventSK(run, e.Seq))
	item["run"] = s(run)
	item["seq"] = n(e.Seq)
	item["time"] = nanos(e.Time)
	item["kind"] = s(string(e.Kind))
	item["body"] = &types.AttributeValueMemberB{Value: e.Body} // the SDK copies it into the request
	item["expires"] = nanos(e.Expires)
	switch {
	case e.UntilEnd:
		item["until_end"] = yes
		item["ttl"] = ttl(e.Time.Add(UntilEndTTL))
	case !e.Expires.IsZero():
		item["ttl"] = ttl(e.Expires)
	}
	return item
}

func eventOf(id string, item map[string]types.AttributeValue) session.Event {
	e := session.Event{
		Session: id, Run: getS(item, "run"), Seq: getN(item, "seq"),
		Time:     getTime(item, "time"),
		Kind:     session.EventKind(getS(item, "kind")),
		Expires:  getTime(item, "expires"),
		UntilEnd: getBool(item, "until_end"),
	}
	if b, ok := item["body"].(*types.AttributeValueMemberB); ok {
		e.Body = b.Value // a fresh slice from the response: the caller's own
	}
	return e
}

func (st *Store) Create(ctx context.Context, se session.Session) error {
	se.Held = nil
	index := pkey(indexPK(se.Tenant, se.Agent), se.ID)
	if !se.Expires.IsZero() {
		index["ttl"] = ttl(se.Expires)
	}
	_, err := st.db.TransactWriteItems(ctx, &ddb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Put: &types.Put{TableName: &st.table, Item: metaItem(se), ConditionExpression: aws.String("attribute_not_exists(pk)")}},
		{Put: &types.Put{TableName: &st.table, Item: index}},
	}})
	return err
}

// meta reads the tenant's live session. A session being deleted, or expired,
// is not found.
func (st *Store) meta(ctx context.Context, tenant, id string) (meta, error) {
	m, err := st.metaAny(ctx, tenant, id)
	if err == nil && (m.deleting || m.Expired(st.now())) {
		return meta{}, session.ErrNotFound
	}
	return m, err
}

// metaAny is meta, including a session being deleted or expired.
func (st *Store) metaAny(ctx context.Context, tenant, id string) (meta, error) {
	out, err := st.db.GetItem(ctx, &ddb.GetItemInput{TableName: &st.table, Key: pkey(sessionPK(tenant, id), metaSK), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return meta{}, err
	}
	if out.Item == nil {
		return meta{}, session.ErrNotFound
	}
	return metaOf(tenant, out.Item), nil
}

// state reads the tenant's live session and what it knows of one run: open,
// from the session item, or ended, from the run's ended item. One request.
func (st *Store) state(ctx context.Context, tenant, id, run string) (meta, session.RunState, error) {
	items, err := st.batchGet(ctx, []map[string]types.AttributeValue{pkey(sessionPK(tenant, id), metaSK), pkey(sessionPK(tenant, id), endedSK(run))})
	if err != nil {
		return meta{}, session.RunState{}, err
	}
	var m meta
	var ended map[string]types.AttributeValue
	for _, item := range items {
		if getS(item, "sk") == metaSK {
			m = metaOf(tenant, item)
		} else {
			ended = item
		}
	}
	if m.ID == "" || m.deleting || m.Expired(st.now()) {
		return meta{}, session.RunState{}, session.ErrNotFound
	}
	if ended != nil {
		return m, session.RunState{Seq: getN(ended, "seq"), Ended: true}, nil
	}
	return m, m.run(run), nil
}

// notEnded is the check, in a transaction, that run has no ended item.
func (st *Store) notEnded(tenant, id, run string) types.TransactWriteItem {
	return types.TransactWriteItem{ConditionCheck: &types.ConditionCheck{
		TableName: &st.table, Key: pkey(sessionPK(tenant, id), endedSK(run)),
		ConditionExpression: aws.String("attribute_not_exists(pk)"),
	}}
}

// change is one conditional update of a session item, built up in parts.
type change struct {
	set, remove, add, del []string
	cond                  []string
	names                 map[string]string
	values                map[string]types.AttributeValue
}

func newChange() *change {
	return &change{
		cond:   []string{"attribute_exists(pk)", "attribute_not_exists(deleting)"},
		names:  map[string]string{},
		values: map[string]types.AttributeValue{},
	}
}

func (c *change) update(table string, k map[string]types.AttributeValue) *types.Update {
	var expr []string
	for _, part := range []struct {
		verb  string
		items []string
	}{{"SET", c.set}, {"REMOVE", c.remove}, {"ADD", c.add}, {"DELETE", c.del}} {
		if len(part.items) > 0 {
			expr = append(expr, part.verb+" "+strings.Join(part.items, ", "))
		}
	}
	u := &types.Update{
		TableName: &table, Key: k,
		UpdateExpression:    aws.String(strings.Join(expr, " ")),
		ConditionExpression: aws.String(strings.Join(c.cond, " AND ")),
	}
	if len(c.names) > 0 {
		u.ExpressionAttributeNames = c.names
	}
	if len(c.values) > 0 {
		u.ExpressionAttributeValues = c.values
	}
	return u
}

// expiry adds to c the session's move from m.Expires to expires, with
// heldAfter the runs it holds once c applies: the session carries a TTL only
// when it holds none. Any change is conditional on m's Expires and held runs
// being current, so a TTL is never set on a session that holds a run, or left
// off one that doesn't. It returns the matching change to the session's
// index entry, if any.
func (c *change) expiry(table string, m meta, expires time.Time, heldAfter int) *types.Update {
	ttlBefore := len(m.Held) == 0 && !m.Expires.IsZero()
	ttlAfter := heldAfter == 0 && !expires.IsZero()
	if expires.Equal(m.Expires) && ttlBefore == ttlAfter {
		return nil
	}
	c.values[":expires"], c.values[":read_expires"] = nanos(expires), nanos(m.Expires)
	c.set = append(c.set, "expires = :expires")
	c.cond = append(c.cond, "expires = :read_expires")
	if len(m.Held) == 0 {
		c.cond = append(c.cond, "attribute_not_exists(held)")
	} else {
		c.values[":held_n"] = n(int64(len(m.Held)))
		c.cond = append(c.cond, "size(held) = :held_n")
	}
	if !ttlAfter {
		return nil // Hold removes a TTL; nothing here sets one
	}
	c.names["#ttl"] = "ttl"
	c.values[":ttl"] = ttl(expires)
	c.set = append(c.set, "#ttl = :ttl")
	return &types.Update{
		TableName: &table, Key: pkey(indexPK(m.Tenant, m.Agent), m.ID),
		UpdateExpression:          aws.String("SET #ttl = :ttl"),
		ConditionExpression:       aws.String("attribute_exists(pk)"),
		ExpressionAttributeNames:  map[string]string{"#ttl": "ttl"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":ttl": ttl(expires)},
	}
}

// runCond adds to c that run is still open, with state's last Seq. Ending
// the run takes its entry out of the map in the same write that stores its
// ended item, so this also says it hasn't ended.
func (c *change) runCond(run string, state session.RunState) {
	c.names["#run"] = run
	c.values[":seq"] = n(state.Seq)
	c.cond = append(c.cond, "runs.#run.seq = :seq")
}

// errRace means a conditional write lost to another: read again and retry.
var errRace = errors.New("dynamodb: lost a race")

// retry runs fn until it doesn't lose a race, a few times at most.
func retry(ctx context.Context, fn func() error) error {
	for try := 0; ; try++ {
		if try > 0 {
			if err := backoff(ctx, try); err != nil {
				return err
			}
		}
		err := fn()
		if !errors.Is(err, errRace) || try == 5 {
			return err
		}
	}
}

func (st *Store) Begin(ctx context.Context, tenant, id, run string, created time.Time) error {
	m, state, err := st.state(ctx, tenant, id, run)
	switch {
	case err != nil:
		return err
	case state.Open:
		return nil
	case state.Ended:
		return session.ErrEnded
	case st.now().Sub(created) > session.MaxStartDelay:
		return session.ErrLate
	}
	c := newChange()
	c.names["#run"] = run
	c.values[":run"] = &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"seq": n(0), "created": nanos(created)}}
	c.cond = append(c.cond, "attribute_not_exists(runs.#run)")
	c.set = append(c.set, "runs.#run = :run")
	_, err = st.db.TransactWriteItems(ctx, &ddb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Update: c.update(st.table, pkey(sessionPK(tenant, m.ID), metaSK))},
		st.notEnded(tenant, id, run),
	}})
	var cancelled *types.TransactionCanceledException
	if errors.As(err, &cancelled) {
		_, now, rerr := st.state(ctx, tenant, id, run)
		switch {
		case rerr != nil:
			return rerr
		case now.Open:
			return nil // an earlier attempt got through
		case now.Ended:
			return session.ErrEnded
		}
	}
	return err
}

func (st *Store) Append(ctx context.Context, tenant, id, run string, events []session.Event) error {
	return retry(ctx, func() error { return st.append(ctx, tenant, id, run, events) })
}

func (st *Store) append(ctx context.Context, tenant, id, run string, events []session.Event) error {
	m, state, err := st.state(ctx, tenant, id, run)
	if err != nil {
		return err
	}
	if !state.Open {
		return session.ErrEnded
	}
	next, updated, expires := state.Seq, m.Updated, m.Expires
	var add []session.Event
	for _, e := range events {
		if e.Seq <= state.Seq {
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

	c := newChange()
	c.runCond(run, state)
	c.values[":updated"] = nanos(updated)
	c.set = append(c.set, "updated = :updated")
	c.values[":next"] = n(next)
	c.set = append(c.set, "runs.#run.seq = :next")
	index := c.expiry(st.table, m, expires, len(m.Held))
	items := []types.TransactWriteItem{{Update: c.update(st.table, pkey(sessionPK(tenant, id), metaSK))}}
	for _, e := range add {
		items = append(items, types.TransactWriteItem{Put: &types.Put{
			TableName: &st.table, Item: eventItem(tenant, id, run, e), ConditionExpression: aws.String("attribute_not_exists(pk)"),
		}})
	}
	if index != nil {
		items = append(items, types.TransactWriteItem{Update: index})
	}
	_, err = st.db.TransactWriteItems(ctx, &ddb.TransactWriteItemsInput{TransactItems: items})
	var cancelled *types.TransactionCanceledException
	if !errors.As(err, &cancelled) {
		return err
	}
	// Someone got there first: the session went, the run ended, an earlier
	// attempt of this very append landed, or another run wrote the session
	// meanwhile. Say which.
	_, now, rerr := st.state(ctx, tenant, id, run)
	switch {
	case rerr != nil:
		return rerr
	case !now.Open:
		return session.ErrEnded
	case now.Seq >= next:
		return nil
	}
	return errRace
}

// partition reads every item in a partition whose sort key starts with
// prefix, in sort key order.
func (st *Store) partition(ctx context.Context, pk, prefix string, filter func(map[string]types.AttributeValue) bool) ([]map[string]types.AttributeValue, error) {
	var out []map[string]types.AttributeValue
	in := &ddb.QueryInput{
		TableName:                 &st.table,
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(pk)},
		ConsistentRead:            aws.Bool(true),
	}
	if prefix != "" {
		in.KeyConditionExpression = aws.String("pk = :pk AND begins_with(sk, :prefix)")
		in.ExpressionAttributeValues[":prefix"] = s(prefix)
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

// events returns the unexpired events among items, in SortEvents order.
func (st *Store) events(id string, items []map[string]types.AttributeValue) []session.Event {
	now := st.now()
	var out []session.Event
	for _, item := range items {
		if e := eventOf(id, item); !expired(e.Expires, now) {
			out = append(out, e)
		}
	}
	session.SortEvents(out)
	return out
}

func (st *Store) Read(ctx context.Context, tenant, id string) (session.Session, []session.Event, error) {
	items, err := st.partition(ctx, sessionPK(tenant, id), "", nil)
	if err != nil {
		return session.Session{}, nil, err
	}
	var m meta
	found := false
	events := items[:0:0]
	for _, item := range items {
		switch sk := getS(item, "sk"); {
		case sk == metaSK:
			m, found = metaOf(tenant, item), true
		case strings.HasPrefix(sk, "E|"):
			events = append(events, item)
		}
	}
	if !found || m.deleting || m.Expired(st.now()) {
		return session.Session{}, nil, session.ErrNotFound
	}
	return m.Session, st.events(id, events), nil
}

func (st *Store) ReadRun(ctx context.Context, tenant, id, run string) (session.RunState, []session.Event, error) {
	_, state, err := st.state(ctx, tenant, id, run)
	if err != nil {
		return session.RunState{}, nil, err
	}
	items, err := st.partition(ctx, sessionPK(tenant, id), eventPrefix(run), nil)
	if err != nil {
		return session.RunState{}, nil, err
	}
	return state, st.events(id, items), nil
}

func (st *Store) State(ctx context.Context, tenant, id, run string) (session.RunState, error) {
	_, state, err := st.state(ctx, tenant, id, run)
	return state, err
}

func (st *Store) List(ctx context.Context, tenant, agent string, limit int) ([]session.Session, error) {
	index, err := st.partition(ctx, indexPK(tenant, agent), "", nil)
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
		if m := metaOf(tenant, item); !m.deleting && !m.Expired(now) {
			out = append(out, m.Session)
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

func (st *Store) Hold(ctx context.Context, tenant, id, run string) error {
	m, state, err := st.state(ctx, tenant, id, run)
	if err != nil {
		return err
	}
	if !state.Open {
		return session.ErrEnded
	}
	c := newChange()
	c.names["#ttl"], c.names["#run"] = "ttl", run
	c.cond = append(c.cond, "attribute_exists(runs.#run)")
	c.values[":held"] = &types.AttributeValueMemberSS{Value: []string{run}}
	c.add = append(c.add, "held :held")
	c.remove = append(c.remove, "#ttl")
	_, err = st.db.TransactWriteItems(ctx, &ddb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Update: c.update(st.table, pkey(sessionPK(tenant, id), metaSK))},
		{Update: &types.Update{
			TableName: &st.table, Key: pkey(indexPK(tenant, m.Agent), id),
			UpdateExpression:         aws.String("REMOVE #ttl"),
			ConditionExpression:      aws.String("attribute_exists(pk)"),
			ExpressionAttributeNames: map[string]string{"#ttl": "ttl"},
		}},
	}})
	var cancelled *types.TransactionCanceledException
	if errors.As(err, &cancelled) {
		_, now, rerr := st.state(ctx, tenant, id, run)
		switch {
		case rerr != nil:
			return rerr
		case !now.Open:
			return session.ErrEnded
		}
	}
	return err
}

func (st *Store) EndRun(ctx context.Context, tenant, id, run string, expires time.Time) error {
	if _, state, err := st.state(ctx, tenant, id, run); err != nil {
		return err
	} else if !state.Open {
		return session.ErrEnded
	}
	// Drop what was kept only until the end first: if this fails, the run
	// is still going and EndRun is retried, so nothing is left behind.
	untilEnd, err := st.partition(ctx, sessionPK(tenant, id), eventPrefix(run), func(item map[string]types.AttributeValue) bool {
		return getBool(item, "until_end")
	})
	if err != nil {
		return err
	}
	if err := st.deleteKeys(ctx, keysOf(untilEnd)); err != nil {
		return err
	}
	return retry(ctx, func() error { return st.endRun(ctx, tenant, id, run, expires) })
}

// endRun takes the run out of the session's open runs and stores its ended
// item, releases its hold and moves the session's expiry on, in one
// conditional write.
func (st *Store) endRun(ctx context.Context, tenant, id, run string, expires time.Time) error {
	m, state, err := st.state(ctx, tenant, id, run)
	if err != nil {
		return err
	}
	if !state.Open {
		return session.ErrEnded
	}
	created := m.created(run)
	c := newChange()
	c.runCond(run, state)
	c.remove = append(c.remove, "runs.#run")
	heldAfter := len(m.Held)
	if state.Held {
		c.values[":held"] = &types.AttributeValueMemberSS{Value: []string{run}}
		c.del = append(c.del, "held :held")
		heldAfter--
	}
	if m.Expires.IsZero() || !expires.After(m.Expires) {
		expires = m.Expires // kept until deleted, or already later
	}
	if state.Held && heldAfter > 0 {
		// Other runs stay held: they set the TTL when the last of them ends.
		c.values[":held_n"] = n(int64(len(m.Held)))
		c.cond = append(c.cond, "size(held) = :held_n")
	}
	ended := pkey(sessionPK(tenant, id), endedSK(run))
	ended["seq"] = n(state.Seq)
	if !expires.IsZero() {
		// Past MaxStartDelay, Begin refuses the run by its age; until the
		// session's expiry as of now, State still says it ended.
		ended["ttl"] = ttl(later(expires, created.Add(session.MaxStartDelay)))
	}
	items := []types.TransactWriteItem{{}, {Put: &types.Put{
		TableName: &st.table, Item: ended, ConditionExpression: aws.String("attribute_not_exists(pk)"),
	}}}
	if index := c.expiry(st.table, m, expires, heldAfter); index != nil {
		items = append(items, types.TransactWriteItem{Update: index})
	}
	items[0].Update = c.update(st.table, pkey(sessionPK(tenant, id), metaSK))
	_, err = st.db.TransactWriteItems(ctx, &ddb.TransactWriteItemsInput{TransactItems: items})
	var cancelled *types.TransactionCanceledException
	if !errors.As(err, &cancelled) {
		return err
	}
	_, now, rerr := st.state(ctx, tenant, id, run)
	switch {
	case rerr != nil:
		return rerr
	case !now.Open:
		return session.ErrEnded
	}
	return errRace
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (st *Store) Delete(ctx context.Context, tenant, id string) error {
	m, err := st.metaAny(ctx, tenant, id) // a session already being deleted: finish the job
	if err != nil {
		return err
	}
	if !m.deleting && m.Expired(st.now()) {
		return session.ErrNotFound
	}
	return st.deleteSession(ctx, tenant, m.Agent, id)
}

// deleteSession marks the session deleting, then deletes its events, then
// the session, then its index entry. Once marked, nothing more can be
// appended, so no event is left behind; and whichever step fails, a retry
// still finds the session, through Delete (the session item) or DeleteAgent
// (the index entry), and carries on.
func (st *Store) deleteSession(ctx context.Context, tenant, agent, id string) error {
	_, err := st.db.UpdateItem(ctx, &ddb.UpdateItemInput{
		TableName: &st.table, Key: pkey(sessionPK(tenant, id), metaSK),
		UpdateExpression:          aws.String("SET deleting = :yes"),
		ConditionExpression:       aws.String("attribute_exists(pk)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":yes": yes},
	})
	var gone *types.ConditionalCheckFailedException
	if err != nil && !errors.As(err, &gone) { // gone: an earlier attempt deleted the session item
		return err
	}
	items, err := st.partition(ctx, sessionPK(tenant, id), "", func(item map[string]types.AttributeValue) bool {
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
	index, err := st.partition(ctx, indexPK(tenant, agent), "", nil)
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
	items, err := st.partition(ctx, notesPK(tenant, agent), "", nil)
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
	items, err := st.partition(ctx, notesPK(tenant, agent), "", nil)
	if err != nil {
		return err
	}
	return st.deleteKeys(ctx, keysOf(items))
}
