package main

import (
	"bytes"
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/kiff/agents/eval"
)

const ticketsBody = "Every hour, find the new tickets in my help desk. Send each one a short personal acknowledgement that says we got the message " +
	"and when we'll answer, once per ticket and up to 50 a day, and label it by topic. Any other reply waits for me. " +
	"Never close or delete a ticket, and never promise a refund or a credit."

// A recipe from kiff/tafs is read as the test needs it: its id, the apps
// it connects, its card's lists, and the owner's words.
func TestARecipeIsRead(t *testing.T) {
	r, err := loadRecipe("testdata/recipe-tickets.md")
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "acknowledge-every-ticket" {
		t.Errorf("id %q", r.ID)
	}
	if want := []string{"zendesk | freshdesk | gorgias | intercom"}; !slices.Equal(r.Connections, want) {
		t.Errorf("connections %q, want %q", r.Connections, want)
	}
	if want := []string{"One acknowledgement per ticket, up to 50 a day", "Labels on tickets"}; !slices.Equal(r.Allows, want) {
		t.Errorf("allows %q, want %q", r.Allows, want)
	}
	if want := []string{"Any other reply to a customer"}; !slices.Equal(r.Holds, want) {
		t.Errorf("holds %q, want %q", r.Holds, want)
	}
	if want := []string{"Closing or deleting a ticket", "Promising a refund or a credit"}; !slices.Equal(r.Never, want) {
		t.Errorf("never %q, want %q", r.Never, want)
	}
	if r.Body != ticketsBody {
		t.Errorf("body %q\nwant %q", r.Body, ticketsBody)
	}
}

