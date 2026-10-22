package dynamodb_test

import (
	"testing"
	"time"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/registry/registrytest"
	"github.com/kiff/agents/skill"
	"github.com/kiff/agents/store/dynamodb"
)

func TestAgents(t *testing.T) {
	db := local(t)
	registrytest.Run(t, func(t *testing.T) registry.Agents { return dynamodb.NewAgents(db, table(t, db)) })
}

// Deleting an account erases the tenant's own halt and skills, and nothing
// of another tenant's (kiff/tafs#185). Its agents are deleted first, each
// with Delete.
func TestEraseTenant(t *testing.T) {
	db := local(t)
	name := table(t, db)
	agents, skills := dynamodb.NewAgents(db, name), dynamodb.NewSkills(db, name)
	for _, tenant := range []string{"t1", "t2"} {
		if _, err := agents.Halt(ctx, tenant, registry.Tenant, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := skills.Put(ctx, skill.Package{Tenant: tenant, Name: "kiff", Version: "1", Description: "d", Body: "b"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := agents.EraseTenant(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if err := skills.EraseTenant(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if h, _ := agents.Halted(ctx, "t1", registry.Tenant); h.On {
		t.Fatal("t1's halt is still stored")
	}
	if ps, _ := skills.List(ctx, "t1"); len(ps) != 0 {
		t.Fatalf("t1's skills: %v", ps)
	}
	if h, _ := agents.Halted(ctx, "t2", registry.Tenant); !h.On {
		t.Fatal("t2's halt was erased")
	}
	if ps, _ := skills.List(ctx, "t2"); len(ps) != 1 {
		t.Fatal("t2's skills were erased")
	}
	if err := agents.EraseTenant(ctx, "t1"); err != nil {
		t.Fatalf("erasing again: %v", err)
	}
}
