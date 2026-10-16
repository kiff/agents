package dynamodb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/kiff/agents/run"
)

// Runs is a run.Runs on the same table as the session store, in partitions of
// its own:
//
//	run   pk=R|tenant|id     sk=M          the Run as JSON, but its paused turn
//	turn  pk=R|tenant|id     sk=T|n        part n of the paused turn, as JSON
//	list  pk=L|tenant|agent  sk=created|id lists an agent's runs, newest last
//	from  pk=O|tenant|trigger|source  sk=created|id  lists the runs one source
//	                                       started (a run with a Source only)
//	due   pk=W               sk=due|key    a run the sweep must look at, by when
//	hold  pk=H|tenant|exception  sk=M      the run that held that call (ByHold)
//
// The paused turn is cut into parts because what tools returned can be bigger
// than one item holds; the parts are written in the same transaction as the
// run, so a run is never read with half a turn. Only runs that are PENDING or
// RUNNING have a due entry, written and removed with the run's own change, so
// the sweep reads one small partition instead of the table; Due deletes an
// entry whose run is gone or due at another time, so the partition heals. A
// run that has ended carries a TTL on every item from its Expires.
//
// Every run writes the due partition a few times (created, claimed, ended,
// and again for each continue). One partition key takes about 1,000 writes a
// second, so this holds to some 300 run starts a second across all tenants.
// Past that, shard the key by run ID (W|0 to W|15) and have Due read each.
type Runs struct {
	db    API
	table string
	now   func() time.Time
}

// NewRuns returns a run store on table, on the system clock.
func NewRuns(db API, table string) *Runs { return NewRunsAt(db, table, time.Now) }

// NewRunsAt returns a run store whose clock decides what has expired.
func NewRunsAt(db API, table string, now func() time.Time) *Runs {
	return &Runs{db: db, table: table, now: now}
}

var _ run.Runs = (*Runs)(nil)

// turnPart is the most of a paused turn one item holds; MaxTurn, the most a
// run's paused turn may be, within what one transaction can write.
const (
	turnPart = 300 << 10
	MaxTurn  = 3 << 20
)

const duePK = "W"

func runPK(tenant, id string) string         { return key("R", tenant, id) }
func runListPK(tenant, agent string) string  { return key("L", tenant, agent) }
func holdPK(tenant, exception string) string { return key("H", tenant, exception) }

// holdItems points each of exs at r, with t. A hold item lives as long as
// its run: it gets the run's TTL when the run ends, so an alert for a run
// that has ended still finds it.
func (rs *Runs) holdItems(r run.Run, exs []string, t types.AttributeValue) []types.TransactWriteItem {
	var out []types.TransactWriteItem
	for _, ex := range exs {
		item := pkey(holdPK(r.Tenant, ex), metaSK)
		item["id"] = s(r.ID)
		out = append(out, rs.put(withTTL(item, t), ""))
	}
	return out
}

func sourcePK(tenant string, trigger run.Trigger, source string) string {
	return key("O", tenant, string(trigger), source)
}
func turnSK(n int) string        { return fmt.Sprintf("T|%04d", n) }
func runListSK(r run.Run) string { return fmt.Sprintf("%020d|%s", r.Created.UnixNano(), r.ID) }
func dueSK(r run.Run) string {
	return fmt.Sprintf("%020d|%s", r.Due().UnixNano(), key("", r.Tenant, r.ID))
}

// stored is a run as its items hold it.
type stored struct {
	head  []byte   // the run without its paused turn, as JSON
	parts [][]byte // the paused turn, as JSON, cut into parts
	ttl   types.AttributeValue
}

func storedOf(r run.Run) (stored, error) {
	turn := r.Paused
	r.Paused = nil
	head, err := json.Marshal(r)
	if err != nil {
		return stored{}, err
	}
	var st stored
	st.head = head
	if turn != nil {
		raw, err := json.Marshal(turn)
		if err != nil {
			return stored{}, err
		}
		if len(raw) > MaxTurn {
			return stored{}, fmt.Errorf("dynamodb: run %s: its paused turn is %d bytes, over %d", r.ID, len(raw), MaxTurn)
		}
		for len(raw) > 0 {
			n := min(len(raw), turnPart)
			st.parts, raw = append(st.parts, raw[:n]), raw[n:]
		}
	}
	if r.Status.Final() && !r.Expires.IsZero() {
		st.ttl = ttl(r.Expires)
	}
	return st, nil
}

func withTTL(item map[string]types.AttributeValue, t types.AttributeValue) map[string]types.AttributeValue {
	if t != nil {
		item["ttl"] = t
	}
	return item
}

