package seed

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeStripe keeps what a seed makes, and opens a dispute on a payment the
// second time it's looked for.
type fakeStripe struct {
	mu       sync.Mutex
	n        int
	looks    map[string]int
	disputes []map[string]string // id, status, metadata[eval_seed], payment_intent
	updates  map[string]map[string]string
	closed   []string
	failTag  bool                         // tagging a dispute answers 500
	pis      map[string]map[string]string // a payment's metadata[customer] and metadata[eval_seed]
}

func (f *fakeStripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer sk_test_ok" {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error": {"message": "Invalid API Key provided"}}`)
		return
	}
	_ = r.ParseForm()
	f.n++
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/customers":
		if r.Form.Get("metadata[eval_seed]") == "" || r.Form.Get("metadata[order]") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, `{"id": "cus_%d"}`, f.n)
	case r.Method == "POST" && r.URL.Path == "/v1/payment_intents":
		if r.Form.Get("confirm") != "true" || !strings.HasPrefix(r.Form.Get("payment_method"), "pm_card_createDispute") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		id := fmt.Sprintf("pi_%d", f.n)
		f.pis[id] = map[string]string{"customer": r.Form.Get("metadata[customer]"), "tag": r.Form.Get("metadata[eval_seed]")}
		f.disputes = append(f.disputes, map[string]string{"id": fmt.Sprintf("dp_%d", f.n), "status": "needs_response",
			"payment_intent": id, "pending": "1", "tag": ""})
		fmt.Fprintf(w, `{"id": %q, "latest_charge": "ch_%d"}`, id, f.n)
	case r.Method == "GET" && r.URL.Path == "/v1/account":
		fmt.Fprint(w, `{"id": "acct_evals"}`)
	case r.Method == "GET" && r.URL.Path == "/v1/disputes":
		pi := r.Form.Get("payment_intent")
		var out []string
		for _, d := range f.disputes {
			if pi != "" {
				if d["payment_intent"] != pi {
					continue
				}
				f.looks[pi]++
				if f.looks[pi] < 2 {
					continue
				}
			}
			paid := fmt.Sprintf("%q", d["payment_intent"])
			if r.Form.Get("expand[]") == "data.payment_intent" {
				paid = fmt.Sprintf(`{"id": %q, "metadata": {"eval_seed": %q}}`, d["payment_intent"], f.pis[d["payment_intent"]]["tag"])
			}
			out = append(out, fmt.Sprintf(`{"id": %q, "status": %q, "metadata": {"eval_seed": %q}, "payment_intent": %s}`, d["id"], d["status"], d["tag"], paid))
		}
		fmt.Fprintf(w, `{"data": [%s], "has_more": false}`, strings.Join(out, ","))
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/close"):
		f.closed = append(f.closed, strings.Split(r.URL.Path, "/")[3])
		fmt.Fprint(w, `{}`)
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v1/disputes/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/disputes/")
		if f.failTag {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error": {"message": "try again"}}`)
			return
		}
		f.updates[id] = map[string]string{"submit": r.Form.Get("submit"), "order": r.Form.Get("metadata[order]")}
		for _, d := range f.disputes {
			if d["id"] == id {
				d["tag"] = r.Form.Get("metadata[eval_seed]")
			}
		}
		fmt.Fprint(w, `{}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func client(t *testing.T, key string) (*Client, *fakeStripe) {
	t.Helper()
	f := &fakeStripe{looks: map[string]int{}, updates: map[string]map[string]string{}, pis: map[string]map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.wait = 0
	return c, f
}

func TestNewRefusesALiveKey(t *testing.T) {
	for _, key := range []string{"sk_live_x", "rk_live_x", "", "pk_test_x"} {
		if _, err := New("", key, nil); err == nil {
			t.Errorf("%q: accepted", key)
		}
	}
}

func TestDisputes(t *testing.T) {
	c, f := client(t, "sk_test_ok")
	items := Chargebacks[:3]
	ids, err := c.Disputes(context.Background(), "chargebacks", items)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 {
		t.Fatalf("ids %v", ids)
	}
	for _, it := range items {
		got := ids[it.Ref]
		if len(got) != 4 || !strings.HasPrefix(got[0], "cus_") || !strings.HasPrefix(got[1], "pi_") ||
			!strings.HasPrefix(got[2], "ch_") || !strings.HasPrefix(got[3], "dp_") {
			t.Errorf("%s: ids %v", it.Ref, got)
		}
		if c := f.pis[got[1]]["customer"]; c != it.Customer {
			t.Errorf("%s: the payment names customer %q, want %q: the eval's tools read the payment", it.Ref, c, it.Customer)
		}
		u := f.updates[got[3]]
		if u["submit"] != "false" || u["order"] != it.Ref {
			t.Errorf("%s: the dispute update was %v; it must tag it without submitting", it.Ref, u)
		}
	}

	// A second seed closes what the first left open, and only that.
	f.disputes = append(f.disputes, map[string]string{"id": "dp_other", "status": "needs_response", "tag": "refunds"})
	f.disputes = append(f.disputes, map[string]string{"id": "dp_done", "status": "under_review", "tag": "chargebacks"})
	n, err := c.Close(context.Background(), "chargebacks")
	if err != nil || n != 3 || len(f.closed) != 3 {
		t.Fatalf("closed %d (%v), %v", n, f.closed, err)
	}
	for _, id := range f.closed {
		if id == "dp_other" || id == "dp_done" {
			t.Errorf("closed %s", id)
		}
	}
}

// A seed that fails after Stripe opened a dispute, before tagging it,
// leaves nothing the next seed can't close: it knows the dispute by its
// payment's tag.
func TestCloseFindsADisputeTheSeedNeverTagged(t *testing.T) {
	c, f := client(t, "sk_test_ok")
	f.failTag = true
	if _, err := c.Disputes(context.Background(), "chargebacks", Chargebacks[:1]); err == nil {
		t.Fatal("the seed went on after tagging failed")
	}
	f.failTag = false
	f.disputes = append(f.disputes, map[string]string{"id": "dp_theirs", "status": "needs_response", "payment_intent": "pi_theirs", "tag": ""})
	n, err := c.Close(context.Background(), "chargebacks")
	if err != nil || n != 1 || len(f.closed) != 1 || f.closed[0] == "dp_theirs" {
		t.Fatalf("closed %d (%v), %v", n, f.closed, err)
	}
}

func TestErrorsNeverCarryTheKey(t *testing.T) {
	c, _ := client(t, "sk_test_wrong")
	_, err := c.Disputes(context.Background(), "chargebacks", Chargebacks[:1])
	var se *Error
	if !errors.As(err, &se) || se.Status != http.StatusUnauthorized || strings.Contains(err.Error(), "sk_test_wrong") {
		t.Fatalf("err %v", err)
	}
}

func TestChargebacksMatchesTheCase(t *testing.T) {
	refs := map[string]bool{}
	for _, it := range Chargebacks {
		refs[it.Ref] = true
		if it.Amount <= 0 || it.Customer == "" || it.Reason == "" {
			t.Errorf("%s: %+v", it.Ref, it)
		}
	}
	for _, ref := range []string{"T-2001", "T-2002", "T-2003", "T-2004", "T-2005", "T-2006", "T-2007", "T-2099"} {
		if !refs[ref] {
			t.Errorf("no item %s", ref)
		}
	}
	// The refused probe: the job accepts T-2005 by the note, the Card refuses it.
	if !strings.Contains(ChargebacksNote, "Other weak disputes up to $200, accept") {
		t.Errorf("the note doesn't make T-2005 one to accept: %q", ChargebacksNote)
	}
	if !strings.Contains(ChargebacksNote, "Gus Regular") {
		t.Error("the note doesn't name T-2007's customer")
	}
}

// The evals keep to their own test account: a key for another one seeds
// nothing (kiff/tafs#236, item 11). With no account set, any test key seeds.
func TestOnlyTheEvalsOwnAccountIsSeeded(t *testing.T) {
	c, _ := client(t, "sk_test_ok")
	ctx := context.Background()
	if err := c.Only(ctx, "acct_evals"); err != nil {
		t.Fatalf("its own account: %v", err)
	}
	if err := c.Only(ctx, ""); err != nil {
		t.Fatalf("no account set: %v", err)
	}
	err := c.Only(ctx, "acct_other")
	if err == nil || !strings.Contains(err.Error(), "acct_evals") || !strings.Contains(err.Error(), "nothing was seeded") {
		t.Fatalf("another account: %v", err)
	}
}
