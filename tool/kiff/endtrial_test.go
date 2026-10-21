package kiff_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/tool/kiff"
)

// trialAPI is a fake KIFF with the trial routes (kiff/tafs#173): drf_open is
// trying, and ending it answers its final counts; drf_ended was ended
// already (Install), so the end is a 409 and the read says how it ended;
// drf_stuck says it's ended but reads as trying; drf_down fails.
func trialAPI(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			http.Error(w, `{"error":"missing Authorization header"}`, http.StatusUnauthorized)
			return
		}
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "DELETE /v1/me/agent-drafts/drf_open/trial":
			_, _ = w.Write([]byte(`{"status":"ended","ended_by":"expired","calls":4,"allowed":2,"held":1,"refused":1}`))
		case "DELETE /v1/me/agent-drafts/drf_ended/trial", "DELETE /v1/me/agent-drafts/drf_stuck/trial":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"not_trying"}`))
		case "GET /v1/me/agent-drafts/drf_ended/trial":
			_, _ = w.Write([]byte(`{"status":"ended","ended_by":"install","calls":2,"allowed":2,"held":0,"refused":0}`))
		case "GET /v1/me/agent-drafts/drf_stuck/trial":
			_, _ = w.Write([]byte(`{"status":"trying","calls":1}`))
		case "DELETE /v1/me/agent-drafts/drf_down/trial":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestEndTrialAnswersKIFFsCounts(t *testing.T) {
	srv, seen := trialAPI(t)
	ctx := context.Background()
	got, err := kiff.EndTrial(ctx, srv.Client(), srv.URL, testKey, "drf_open")
	if err != nil || got != (registry.TrialCounts{Calls: 4, Held: 1, Refused: 1}) {
		t.Fatalf("EndTrial = %+v, %v", got, err)
	}
	// Ended already: the read says how, with its counts.
	got, err = kiff.EndTrial(ctx, srv.Client(), srv.URL, testKey, "drf_ended")
	if err != nil || got != (registry.TrialCounts{Calls: 2}) {
		t.Fatalf("EndTrial of an ended trial = %+v, %v", got, err)
	}
	if want := "DELETE /v1/me/agent-drafts/drf_ended/trial,GET /v1/me/agent-drafts/drf_ended/trial"; strings.Join((*seen)[1:], ",") != want {
		t.Fatalf("asked %v", *seen)
	}
	for _, draft := range []string{"drf_stuck", "drf_down", "drf_none", ""} {
		if _, err := kiff.EndTrial(ctx, srv.Client(), srv.URL, testKey, draft); err == nil {
			t.Errorf("EndTrial of %q: no error", draft)
		} else if strings.Contains(err.Error(), testKey) {
			t.Errorf("the error carries the key: %v", err)
		}
	}
}