func sameTTL(a, b types.AttributeValue) bool {
	x, _ := a.(*types.AttributeValueMemberN)
	y, _ := b.(*types.AttributeValueMemberN)
	return (x == nil && y == nil) || (x != nil && y != nil && x.Value == y.Value)
}

func (rs *Runs) put(item map[string]types.AttributeValue, cond string) types.TransactWriteItem {
	p := &types.Put{TableName: &rs.table, Item: item}
	if cond != "" {
		p.ConditionExpression = aws.String(cond)
	}
	return types.TransactWriteItem{Put: p}
}

func (rs *Runs) del(k map[string]types.AttributeValue) types.TransactWriteItem {
	return types.TransactWriteItem{Delete: &types.Delete{TableName: &rs.table, Key: k}}
}

func (rs *Runs) runItem(r run.Run, st stored) map[string]types.AttributeValue {
	item := pkey(runPK(r.Tenant, r.ID), metaSK)
	item["run"] = &types.AttributeValueMemberB{Value: st.head}
	item["version"] = n(r.Version)
	item["parts"] = n(int64(len(st.parts)))
	item["who"] = s(who(r))
	return withTTL(item, st.ttl)
}

// who is what a run is, past its key (tenant and ID): the rest of what
// run.Same compares. Put's condition checks it against the stored run, so a
// prev that wasn't read from the store can't move a run between lists.
func who(r run.Run) string {
	return key("", r.Agent, r.Session, string(r.Trigger), r.Source, fmt.Sprint(r.Created.UnixNano()))
}

func (rs *Runs) partItem(r run.Run, i int, part []byte, t types.AttributeValue) map[string]types.AttributeValue {
	item := pkey(runPK(r.Tenant, r.ID), turnSK(i))
	item["b"] = &types.AttributeValueMemberB{Value: part}
	return withTTL(item, t)
}

// lists returns the keys of the lists r is in: its agent's, and its
// source's if it has one.
func lists(r run.Run) []map[string]types.AttributeValue {
	out := []map[string]types.AttributeValue{pkey(runListPK(r.Tenant, r.Agent), runListSK(r))}
	if r.Source != "" {
		out = append(out, pkey(sourcePK(r.Tenant, r.Trigger, r.Source), runListSK(r)))
	}
	return out
}

// listItems puts r in its lists.
func (rs *Runs) listItems(r run.Run, t types.AttributeValue) []types.TransactWriteItem {
	var out []types.TransactWriteItem
	for _, item := range lists(r) {
		item["id"] = s(r.ID)
		out = append(out, rs.put(withTTL(item, t), ""))
	}
	return out
}

func (rs *Runs) dueItem(r run.Run) map[string]types.AttributeValue {
	item := pkey(duePK, dueSK(r))
	item["tenant"], item["id"] = s(r.Tenant), s(r.ID)
	return item
}

func (rs *Runs) Create(ctx context.Context, r run.Run) error {
	st, err := storedOf(r)
	if err != nil {
		return err
	}
	items := append([]types.TransactWriteItem{rs.put(rs.runItem(r, st), "attribute_not_exists(pk)")}, rs.listItems(r, st.ttl)...)
	items = append(items, rs.holdItems(r, run.HoldsOf(r), st.ttl)...)
	for i, part := range st.parts {
		items = append(items, rs.put(rs.partItem(r, i, part, st.ttl), ""))
	}
	if !r.Due().IsZero() {
		items = append(items, rs.put(rs.dueItem(r), ""))
	}
	_, err = rs.db.TransactWriteItems(ctx, &ddb.TransactWriteItemsInput{TransactItems: items})
	var cancelled *types.TransactionCanceledException
	if errors.As(err, &cancelled) && len(cancelled.CancellationReasons) > 0 && aws.ToString(cancelled.CancellationReasons[0].Code) == "ConditionalCheckFailed" {
		return run.ErrExists
	}
	return err
}

// read reads a run's items, expired or not, or ErrNotFound. A big paused
// turn spans several pages of a query, and a write can land between them, so
// a run with a turn is checked against its head read again: if the version
// moved, what was read may mix two turns, and it is read again.
func (rs *Runs) read(ctx context.Context, tenant, id string) (run.Run, error) {
	for try := 0; ; try++ {
		r, err := rs.readOnce(ctx, tenant, id)
		if err != nil && (errors.Is(err, run.ErrNotFound) || !errors.Is(err, errTorn)) {
			return r, err
		}
		if err == nil && r.Paused == nil {
			return r, nil // one item: one read, nothing to mix
		}
		if err == nil {
			out, gerr := rs.db.GetItem(ctx, &ddb.GetItemInput{TableName: &rs.table, Key: pkey(runPK(tenant, id), metaSK),
				ConsistentRead: aws.Bool(true), ProjectionExpression: aws.String("version")})
			if gerr != nil {
				return run.Run{}, gerr
			}
			if out.Item != nil && getN(out.Item, "version") == r.Version {
				return r, nil
			}
		}
		if try == 5 {
			return run.Run{}, fmt.Errorf("dynamodb: run %s kept changing while it was read", id)
		}
	}
}

