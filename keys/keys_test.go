package keys_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kiff/agents/drafts"
	"github.com/kiff/agents/keys"
)

var ctx = context.Background()

// table is a keys.Store in memory, with a hook that runs before a Create,
// to put another write in between.
type table struct {
	keys         map[string]string
	removed      map[string]bool
	settled      map[string]bool
	beforeCreate func()
}

func newTable() *table {
	return &table{keys: map[string]string{}, removed: map[string]bool{}, settled: map[string]bool{}}
}

func (t *table) Get(_ context.Context, tenant, ref string) (string, error) {
	id := tenant + "|" + ref
	if t.removed[id] {
		return "", keys.ErrRemoved
	}
	if k, ok := t.keys[id]; ok {
		return k, nil
	}
	return "", keys.ErrNotFound
}

func (t *table) Create(_ context.Context, tenant, ref, key string) error {
	if t.beforeCreate != nil {
		f := t.beforeCreate
		t.beforeCreate = nil
		f()
	}
	id := tenant + "|" + ref
	if _, ok := t.keys[id]; ok || t.removed[id] {
		return keys.ErrTaken
	}
	t.keys[id] = key
	return nil
}

func (t *table) Put(_ context.Context, tenant, ref, key string) error {
	id := tenant + "|" + ref
	delete(t.removed, id)
	t.keys[id] = key
	return nil
}

func (t *table) Remove(_ context.Context, tenant, ref string, settled bool) error {
	id := tenant + "|" + ref
	delete(t.keys, id)
	t.removed[id], t.settled[id] = true, settled
	return nil
}

// secrets is the old store in memory; a delete can be made to fail.
// secrets is the old secrets: a retired one is kept, unread, until deleted.
type secrets struct {
	keys       map[string]string
	retired    map[string]string
	deleteFail bool // retires and deletes fail
	deletes    int
	retires    int
}

func (s *secrets) Key(_ context.Context, tenant, ref string) (string, error) {
	if k, ok := s.keys[tenant+"/"+ref]; ok {
		return k, nil
	}
	return "", keys.ErrNotFound
}

func (s *secrets) Retire(_ context.Context, tenant, ref string) error {
	s.retires++
	if s.deleteFail {
		return errors.New("secretsmanager: throttled")
	}
	if k, ok := s.keys[tenant+"/"+ref]; ok {
		s.retired[tenant+"/"+ref] = k
		delete(s.keys, tenant+"/"+ref)
	}
	return nil
}

func (s *secrets) Delete(_ context.Context, tenant, ref string) error {
	s.deletes++
	if s.deleteFail {
		return errors.New("secretsmanager: throttled")
	}
	delete(s.keys, tenant+"/"+ref)
	delete(s.retired, tenant+"/"+ref)
	return nil
}

type logged []string

func (l *logged) log(msg, tenant, ref string, err error) {
	*l = append(*l, fmt.Sprint(msg, " ", tenant, " ", ref, " ", err))
}

func moving(old map[string]string) (keys.Moving, *table, *secrets, *logged) {
	t, s, l := newTable(), &secrets{keys: old, retired: map[string]string{}}, &logged{}
	return keys.Moving{New: t, Old: s, Log: l.log}, t, s, l
}

// A key found only in its old secret is moved on that first read: kept in
// the table, its secret retired, restorable for a while, and read from the
// table after.
func TestAKeyMovesOnItsFirstRead(t *testing.T) {
	m, tb, old, _ := moving(map[string]string{"t1/kiff/taf_1": "kiff_key_a"})
	if k, err := m.Key(ctx, "t1", "kiff/taf_1"); err != nil || k != "kiff_key_a" {
		t.Fatalf("%q %v", k, err)
	}
	if tb.keys["t1|kiff/taf_1"] != "kiff_key_a" || len(old.keys) != 0 || old.retired["t1/kiff/taf_1"] != "kiff_key_a" || old.deletes != 0 {
		t.Fatalf("not moved: table %v, old %v, retired %v, %d deletes", tb.keys, old.keys, old.retired, old.deletes)
	}
	if k, err := m.Key(ctx, "t1", "kiff/taf_1"); err != nil || k != "kiff_key_a" || old.retires != 1 {
		t.Fatalf("second read %q %v, %d retires", k, err, old.retires)
	}
	if _, err := m.Key(ctx, "t2", "kiff/taf_1"); !errors.Is(err, keys.ErrNotFound) {
		t.Fatalf("another tenant: %v", err)
	}
}

