package drafts_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kiff/agents/drafts"
	"github.com/kiff/agents/drafts/kiffapi"
	"github.com/kiff/agents/store/memory"
	tooldrafts "github.com/kiff/agents/tool/drafts"
)

// The model reads the draft's id, agent and status; the claim secret KIFF
// sent goes to the store. KIFF's refusal is a result the model can read.
func TestSubmit(t *testing.T) {
	refuse := false
	var body, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		body, auth = string(b), r.Header.Get("Authorization")
		if refuse {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"card 0: at least one grant is required"}`))
			return
		}
		w.WriteHeader(201)
		w.Write([]byte(`{"id":"drf_1","agent_id":"agt_refunds","status":"draft","version":"v1","claim_secret":"kiff_claim_s3cret"}`))
	}))
	defer srv.Close()
	m := memory.NewDrafts()
	tl := tooldrafts.New(&drafts.Service{Store: m, Keys: m, KIFF: kiffapi.New(srv.URL, nil)}, "t1", "kiff_key_setup")
	r, err := tl.Call(context.Background(), json.RawMessage(`{"action":"submit","agent_id":"agt_refunds","label":"Refunds","cards":[{"id":"c1"}]}`))
	if err != nil || r.IsError {
		t.Fatalf("%+v %v", r, err)
	}
	if strings.Contains(r.Text, "kiff_claim") || !strings.Contains(r.Text, `"id":"drf_1"`) {
		t.Fatalf("the model read %s", r.Text)
	}
	if auth != "Bearer kiff_key_setup" || body != `{"agent_id":"agt_refunds","cards":[{"id":"c1"}],"label":"Refunds"}` {
		t.Fatalf("sent %s with %q", body, auth)
	}
	if rec, _ := m.Get(context.Background(), "t1", "drf_1"); rec.Secret != "kiff_claim_s3cret" {
		t.Fatalf("kept %+v", rec)
	}
	refuse = true
	r, err = tl.Call(context.Background(), json.RawMessage(`{"action":"submit","agent_id":"agt_x","cards":[{}]}`))
	if err != nil || !r.IsError || !strings.Contains(r.Text, "at least one grant") {
		t.Fatalf("refused: %+v %v", r, err)
	}
	for _, in := range []string{`{"action":"claim","agent_id":"a","cards":[]}`, `{"action":"submit","cards":[]}`, `{"action":"submit","agent_id":"a","cards":{}}`, `{"action":"submit","agent_id":"a","cards":[],"secret":"x"}`} {
		if r, _ := tl.Call(context.Background(), json.RawMessage(in)); !r.IsError {
			t.Errorf("%s was accepted", in)
		}
	}
}