// errTorn means a read saw a turn its run's head doesn't match.
var errTorn = errors.New("dynamodb: a torn read of a paused turn")

func (rs *Runs) readOnce(ctx context.Context, tenant, id string) (run.Run, error) {
	items, err := (&Store{db: rs.db, table: rs.table}).partition(ctx, runPK(tenant, id), "", nil)
	if err != nil {
		return run.Run{}, err
	}
	var head []byte
	parts := map[string][]byte{}
	count := int64(0)
	for _, item := range items {
		sk := getS(item, "sk")
		if sk == metaSK {
			if b, ok := item["run"].(*types.AttributeValueMemberB); ok {
				head, count = b.Value, getN(item, "parts")
			}
		} else if b, ok := item["b"].(*types.AttributeValueMemberB); ok {
			parts[sk] = b.Value
		}
	}
	if head == nil {
		return run.Run{}, run.ErrNotFound
	}
	var r run.Run
	if err := json.Unmarshal(head, &r); err != nil {
		return run.Run{}, err
	}
	if count > 0 {
		var turn bytes.Buffer
		for i := range int(count) {
			part, ok := parts[turnSK(i)]
			if !ok {
				return run.Run{}, fmt.Errorf("%w: run %s: part %d is missing", errTorn, id, i)
			}
			turn.Write(part)
		}
		r.Paused = new(run.Turn)
		if err := json.Unmarshal(turn.Bytes(), r.Paused); err != nil {
			return run.Run{}, fmt.Errorf("%w: run %s: %w", errTorn, id, err)
		}
	}
	return r, nil
}

func (rs *Runs) gone(r run.Run) bool {
	return r.Status.Final() && !r.Expires.IsZero() && r.Expires.Before(rs.now())
}

func (rs *Runs) Get(ctx context.Context, tenant, id string) (run.Run, error) {
	r, err := rs.read(ctx, tenant, id)
	if err == nil && rs.gone(r) {
		return run.Run{}, run.ErrNotFound
	}
	return r, err
}

func (rs *Runs) Put(ctx context.Context, prev, next run.Run) error {
	if !run.Same(prev, next) {
		return run.ErrFixed
	}
	if next.Version != prev.Version+1 {
		return run.ErrConflict
	}
	was, err := storedOf(prev)
	if err != nil {
		return err
	}
	now, err := storedOf(next)
	if err != nil {
		return err
	}
	runItem := rs.runItem(next, now)
	cond := &types.Put{
		TableName: &rs.table, Item: runItem,
		ConditionExpression: aws.String("version = :version AND parts = :parts AND who = :who"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":version": n(prev.Version), ":parts": n(int64(len(was.parts))), ":who": s(who(prev)),
		},
	}
	items := []types.TransactWriteItem{{Put: cond}}
	rewrite := !sameTTL(was.ttl, now.ttl) || len(was.parts) != len(now.parts)
	for i := 0; !rewrite && i < len(now.parts); i++ {
		rewrite = !bytes.Equal(was.parts[i], now.parts[i])
	}
	if rewrite {
		for i, part := range now.parts {
			items = append(items, rs.put(rs.partItem(next, i, part, now.ttl), ""))
		}
		for i := len(now.parts); i < len(was.parts); i++ {
			items = append(items, rs.del(pkey(runPK(next.Tenant, next.ID), turnSK(i))))
		}
	}
	if !sameTTL(was.ttl, now.ttl) {
		items = append(items, rs.listItems(next, now.ttl)...)
	}
	// Holds: new ones are written; when the TTL changes (the run ended),
	// every hold the run ever had takes it.
	holds := run.HoldsOf(prev, next)
	if sameTTL(was.ttl, now.ttl) {
		had := run.HoldsOf(prev)
		holds = slices.DeleteFunc(holds, func(ex string) bool { return slices.Contains(had, ex) })
	}
	items = append(items, rs.holdItems(next, holds, now.ttl)...)
	if !prev.Due().Equal(next.Due()) {
		if !prev.Due().IsZero() {
			items = append(items, rs.del(pkey(duePK, dueSK(prev))))
		}
		if !next.Due().IsZero() {
			items = append(items, rs.put(rs.dueItem(next), ""))
		}
	}
	_, err = rs.db.TransactWriteItems(ctx, &ddb.TransactWriteItemsInput{TransactItems: items})
	var cancelled *types.TransactionCanceledException
	if errors.As(err, &cancelled) {
		stored, gerr := rs.Get(ctx, prev.Tenant, prev.ID)
		switch {
		case errors.Is(gerr, run.ErrNotFound):
			return run.ErrNotFound
		case gerr == nil && !run.Same(stored, next):
			return run.ErrFixed
		}
		return run.ErrConflict
	}
	return err
}

