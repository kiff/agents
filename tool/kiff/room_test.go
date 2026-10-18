package kiff_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/kiff/agents/tool/kiff"
)

// cardGateway answers kiff_card with structured, and records the tools called.
func cardGateway(t *testing.T, structured string, isError bool) (*httptest.Server, *[]string) {
	t.Helper()
	var called []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		if msg.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		result := map[string]any{}
		if msg.Method == "tools/call" {
			called = append(called, msg.Params.Name)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "You act as agent a."}},
				"isError": isError, "structuredContent": json.RawMessage(structured)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv, &called
}

// CardRoom reads each Card limit's room for each action it covers, from
// kiff_card's structured answer, and calls nothing else.
func TestCardRoomReadsWhatEachLimitHasLeft(t *testing.T) {
	srv, called := cardGateway(t, `{"agent_id":"a","cards":[
	  {"id":"c-update","grants":[{"action":"update_dispute"}],"on_exceed":"owner","limits":[
	    {"quantity":"sum(amount)","window":"calendar_day","limit":1000000,"remaining":1000,"forecast":{"resets_at":"2026-10-19T00:00:00Z"}}]},
	  {"id":"c-reads","grants":[{"action":"list_disputes"},{"action":"retrieve_dispute"}],"limits":[
	    {"quantity":"count","window":"calendar_day","limit":500,"remaining":378,"forecast":{}}]}]}`, false)
	rooms, err := kiff.New(srv.URL, testKey, srv.Client()).CardRoom(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reset := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	want := []kiff.Room{
		{Card: "c-update", Action: "update_dispute", Quantity: "sum(amount)", Window: "calendar_day", Limit: 1000000, Remaining: 1000, ResetsAt: reset},
		{Card: "c-reads", Action: "list_disputes", Quantity: "count", Window: "calendar_day", Limit: 500, Remaining: 378},
		{Card: "c-reads", Action: "retrieve_dispute", Quantity: "count", Window: "calendar_day", Limit: 500, Remaining: 378},
	}
	if !reflect.DeepEqual(rooms, want) {
		t.Fatalf("rooms %+v", rooms)
	}
	if !reflect.DeepEqual(*called, []string{"kiff_card"}) {
		t.Fatalf("called %v", *called)
	}

	errSrv, _ := cardGateway(t, `{}`, true)
	if _, err := kiff.New(errSrv.URL, testKey, errSrv.Client()).CardRoom(context.Background()); err == nil {
		t.Fatal("kiff_card's error read as room")
	}
}
