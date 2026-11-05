package session

import "testing"

// A call's input digest tells a retry (the same input) from other work with
// the same tool, and reveals nothing of the input.
func TestInputHashTellsARetryFromOtherWork(t *testing.T) {
	a, again, b := InputHash([]byte(`{"to":"A"}`)), InputHash([]byte(`{"to":"A"}`)), InputHash([]byte(`{"to":"B"}`))
	if a == "" || a != again || a == b || len(a) != 16 {
		t.Fatalf("hashes: %q %q %q", a, again, b)
	}
	if InputHash(nil) != "" || InputHash([]byte{}) != "" {
		t.Fatal("an empty input has a digest")
	}
}
