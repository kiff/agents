package dynamodb_test

import (
	"context"
	"strings"
	"sync"
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
	r := run.New("t1", "refunds", "s1", run.BySchedule, "go", at)
	r.Source = "sch1"
	if err := rs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	if by := scan(t, db, name); len(by["W"]) != 1 || len(by["L"]) != 1 || len(by["O"]) != 1 || len(by["RM"]) != 1 {
		t.Fatalf("a pending run: %d due, %d list, %d source list, %d run items", len(by["W"]), len(by["L"]), len(by["O"]), len(by["RM"]))
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
	for _, kind := range []string{"RM", "L", "O"} {
		for _, item := range by[kind] {
			if got, ok := item["ttl"].(*types.AttributeValueMemberN); !ok || got.Value != want {
				t.Fatalf("%s item: ttl %v, want %s", kind, item["ttl"], want)
			}
		}
	}

	// Removing the agent removes every item of a paused run, its turn too.
	paused := run.New("t1", "refunds", "s1", run.BySchedule, "go", at)
	paused.Source, paused.Status, paused.Paused = "sch1", run.Paused, turn
	if err := rs.Create(ctx, paused); err != nil {
		t.Fatal(err)
	}
	if err := rs.DeleteAgent(ctx, "t1", "refunds"); err != nil {
		t.Fatal(err)
	}
	if by := scan(t, db, name); len(by["RM"])+len(by["RT"])+len(by["L"])+len(by["O"])+len(by["W"]) != 0 {
		t.Fatalf("left after removing the agent: %d run, %d part, %d list, %d source list, %d due items", len(by["RM"]), len(by["RT"]), len(by["L"]), len(by["O"]), len(by["W"]))
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
	if by := scan(t, db, name); len(by["W"]) != 0 {
		t.Fatalf("the stale entry is still there: the next sweep reads it again")
	}
}

// A run removed while a claim lands on it (moving its due entry) leaves
// nothing behind: the delete is conditional on the version it read.
func TestARunRemovedWhileClaimedLeavesNothing(t *testing.T) {
	db := local(t)
	name := table(t, db)
	at := now()
	plain := dynamodb.NewRunsAt(db, name, func() time.Time { return at })
	r := run.New("t1", "refunds", "s1", run.ByOwner, "go", at)
	if err := plain.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	claim := func() {
		if _, err := run.Claim(ctx, plain, "t1", r.ID, "i1", at.Add(15*time.Minute), at); err != nil {
			t.Errorf("claiming meanwhile: %v", err)
		}
	}
	racy := dynamodb.NewRunsAt(&racing{API: db, before: claim}, name, func() time.Time { return at })
	if err := racy.Delete(ctx, "t1", r.ID); err != nil {
		t.Fatal(err)
	}
	if by := scan(t, db, name); len(by["RM"])+len(by["L"])+len(by["W"]) != 0 {
		t.Fatalf("left behind: %d run, %d list, %d due items", len(by["RM"]), len(by["L"]), len(by["W"]))
	}
}

// pageRace runs before, once, after the first page of a query of a run's
// partition: a write landing between the pages of a big read.
type pageRace struct {
	dynamodb.API
	once   sync.Once
	before func()
}

func (p *pageRace) Query(ctx context.Context, in *ddb.QueryInput, opts ...func(*ddb.Options)) (*ddb.QueryOutput, error) {
	out, err := p.API.Query(ctx, in, opts...)
	if pk, ok := in.ExpressionAttributeValues[":pk"].(*types.AttributeValueMemberS); err == nil && ok && strings.HasPrefix(pk.Value, "R|") && out.LastEvaluatedKey != nil {
		p.once.Do(p.before)
	}
	return out, err
}

// A paused turn bigger than one page of a query is read whole, from one
// version, even when a write replaces it between the pages: never part of one
// turn and part of the next.
func TestABigTurnIsNeverReadTorn(t *testing.T) {
	db := local(t)
	name := table(t, db)
	at := now()
	plain := dynamodb.NewRunsAt(db, name, func() time.Time { return at })
	r := run.New("t1", "refunds", "s1", run.ByOwner, "go", at)
	r.Status = run.Paused
	r.Paused = &run.Turn{Results: []model.Block{{Kind: model.ToolResult, Result: strings.Repeat("x", 1500<<10)}}}
	if err := plain.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	replace := func() {
		_, err := run.Change(ctx, plain, r, at, func(n *run.Run) error {
			n.Paused = &run.Turn{Results: []model.Block{{Kind: model.ToolResult, Result: strings.Repeat("y", 1500<<10)}}}
			return nil
		})
		if err != nil {
			t.Errorf("replacing the turn meanwhile: %v", err)
		}
	}
	racy := dynamodb.NewRunsAt(&pageRace{API: db, before: replace}, name, func() time.Time { return at })
	got, err := racy.Get(ctx, "t1", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	result := got.Paused.Results[0].Result
	xs, ys := strings.Count(result, "x"), strings.Count(result, "y")
	if (xs != 0 && ys != 0) || len(result) != 1500<<10 {
		t.Fatalf("read version %d with %d x and %d y bytes: a torn turn", got.Version, xs, ys)
	}
	if got.Version != 2 || ys == 0 {
		t.Fatalf("read version %d (%d y bytes), want the new turn at version 2", got.Version, ys)
	}
}

// A hold item points at its run, takes the run's TTL when the run ends (its
// requirements gone), and is removed with the run.
func TestHoldItems(t *testing.T) {
	db := local(t)
	name := table(t, db)
	at := now()
	rs := dynamodb.NewRunsAt(db, name, func() time.Time { return at })
	r := run.New("t1", "refunds", "s1", run.ByOwner, "go", at)
	if err := rs.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	r, err := run.Claim(ctx, rs, "t1", r.ID, "i1", at.Add(15*time.Minute), at)
	if err != nil {
		t.Fatal(err)
	}
	r, err = run.Save(ctx, rs, r, at, func(n *run.Run) error {
		n.Status = run.Paused
		n.Requirements = []run.Requirement{{CallID: "c1", Tool: "refund", Exception: "exc_1", Answered: at}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if by := scan(t, db, name); len(by["H"]) != 1 || by["H"][0]["ttl"] != nil {
		t.Fatalf("a paused run: %d hold items, ttl %v", len(by["H"]), by["H"])
	}
	r, err = run.Claim(ctx, rs, "t1", r.ID, "i2", at.Add(15*time.Minute), at)
	if err != nil {
		t.Fatal(err)
	}
	expires := at.Add(24 * time.Hour)
	if _, err := run.Save(ctx, rs, r, at, func(n *run.Run) error {
		n.Status, n.Requirements, n.Expires = run.Completed, nil, expires
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	by := scan(t, db, name)
	if len(by["H"]) != 1 {
		t.Fatalf("an ended run: %d hold items, want 1", len(by["H"]))
	}
	if got, ok := by["H"][0]["ttl"].(*types.AttributeValueMemberN); !ok || got.Value != itoa(expires.Unix()) {
		t.Fatalf("an ended run's hold: ttl %v, want %d", by["H"][0]["ttl"], expires.Unix())
	}

	// A run paused on two holds in turn: both items take its TTL when it
	// ends, and deleting it while paused on the second leaves neither.
	twice := func(exs ...string) run.Run {
		t.Helper()
		r := run.New("t1", "refunds", "s1", run.ByOwner, "go", at)
		if err := rs.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		for _, ex := range exs {
			r, err = run.Claim(ctx, rs, "t1", r.ID, "i", at.Add(15*time.Minute), at)
			if err != nil {
				t.Fatal(err)
			}
			r, err = run.Save(ctx, rs, r, at, func(n *run.Run) error {
				n.Status = run.Paused
				n.Requirements = []run.Requirement{{CallID: ex, Tool: "refund", Exception: ex, Answered: at}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		return r
	}
	ended := twice("exc_2", "exc_3")
	ended, err = run.Claim(ctx, rs, "t1", ended.ID, "i", at.Add(15*time.Minute), at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Save(ctx, rs, ended, at, func(n *run.Run) error {
		n.Status, n.Requirements, n.Expires = run.Completed, nil, expires
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	by = scan(t, db, name)
	if len(by["H"]) != 3 {
		t.Fatalf("%d hold items, want 3", len(by["H"]))
	}
	for _, item := range by["H"] {
		if got, ok := item["ttl"].(*types.AttributeValueMemberN); !ok || got.Value != itoa(expires.Unix()) {
			t.Fatalf("hold %v: ttl %v, want the run's", item["pk"], item["ttl"])
		}
	}
	paused := twice("exc_4", "exc_5")
	if err := rs.Delete(ctx, "t1", paused.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(scan(t, db, name)["H"]); n != 3 {
		t.Fatalf("%d hold items after deleting a run paused twice, want the 3 of the ended runs", n)
	}
}

// pagedQueries is a client whose Query answers in pages of active items and
// records what it was asked. Nothing else is called.
type pagedQueries struct {
	dynamodb.API
	asked []*ddb.QueryInput
}

func (p *pagedQueries) Query(_ context.Context, in *ddb.QueryInput, _ ...func(*ddb.Options)) (*ddb.QueryOutput, error) {
	cp := *in
	p.asked = append(p.asked, &cp)
	item := func(id string) map[string]types.AttributeValue {
		return map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "X"}, "sk": &types.AttributeValueMemberS{Value: id}}
	}
	if in.ExclusiveStartKey == nil {
		return &ddb.QueryOutput{Items: []map[string]types.AttributeValue{item("a"), item("b")}, LastEvaluatedKey: item("b")}, nil
	}
	return &ddb.QueryOutput{Items: []map[string]types.AttributeValue{item("c")}}, nil
}

// The count reads consistently, every page: a run that just started counts,
// one that just ended doesn't. DynamoDB Local reads consistently whatever is
// asked, so only the request shows it.
func TestTheActiveCountReadsConsistently(t *testing.T) {
	db := &pagedQueries{}
	n, err := dynamodb.NewRuns(db, "agents").CountActive(context.Background(), "t1")
	if err != nil || n != 3 {
		t.Fatalf("%d (%v), want 3 over two pages", n, err)
	}
	for i, in := range db.asked {
		if in.ConsistentRead == nil || !*in.ConsistentRead || in.TableName == nil || *in.TableName != "agents" {
			t.Errorf("page %d asked %+v, want a consistent read of the table", i, in)
		}
	}
}

// A run is in the operators' paused list only while it is paused: its entry
// goes when it is claimed again, and when it is deleted. An entry left behind
// (written outside a run's own write) never lists a run that isn't paused.
func TestPausedListItems(t *testing.T) {
	db := local(t)
	name := table(t, db)
	at := now()
	rs := dynamodb.NewRunsAt(db, name, func() time.Time { return at })
	pause := func() run.Run {
		t.Helper()
		r := run.New("t1", "refunds", "s1", run.ByOwner, "go", at)
		if err := rs.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		r, err := run.Claim(ctx, rs, "t1", r.ID, "i1", at.Add(15*time.Minute), at)
		if err != nil {
			t.Fatal(err)
		}
		r, err = run.Save(ctx, rs, r, at, func(n *run.Run) error { n.Status = run.Paused; return nil })
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := pause()
	if by := scan(t, db, name); len(by["P"]) != 1 || len(by["A"]) != 1 {
		t.Fatalf("a paused run: %d paused entries, %d in the list of all; want 1 and 1", len(by["P"]), len(by["A"]))
	}
	item := scan(t, db, name)["P"][0]
	if _, err := run.Claim(ctx, rs, "t1", r.ID, "i2", at.Add(15*time.Minute), at); err != nil {
		t.Fatal(err)
	}
	if by := scan(t, db, name); len(by["P"]) != 0 {
		t.Fatalf("claimed again: %d paused entries, want none", len(by["P"]))
	}
	other := pause()
	if err := rs.Delete(ctx, "t1", other.ID); err != nil {
		t.Fatal(err)
	}
	if by := scan(t, db, name); len(by["P"]) != 0 {
		t.Fatalf("deleted while paused: %d paused entries, want none", len(by["P"]))
	}
	// An entry left behind for r, which is now RUNNING: not listed.
	if _, err := db.PutItem(ctx, &ddb.PutItemInput{TableName: &name, Item: item}); err != nil {
		t.Fatal(err)
	}
	if paused, err := rs.ListPaused(ctx, 0); err != nil || len(paused) != 0 {
		t.Fatalf("listed %d paused runs from an entry left behind, err = %v", len(paused), err)
	}
}
