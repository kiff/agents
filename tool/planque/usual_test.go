package planque

import (
	"encoding/json"
	"strings"
	"testing"
)

// planque keeps usual_access on the request's connector; the model reads it at
// the grant's top, so it can say a sign-in is wider before the owner signs in
// (kiff/tafs connect-an-app@28). A narrowed sign-in carries none, and nothing
// else of the request reaches the model.
func TestAGrantSaysWhenItsSignInIsWider(t *testing.T) {
	for _, c := range []struct {
		answer string
		wide   bool
	}{
		{`{"id":"grt_1","status":"pending","request":{"connector":{"usual_access":true,"scopes":["a"]}}}`, true},
		{`{"id":"grt_2","status":"pending","request":{"connector":{"scopes":["a"]}}}`, false},
		{`{"id":"grt_3","status":"pending","request":{}}`, false},
	} {
		var g grantView
		if err := json.Unmarshal([]byte(c.answer), &g); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(g)
		if g.UsualAccess != c.wide || strings.Contains(string(b), `"usual_access"`) != c.wide || strings.Contains(string(b), "scopes") || g.Status != "pending" {
			t.Errorf("%s: read as %s", c.answer, b)
		}
	}
	// Wrapped, as planque answers a catalog grant.
	var made struct {
		Grant grantView `json:"grant"`
	}
	if err := json.Unmarshal([]byte(`{"grant":{"id":"grt_9","request":{"connector":{"usual_access":true}}},"needs_key":false}`), &made); err != nil || !made.Grant.UsualAccess || made.Grant.ID != "grt_9" {
		t.Fatalf("a wrapped grant: %+v %v", made.Grant, err)
	}
}
