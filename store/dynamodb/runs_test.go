package dynamodb_test

import (
	"strings"
	"testing"
	"time"

	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/run/runtest"
	"github.com/kiff/agents/store/dynamodb"
)

func TestRuns(t *testing.T) {
	db := local(t)
	runtest.Run(t, func(t *testing.T, now func() time.Time) run.Runs {
		return dynamodb.NewRunsAt(db, table(t, db), now)
	})
}

// scan returns every item of the table, by kind: the first letter of its pk,
// and of its sk for run partitions.
func scan(t *testing.T, db *ddb.Client, name string) map[string][]map[string]types.AttributeValue {
	t.Helper()
	out, err := db.Scan(ctx, &ddb.ScanInput{TableName: &name})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string][]map[string]types.AttributeValue{}
	for _, item := range out.Items {
		k := item["pk"].(*types.AttributeValueMemberS).Value[:1]
		if k == "R" {
			k += item["sk"].(*types.AttributeValueMemberS).Value[:1]
		}
		by[k] = append(by[k], item)
	}
	return by
}

// A paused turn bigger than an item is cut into parts and comes back whole; a
// run that has ended carries its TTL on every item it has, and leaves the
// sweep's partition; one too big for a transaction is refused, and nothing of
// it is written.
func TestRunItems(t *testing.T) {
	db := local(t)
	name := table(t, db)
	at := now()
	rs := dynamodb.NewRunsAt(db, name, func() time.Time { return at })
	r := run.New("t1", "refunds", "s1", run.ByOwner, "go", at)
	if err := rs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	if by := scan(t, db, name); len(by["W"]) != 1 || len(by["L"]) != 1 || len(by["RM"]) != 1 {
		t.Fatalf("a pending run: %d due, %d list, %d run items", len(by["W"]), len(by["L"]), len(by["RM"]))
	}
	r, err := run.Claim(ctx, rs, "t1", r.ID, "i1", at.Add(15*time.Minute), at)
	if err != nil {
		t.Fatal(err)
	}
	turn := &run.Turn{Results: []model.Block{{Kind: model.ToolResult, Result: strings.Repeat("x", 700<<10)}}}
	r, err = run.Save(ctx, rs, r, at, func(n *run.Run) error { n.Status, n.Paused = run.Paused, turn; return nil })
	if err != nil {
		t.Fatal(err)
	}
	by := scan(t, db, name)
	if len(by["RT"]) != 3 || len(by["W"]) != 0 {
		t.Fatalf("a paused run with a 700 KB turn: %d parts, %d due items; want 3 and none", len(by["RT"]), len(by["W"]))
	}
	r, err = run.Claim(ctx, rs, "t1", r.ID, "i2", at.Add(15*time.Minute), at)
	if err != nil {
		t.Fatal(err)
	}
	expires := at.Add(24 * time.Hour)
	_, err = run.Save(ctx, rs, r, at, func(n *run.Run) error {
		n.Status, n.Expires, n.Paused = run.Completed, expires, nil
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	by = scan(t, db, name)
	if len(by["RT"]) != 0 || len(by["W"]) != 0 {
		t.Fatalf("a completed run: %d parts, %d due items left", len(by["RT"]), len(by["W"]))
	}
	want := itoa(expires.Unix())
	for _, kind := range []string{"RM", "L"} {
		for _, item := range by[kind] {
			if got, ok := item["ttl"].(*types.AttributeValueMemberN); !ok || got.Value != want {
				t.Fatalf("%s item: ttl %v, want %s", kind, item["ttl"], want)
			}
		}
	}

	// Removing the agent removes every item of a paused run, its turn too.
	paused := run.New("t1", "refunds", "s1", run.ByOwner, "go", at)
	paused.Status, paused.Paused = run.Paused, turn
	if err := rs.Create(ctx, paused); err != nil {
		t.Fatal(err)
	}
	if err := rs.DeleteAgent(ctx, "t1", "refunds"); err != nil {
		t.Fatal(err)
	}
	if by := scan(t, db, name); len(by["RM"])+len(by["RT"])+len(by["L"])+len(by["W"]) != 0 {
		t.Fatalf("left after removing the agent: %d run, %d part, %d list, %d due items", len(by["RM"]), len(by["RT"]), len(by["L"]), len(by["W"]))
	}

	huge := run.New("t1", "refunds", "s1", run.ByOwner, "go", at)
	huge.Paused = &run.Turn{Results: []model.Block{{Kind: model.ToolResult, Result: strings.Repeat("x", dynamodb.MaxTurn)}}}
	if err := rs.Create(ctx, huge); err == nil || !strings.Contains(err.Error(), "paused turn") {
		t.Fatalf("a turn over MaxTurn: err = %v", err)
	}
	if _, err := rs.Get(ctx, "t1", huge.ID); err == nil {
		t.Fatal("part of a refused run was written")
	}
}

// Due reads the runs its entries name and keeps only those still due: an
// entry that outlived its run's move (a write elsewhere, by hand) sends the
// sweep after nothing.
func TestDueChecksTheRunItself(t *testing.T) {
	db := local(t)
	name := table(t, db)
	at := now()
	rs := dynamodb.NewRunsAt(db, name, func() time.Time { return at })
	r := run.New("t1", "refunds", "s1", run.ByOwner, "go", at)
	if err := rs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	r, err := run.Change(ctx, rs, r, at, func(n *run.Run) error { n.Status = run.Cancelled; return nil })
	if err != nil {
		t.Fatal(err)
	}
	stale := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: "W"}, "sk": &types.AttributeValueMemberS{Value: "00000000000000000001|x"},
		"tenant": &types.AttributeValueMemberS{Value: "t1"}, "id": &types.AttributeValueMemberS{Value: r.ID},
	}
	if _, err := db.PutItem(ctx, &ddb.PutItemInput{TableName: &name, Item: stale}); err != nil {
		t.Fatal(err)
	}
	if due, err := rs.Due(ctx, at.Add(time.Hour), 10); err != nil || len(due) != 0 {
		t.Fatalf("due: %d runs, err = %v; want none: the run was cancelled", len(due), err)
	}
}
