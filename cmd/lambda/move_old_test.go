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
