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

// Answers reads kiff_pending page by page, keeps only the asked holds KIFF
// has answered, as a run records them, and stops once each asked hold is
// found.
func TestAnswersReadsKIFFsAnswersOnHeldCalls(t *testing.T) {
	pages := map[string]string{
		"": `{"calls":[
		  {"state":"held","answer":"approved","exception_id":"exc_1"},
		  {"state":"held","answer":"waiting","exception_id":"exc_2"},
		  {"state":"held","answer":"refused","exception_id":"exc_other"},
		  {"state":"unknown","exception_id":""}],"next_cursor":"p2"}`,
		"p2": `{"calls":[
		  {"state":"held","answer":"withdrawn","exception_id":"exc_3"},
		  {"state":"held","answer":"ended","exception_id":"exc_4"}],"next_cursor":"p3"}`,
		"p3": `{"calls":[{"state":"held","answer":"approved","exception_id":"exc_never"}]}`,
	}
	var cursors []string
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
			if msg.Params.Name != "kiff_pending" {
				t.Errorf("called %s", msg.Params.Name)
			}
			cursors = append(cursors, msg.Params.Arguments.Cursor)
			result = map[string]any{"content": []any{}, "structuredContent": json.RawMessage(pages[msg.Params.Arguments.Cursor])}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
	}))
	defer srv.Close()
	got, err := kiff.New(srv.URL, testKey, srv.Client()).Answers(context.Background(), []string{"exc_1", "exc_2", "exc_3", "exc_4"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"exc_1": "approved", "exc_3": "invalidated", "exc_4": "expired"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("answers %v, want %v", got, want)
	}
	if !reflect.DeepEqual(cursors, []string{"", "p2"}) {
		t.Fatalf("read pages %q: want it to stop once every hold was found", cursors)
	}
}
