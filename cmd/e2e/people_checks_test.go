package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func stepNinePeople() (admin, maria, luis *someone, people []*someone) {
	maria = &someone{name: "maria", role: "editor", inside: "ch_e2e_maria_1", held: "ch_e2e_mariaheld_1"}
	luis = &someone{name: "luis", role: "viewer", inside: "ch_e2e_luis_1", held: "ch_e2e_luisheld_1"}
	return &someone{name: "the admin"}, maria, luis, []*someone{maria, luis}
}

// Who sees which held call in Needs you: the admin all; an editor their own
// and the ones they may answer, which separation of duties takes away; a
// viewer only their own.
func TestWhoSeesWhichHold(t *testing.T) {
	admin, maria, luis, people := stepNinePeople()
	for _, c := range []struct {
		r         *someone
		separated bool
		want      []string
	}{
		{admin, false, []string{maria.held, luis.held}},
		{admin, true, []string{maria.held, luis.held}},
		{maria, false, []string{maria.held, luis.held}},
		{maria, true, []string{maria.held}},
		{luis, false, []string{luis.held}},
		{luis, true, []string{luis.held}},
	} {
		if got := wantedHolds(c.r, people, c.separated); !sameSet(got, c.want) {
			t.Errorf("%s, separated %v: %v; want %v", c.r.name, c.separated, got, c.want)
		}
	}
	if ownRefunds(admin, people) != 2 || ownRefunds(maria, people) != 1 || ownRefunds(luis, people) != 1 {
		t.Error("the admin sees both sent refunds, each person their own")
	}
}

// A held call is told apart by the run's charge it acts on, and only the
// shared taf's count; a sent refund is a write KIFF allowed, not a read and
// not a hold.
func TestHoldsAndRefundsAreReadFromKIFF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/me/card-exceptions":
			_ = json.NewEncoder(w).Encode(map[string]any{"exceptions": []any{
				map[string]any{"id": "exc_1", "subject": "taf-shared", "resources": map[string]any{"charge": "ch_e2e_luisheld_1"}},
				map[string]any{"id": "exc_2", "subject": "taf-other", "resources": map[string]any{"charge": "ch_e2e_x"}},
				map[string]any{"id": "exc_3", "subject": "taf-shared", "parameters": map[string]any{"charge": "ch_e2e_mariaheld_1", "amount": 900}},
			}})
		case "/v1/me/activity":
			if r.URL.Query().Get("actor") != "taf-shared" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"calls": []any{
				map[string]any{"outcome": "allowed"},
				map[string]any{"outcome": "allowed", "read_only": true},
				map[string]any{"outcome": "allowed", "held": true},
				map[string]any{"outcome": "refused"},
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	reader := &client{base: srv.URL, http: srv.Client()}
	hs, err := holdsOf(context.Background(), reader, "taf-shared")
	if err != nil || len(hs) != 2 || hs[0] != (hold{"exc_1", "ch_e2e_luisheld_1"}) || hs[1].charge != "ch_e2e_mariaheld_1" {
		t.Fatalf("holds %v %v", hs, err)
	}
	if n, err := refundCount(context.Background(), reader, "taf-shared"); err != nil || n != 1 {
		t.Fatalf("refunds %d %v", n, err)
	}
}

// The people's secrets are both there or neither; a key that doesn't parse
// is an error.
func TestPeopleFromTheEnvironment(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	key := base64.StdEncoding.EncodeToString(der)
	full := map[string]string{"E2E_MARIA_SESSION": "s1", "E2E_MARIA_PHONE_KEY": key, "E2E_LUIS_SESSION": "s2", "E2E_LUIS_PHONE_KEY": key}
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	ps, err := peopleFrom(get(full), "https://kiff.test", "https://cloud.test", http.DefaultClient)
	if err != nil || len(ps) != 2 || ps[0].name != "maria" || ps[0].role != "editor" || ps[1].role != "viewer" {
		t.Fatalf("%v %v", ps, err)
	}
	if ps, err := peopleFrom(get(map[string]string{}), "", "", http.DefaultClient); err != nil || ps != nil {
		t.Fatalf("none set: %v %v", ps, err)
	}
	half := map[string]string{"E2E_MARIA_SESSION": "s1", "E2E_MARIA_PHONE_KEY": key}
	if _, err := peopleFrom(get(half), "", "", http.DefaultClient); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("one set: %v", err)
	}
	bad := map[string]string{"E2E_MARIA_SESSION": "s1", "E2E_MARIA_PHONE_KEY": "x", "E2E_LUIS_SESSION": "s2", "E2E_LUIS_PHONE_KEY": key}
	if _, err := peopleFrom(get(bad), "", "", http.DefaultClient); err == nil {
		t.Fatal("a key that doesn't parse")
	}
}
