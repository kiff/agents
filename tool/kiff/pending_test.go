package kiff_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/kiff/agents/tool/kiff"
)

// pendingGateway answers kiff_pending with pages, one per call, and records
// each call's tool and cursor.
func pendingGateway(t *testing.T, pages []string, isError bool) (*httptest.Server, *[]string) {
	t.Helper()
	var called []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name      string `json:"name"`
				Arguments struct {
					Cursor string `json:"cursor"`
				} `json:"arguments"`
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
			called = append(called, msg.Params.Name+" "+msg.Params.Arguments.Cursor)
			page := `{}`
			if n := len(called) - 1; n < len(pages) {
				page = pages[n]
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "Your pending KIFF calls"}},
				"isError": isError, "structuredContent": json.RawMessage(page)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv, &called
}

// Pending reads every page of kiff_pending, following next_cursor, and
// calls nothing else.
func TestPendingReadsEveryPage(t *testing.T) {
	srv, called := pendingGateway(t, []string{
		`{"calls":[{"tool":"update_dispute","state":"held","answer":"waiting","exception_id":"exc_1","kiff_operation_id":"op-1"}],"next_cursor":"c2"}`,
		`{"calls":[{"tool":"update_dispute","state":"unknown","kiff_operation_id":"op-2"}]}`,
	}, false)
	got, err := kiff.New(srv.URL, testKey, srv.Client()).Pending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []kiff.Pending{
		{Tool: "update_dispute", State: "held", Answer: "waiting", ExceptionID: "exc_1"},
		{Tool: "update_dispute", State: "unknown"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pending %+v", got)
	}
	if !reflect.DeepEqual(*called, []string{"kiff_pending ", "kiff_pending c2"}) {
		t.Fatalf("called %v", *called)
	}

	errSrv, _ := pendingGateway(t, nil, true)
	if _, err := kiff.New(errSrv.URL, testKey, errSrv.Client()).Pending(context.Background()); err == nil {
		t.Fatal("kiff_pending's error read as a listing")
	}
}

// A listing that never ends stops at its bound, with an error.
func TestPendingStopsAtItsBound(t *testing.T) {
	pages := make([]string, 30)
	for i := range pages {
		pages[i] = `{"calls":[],"next_cursor":"again"}`
	}
	srv, called := pendingGateway(t, pages, false)
	if _, err := kiff.New(srv.URL, testKey, srv.Client()).Pending(context.Background()); err == nil {
		t.Fatal("an endless listing ended without an error")
	}
	if len(*called) != 10 {
		t.Fatalf("%d pages read", len(*called))
	}
}