// byKey reads runs by tenant and id, without their paused turns, gone or
// not; a run that isn't there is left out.
func (rs *Runs) byKey(ctx context.Context, keys [][2]string) (map[[2]string]run.Run, error) {
	ks := make([]map[string]types.AttributeValue, len(keys))
	for i, k := range keys {
		ks[i] = pkey(runPK(k[0], k[1]), metaSK)
	}
	items, err := (&Store{db: rs.db, table: rs.table}).batchGet(ctx, ks)
	if err != nil {
		return nil, err
	}
	out := map[[2]string]run.Run{}
	for _, item := range items {
		b, _ := item["run"].(*types.AttributeValueMemberB)
		if b == nil {
			continue
		}
		var r run.Run
		if err := json.Unmarshal(b.Value, &r); err != nil {
			return nil, err
		}
		out[[2]string{r.Tenant, r.ID}] = r
	}
	return out, nil
}

// runs reads runs by tenant and id, in that order, leaving out those gone.
// They come without their paused turn.
func (rs *Runs) runs(ctx context.Context, keys [][2]string) ([]run.Run, error) {
	byKey, err := rs.byKey(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := make([]run.Run, 0, len(keys))
	for _, k := range keys {
		if r, ok := byKey[k]; ok && !rs.gone(r) {
			out = append(out, r)
		}
	}
	return out, nil
}

// query pages through a partition in sort key order (or its reverse), until
// enough is done.
func (rs *Runs) query(ctx context.Context, in *ddb.QueryInput, each func([]map[string]types.AttributeValue) (enough bool, err error)) error {
	in.TableName, in.ConsistentRead = &rs.table, aws.Bool(true)
	for {
		page, err := rs.db.Query(ctx, in)
		if err != nil {
			return err
		}
		if enough, err := each(page.Items); err != nil || enough {
			return err
		}
		if page.LastEvaluatedKey == nil {
			return nil
		}
		in.ExclusiveStartKey = page.LastEvaluatedKey
	}
}

// List leaves out the runs' paused turns: Get has them.
func (rs *Runs) List(ctx context.Context, tenant, agent string, limit int) ([]run.Run, error) {
	return rs.list(ctx, tenant, runListPK(tenant, agent), limit)
}

// ListSource leaves out the runs' paused turns: Get has them.
func (rs *Runs) ListSource(ctx context.Context, tenant string, trigger run.Trigger, source string, limit int) ([]run.Run, error) {
	if source == "" {
		return nil, nil
	}
	return rs.list(ctx, tenant, sourcePK(tenant, trigger, source), limit)
}

// list reads the tenant's runs in a list partition, newest first.
func (rs *Runs) list(ctx context.Context, tenant, pk string, limit int) ([]run.Run, error) {
	var out []run.Run
	in := &ddb.QueryInput{
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(pk)},
		ScanIndexForward:          aws.Bool(false),
	}
	if limit > 0 {
		in.Limit = aws.Int32(int32(min(limit, 100)))
	}
	err := rs.query(ctx, in, func(items []map[string]types.AttributeValue) (bool, error) {
		keys := make([][2]string, len(items))
		for i, item := range items {
			keys[i] = [2]string{tenant, getS(item, "id")}
		}
		runs, err := rs.runs(ctx, keys)
		out = append(out, runs...)
		return limit > 0 && len(out) >= limit, err
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, err
}

// Due leaves out the runs' paused turns: the runs it returns have none.
func (rs *Runs) Due(ctx context.Context, now time.Time, limit int) ([]run.Run, error) {
	var out []run.Run
	in := &ddb.QueryInput{
		KeyConditionExpression:    aws.String("pk = :pk AND sk < :now"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(duePK), ":now": s(fmt.Sprintf("%020d", now.UnixNano()))},
	}
	if limit > 0 {
		in.Limit = aws.Int32(int32(min(limit, 100)))
	}
	var stale []map[string]types.AttributeValue
	err := rs.query(ctx, in, func(items []map[string]types.AttributeValue) (bool, error) {
		keys := make([][2]string, len(items))
		for i, item := range items {
			keys[i] = [2]string{getS(item, "tenant"), getS(item, "id")}
		}
		byKey, err := rs.byKey(ctx, keys)
		if err != nil {
			return false, err
		}
		for i, item := range items {
			r, ok := byKey[keys[i]]
			switch {
			case !ok || r.Due().IsZero() || dueSK(r) != getS(item, "sk"):
				// its run is gone, or due at another time: an entry left
				// behind (read after the run's own write, which is newer)
				stale = append(stale, pkey(duePK, getS(item, "sk")))
			case !rs.gone(r) && r.Due().Before(now):
				out = append(out, r)
			}
		}
		return limit > 0 && len(out) >= limit, nil
	})
	if err == nil {
		err = (&Store{db: rs.db, table: rs.table}).deleteKeys(ctx, stale)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, err
}

func (rs *Runs) Delete(ctx context.Context, tenant, id string) error {
	r, err := rs.Get(ctx, tenant, id)
	if err != nil {
		return err
	}
	return rs.remove(ctx, r)
}

// remove deletes every item of r, read again until a delete goes through as
// read: a write that lands meanwhile (a claim moving its due entry) can't
// leave items behind.
func (rs *Runs) remove(ctx context.Context, r run.Run) error {
	for try := 0; ; try++ {
		err := rs.removeAt(ctx, r)
		var cancelled *types.TransactionCanceledException
		if !errors.As(err, &cancelled) || try == 5 {
			return err
		}
		if r, err = rs.read(ctx, r.Tenant, r.ID); errors.Is(err, run.ErrNotFound) {
			return nil // removed meanwhile
		} else if err != nil {
			return err
		}
	}
}

// removeAt deletes every item of r, as read, in one transaction, if the run
// is still at r's Version.
func (rs *Runs) removeAt(ctx context.Context, r run.Run) error {
	st, err := storedOf(r)
	if err != nil {
		return err
	}
	items := []types.TransactWriteItem{
		{Delete: &types.Delete{
			TableName: &rs.table, Key: pkey(runPK(r.Tenant, r.ID), metaSK),
			ConditionExpression:       aws.String("version = :version"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":version": n(r.Version)},
		}},
	}
	for _, k := range lists(r) {
		items = append(items, rs.del(k))
	}
	for _, ex := range run.HoldsOf(r) {
		items = append(items, rs.del(pkey(holdPK(r.Tenant, ex), metaSK)))
	}
	for i := range st.parts {
		items = append(items, rs.del(pkey(runPK(r.Tenant, r.ID), turnSK(i))))
	}
	if !r.Due().IsZero() {
		items = append(items, rs.del(pkey(duePK, dueSK(r))))
	}
	_, err = rs.db.TransactWriteItems(ctx, &ddb.TransactWriteItemsInput{TransactItems: items})
	return err
}

// DeleteAgent removes every run in the agent's list, expired ones included,
// and then whatever is left of the list.
func (rs *Runs) DeleteAgent(ctx context.Context, tenant, agent string) error {
	in := &ddb.QueryInput{
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(runListPK(tenant, agent))},
	}
	var left []map[string]types.AttributeValue
	err := rs.query(ctx, in, func(items []map[string]types.AttributeValue) (bool, error) {
		for _, item := range items {
			r, err := rs.read(ctx, tenant, getS(item, "id"))
			switch {
			case errors.Is(err, run.ErrNotFound):
				left = append(left, pkey(getS(item, "pk"), getS(item, "sk")))
			case err != nil:
				return false, err
			case strings.HasPrefix(getS(item, "sk"), runListSK(r)):
				if err := rs.remove(ctx, r); err != nil {
					return false, err
				}
			}
		}
		return false, nil
	})
	if err != nil {
		return err
	}
	return (&Store{db: rs.db, table: rs.table}).deleteKeys(ctx, left)
}

func (rs *Runs) ByHold(ctx context.Context, tenant, exception string) (run.Run, error) {
	out, err := rs.db.GetItem(ctx, &ddb.GetItemInput{TableName: &rs.table, Key: pkey(holdPK(tenant, exception), metaSK),
		ConsistentRead: aws.Bool(true)})
	if err != nil {
		return run.Run{}, err
	}
	if out.Item == nil {
		return run.Run{}, run.ErrNotFound
	}
	return rs.Get(ctx, tenant, getS(out.Item, "id"))
}
