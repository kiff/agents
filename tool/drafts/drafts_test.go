package drafts_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kiff/agents/drafts"
	"github.com/kiff/agents/drafts/kiffapi"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
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
	if strings.Contains(r.Text, "kiff_claim") || r.Text != `{"id":"drf_1","agent_id":"agt_refunds","status":"draft"}` {
		t.Fatalf("the model read %s", r.Text)
	}
	// What KIFF takes goes as the model wrote it: each Card's id and the rest.
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
	for _, in := range []string{`{"action":"claim","agent_id":"a","cards":[]}`, `{"action":"submit","cards":[]}`, `{"action":"submit","agent_id":"a","cards":{}}`, `{"action":"submit","agent_id":"a","cards":[],"secret":"x"}`,
		`{"action":"submit","agent_id":"a","cards":[1]}`} {
		if r, _ := tl.Call(context.Background(), json.RawMessage(in)); !r.IsError {
			t.Errorf("%s was accepted", in)
		}
	}
}

// kiffCards answers drafts as KIFF's draftCards does: a Card whose id the
// draft's agent holds is revised, a new id is a new Card, and any other
// existing id is refused (409), naming the first such Card by its place.
// held is each existing Card's id and its agent; sent is every body.
func kiffCards(t *testing.T, held map[string]string, sent *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			AgentID string `json:"agent_id"`
			Cards   []struct {
				ID string `json:"id"`
			} `json:"cards"`
		}
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		*sent = append(*sent, string(b))
		if err := json.Unmarshal(b, &in); err != nil {
			t.Errorf("KIFF got %s: %v", b, err)
		}
		revision := false
		for i, c := range in.Cards {
			agent, ok := held[c.ID]
			if ok && agent != in.AgentID {
				w.WriteHeader(http.StatusConflict)
				w.Write([]byte(`{"error":"card ` + strconv.Itoa(i) + `: a Card with this id exists, and it isn't this agent's alone and in force"}`))
				return
			}
		}
		for _, agent := range held {
			revision = revision || agent == in.AgentID
		}
		w.WriteHeader(201)
		if revision {
			w.Write([]byte(`{"id":"drf_r","agent_id":"` + in.AgentID + `","status":"draft","revision":true}`))
			return
		}
		w.Write([]byte(`{"id":"drf_n","agent_id":"` + in.AgentID + `","status":"draft","claim_secret":"kiff_claim_s3cret"}`))
	}))
}

