package dynamodb_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strconv"
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
	at := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return at }
	st := dynamodb.NewAt(db, name, clock)
	r, err := session.NewRecorderAt(st, session.DefaultPolicy(30*24*time.Hour), clock)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := r.Start(ctx, "t1", "refunds", session.KindRun)
	if err != nil {
		t.Fatal(err)
	}
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
		case sk == "E|00000000000000000002":
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
	if err := rec.End(ctx, session.Ended); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
