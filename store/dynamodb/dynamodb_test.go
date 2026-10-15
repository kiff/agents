package dynamodb_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/kiff/agents/session"
	"github.com/kiff/agents/session/sessiontest"
	"github.com/kiff/agents/store/dynamodb"
)

var ctx = context.Background()

// local returns a client for DynamoDB Local at DYNAMODB_ENDPOINT, or skips.
// It refuses any endpoint that isn't on this machine: tests never create
// tables, or pay for data, on AWS.
func local(t *testing.T) *ddb.Client {
	t.Helper()
	endpoint := os.Getenv("DYNAMODB_ENDPOINT")
	if endpoint == "" {
		t.Skip("DYNAMODB_ENDPOINT not set; CI runs these against DynamoDB Local")
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") {
		t.Fatalf("DYNAMODB_ENDPOINT %q is not local: these tests only run against DynamoDB Local", endpoint)
	}
	return ddb.New(ddb.Options{
		Region:       "eu-west-1",
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider("local", "local", ""),
	})
}

// table makes a table for one test and deletes it when the test ends.
func table(t *testing.T, db *ddb.Client) string {
	t.Helper()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "sessions-test-" + hex.EncodeToString(b)
	if err := dynamodb.CreateTable(ctx, db, name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.DeleteTable(context.Background(), &ddb.DeleteTableInput{TableName: &name}); err != nil {
			t.Errorf("deleting test table %s: %v", name, err)
		}
	})
	return name
}

func TestStore(t *testing.T) {
	db := local(t)
	sessiontest.Run(t, func(t *testing.T, now func() time.Time) sessiontest.Backend {
		return dynamodb.NewAt(db, table(t, db), now)
	})
}

// The table is on-demand with TTL on, and nothing else that costs.
func TestTableIsFrugal(t *testing.T) {
	db := local(t)
	name := table(t, db)
	d, err := db.DescribeTable(ctx, &ddb.DescribeTableInput{TableName: &name})
	if err != nil {
		t.Fatal(err)
	}
	if bm := d.Table.BillingModeSummary; bm == nil || bm.BillingMode != types.BillingModePayPerRequest {
		t.Fatalf("billing: %+v, want on-demand", bm)
	}
	if len(d.Table.GlobalSecondaryIndexes) > 0 || len(d.Table.LocalSecondaryIndexes) > 0 {
		t.Fatal("the table has secondary indexes")
	}
	ttl, err := db.DescribeTimeToLive(ctx, &ddb.DescribeTimeToLiveInput{TableName: &name})
	if err != nil {
		t.Fatal(err)
	}
	if d := ttl.TimeToLiveDescription; d == nil || aws.ToString(d.AttributeName) != "ttl" ||
		(d.TimeToLiveStatus != types.TimeToLiveStatusEnabled && d.TimeToLiveStatus != types.TimeToLiveStatusEnabling) {
		t.Fatalf("TTL: %+v, want enabled on ttl", d)
	}
}

// Every item whose data expires carries a TTL, so DynamoDB deletes it for
// free; an event kept until the end gets one too, in case its run never ends.
func TestItemsCarryTTL(t *testing.T) {
	db := local(t)
	name := table(t, db)
	at := now() // TTLs in the past would be deleted by DynamoDB Local itself
	clock := func() time.Time { return at }
	st := dynamodb.NewAt(db, name, clock)
	r, err := session.NewRecorderAt(st, session.DefaultPolicy(30*24*time.Hour), clock)
	if err != nil {
		t.Fatal(err)
	}
	se, err := r.Open(ctx, "t1", "refunds", session.KindRun)
	if err != nil {
		t.Fatal(err)
	}
	rec := started(t, r, se, "r1")
	_ = rec.Record(session.Report, "kept")
	_ = rec.Record(session.ToolResult, "until the end")
	if err := rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.PutNote(ctx, session.Note{Tenant: "t1", Agent: "refunds", Name: "n", Text: "x"}); err != nil {
		t.Fatal(err)
	}

	out, err := db.Scan(ctx, &ddb.ScanInput{TableName: &name})
	if err != nil {
		t.Fatal(err)
	}
	month := at.Add(30 * 24 * time.Hour).Unix()
	week := at.Add(dynamodb.UntilEndTTL).Unix()
	seen := 0
	for _, item := range out.Items {
		pk := item["pk"].(*types.AttributeValueMemberS).Value
		sk := item["sk"].(*types.AttributeValueMemberS).Value
		got, has := item["ttl"].(*types.AttributeValueMemberN)
		want := month
		switch {
		case pk[0] == 'N':
			if has {
				t.Fatalf("a note has a TTL: memory lives until it is forgotten")
			}
			continue
		case sk == "E|2:r1|00000000000000000002":
			want = week
		}
		if !has || got.Value != itoa(want) {
			t.Fatalf("item %s %s: ttl %v, want %d", pk, sk, item["ttl"], want)
		}
		seen++
	}
	if seen != 4 { // session, index, two events
		t.Fatalf("checked %d items with a TTL, want 4", seen)
	}
	if err := rec.End(ctx); err != nil {
		t.Fatal(err)
	}
}