// kiff-qa's pin 1: a removed key never comes back. Removed while it's still
// only an old secret, and that secret's delete fails: the removal answers
// the failure (tafs cloud tries again), and a read answers not found, from
// the removed key's mark, kept while the secret may be there.
func TestARemovedKeyNeverComesBackFromItsOldSecret(t *testing.T) {
	m, tb, old, _ := moving(map[string]string{"t1/kiff/taf_1": "kiff_key_a"})
	old.deleteFail = true
	if err := m.Remove(ctx, "t1", "kiff/taf_1"); err == nil {
		t.Fatal("a removal whose old secret stayed answered done")
	}
	if !tb.removed["t1|kiff/taf_1"] || tb.settled["t1|kiff/taf_1"] {
		t.Fatalf("the mark: removed %v, settled %v", tb.removed, tb.settled)
	}
	k, err := m.Key(ctx, "t1", "kiff/taf_1")
	if !errors.Is(err, keys.ErrNotFound) || !errors.Is(err, keys.ErrRemoved) || k != "" {
		t.Fatalf("read after the removal: %q %v", k, err)
	}
	// Tried again, it goes through, and the mark is settled.
	old.deleteFail = false
	if err := m.Remove(ctx, "t1", "kiff/taf_1"); err != nil || len(old.keys) != 0 || !tb.settled["t1|kiff/taf_1"] {
		t.Fatalf("again: %v, old %v, settled %v", err, old.keys, tb.settled)
	}
}

