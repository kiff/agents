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

// State is what is kept here of each draft the chat's components name: it
// never shows the claim secret, and says when nothing is kept of one.
func TestStateSaysWhatIsKeptOfEachShownDraft(t *testing.T) {
	m := memory.NewDrafts()
	ctx := context.Background()
	_ = m.Put(ctx, "t1", "drf_1", drafts.Record{AgentID: "refunds-a1", Secret: "kiff_claim_s3cret"})
	_ = m.Put(ctx, "t1", "drf_2", drafts.Record{AgentID: "refunds-a1", KeyRef: "kiff/refunds-a1", Claimed: true})
	tl := tooldrafts.New(&drafts.Service{Store: m, Keys: m}, "t1", "kiff_key_setup")
	text, err := tl.State(ctx, []json.RawMessage{
		json.RawMessage(`{"type":"approval","draft":"drf_1"}`), json.RawMessage(`{"type":"approval","draft":"drf_2"}`), json.RawMessage(`{"type":"approval","draft":"drf_x"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"drf_1, for agent refunds-a1: submitted; not claimed yet", "drf_2, for agent refunds-a1: confirmed by the owner", "drf_x: nothing kept of it here"} {
		if !strings.Contains(text, want) {
			t.Fatalf("State said %q, want %q in it", text, want)
		}
	}
	if strings.Contains(text, "kiff_claim") {
		t.Fatal("State showed a claim secret")
	}
	if text, _ := tl.State(ctx, nil); text != "" {
		t.Fatalf("with no draft shown, State said %q", text)
	}
}