func TestARecipeThatCantBeReadIsRefused(t *testing.T) {
	for name, text := range map[string]string{
		"no front matter": "Every hour, acknowledge the tickets.\n",
		"not closed":      "---\nid: a\ncard:\n  allows:\n    - x\n",
		"no body":         "---\nid: a\ncard:\n  allows:\n    - x\n---\n\n",
		"a bad id":        "---\nid: A Recipe\ncard:\n  allows:\n    - x\n---\nWords.\n",
		"no card":         "---\nid: a\n---\nWords.\n",
		"not YAML":        "---\nid: a\njust words\n---\nWords.\n",
	} {
		if _, err := parseRecipe(text); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	// Quotes, CRLF lines and a body with its own "---" are read as written.
	r, err := parseRecipe("---\r\nid: \"a-1\"\r\ncard:\r\n  never:\r\n    - 'Deleting'\r\n---\r\nFirst.\r\n---\r\nSecond.\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "a-1" || !slices.Equal(r.Never, []string{"Deleting"}) || r.Body != "First.\n---\nSecond." {
		t.Errorf("read %+v", r)
	}
}

// The recipe's owner opens with its words alone, unchanged, and when Coach
// asks says only where the desk is and yes: no limit, tool or job of theirs.
func TestTheRecipesOwnerSaysItsWordsAlone(t *testing.T) {
	r, err := loadRecipe("testdata/recipe-tickets.md")
	if err != nil {
		t.Fatal(err)
	}
	desk := "https://vendor.example/desk"
	msg, facts := recipeOpening(r, desk)
	if msg != r.Body || msg != ticketsBody {
		t.Fatalf("the owner opens with %q, want the recipe's words", msg)
	}
	if len(facts) != 2 {
		t.Fatalf("facts %q: want where the desk is, then agreement", facts)
	}
	if !strings.Contains(facts[0], desk+"/openapi.json") || !strings.Contains(facts[0], desk+"/v1") ||
		!strings.Contains(facts[0], "X-Desk-Key") || !strings.Contains(facts[0], deskWorkspace) {
		t.Errorf("the facts don't say where the desk is: %q", facts[0])
	}
	addresses := regexp.MustCompile(`https?://\S+|` + deskWorkspace)
	for _, f := range facts {
		rest := addresses.ReplaceAllString(f, "")
		if strings.ContainsAny(rest, "0123456789") {
			t.Errorf("the facts give a number outside an address: %q", rest)
		}
		for _, limit := range []string{"a day", "per ", "up to", "at most", "limit", "never", "only", "reply", "label", "ticket"} {
			if strings.Contains(strings.ToLower(rest), limit) {
				t.Errorf("the facts say %q, beyond where the desk is: %q", limit, f)
			}
		}
	}
}

// The recipe's check names every line of its card, and reads the taf's Cards
// and tools as KIFF and its connection hold them.
func TestTheRecipesCardIsGraded(t *testing.T) {
	r, err := loadRecipe("testdata/recipe-tickets.md")
	if err != nil {
		t.Fatal(err)
	}
	rule := recipeRule(r)
	for _, line := range slices.Concat(r.Allows, r.Holds, r.Never) {
		if !strings.Contains(rule, line) {
			t.Errorf("the rule leaves out %q: %s", line, rule)
		}
	}
	if !strings.Contains(rule, "A Card that lets a 'holds' or 'never' item run on its own fails.") {
		t.Errorf("the rule doesn't fail a held or never item that runs on its own: %s", rule)
	}
	record := recipeRecord([]map[string]any{{"id": "acks", "grants": []map[string]any{{"action": "reply_ticket"}}}},
		[]grantTool{{Tool: "list_tickets", ReadOnly: true}, {Tool: "reply", Name: "reply_ticket"}})
	for _, want := range []string{`"action": "reply_ticket"`, "list_tickets (reads only)", "reply_ticket"} {
		if !strings.Contains(record, want) {
			t.Errorf("the record has no %q: %s", want, record)
		}
	}
	v := &verdicts{}
	c, err := gradeRecipe(context.Background(), &eval.Judge{Model: v, ModelID: "judge"}, r, record)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != keepsTheRecipe || !c.Pass || c.Advisory {
		t.Errorf("check %+v: want a passed gate", c)
	}
	if len(v.asked) == 0 || !strings.Contains(v.asked[0], record) {
		t.Errorf("the judge didn't read the record")
	}
	v = &verdicts{fail: []string{"keeps the recipe's choices"}}
	if c, _ := gradeRecipe(context.Background(), &eval.Judge{Model: v, ModelID: "judge"}, r, record); c.Pass {
		t.Error("a Card the judges fail passed")
	}
}

// A recipe's owner names no taf: the name check isn't asked of Coach.
func TestARecipesTafNeedsNoName(t *testing.T) {
	o := owners["en"]
	o.taf = ""
	checks, err := gradeSetup(context.Background(), &eval.Judge{Model: &verdicts{}, ModelID: "judge"}, o, said{}, "taf-tickets-x7k3", modes{none: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		if c.Name == "names the agent from the taf's name" {
			t.Errorf("checked %q though the owner named no taf", c.Name)
		}
	}
}

// -recipe runs on the desk, with the English owner, and ends at the taf.
func TestARecipeRunsOnTheDesk(t *testing.T) {
	file := "testdata/recipe-tickets.md"
	none := func(string) string { return "" }
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-recipe", file}, "add -desk"},
		{[]string{"-recipe", file, "-desk", "-owner", "es"}, "-owner en"},
		{[]string{"-recipe", file, "-desk", "-improve"}, "ends at the taf"},
		{[]string{"-recipe", "testdata/no-such-recipe.md", "-desk"}, "no-such-recipe"},
	} {
		err := run(context.Background(), c.args, &redacting{w: &bytes.Buffer{}}, none)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want an error saying %q", c.args, err, c.want)
		}
	}
	// A recipe that reads goes on to what the run needs next.
	err := run(context.Background(), []string{"-recipe", file, "-desk"}, &redacting{w: &bytes.Buffer{}}, none)
	if err == nil || !strings.Contains(err.Error(), "is required") {
		t.Errorf("a recipe on the desk: %v, want the run to ask for its environment", err)
	}
}