// ttls returns the TTL of the session's item and of its index entry, or -1
// for none.
func ttls(t *testing.T, db *ddb.Client, table string) (session, index int64) {
	t.Helper()
	out, err := db.Scan(ctx, &ddb.ScanInput{TableName: &table})
	if err != nil {
		t.Fatal(err)
	}
	session, index = -1, -1
	for _, item := range out.Items {
		pk := item["pk"].(*types.AttributeValueMemberS).Value
		sk := item["sk"].(*types.AttributeValueMemberS).Value
		v := int64(-1)
		if n, ok := item["ttl"].(*types.AttributeValueMemberN); ok {
			v, _ = strconv.ParseInt(n.Value, 10, 64)
		}
		switch {
		case pk[0] == 'S' && sk == "M":
			session = v
		case pk[0] == 'A':
			index = v
		}
	}
	return session, index
}

// While a run is held, neither the session nor its index entry carries a
// TTL, so DynamoDB can't delete them; once the last held run ends, both get
// one again, from then.
func TestAHeldSessionCarriesNoTTL(t *testing.T) {
	db := local(t)
	name := table(t, db)
	at := now() // TTLs in the past would be deleted by DynamoDB Local itself
	clock := func() time.Time { return at }
	st := dynamodb.NewAt(db, name, clock)
	r, err := session.NewRecorderAt(st, session.DefaultPolicy(24*time.Hour), clock)
	if err != nil {
		t.Fatal(err)
	}
	se, err := r.Open(ctx, "t1", "refunds", session.KindChat)
	if err != nil {
		t.Fatal(err)
	}
	first, second := started(t, r, se, "r1"), started(t, r, se, "r2")
	for _, rec := range []*session.Recording{first, second} {
		_ = rec.Record(session.OwnerMessage, "refund order 12")
		if err := rec.Pause(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if s, i := ttls(t, db, name); s != -1 || i != -1 {
		t.Fatalf("held: session ttl %d, index ttl %d, want none", s, i)
	}
	at = at.Add(3 * 24 * time.Hour)
	more, err := r.Resume(ctx, se, "r1")
	if err != nil {
		t.Fatal(err)
	}
	_ = more.Record(session.AgentMessage, "refunded") // moves Expires on while r2 is held
	if err := more.End(ctx); err != nil {
		t.Fatal(err)
	}
	if s, i := ttls(t, db, name); s != -1 || i != -1 {
		t.Fatalf("one run still held: session ttl %d, index ttl %d, want none", s, i)
	}
	at = at.Add(time.Hour)
	last, err := r.Resume(ctx, se, "r2")
	if err != nil {
		t.Fatal(err)
	}
	if err := last.End(ctx); err != nil {
		t.Fatal(err)
	}
	want := at.Add(24 * time.Hour).Unix()
	if s, i := ttls(t, db, name); s != want || i != want {
		t.Fatalf("none held: session ttl %d, index ttl %d, want %d", s, i, want)
	}
}

// The session item holds only the runs open in it: however long a thread
// grows, writing to it costs the same. Ended runs leave an item each instead.
func TestTheSessionItemHoldsOnlyOpenRuns(t *testing.T) {
	db := local(t)
	name := table(t, db)
	st := dynamodb.New(db, name)
	r, err := session.NewRecorder(st, session.DefaultPolicy(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	se, err := r.Open(ctx, "t1", "refunds", session.KindChat)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		rec := started(t, r, se, "r"+itoa(int64(i)))
		_ = rec.Record(session.OwnerMessage, "message")
		if err := rec.End(ctx); err != nil {
			t.Fatal(err)
		}
	}
	open := started(t, r, se, "open")
	_ = open.Record(session.OwnerMessage, "message")
	if err := open.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	out, err := db.Scan(ctx, &ddb.ScanInput{TableName: &name})
	if err != nil {
		t.Fatal(err)
	}
	ended := 0
	for _, item := range out.Items {
		sk := item["sk"].(*types.AttributeValueMemberS).Value
		switch {
		case sk == "M":
			runs := item["runs"].(*types.AttributeValueMemberM).Value
			if len(runs) != 1 || runs["open"] == nil {
				t.Fatalf("the session item holds %d runs, want only the open one", len(runs))
			}
		case strings.HasPrefix(sk, "R|"):
			ended++
		}
	}
	if ended != 20 {
		t.Fatalf("%d ended items, want 20", ended)
	}
	if err := open.End(ctx); err != nil {
		t.Fatal(err)
	}
}

// racing runs before, once, the first transaction that writes to sessions:
// another writer getting in between a store's read and its write.
type racing struct {
	dynamodb.API
	once   sync.Once
	before func()
}

func (r *racing) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, opts ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	r.once.Do(r.before)
	return r.API.TransactWriteItems(ctx, in, opts...)
}

// A run that ends between an Append's or a Hold's read and its write takes
// nothing: the write itself checks the run is still open.
func TestARunEndedMidWriteTakesNothing(t *testing.T) {
	db := local(t)
	name := table(t, db)
	plain := dynamodb.New(db, name)
	se := session.Session{ID: "s1", Tenant: "t1", Agent: "refunds", Kind: session.KindRun}
	if err := plain.Create(ctx, se); err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{"r1", "r2"} {
		if err := plain.Begin(ctx, "t1", "s1", run, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	end := func(run string) func() {
		return func() {
			if err := plain.EndRun(ctx, "t1", "s1", run, time.Time{}); err != nil {
				t.Errorf("ending %s meanwhile: %v", run, err)
			}
		}
	}
	appendRace := dynamodb.New(&racing{API: db, before: end("r1")}, name)
	err := appendRace.Append(ctx, "t1", "s1", "r1", []session.Event{{Seq: 1, Kind: session.Report, Body: []byte(`1`)}})
	if !errors.Is(err, session.ErrEnded) {
		t.Fatalf("append racing the end: err = %v, want ErrEnded", err)
	}
	if _, events, _ := plain.Read(ctx, "t1", "s1"); len(events) != 0 {
		t.Fatalf("%d events landed after the run ended", len(events))
	}
	holdRace := dynamodb.New(&racing{API: db, before: end("r2")}, name)
	if err := holdRace.Hold(ctx, "t1", "s1", "r2"); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("hold racing the end: err = %v, want ErrEnded", err)
	}
	if s, _, _ := plain.Read(ctx, "t1", "s1"); len(s.Held) != 0 {
		t.Fatalf("an ended run is held: %v", s.Held)
	}
}

// started starts recording run in se, as a run created when se opened.
func started(t *testing.T, r *session.Recorder, se session.Session, run string) *session.Recording {
	t.Helper()
	rec, err := r.Start(ctx, se, run, se.Started)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// An ended run's ended item may go by TTL while its session lives on (another
// run holds it, or the thread is busy). The run still takes nothing: only an
// open run takes events or a hold, and that is on the session item. Nor can it
// begin again: it was created more than MaxStartDelay ago by then.
func TestAnEndedRunStaysEndedOnceItsEndedItemExpires(t *testing.T) {
	db := local(t)
	name := table(t, db)
	at := now() // TTLs in the past would be deleted by DynamoDB Local itself
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return at }
	st := dynamodb.NewAt(db, name, clock)
	r, err := session.NewRecorderAt(st, session.DefaultPolicy(time.Hour), clock)
	if err != nil {
		t.Fatal(err)
	}
	se, err := r.Open(ctx, "t1", "refunds", session.KindChat)
	if err != nil {
		t.Fatal(err)
	}
	ended, held := started(t, r, se, "r1"), started(t, r, se, "r2")
	_ = ended.Record(session.OwnerMessage, "1")
	if err := ended.End(ctx); err != nil {
		t.Fatal(err)
	}
	_ = held.Record(session.OwnerMessage, "2")
	if err := held.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	at = at.Add(2 * time.Hour) // r1's ended item is past its TTL; r2 keeps the session
	mu.Unlock()
	// DynamoDB Local doesn't expire items: delete it, as TTL would.
	if _, err := db.DeleteItem(ctx, &ddb.DeleteItemInput{TableName: &name, Key: map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: "S|2:t1|" + itoa(int64(len(se.ID))) + ":" + se.ID},
		"sk": &types.AttributeValueMemberS{Value: "R|2:r1"},
	}, ConditionExpression: aws.String("attribute_exists(pk)")}); err != nil {
		t.Fatalf("deleting r1's ended item: %v", err)
	}
	if state, err := st.State(ctx, "t1", se.ID, "r1"); err != nil || state.Open {
		t.Fatalf("r1's state = %+v, err = %v: it isn't open", state, err)
	}
	if err := st.Hold(ctx, "t1", se.ID, "r1"); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("hold of the ended run: err = %v, want ErrEnded", err)
	}
	if err := st.Append(ctx, "t1", se.ID, "r1", []session.Event{{Seq: 2, Time: clock(), Kind: session.Report, Body: []byte(`"late"`)}}); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("append to the ended run: err = %v, want ErrEnded", err)
	}
	if err := st.EndRun(ctx, "t1", se.ID, "r1", time.Time{}); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("ending the ended run: err = %v, want ErrEnded", err)
	}
	if err := st.Begin(ctx, "t1", se.ID, "r1", se.Started); !errors.Is(err, session.ErrLate) {
		t.Fatalf("beginning the ended run again: err = %v, want ErrLate", err)
	}
	if _, events, err := st.ReadRun(ctx, "t1", se.ID, "r1"); err != nil || len(events) != 0 {
		t.Fatalf("r1's events: %d, err = %v", len(events), err)
	}
}

