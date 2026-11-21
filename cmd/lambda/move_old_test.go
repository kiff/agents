package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/kiff/agents/registry/secretsmanager"
)

// copier copies by tenant/ref as told: "copied", "already" or an error.
type copier map[string]string

func (c copier) Copy(_ context.Context, tenant, ref string) (bool, error) {
	switch v := c[tenant+"/"+ref]; v {
	case "copied":
		return true, nil
	case "already":
		return false, nil
	default:
		return false, errors.New(v)
	}
}

// move_old_keys copies each listed key, counts what it did, goes on past a
// failure and reports it, and logs references only: never a key.
func TestMoveOldKeysCountsEachAndGoesOnPastAFailure(t *testing.T) {
	var logs bytes.Buffer
	listed := []secretsmanager.OldKey{{Tenant: "t1", Ref: "kiff"}, {Tenant: "t2", Ref: "kiff"}, {Tenant: "t3", Ref: "kiff"}, {Tenant: "t4", Ref: "apps/x"}}
	move := moveOldKeys(func(context.Context) ([]secretsmanager.OldKey, error) { return listed, nil },
		copier{"t1/kiff": "copied", "t2/kiff": "already", "t3/kiff": "kms: throttled", "t4/apps/x": "copied"},
		slog.New(slog.NewTextHandler(&logs, nil)))
	got, err := move(ctx)
	if err == nil || !strings.Contains(err.Error(), "kms: throttled") {
		t.Errorf("the failure isn't reported: %v", err)
	}
	if got != (moveCounts{Listed: 4, Copied: 2, Already: 1, Failed: 1}) {
		t.Errorf("counts: %+v", got)
	}
	if !strings.Contains(logs.String(), "ref=apps/x") || !strings.Contains(logs.String(), "tenant=t3") {
		t.Errorf("each move isn't logged by reference: %s", logs.String())
	}

	failing := moveOldKeys(func(context.Context) ([]secretsmanager.OldKey, error) { return nil, errors.New("list: denied") }, copier{}, slog.New(slog.DiscardHandler))
	if c, err := failing(ctx); err == nil || c != (moveCounts{}) {
		t.Errorf("a failed list: %+v, %v", c, err)
	}
}

// The event runs the move and answers its counts; where the keys aren't
// kept in the table, it's refused.
func TestTheMoveOldKeysEvent(t *testing.T) {
	f, _, _ := newFunction(t)
	if _, err := f.handle(ctx, json.RawMessage(`{"move_old_keys":true}`)); err == nil {
		t.Error("ran with no table for the keys")
	}
	f.moveOld = func(context.Context) (moveCounts, error) { return moveCounts{Listed: 13, Copied: 13}, nil }
	out, err := f.handle(ctx, json.RawMessage(`{"move_old_keys":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(out); string(b) != `{"listed":13,"copied":13,"already":0,"failed":0}` {
		t.Errorf("answered %s", b)
	}
}

type oldAlertAccounts map[string][2]string

func (o oldAlertAccounts) Account(_ context.Context, account string) (string, string, error) {
	if v, ok := o[account]; ok {
		return v[0], v[1], nil
	}
	return "", "", errors.New("no such secret")
}

// alertTable is the table's create-only write, in memory.
type alertTable map[string]string

func (a alertTable) Create(_ context.Context, tenant, secret string) (bool, error) {
	if _, ok := a[tenant]; ok {
		return false, nil
	}
	a[tenant] = secret
	return true, nil
}

// move_old_accounts copies each account's secret to its own tenant,
// create-only: one the API already wrote stays, one naming another tenant
// isn't copied, an unreadable one is counted failed; no secret is logged.
func TestMoveOldAccountsCopiesEachToItsOwnTenant(t *testing.T) {
	var logs bytes.Buffer
	table := alertTable{"t2": "whsec_from_api"}
	move := moveOldAccounts(func(context.Context) ([]string, error) { return []string{"t1", "t2", "t3", "t4"}, nil },
		oldAlertAccounts{"t1": {"t1", "whsec_one"}, "t2": {"t2", "whsec_old"}, "t3": {"t9", "whsec_three"}},
		table, slog.New(slog.NewTextHandler(&logs, nil)))
	got, err := move(ctx)
	if err == nil {
		t.Error("two failures, no error")
	}
	if got != (moveCounts{Listed: 4, Copied: 1, Already: 1, Failed: 2}) {
		t.Errorf("counts: %+v", got)
	}
	if table["t1"] != "whsec_one" || table["t2"] != "whsec_from_api" || table["t9"] != "" || len(table) != 2 {
		t.Errorf("table: %v", table)
	}
	if strings.Contains(logs.String(), "whsec_") || (err != nil && strings.Contains(err.Error(), "whsec_")) {
		t.Errorf("a secret was logged or returned: %s %v", logs.String(), err)
	}
}

// The event is refused where the alert secrets aren't kept in the table.
func TestTheMoveOldAccountsEvent(t *testing.T) {
	f, _, _ := newFunction(t)
	if _, err := f.handle(ctx, json.RawMessage(`{"move_old_accounts":{}}`)); err == nil {
		t.Error("ran with no table for the alert secrets")
	}
	f.moveOldAccounts = func(context.Context) (moveCounts, error) { return moveCounts{Listed: 7, Copied: 7}, nil }
	if out, err := f.handle(ctx, json.RawMessage(`{"move_old_accounts":{}}`)); err != nil || out != (moveCounts{Listed: 7, Copied: 7}) {
		t.Errorf("answered %v, %v", out, err)
	}
}

// thenErase runs the next erase only after the first succeeded.
func TestThenEraseStopsAtAFailure(t *testing.T) {
	var ran []string
	step := func(name string, fail bool) func(context.Context, string) error {
		return func(context.Context, string) error {
			ran = append(ran, name)
			if fail {
				return errors.New(name + " failed")
			}
			return nil
		}
	}
	if err := thenErase(step("first", true), step("alerts", false))(ctx, "t1"); err == nil || strings.Join(ran, ",") != "first" {
		t.Errorf("after a failure: %v, ran %v", err, ran)
	}
	ran = nil
	if err := thenErase(nil, step("alerts", false))(ctx, "t1"); err != nil || strings.Join(ran, ",") != "alerts" {
		t.Errorf("alone: %v, ran %v", err, ran)
	}
}
