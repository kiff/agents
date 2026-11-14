package planque

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type catalogTransport func(*http.Request) (*http.Response, error)

func (f catalogTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func catalogFixture(t *testing.T, serve func(*http.Request) (int, string)) *Tool {
	t.Helper()
	return New("https://planque.example", "account-example", "test-own-key", &http.Client{Transport: catalogTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/accounts/account-example/grants/grant-example/catalog" || r.Header.Get("Authorization") != "Bearer test-own-key" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		status, body := serve(r)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})})
}

func pageJSON(tools []map[string]any, next string) string {
	b, _ := json.Marshal(map[string]any{"grant": "grant-example", "server": "https://vendor.example", "tools": tools, "next_cursor": next})
	return string(b)
}
func catalogCall(t *testing.T, tool *Tool, args map[string]any) (map[string]any, bool) {
	t.Helper()
	args["action"] = "catalog"
	args["grant"] = "grant-example"
	b, _ := json.Marshal(args)
	res, err := tool.Call(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("invalid catalog JSON: %v (%s)", err, res.Text)
	}
	if len(res.Text) > MaxCatalog {
		t.Fatalf("reply exceeds bound: %d", len(res.Text))
	}
	return out, res.IsError
}

func TestCatalogFollowsVendorPages(t *testing.T) {
	var seen []string
	next := "page +/?&=two"
	tl := catalogFixture(t, func(r *http.Request) (int, string) {
		cursor := r.URL.Query().Get("cursor")
		seen = append(seen, cursor)
		switch cursor {
		case "":
			return 200, pageJSON([]map[string]any{{"name": "append", "input_schema": map[string]any{"type": "object"}}}, next)
		case next:
			return 200, pageJSON(nil, "third")
		case "third":
			return 200, pageJSON([]map[string]any{{"name": "find_page", "description": "Find a page", "read_only": true, "input_schema": map[string]any{"type": "object", "required": []string{"query"}}}}, "")
		default:
			t.Fatalf("cursor was not escaped: %q", cursor)
			return 500, ""
		}
	})
	out, isErr := catalogCall(t, tl, map[string]any{})
	if isErr || out["truncated"] == true || out["next_cursor"] != nil {
		t.Fatalf("incomplete: %v", out)
	}
	tools := out["tools"].([]any)
	if len(tools) != 2 || tools[1].(map[string]any)["name"] != "find_page" {
		t.Fatalf("lost later tool: %v", out)
	}
	if !reflect.DeepEqual(seen, []string{"", next, "third"}) {
		t.Fatalf("pages %v", seen)
	}
	seen = nil
	detail, isErr := catalogCall(t, tl, map[string]any{"tool": "find_page"})
	if isErr || len(detail["tools"].([]any)) != 1 || detail["tools"].([]any)[0].(map[string]any)["input_schema"] == nil {
		t.Fatalf("detail: %v", detail)
	}
	if len(seen) != 3 {
		t.Fatalf("detail searched %d pages", len(seen))
	}
	res, err := tl.Call(context.Background(), json.RawMessage(`{"action":"catalog","grant":"grant-example","tool":"absent"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Text, "no tool named absent") || !strings.Contains(res.Text, "catalog {grant, q}") {
		t.Fatalf("absent: %+v %v", res, err)
	}
}

// A managed app's tool is found by its provider's slug as by its name: the
// app's guide and a search name the slug, the catalog another name (14 Nov,
// Gmail: "the catalog has no tool named GMAIL_FETCH_EMAILS").
func TestCatalogFindsAManagedToolBySlug(t *testing.T) {
	tl := catalogFixture(t, func(r *http.Request) (int, string) {
		return 200, pageJSON([]map[string]any{
			{"name": "connector_aa11", "slug": "MAIL_LIST_THREADS", "input_schema": map[string]any{"type": "object"}},
			{"name": "connector_bb22", "slug": "MAIL_FETCH_EMAILS", "description": "Lists messages", "read_only": true, "input_schema": map[string]any{"type": "object"}},
		}, "")
	})
	for _, asked := range []string{"MAIL_FETCH_EMAILS", "mail_fetch_emails", "connector_bb22"} {
		out, isErr := catalogCall(t, tl, map[string]any{"tool": asked})
		tools, _ := out["tools"].([]any)
		if isErr || len(tools) != 1 || tools[0].(map[string]any)["name"] != "connector_bb22" || tools[0].(map[string]any)["slug"] != "MAIL_FETCH_EMAILS" {
			t.Fatalf("by %q: %v", asked, out)
		}
	}
	if res, err := tl.Call(context.Background(), json.RawMessage(`{"action":"catalog","grant":"grant-example","tool":"MAIL_SEND"}`)); err != nil || !res.IsError {
		t.Fatalf("an absent slug: %+v %v", res, err)
	}
}

func TestCatalogResumesInsideVendorPage(t *testing.T) {
	const count = 4000
	tools := make([]map[string]any, count)
	for i := range tools {
		tools[i] = map[string]any{"name": fmt.Sprintf("tool_%04d_%s", i, strings.Repeat("x", 40)), "description": strings.Repeat("words ", 20), "input_schema": map[string]any{"type": "object"}}
	}
	calls := 0
	tl := catalogFixture(t, func(r *http.Request) (int, string) { calls++; return 200, pageJSON(tools, "") })
	names := map[string]bool{}
	cursor := ""
	for turn := 0; turn < 10; turn++ {
		out, isErr := catalogCall(t, tl, map[string]any{"cursor": cursor})
		if isErr {
			t.Fatalf("catalog: %v", out)
		}
		for _, raw := range out["tools"].([]any) {
			name := raw.(map[string]any)["name"].(string)
			if names[name] {
				t.Fatalf("listed twice: %s", name)
			}
			names[name] = true
		}
		next, _ := out["next_cursor"].(string)
		if next == "" {
			break
		}
		if out["truncated"] != true || next == cursor || strings.Contains(fmt.Sprint(out["note"]), "Every tool is listed") {
			t.Fatalf("false completeness: %v", out["note"])
		}
		cursor = next
	}
	if len(names) != count || calls < 2 {
		t.Fatalf("recovered %d/%d names in %d calls", len(names), count, calls)
	}
}

func TestCatalogResumesAfterPageLimit(t *testing.T) {
	tl := catalogFixture(t, func(r *http.Request) (int, string) {
		cursor := r.URL.Query().Get("cursor")
		if cursor == "" {
			cursor = "0"
		}
		var n int
		fmt.Sscanf(cursor, "%d", &n)
		next := fmt.Sprint(n + 1)
		if n == 20 {
			next = ""
		}
		return 200, pageJSON([]map[string]any{{"name": fmt.Sprintf("tool_%d", n)}}, next)
	})
	out, isErr := catalogCall(t, tl, map[string]any{})
	if isErr || len(out["tools"].([]any)) != 20 || out["truncated"] != true || out["next_cursor"] == nil {
		t.Fatalf("bound: %v", out)
	}
	rest, isErr := catalogCall(t, tl, map[string]any{"cursor": out["next_cursor"]})
	if isErr || rest["truncated"] == true || rest["tools"].([]any)[0].(map[string]any)["name"] != "tool_20" {
		t.Fatalf("rest: %v", rest)
	}
	search, isErr := catalogCall(t, tl, map[string]any{"tool": "tool_20"})
	if !isErr || search["next_cursor"] == nil || !strings.Contains(fmt.Sprint(search["note"]), "incomplete") {
		t.Fatalf("false absent: %v", search)
	}
	found, isErr := catalogCall(t, tl, map[string]any{"tool": "tool_20", "cursor": search["next_cursor"]})
	if isErr || found["tools"].([]any)[0].(map[string]any)["name"] != "tool_20" {
		t.Fatalf("continued search: %v", found)
	}
}

func TestCatalogAcceptsWireLargerThanModelReply(t *testing.T) {
	tl := catalogFixture(t, func(r *http.Request) (int, string) {
		return 200, pageJSON([]map[string]any{
			{"name": "first", "description": strings.Repeat("a", 600000)},
			{"name": "second", "description": strings.Repeat("b", 600000)},
			{"name": "search", "input_schema": map[string]any{"type": "object", "description": strings.Repeat("<", 10000)}},
		}, "")
	})
	out, isErr := catalogCall(t, tl, map[string]any{})
	if isErr || len(out["tools"].([]any)) != 3 || out["truncated"] == true || out["note"] == nil {
		t.Fatalf("catalog: %v", out)
	}
	full, isErr := catalogCall(t, tl, map[string]any{"tool": "search"})
	if isErr || full["tools"].([]any)[0].(map[string]any)["input_schema"] == nil {
		t.Fatalf("full: %v", full)
	}
}

func TestCatalogRejectsBrokenPagination(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"cycle", 200, pageJSON(nil, "loop")},
		{"failed second page", 503, `{"error":"try later"}`},
		{"invalid second page", 200, "{"},
		{"changed grant", 200, `{"grant":"other","server":"https://vendor.example","tools":[]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			tl := catalogFixture(t, func(r *http.Request) (int, string) {
				calls++
				if calls == 1 {
					return 200, pageJSON(nil, "loop")
				}
				return test.status, test.body
			})
			res, err := tl.Call(context.Background(), json.RawMessage(`{"action":"catalog","grant":"grant-example","tool":"find_page"}`))
			if err == nil && !res.IsError {
				t.Fatalf("reported success: %+v", res)
			}
			if strings.Contains(res.Text, "no tool named") || calls > 2 {
				t.Fatalf("false absent or unbounded loop: %v (%d)", res, calls)
			}
		})
	}
}

func TestCatalogDoesNotClaimSkippedEntriesAreComplete(t *testing.T) {
	tl := catalogFixture(t, func(*http.Request) (int, string) {
		return 200, `{"grant":"grant-example","server":"https://vendor.example","tools":[{"name":"append"}],"skipped":1}`
	})
	out, isErr := catalogCall(t, tl, map[string]any{})
	if isErr || out["truncated"] != true || out["skipped"] != float64(1) || strings.Contains(fmt.Sprint(out["note"]), "Every tool is listed") {
		t.Fatalf("false completeness: %v", out)
	}
	search, isErr := catalogCall(t, tl, map[string]any{"tool": "find_page"})
	if !isErr || !strings.Contains(fmt.Sprint(search["note"]), "incomplete") {
		t.Fatalf("skipped entry claimed absent: %v", search)
	}
}

type brokenCatalogBody struct{}

func (brokenCatalogBody) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (brokenCatalogBody) Close() error             { return nil }

func TestCatalogHandlesWireFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		body io.ReadCloser
	}{
		{"read failure", brokenCatalogBody{}},
		{"too large", io.NopCloser(strings.NewReader(strings.Repeat(" ", (16<<20)+1)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tl := New("https://planque.example", "account-example", "test-own-key", &http.Client{Transport: catalogTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: tc.body}, nil
			})})
			_, err := tl.Call(context.Background(), json.RawMessage(`{"action":"catalog","grant":"grant-example"}`))
			if err == nil {
				t.Fatal("wire error was ignored")
			}
		})
	}
	calls := 0
	tl := catalogFixture(t, func(*http.Request) (int, string) { calls++; return 200, pageJSON(nil, "") })
	for _, cursor := range []string{"not:a:cursor", strings.Repeat("a", 5000)} {
		b, _ := json.Marshal(map[string]any{"action": "catalog", "grant": "grant-example", "cursor": cursor})
		res, err := tl.Call(context.Background(), b)
		if err != nil || !res.IsError || calls != 0 {
			t.Fatalf("invalid cursor reached vendor: %+v %v", res, err)
		}
	}
}
