package main

import (
	"strings"
	"testing"
)

// The bar for connecting an app, counted from the run: one approval per app
// plus its Card, no raw error shown, no loop (the 13 Nov go-to-market runs).
func TestTheConnectBar(t *testing.T) {
	sheets := connectAsk{Entry: "connector:managed:googlesheets", Uses: []string{"SHEETS_APPEND"}, Grant: "grt_s"}
	gmail := connectAsk{Entry: "connector:managed:gmail", Uses: []string{"GMAIL_SEND_EMAIL"}, Grant: "grt_g"}
	qonto := connectAsk{Entry: "qonto-key@2", Uses: []string{"list_client_invoices"}, Grant: "grt_q", Declared: true, API: true}
	clean := []string{"Connect Google Sheets and Gmail for Overdue invoices?", "Done: your taf sends reminders on weekdays."}

	// Each app approved once, a key and its Card in one: every gate passes.
	g := byName(gradeConnectBar([]connectAsk{sheets, gmail, qonto},
		[]approvalSeen{{Grant: "grt_s"}, {Grant: "grt_g"}, {Grant: "grt_q", Draft: "drf_1", Key: true}}, clean))
	for _, name := range []string{oneApprovalPerApp, noRawErrorShown, noLoopAfterRefusal} {
		if !g[name].Pass {
			t.Errorf("%s: %+v", name, g[name])
		}
	}
	// An app, then its Card in an approval of its own: still one each.
	if r := byName(gradeConnectBar([]connectAsk{sheets}, []approvalSeen{{Grant: "grt_s"}, {Grant: "grt_s", Draft: "drf_2"}}, clean))[oneApprovalPerApp]; !r.Pass {
		t.Errorf("an app and its Card: %+v", r)
	}

	// Job 3: LinkedIn approved five times, across new grants for the same app.
	li := func(grant string) connectAsk {
		return connectAsk{Entry: "connector:managed:linkedin", Uses: []string{"LINKEDIN_CREATE_POST"}, Grant: grant}
	}
	five := []connectAsk{li("grt_1"), li("grt_2"), li("grt_3")}
	r := byName(gradeConnectBar(five, []approvalSeen{{Grant: "grt_1"}, {Grant: "grt_2"}, {Grant: "grt_3"}}, clean))[oneApprovalPerApp]
	if r.Pass || !strings.Contains(r.Why, "connector:managed:linkedin: 3 approval(s)") {
		t.Errorf("one app approved three times: %+v", r)
	}
	// A reused grant counts for the app of the request that made it.
	reused := []connectAsk{li("grt_1"), {Reuse: "grt_1", Uses: []string{"LINKEDIN_GET_COMPANY"}, Grant: "grt_1b"}}
	if r := byName(gradeConnectBar(reused, []approvalSeen{{Grant: "grt_1"}, {Grant: "grt_1b"}}, clean))[oneApprovalPerApp]; r.Pass {
		t.Errorf("a reuse asked for approval again: %+v", r)
	}

	// The words the owner saw today, each a raw error.
	for _, said := range []string{
		"[tafs coach lines] [connection refused] Coach couldn't connect the app yet.",
		"Coach couldn't connect the app yet. Why: fixed argument it must be a dotted path with a JSON value",
		"Why: read the app's tool catalog before selecting this action",
		"LinkedIn refused it (it answered 403). Ensure you have the scope.",
		"The server said HTTP 401.",
		"[tafs time zone] The account's time zone is Europe/Paris.\nThe owner says: hi",
		"It failed: sign_in_not_confirmed",
	} {
		r := byName(gradeConnectBar(nil, nil, append(clean, said)))[noRawErrorShown]
		if r.Pass {
			t.Errorf("%q passed", said)
		}
	}
	// Plain words about what an app can't do are the person's to read.
	for _, said := range []string{
		"LinkedIn can't post to a company page with this sign-in. I can post on your own profile instead.",
		"Your desk replies start with a line saying an AI agent wrote them.",
		"Up to 403 euros a day.",
	} {
		if r := byName(gradeConnectBar(nil, nil, []string{said}))[noRawErrorShown]; !r.Pass {
			t.Errorf("%q: %+v", said, r)
		}
	}

	// A refused request sent again as it was is a loop; changed, it isn't.
	refused := connectAsk{Entry: "connector:managed:linkedin", Uses: []string{"LINKEDIN_CREATE_POST", "LINKEDIN_GET_COMPANY"}, Refused: "it answered 403"}
	same := refused
	same.Refused = ""
	if r := byName(gradeConnectBar([]connectAsk{refused, same}, nil, clean))[noLoopAfterRefusal]; r.Pass || !strings.Contains(r.Why, "asked again as it was") {
		t.Errorf("a refused request sent again: %+v", r)
	}
	changed := connectAsk{Entry: "connector:managed:linkedin", Uses: []string{"LINKEDIN_CREATE_POST"}, Grant: "grt_9"}
	if r := byName(gradeConnectBar([]connectAsk{refused, changed}, nil, clean))[noLoopAfterRefusal]; !r.Pass {
		t.Errorf("a changed request after a refusal: %+v", r)
	}
	// The same approval shown twice is a loop.
	if r := byName(gradeConnectBar([]connectAsk{sheets}, []approvalSeen{{Grant: "grt_s"}, {Grant: "grt_s"}}, clean))[noLoopAfterRefusal]; r.Pass || !strings.Contains(r.Why, "grt_s shown 2 times") {
		t.Errorf("an approval shown again: %+v", r)
	}

	// Nothing connected: the counts are skipped, the words still checked.
	none := byName(gradeConnectBar(nil, nil, clean))
	if !none[oneApprovalPerApp].Skipped || !none[noLoopAfterRefusal].Skipped || !none[noRawErrorShown].Pass {
		t.Errorf("nothing connected: %+v", none)
	}
}
