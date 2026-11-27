package planque

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A tool asked for by a name the grant doesn't have is refused with every tool
// it does have, by name, one line each. 27 Nov, the Qonto re-test: Setup asked
// the catalog twice for list_bank_accounts (Qonto's own balance read, which the
// grant didn't serve) and was told only to search by words. The tools below are
// a made-up catalog, not Qonto's.
func TestCatalogRefusalForAnUnknownToolListsItsTools(t *testing.T) {
	tl := catalogFixture(t, func(*http.Request) (int, string) {
		return 200, pageJSON([]map[string]any{
			{"name": "get_organization", "description": "Retrieve the organization with its bank accounts, their balances and IBANs.\nMore detail on a second line.", "read_only": true, "input_schema": map[string]any{"type": "object"}},
			{"name": "list_transactions", "description": "List a bank account's transactions.", "read_only": true, "input_schema": map[string]any{"type": "object"}},
			{"name": "connector_aa11", "slug": "MAIL_SEND", "input_schema": map[string]any{"type": "object"}},
		}, "")
	})
	res, err := tl.Call(context.Background(), json.RawMessage(`{"action":"catalog","grant":"grant-example","tool":"list_bank_accounts"}`))
	if err != nil || !res.IsError {
		t.Fatalf("not refused: %+v %v", res, err)
	}
	for _, want := range []string{
		"the catalog has no tool named list_bank_accounts.",
		"\n- get_organization: Retrieve the organization with its bank accounts, their balances and IBANs.",
		"\n- list_transactions: List a bank account's transactions.",
		"\n- connector_aa11 (MAIL_SEND)",
		"catalog {grant, q} finds tools by words",
		"before telling the owner something can't be done",
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("the refusal lacks %q:\n%s", want, res.Text)
		}
	}
	// A description's first line only: the rest is catalog {grant, tool}'s.
	if strings.Contains(res.Text, "second line") {
		t.Errorf("the refusal carries more than a first line:\n%s", res.Text)
	}
}

// However many tools a grant has, the refusal stays within what the model may
// see, and counts the ones it couldn't list.
func TestCatalogRefusalForAnUnknownToolIsBounded(t *testing.T) {
	tools := make([]map[string]any, 3000)
	for i := range tools {
		tools[i] = map[string]any{"name": fmt.Sprintf("tool_%04d", i), "description": strings.Repeat("é", 400), "input_schema": map[string]any{"type": "object"}}
	}
	tl := catalogFixture(t, func(*http.Request) (int, string) { return 200, pageJSON(tools, "") })
	res, err := tl.Call(context.Background(), json.RawMessage(`{"action":"catalog","grant":"grant-example","tool":"absent"}`))
	if err != nil || !res.IsError {
		t.Fatalf("not refused: %+v %v", res, err)
	}
	if len(res.Text) > MaxCatalog {
		t.Fatalf("the refusal is %d bytes, over %d", len(res.Text), MaxCatalog)
	}
	if !strings.Contains(res.Text, "\n- tool_0000: ") || !strings.Contains(res.Text, " more, found by words.") {
		t.Fatalf("the refusal neither lists the first tools nor counts the rest: %.300s", res.Text)
	}
	if !strings.Contains(res.Text, "…") || strings.ContainsRune(res.Text, '\uFFFD') {
		t.Fatalf("a long description isn't cut on a character: %.300s", res.Text)
	}
}

func TestFirstLineFoldsAndCutsOnACharacter(t *testing.T) {
	for in, want := range map[string]string{
		"":                      "",
		"  one   two \nthree":   "one two",
		"short":                 "short",
		strings.Repeat("é", 10): strings.Repeat("é", 4) + "…",
	} {
		if got := firstLine(in, 11); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}