// A Card the agent holds goes by its own id, so an improvement revises it
// (kiff/agents#359's review). Only a Card KIFF refuses as one the agent
// doesn't hold goes again, under the agent's name: once each, in the same
// call, and the model reads that it is a new Card.
func TestADraftRenamesOnlyTheCardsKIFFRefused(t *testing.T) {
	held := map[string]string{"reads": "agt_refunds", "c1": "agt_old", "writes": "agt_old", "agt_new-taken": "agt_old"}
	var sent []string
	srv := kiffCards(t, held, &sent)
	defer srv.Close()
	m := memory.NewDrafts()
	tl := tooldrafts.New(&drafts.Service{Store: m, Keys: m, KIFF: kiffapi.New(srv.URL, nil)}, "t1", "kiff_key_setup")
	submit := func(agent, cards string) tool.Result {
		t.Helper()
		sent = nil
		r, err := tl.Call(context.Background(), json.RawMessage(`{"action":"submit","agent_id":"`+agent+`","label":"Refunds","cards":`+cards+`}`))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// An improvement for an agent holding "reads" that names "reads" keeps "reads": KIFF revises it.
	r := submit("agt_refunds", `[{"id":"reads","subject":"agt_refunds","aggregates":[{"quantity":"count","limit":20,"window":"rolling_7d"}]}]`)
	if r.IsError || r.Text != `{"id":"drf_r","agent_id":"agt_refunds","status":"draft","revision":true}` ||
		!slices.Equal(sent, []string{`{"agent_id":"agt_refunds","cards":[{"aggregates":[{"quantity":"count","limit":20,"window":"rolling_7d"}],"id":"reads","subject":"agt_refunds"}],"label":"Refunds"}`}) {
		t.Fatalf("the revision: %+v, sent %q", r, sent)
	}

	// The same improvement adding "writes", another agent's: "reads" stays, and only "writes" goes under the agent's name.
	r = submit("agt_refunds", `[{"id":"reads"},{"id":"writes"}]`)
	if r.IsError || !slices.Equal(sent, []string{
		`{"agent_id":"agt_refunds","cards":[{"id":"reads"},{"id":"writes"}],"label":"Refunds"}`,
		`{"agent_id":"agt_refunds","cards":[{"id":"reads"},{"id":"agt_refunds-writes"}],"label":"Refunds"}`,
	}) {
		t.Fatalf("the revision with a new Card: %+v, sent %q", r, sent)
	}
	if !strings.HasPrefix(r.Text, `{"id":"drf_r",`) || !strings.Contains(r.Text, `the id "writes" that this agent doesn't hold alone and in force`) ||
		!strings.Contains(r.Text, "(agt_refunds-…): a new Card, not a revision") || strings.Contains(r.Text, `"reads"`) {
		t.Fatalf("the model read %s", r.Text)
	}

	// A new taf whose two ids are another agent's: each goes again under its name, one KIFF answer each, no turn.
	r = submit("agt_new", `[{"id":"c1","grants":[{"action":"list_inboxes"}]},{"id":"writes"}]`)
	if r.IsError || len(sent) != 3 || sent[2] != `{"agent_id":"agt_new","cards":[{"grants":[{"action":"list_inboxes"}],"id":"agt_new-c1"},{"id":"agt_new-writes"}],"label":"Refunds"}` ||
		!strings.Contains(r.Text, `the id "c1", "writes" that`) || strings.Contains(r.Text, "kiff_claim") {
		t.Fatalf("the new taf: %+v, sent %q", r, sent)
	}
	if rec, _ := m.Get(context.Background(), "t1", "drf_n"); rec.Secret != "kiff_claim_s3cret" || rec.AgentID != "agt_new" {
		t.Fatalf("kept %+v", rec)
	}

	// A refused id already under the agent's name has nowhere else to go: KIFF's words reach the model, after one answer.
	r = submit("agt_new", `[{"id":"agt_new-taken"}]`)
	if !r.IsError || len(sent) != 1 || r.Text != "KIFF answered 409: card 0: a Card with this id exists, and it isn't this agent's alone and in force" {
		t.Fatalf("the refused own id: %+v, sent %q", r, sent)
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

// granted is a fake of the setup agent's planque listing (tool.Granted).
type granted struct {
	grants [][]string
	known  bool
	err    error
}

func (g granted) AgentTools(context.Context, string) ([][]string, bool, error) {
	return g.grants, g.known, g.err
}

// A draft whose Cards leave one of the taf's granted tools uncovered isn't
// sent: the model reads which, and drafts again. Its own reads were never
// listed. When planque can't be read, or can't tell its own reads apart,
// the draft goes as before (Eval 42781271564: get_charge granted, its Card
// covering only read_charge, every call of it refused). A connection the
// draft names none of is left to the Cards in force.
func TestADraftThatLeavesAGrantedToolUncoveredIsNotSent(t *testing.T) {
	sent := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent++
		w.WriteHeader(201)
		w.Write([]byte(`{"id":"drf_1","agent_id":"agt_refunds","status":"draft","version":"v1","claim_secret":"kiff_claim_s3cret"}`))
	}))
	defer srv.Close()
	m := memory.NewDrafts()
	newTool := func(g tool.Granted) *tooldrafts.Tool {
		tl := tooldrafts.New(&drafts.Service{Store: m, Keys: m, KIFF: kiffapi.New(srv.URL, nil)}, "t1", "kiff_key_setup")
		tl.CheckGrants(g)
		return tl
	}
	draft := json.RawMessage(`{"action":"submit","agent_id":"agt_refunds","label":"Refunds","cards":[` +
		`{"id":"refunds","grants":[{"action":"Create_Refund_k3"},{"action":" read_charge "}]}]}`)
	given := [][]string{{"create_refund_k3", "read_charge", "get_charge"}}

	r, err := newTool(granted{grants: given, known: true}).Call(context.Background(), draft)
	if err != nil || !r.IsError || !strings.Contains(r.Text, "get_charge") || strings.Contains(r.Text, "read_charge,") || sent != 0 {
		t.Fatalf("an uncovered tool: %+v %v, sent %d", r, err, sent)
	}
	for name, g := range map[string]granted{
		"every tool covered":     {grants: [][]string{{"create_refund_k3", "read_charge"}}, known: true},
		"an older planque":       {grants: given, known: false},
		"planque can't be read":  {err: context.DeadlineExceeded},
		"no grant connected yet": {known: true},
		// A revision whose Cards cover only its new connection: the older
		// one's tools are on Cards in force the setup agent can't read.
		"another connection untouched": {grants: [][]string{{"create_refund_k3", "read_charge"}, {"list_disputes", "update_dispute"}}, known: true},
	} {
		before := sent
		if r, err := newTool(g).Call(context.Background(), draft); err != nil || r.IsError || sent != before+1 {
			t.Errorf("%s: %+v %v", name, r, err)
		}
	}
	// No planque to ask: the draft goes, as before.
	tl := tooldrafts.New(&drafts.Service{Store: m, Keys: m, KIFF: kiffapi.New(srv.URL, nil)}, "t1", "kiff_key_setup")
	if r, _ := tl.Call(context.Background(), draft); r.IsError {
		t.Errorf("without a planque: %+v", r)
	}
}
