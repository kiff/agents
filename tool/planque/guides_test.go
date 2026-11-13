package planque_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/tool/planque"
)

// The tool attaches an app's guide itself, as data, never by the model's
// choice: a search carries the guides of the first apps it names (at most
// three, in its order), a request by entry carries that app's. An app with
// no guide is left out, and a guide checked long ago says so.
func TestGuidesComeWithSearchAndRequest(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/accounts/tnt_1/catalog/tools":
			w.Write([]byte(`{"tools":[{"entry":"connector:composio:gmail","slug":"GMAIL_SEND"},{"entry":"connector:composio:gmail","slug":"GMAIL_LIST"},{"entry":"connector:composio:googlesheets","slug":"SHEETS_READ"},{"entry":"qonto-key@2","slug":"list_invoices"},{"entry":"connector:composio:slack","slug":"SLACK_POST"}]}`))
		case "/v1/accounts/tnt_1/catalog/connector:composio:gmail/guide":
			w.Write([]byte(`{"entry":"connector:composio:gmail","app":"gmail","checked":"` + today + `","guide":"# Gmail\n\nKeep the thread id."}`))
		case "/v1/accounts/tnt_1/catalog/qonto-key@2/guide":
			w.Write([]byte(`{"entry":"qonto-key@2","app":"qonto","checked":"2020-01-01","guide":"# Qonto"}`))
		case "/v1/accounts/tnt_1/catalog/connector:composio:gmail/grants":
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"grant":{"id":"grt_1","status":"pending"},"needs_key":false}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"no guide for this app yet"}`))
		}
	}))
	t.Cleanup(srv.Close)
	tl := planque.New(srv.URL, "tnt_1", "k_setup", nil)

	text, isErr := call(t, tl, `{"action":"search","q":"send reminder"}`)
	var found struct {
		Guides []planque.Guide `json:"guides"`
		Note   string          `json:"guides_note"`
		Tools  []any           `json:"tools"`
	}
	if isErr || json.Unmarshal([]byte(text), &found) != nil || len(found.Tools) != 5 {
		t.Fatalf("search: %s", text)
	}
	if len(found.Guides) != 2 || found.Guides[0].App != "gmail" || found.Guides[1].App != "qonto" || found.Note != planque.GuideNote {
		t.Fatalf("guides: %+v", found.Guides)
	}
	if found.Guides[0].Stale != "" || !strings.Contains(found.Guides[1].Stale, "90 days") {
		t.Fatalf("staleness: %+v", found.Guides)
	}
	for _, p := range asked {
		if strings.Contains(p, "slack") {
			t.Fatalf("a fourth app's guide was asked for: %v", asked)
		}
	}

	text, isErr = call(t, tl, `{"action":"request","entry":"connector:composio:gmail","agent":"reminders-a1"}`)
	var made struct {
		ID    string         `json:"id"`
		Guide *planque.Guide `json:"guide"`
	}
	if isErr || json.Unmarshal([]byte(text), &made) != nil || made.ID != "grt_1" || made.Guide == nil || made.Guide.App != "gmail" {
		t.Fatalf("request: %s", text)
	}

	// Planque's search with no app that has a guide: the answer as it was.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/accounts/tnt_1/catalog/tools" {
			w.Write([]byte(`{"tools":[{"entry":"connector:composio:slack","slug":"SLACK_POST"}]}`))
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(srv2.Close)
	if text, _ := call(t, planque.New(srv2.URL, "tnt_1", "k_setup", nil), `{"action":"search","q":"post"}`); strings.Contains(text, "guides") {
		t.Fatalf("guides on a search with none: %s", text)
	}
}