// The old secret goes first: a removal deletes it before the table's key.
func TestARemovalDeletesTheOldSecretFirst(t *testing.T) {
	m, tb, old, _ := moving(map[string]string{"t1/kiff/taf_1": "kiff_key_a"})
	var order []string
	m.Old = orderedOld{old, &order}
	m.New = orderedNew{tb, &order}
	if err := m.Remove(ctx, "t1", "kiff/taf_1"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "old delete,new remove" {
		t.Fatalf("order: %v", order)
	}
}

type orderedOld struct {
	*secrets
	order *[]string
}

func (o orderedOld) Delete(ctx context.Context, tenant, ref string) error {
	*o.order = append(*o.order, "old delete")
	return o.secrets.Delete(ctx, tenant, ref)
}

type orderedNew struct {
	*table
	order *[]string
}

func (o orderedNew) Remove(ctx context.Context, tenant, ref string, settled bool) error {
	*o.order = append(*o.order, "new remove")
	return o.table.Remove(ctx, tenant, ref, settled)
}

// kiff-qa's pin 4: a replace racing a move keeps the replacing key. The
// move read the old key; a Put lands before its create-only write, which
// loses, isn't retried over the newer key, and the old secret is still
// deleted. The read answers the newer key.
func TestAPutRacingAMoveKeepsTheReplacingKey(t *testing.T) {
	m, tb, old, _ := moving(map[string]string{"t1/kiff/taf_1": "kiff_key_old"})
	tb.beforeCreate = func() {
		if err := m.Put(ctx, "t1", "kiff/taf_1", "kiff_key_new"); err != nil {
			t.Fatal(err)
		}
	}
	k, err := m.Key(ctx, "t1", "kiff/taf_1")
	if err != nil || k != "kiff_key_new" {
		t.Fatalf("read %q %v", k, err)
	}
	if tb.keys["t1|kiff/taf_1"] != "kiff_key_new" || len(old.keys) != 0 {
		t.Fatalf("table %v, old %v", tb.keys, old.keys)
	}
}

// A removal racing a move: the move read the key just before, its create
// loses to the mark, and the read answers removed, never the key.
func TestARemovalRacingAMoveWins(t *testing.T) {
	m, tb, old, _ := moving(map[string]string{"t1/kiff/taf_1": "kiff_key_old"})
	tb.beforeCreate = func() {
		if err := m.Remove(ctx, "t1", "kiff/taf_1"); err != nil {
			t.Fatal(err)
		}
	}
	k, err := m.Key(ctx, "t1", "kiff/taf_1")
	if !errors.Is(err, keys.ErrRemoved) || k != "" {
		t.Fatalf("read %q %v", k, err)
	}
	if _, ok := tb.keys["t1|kiff/taf_1"]; ok || len(old.keys) != 0 {
		t.Fatalf("table %v, old %v", tb.keys, old.keys)
	}
}

// A put replaces the key in the table and retires its old secret, never
// deleting it at once.
func TestAPutRetiresTheOldSecret(t *testing.T) {
	m, tb, old, _ := moving(map[string]string{"t1/kiff/taf_1": "kiff_key_old"})
	if err := m.Put(ctx, "t1", "kiff/taf_1", "kiff_key_new"); err != nil {
		t.Fatal(err)
	}
	if tb.keys["t1|kiff/taf_1"] != "kiff_key_new" || old.retired["t1/kiff/taf_1"] != "kiff_key_old" || old.deletes != 0 {
		t.Fatalf("table %v, retired %v, %d deletes", tb.keys, old.retired, old.deletes)
	}
}

// A removal after a move deletes the retired secret at once: a removed key
// isn't kept for a week.
func TestARemovalDeletesARetiredSecret(t *testing.T) {
	m, _, old, _ := moving(map[string]string{"t1/kiff/taf_1": "kiff_key_a"})
	if _, err := m.Key(ctx, "t1", "kiff/taf_1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx, "t1", "kiff/taf_1"); err != nil {
		t.Fatal(err)
	}
	if len(old.retired)+len(old.keys) != 0 || old.deletes != 1 {
		t.Fatalf("left: retired %v, old %v, %d deletes", old.retired, old.keys, old.deletes)
	}
}

// A put's old secret that can't be retired is logged by reference, never by
// value.
func TestAPutLogsAnUnretiredSecretAndNoKey(t *testing.T) {
	m, tb, old, l := moving(map[string]string{"t1/kiff/taf_1": "kiff_key_old"})
	old.deleteFail = true
	if err := m.Put(ctx, "t1", "kiff/taf_1", "kiff_key_new"); err != nil {
		t.Fatal(err)
	}
	if k, err := m.Key(ctx, "t1", "kiff/taf_1"); err != nil || k != "kiff_key_new" || tb.keys["t1|kiff/taf_1"] != "kiff_key_new" {
		t.Fatalf("read %q %v", k, err)
	}
	if len(*l) == 0 || !strings.Contains((*l)[0], "kiff/taf_1") {
		t.Fatalf("log %v", *l)
	}
	for _, line := range *l {
		if strings.Contains(line, "kiff_key") {
			t.Fatalf("a key in the log: %q", line)
		}
	}
}

// A draft's claim never stores over a key: one in the table, one only in an
// old secret, or a removed key's mark.
func TestAClaimNeverStoresOverAKey(t *testing.T) {
	m, tb, _, _ := moving(map[string]string{"t1/kiff/old": "kiff_key_old"})
	c := keys.Claims{Moving: m}
	tb.keys["t1|kiff/new"] = "kiff_key_new"
	tb.removed["t1|kiff/gone"] = true
	for _, ref := range []string{"kiff/old", "kiff/new", "kiff/gone"} {
		if err := c.Create(ctx, "t1", ref, "kiff_key_claimed"); !errors.Is(err, drafts.ErrTaken) {
			t.Errorf("%s: %v", ref, err)
		}
	}
	if _, _, err := c.Key(ctx, "t1", "kiff/gone"); !errors.Is(err, drafts.ErrTaken) {
		t.Errorf("a removed key reads as %v", err)
	}
	if err := c.Create(ctx, "t1", "kiff/fresh", "kiff_key_claimed"); err != nil {
		t.Fatal(err)
	}
	if k, found, err := c.Key(ctx, "t1", "kiff/fresh"); err != nil || !found || k != "kiff_key_claimed" {
		t.Fatalf("%q %v %v", k, found, err)
	}
	if tb.keys["t1|kiff/old"] != "" || tb.keys["t1|kiff/new"] != "kiff_key_new" {
		t.Fatalf("table %v", tb.keys)
	}
}

// A reference is the tenant's own: nothing that could name another
// tenant's key, or no key, reaches a store.
func TestAReferenceStaysInItsTenant(t *testing.T) {
	m, tb, old, _ := moving(map[string]string{})
	for _, c := range [][2]string{{"t1/x", "kiff/a"}, {"", "kiff/a"}, {"t1", ""}, {"t1", "../t2/kiff/a"}, {"t1", "kiff//a"}, {"t1", "kiff/a/"}, {"..", "kiff/a"}, {"t1", strings.Repeat("a", 257)}} {
		if err := m.Put(ctx, c[0], c[1], "k"); err == nil {
			t.Errorf("put %q %q", c[0], c[1])
		}
		if err := m.Remove(ctx, c[0], c[1]); err == nil {
			t.Errorf("remove %q %q", c[0], c[1])
		}
		if _, err := m.Key(ctx, c[0], c[1]); err == nil || errors.Is(err, keys.ErrNotFound) {
			t.Errorf("read %q %q: %v", c[0], c[1], err)
		}
	}
	if len(tb.keys)+len(tb.removed) != 0 || old.deletes+old.retires != 0 {
		t.Fatalf("reached a store: %v %v %d %d", tb.keys, tb.removed, old.deletes, old.retires)
	}
}

// A migration's copy writes the old secret's key to the table and leaves the
// old secret as it is: no retire, no delete, so nothing is lost if the run
// stops halfway. A key the table has, or removed, is never written over.
func TestACopyLeavesTheOldSecretAndNeverWritesOver(t *testing.T) {
	m, tab, old, _ := moving(map[string]string{"t1/a": "key-a", "t1/b": "key-b-old", "t1/c": "key-c"})
	tab.keys["t1|b"] = "key-b-new"
	tab.removed["t1|c"] = true

	if copied, err := m.Copy(ctx, "t1", "a"); err != nil || !copied {
		t.Fatalf("a: copied %v, %v", copied, err)
	}
	if tab.keys["t1|a"] != "key-a" {
		t.Errorf("a in the table: %q", tab.keys["t1|a"])
	}
	if copied, err := m.Copy(ctx, "t1", "b"); err != nil || copied {
		t.Fatalf("b, already in the table: copied %v, %v", copied, err)
	}
	if tab.keys["t1|b"] != "key-b-new" {
		t.Errorf("b was written over: %q", tab.keys["t1|b"])
	}
	if copied, err := m.Copy(ctx, "t1", "c"); err != nil || copied {
		t.Fatalf("c, removed: copied %v, %v", copied, err)
	}
	if _, ok := tab.keys["t1|c"]; ok || !tab.removed["t1|c"] {
		t.Error("a removed key came back from its old secret")
	}
	if old.retires != 0 || old.deletes != 0 || len(old.keys) != 3 {
		t.Errorf("the old secrets were touched: %d retires, %d deletes, %d left", old.retires, old.deletes, len(old.keys))
	}
	if copied, err := m.Copy(ctx, "t1", "a"); err != nil || copied {
		t.Errorf("a again: copied %v, %v", copied, err)
	}
	if _, err := m.Copy(ctx, "t1", "gone"); !errors.Is(err, keys.ErrNotFound) {
		t.Errorf("no old secret: %v", err)
	}
	if _, err := m.Copy(ctx, "t1", "../t2/a"); err == nil {
		t.Error("copied a reference outside its tenant")
	}
}