// now is the real time to the second: a test's clock starts here, so no TTL
// it writes is already past.
func now() time.Time { return time.Now().UTC().Truncate(time.Second) }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// faulty fails chosen deletes once, to show a retry finishes the job.
type faulty struct {
	dynamodb.API
	mu            sync.Mutex
	failMetaOnce  bool   // the DeleteItem of a session item
	failBatchOnce string // a BatchWriteItem deleting a key with this prefix ("E|" events, "A|" index)
}

var errInjected = errors.New("injected failure")

func (f *faulty) take(flag *bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	was := *flag
	*flag = false
	return was
}

func (f *faulty) DeleteItem(ctx context.Context, in *ddb.DeleteItemInput, opts ...func(*ddb.Options)) (*ddb.DeleteItemOutput, error) {
	if sk, _ := in.Key["sk"].(*types.AttributeValueMemberS); sk != nil && sk.Value == "M" && f.take(&f.failMetaOnce) {
		return nil, errInjected
	}
	return f.API.DeleteItem(ctx, in, opts...)
}

func (f *faulty) BatchWriteItem(ctx context.Context, in *ddb.BatchWriteItemInput, opts ...func(*ddb.Options)) (*ddb.BatchWriteItemOutput, error) {
	f.mu.Lock()
	prefix := f.failBatchOnce
	f.mu.Unlock()
	if prefix != "" {
		for _, writes := range in.RequestItems {
			for _, w := range writes {
				pk := w.DeleteRequest.Key["pk"].(*types.AttributeValueMemberS).Value
				sk := w.DeleteRequest.Key["sk"].(*types.AttributeValueMemberS).Value
				if strings.HasPrefix(sk, prefix) || strings.HasPrefix(pk, prefix) {
					f.mu.Lock()
					f.failBatchOnce = ""
					f.mu.Unlock()
					return nil, errInjected
				}
			}
		}
	}
	return f.API.BatchWriteItem(ctx, in, opts...)
}

