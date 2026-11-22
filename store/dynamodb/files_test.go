package dynamodb_test

import (
	"errors"
	"testing"
	"time"

	"github.com/kiff/agents/files"
	"github.com/kiff/agents/store/dynamodb"
)

func file(tenant, id, agent string, expires time.Time) files.File {
	return files.File{ID: id, Tenant: tenant, Agent: agent, Run: "r1", Session: "s1", Name: "week.csv", MediaType: "text/csv",
		Size: 8, SHA256: "ab", Created: expires.Add(-files.Kept), Expires: expires}
}

// The store keeps a file in its tenant's partition only, lists it, queues it
// when due, and deletes it with its queue entry.
func TestFiles(t *testing.T) {
	db := local(t)
	st := dynamodb.NewFiles(db, table(t, db))
	// The real time, so each file's "ttl" is in the future and DynamoDB
	// Local never deletes one mid-test (see draftT0).
	t0 := time.Now().UTC().Truncate(time.Second)
	a, b, c := file("t1", "fil_a", "refunds", t0), file("t1", "fil_b", "payouts", t0.Add(time.Hour)), file("t2", "fil_c", "refunds", t0)
	for _, f := range []files.File{a, b, c} {
		if err := st.Create(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Create(ctx, a); err == nil {
		t.Error("a second Create of the same id went through")
	}
	got, err := st.Get(ctx, "t1", "fil_a")
	if err != nil || got != a {
		t.Fatalf("Get: %+v, %v; want %+v", got, err, a)
	}
	if _, err := st.Get(ctx, "t2", "fil_a"); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("t2 reads t1's file: %v", err)
	}
	if all, err := st.List(ctx, "t1"); err != nil || len(all) != 2 {
		t.Errorf("t1's list: %+v, %v", all, err)
	}
	due, err := st.Due(ctx, t0, 10)
	if err != nil || len(due) != 2 || due[0].Tenant == due[1].Tenant {
		t.Errorf("due at t0: %+v, %v; want t1's a and t2's c", due, err)
	}
	if err := st.Delete(ctx, "t1", "fil_a"); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "t1", "fil_a"); err != nil {
		t.Errorf("a second delete: %v", err)
	}
	if due, _ := st.Due(ctx, t0.Add(2*time.Hour), 10); len(due) != 2 {
		t.Errorf("due after deleting a: %+v, want b and c", due)
	}
}
