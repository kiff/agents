package managed_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kiff/agents/eval"
	"github.com/kiff/agents/eval/managed"
	agentrun "github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
)

const sessionJSON = `{
  "id": "sesn_1", "type": "session", "status": "idle",
  "created_at": "2026-10-15T10:00:00Z", "updated_at": "2026-10-15T10:05:00Z",
  "deployment_id": "depl_1",
  "metadata": {"tafs_job": "refunds", "tafs_tenant": "tenant-1"},
  "agent": {"model": {"id": "claude-opus-5-5", "effort": {"type": "high"}}},
  "usage": {"input_tokens": 1000, "output_tokens": 200, "cache_read_input_tokens": 50,
            "cache_creation": {"ephemeral_5m_input_tokens": 7, "ephemeral_1h_input_tokens": 3},
            "list_cost": {"amount": "42", "currency": "USD"}}
}`

var pages = []string{
	`{"data": [
    {"id": "e1", "type": "user.message", "processed_at": "2026-10-15T10:00:01Z", "content": [{"type": "text", "text": "Work the refund queue."}]},
    {"id": "e2", "type": "agent.thinking", "processed_at": "2026-10-15T10:00:02Z"},
    {"id": "e3", "type": "agent.mcp_tool_use", "processed_at": "2026-10-15T10:00:03Z", "name": "create_refund", "mcp_server_name": "kiff", "input": {"charge": "ch_1"}},
    {"id": "e4", "type": "agent.mcp_tool_result", "processed_at": "2026-10-15T10:00:04Z", "mcp_tool_use_id": "e3", "content": [{"type": "text", "text": "Held for the owner."}], "is_error": true}
  ], "next_page": "p2"}`,
	`{"data": [
    {"id": "e5", "type": "agent.tool_use", "processed_at": "2026-10-15T10:00:05Z", "name": "read", "input": {"file_path": "/mnt/memory/notes.md"}},
    {"id": "e6", "type": "agent.tool_result", "processed_at": "2026-10-15T10:00:06Z", "tool_use_id": "e5", "content": [{"type": "document", "source": {"type": "text", "media_type": "text/plain", "data": "prefers credit"}}]},
    {"id": "e7", "type": "agent.message", "processed_at": "2026-10-15T10:00:07Z", "content": [{"type": "text", "text": "Working on it."}]},
    {"id": "e8", "type": "span.model_request_end", "processed_at": "2026-10-15T10:00:08Z"},
    {"id": "e9", "type": "agent.message", "processed_at": "2026-10-15T10:00:09Z", "content": [{"type": "text", "text": "Refunded one; T-1004 waits on you."}]},
    {"id": "e10", "type": "session.status_idle", "processed_at": "2026-10-15T10:00:10Z"}
  ], "next_page": null}`,
}