// sessionItems counts what's left of sessions in the table: session items,
// events and index entries (notes aside).
func sessionItems(t *testing.T, db *ddb.Client, table string) int {
	t.Helper()
	out, err := db.Scan(ctx, &ddb.ScanInput{TableName: &table})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, item := range out.Items {
		if pk := item["pk"].(*types.AttributeValueMemberS).Value; pk[0] == 'S' || pk[0] == 'A' {
			n++
		}
	}
	return n
}

// recorded starts a session with two events on a store over api.
func recorded(t *testing.T, api dynamodb.API, table string) session.Session {
	t.Helper()
	r, err := session.NewRecorder(dynamodb.New(api, table), session.DefaultPolicy(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	se, err := r.Open(ctx, "t1", "refunds", session.KindRun)
	if err != nil {
		t.Fatal(err)
	}
	rec := started(t, r, se, "r1")
	_ = rec.Record(session.Report, "a")
	_ = rec.Record(session.Report, "b")
	if err := rec.End(ctx); err != nil {
		t.Fatal(err)
	}
	return rec.Session()
}

// If deleting the session item fails, DeleteAgent still finds the session
// through its index entry on the retry, and leaves nothing.
func TestDeleteAgentRetryAfterSessionItemFails(t *testing.T) {
	db := local(t)
	name := table(t, db)
	f := &faulty{API: db, failMetaOnce: true}
	recorded(t, f, name)
	st := dynamodb.New(f, name)
	if err := st.DeleteAgent(ctx, "t1", "refunds"); !errors.Is(err, errInjected) {
		t.Fatalf("first DeleteAgent: err = %v, want the injected failure", err)
	}
	if err := st.DeleteAgent(ctx, "t1", "refunds"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if n := sessionItems(t, db, name); n != 0 {
		t.Fatalf("%d session items left after the retry", n)
	}
}

// If deleting the index entry fails, the session is already gone from Read,
// and DeleteAgent still removes the entry on the retry.
func TestDeleteAgentRetryAfterIndexFails(t *testing.T) {
	db := local(t)
	name := table(t, db)
	f := &faulty{API: db, failBatchOnce: "A|"}
	se := recorded(t, f, name)
	st := dynamodb.New(f, name)
	if err := st.Delete(ctx, "t1", se.ID); !errors.Is(err, errInjected) {
		t.Fatalf("first Delete: err = %v, want the injected failure", err)
	}
	if _, _, err := st.Read(ctx, "t1", se.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("read after a partial delete: err = %v", err)
	}
	if err := st.DeleteAgent(ctx, "t1", "refunds"); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	if n := sessionItems(t, db, name); n != 0 {
		t.Fatalf("%d session items left", n)
	}
}

// Once a delete has started, nothing more can be appended, so no event can
// land after the events were listed and be left behind; the retry finishes.
func TestNoAppendOnceDeleteStarted(t *testing.T) {
	db := local(t)
	name := table(t, db)
	f := &faulty{API: db, failBatchOnce: "E|"}
	st := dynamodb.New(f, name)
	r, _ := session.NewRecorder(st, session.DefaultPolicy(time.Hour))
	se, err := r.Open(ctx, "t1", "refunds", session.KindRun)
	if err != nil {
		t.Fatal(err)
	}
	rec := started(t, r, se, "r1")
	_ = rec.Record(session.Report, "a")
	if err := rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "t1", rec.Session().ID); !errors.Is(err, errInjected) {
		t.Fatalf("first Delete: err = %v, want the injected failure", err)
	}
	late := []session.Event{{Seq: 2, Time: time.Now(), Kind: session.Report, Body: []byte(`"late"`)}}
	if err := st.Append(ctx, "t1", rec.Session().ID, "r1", late); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("append while deleting: err = %v, want ErrNotFound", err)
	}
	if err := st.EndRun(ctx, "t1", rec.Session().ID, "r1", time.Time{}); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("end while deleting: err = %v, want ErrNotFound", err)
	}
	if err := st.Hold(ctx, "t1", rec.Session().ID, "r1"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("hold while deleting: err = %v, want ErrNotFound", err)
	}
	if err := st.Delete(ctx, "t1", rec.Session().ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if n := sessionItems(t, db, name); n != 0 {
		t.Fatalf("%d session items left", n)
	}
}
