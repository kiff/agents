// Package skilltest is the contract every skill.Store keeps, run against a
// fresh store:
//
//	skilltest.Run(t, func(t *testing.T) skill.Store { return newStore(t) })
package skilltest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kiff/agents/skill"
)

func pkg(tenant, name, version, body string) skill.Package {
	return skill.Package{Tenant: tenant, Name: name, Version: version, Description: "How to " + name + ".", Body: body,
		Files: map[string]string{"reference/limits.md": "Limits for " + name + "."}}
}

// Run checks a store: a version is kept whole and never changes, a retry
// with the same content is fine, lists carry no text, and tenants are apart.
func Run(t *testing.T, newStore func(t *testing.T) skill.Store) {
	ctx := context.Background()

	t.Run("a version is kept whole", func(t *testing.T) {
		st := newStore(t)
		p := pkg("t1", "kiff", "abc123", "# KIFF\nNever route around the Card.")
		if err := st.Put(ctx, p); err != nil {
			t.Fatal(err)
		}
		got, err := st.Get(ctx, "t1", "kiff", "abc123")
		if err != nil || !reflect.DeepEqual(got, p) {
			t.Fatalf("got %+v, %v", got, err)
		}
		got.Files["reference/limits.md"] = "changed"
		if again, _ := st.Get(ctx, "t1", "kiff", "abc123"); again.Files["reference/limits.md"] != "Limits for kiff." {
			t.Fatal("a caller changed the stored files")
		}
	})

	t.Run("a version never changes", func(t *testing.T) {
		st := newStore(t)
		p := pkg("t1", "kiff", "abc123", "first")
		if err := st.Put(ctx, p); err != nil {
			t.Fatal(err)
		}
		if err := st.Put(ctx, p); err != nil {
			t.Fatalf("the same content again: %v", err)
		}
		if err := st.Put(ctx, pkg("t1", "kiff", "abc123", "second")); !errors.Is(err, skill.ErrExists) {
			t.Fatalf("other content: %v, want ErrExists", err)
		}
		if got, _ := st.Get(ctx, "t1", "kiff", "abc123"); got.Body != "first" {
			t.Fatalf("body %q", got.Body)
		}
	})

	t.Run("lists carry no text, by name then version", func(t *testing.T) {
		st := newStore(t)
		for _, p := range []skill.Package{pkg("t1", "refunds", "v1", "r"), pkg("t1", "kiff", "v2", "b"), pkg("t1", "kiff", "v1", "a")} {
			if err := st.Put(ctx, p); err != nil {
				t.Fatal(err)
			}
		}
		got, err := st.List(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, p := range got {
			keys = append(keys, p.Name+"@"+p.Version)
			if p.Body != "" || p.Files != nil || p.Description == "" {
				t.Fatalf("listed %+v", p)
			}
		}
		if !reflect.DeepEqual(keys, []string{"kiff@v1", "kiff@v2", "refunds@v1"}) {
			t.Fatalf("listed %v", keys)
		}
	})

	t.Run("the largest package fits", func(t *testing.T) {
		st := newStore(t)
		// The worst case for an item: a body JSON escapes ('"' becomes two
		// bytes), as large as Check lets it be.
		p := skill.Package{Tenant: "t1", Name: "big", Version: "v1", Description: strings.Repeat("d", skill.MaxDescription)}
		p.Body = strings.Repeat(`"`, (skill.MaxSize-skill.MaxDescription-200)/2)
		if err := p.Check(); err != nil {
			t.Fatal(err)
		}
		body := p.Body
		if err := st.Put(ctx, p); err != nil {
			t.Fatalf("a package at the size limit: %v", err)
		}
		if got, err := st.Get(ctx, "t1", "big", "v1"); err != nil || got.Body != body {
			t.Fatalf("read back %d bytes, %v", len(got.Body), err)
		}
	})

	t.Run("tenants are apart", func(t *testing.T) {
		st := newStore(t)
		if err := st.Put(ctx, pkg("t1", "kiff", "v1", "t1's")); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Get(ctx, "t2", "kiff", "v1"); !errors.Is(err, skill.ErrNotFound) {
			t.Fatalf("another tenant's: %v", err)
		}
		if got, _ := st.List(ctx, "t2"); len(got) != 0 {
			t.Fatalf("t2 lists %+v", got)
		}
		if err := st.Put(ctx, pkg("t2", "kiff", "v1", "t2's")); err != nil {
			t.Fatalf("the same name and version in another tenant: %v", err)
		}
	})
}