func server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-beta") != managed.Beta || r.URL.Query().Get("beta") != "true" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
			return
		}
		switch r.URL.Path {
		case "/v1/sessions/sesn_1":
			_, _ = w.Write([]byte(sessionJSON))
		case "/v1/sessions/sesn_1/events":
			if r.URL.Query().Get("order") != "asc" {
				t.Errorf("events read in order %q; want asc", r.URL.Query().Get("order"))
			}
			if r.URL.Query().Get("page") == "p2" {
				_, _ = w.Write([]byte(pages[1]))
			} else {
				_, _ = w.Write([]byte(pages[0]))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"no such session"}}`))
		}
	}))
}

func body[T any](t *testing.T, e session.Event) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(e.Body, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRead(t *testing.T) {
	srv := server(t)
	defer srv.Close()
	run, err := managed.New(srv.URL, "test-key", nil).Read(context.Background(), "sesn_1")
	if err != nil {
		t.Fatal(err)
	}
	s := run.Session
	if s.ID != "sesn_1" || s.Agent != "refunds" || s.Kind != session.KindRun || run.Status != agentrun.Completed || s.Tenant != "" {
		t.Errorf("session %+v", s)
	}
	var kinds []session.EventKind
	for i, e := range run.Events {
		kinds = append(kinds, e.Kind)
		if e.Seq != int64(i+1) || e.Session != "sesn_1" {
			t.Errorf("event %d: seq %d session %q", i, e.Seq, e.Session)
		}
	}
	want := []session.EventKind{session.OwnerMessage, session.ToolCall, session.ToolResult, session.ToolCall,
		session.ToolResult, session.AgentMessage, session.Report, session.Usage}
	if len(kinds) != len(want) {
		t.Fatalf("kinds %v; want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds %v; want %v", kinds, want)
		}
	}

	call := body[eval.ToolCallBody](t, run.Events[1])
	if call.Tool != "create_refund" || call.Server != "kiff" || string(call.Input) != `{"charge":"ch_1"}` {
		t.Errorf("call %+v", call)
	}
	res := body[eval.ToolResultBody](t, run.Events[2])
	if res.CallID != "e3" || res.Tool != "create_refund" || !res.IsError || res.Text != "Held for the owner." {
		t.Errorf("result %+v", res)
	}
	if local := body[eval.ToolCallBody](t, run.Events[3]); local.Server != "" || local.Tool != "read" {
		t.Errorf("local call %+v", local)
	}
	if doc := body[eval.ToolResultBody](t, run.Events[4]); doc.Text != "prefers credit" {
		t.Errorf("document result %+v", doc)
	}
	if rep, _ := run.Report(); rep != "Refunded one; T-1004 waits on you." {
		t.Errorf("report %q", rep)
	}

	u := body[session.UsageBody](t, run.Events[7])
	if u.Model != "claude-opus-5-5" || u.InputTokens != 1000 || u.OutputTokens != 200 || u.CacheRead != 50 || u.CacheWrite != 10 || u.CostMicroUSD != 420_000 {
		t.Errorf("usage %+v", u)
	}
	if cost, _ := session.Cost(run.Events); cost != 420_000 {
		t.Errorf("cost %d", cost)
	}
	if !run.Events[1].Time.Equal(run.Events[1].Time.UTC()) || run.Events[1].Time.Second() != 3 {
		t.Errorf("time %v", run.Events[1].Time)
	}
}

func TestReadErrorsNeverCarryTheKey(t *testing.T) {
	srv := server(t)
	defer srv.Close()
	_, err := managed.New(srv.URL, "wrong-key-value", nil).Read(context.Background(), "sesn_1")
	var apiErr *managed.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("err %v; want a 401", err)
	}
	if strings.Contains(err.Error(), "wrong-key-value") {
		t.Fatalf("the error carries the key: %v", err)
	}
	_, err = managed.New(srv.URL, "test-key", nil).Read(context.Background(), "sesn_404")
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound || !strings.Contains(err.Error(), "no such session") {
		t.Fatalf("err %v; want a 404 with the API's message", err)
	}
}

func TestMapChatUsageFromEventsAndErrors(t *testing.T) {
	var s managed.Session
	s.ID = "sesn_2"
	s.Status = "terminated"
	s.Agent.Model = json.RawMessage(`"claude-opus-5-5"`)
	var events []managed.Event
	for _, raw := range []string{
		`{"id": "a", "type": "agent.message", "content": [{"type": "text", "text": "hi"}]}`,
		`{"id": "b", "type": "session.usage", "usage": {"input_tokens": 5, "list_cost": {"amount": "1", "currency": "USD"}}}`,
		`{"id": "c", "type": "session.usage", "usage": {"input_tokens": 9, "list_cost": {"amount": "3", "currency": "USD"}}}`,
		`{"id": "d", "type": "session.error", "error": {"type": "credential_host_unreachable_error", "message": "vault vlt_123 credential cred_9"}}`,
		`{"id": "e", "type": "session.error", "error": {"type": "model_overloaded_error", "message": "overloaded"}}`,
	} {
		var e managed.Event
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	run, err := managed.Map(s, events)
	if err != nil {
		t.Fatal(err)
	}
	if run.Session.Kind != session.KindChat || run.Status != agentrun.Error {
		t.Errorf("session %+v", run.Session)
	}
	if run.Events[0].Kind != session.AgentMessage {
		t.Errorf("a chat has no report, got %s", run.Events[0].Kind)
	}
	if msg := body[eval.ErrorBody](t, run.Events[1]).Message; msg != "credential_host_unreachable_error" {
		t.Errorf("credential error kept its message: %q", msg)
	}
	if msg := body[eval.ErrorBody](t, run.Events[2]).Message; msg != "model_overloaded_error: overloaded" {
		t.Errorf("error %q", msg)
	}
	u := body[session.UsageBody](t, run.Events[3])
	if u.InputTokens != 9 || u.CostMicroUSD != 30_000 || u.Model != "claude-opus-5-5" {
		t.Errorf("usage %+v; want the newest snapshot", u)
	}
}

func TestMapRefusesAnotherCurrency(t *testing.T) {
	var s managed.Session
	if err := json.Unmarshal([]byte(`{"id": "x", "usage": {"list_cost": {"amount": "1", "currency": "EUR"}}}`), &s); err != nil {
		t.Fatal(err)
	}
	if _, err := managed.Map(s, nil); err == nil {
		t.Fatal("a cost in EUR was read as dollars")
	}
}
