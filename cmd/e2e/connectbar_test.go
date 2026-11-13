package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kiff/agents/session"
)

// The bar for connecting an app, counted from the run: one approval per app
// plus its Card, no raw error shown, no loop (the 13 Nov go-to-market runs).
func TestTheConnectBar(t *testing.T) {
	sheets := connectAsk{Entry: "connector:managed:googlesheets", Uses: []string{"SHEETS_APPEND"}, Grant: "grt_s"}
	gmail := connectAsk{Entry: "connector:managed:gmail", Uses: []string{"GMAIL_SEND_EMAIL"}, Grant: "grt_g"}
	qonto := connectAsk{Entry: "qonto-key@2", Uses: []string{"list_client_invoices"}, Grant: "grt_q", Declared: true, API: true}
	clean := []string{"Connect Google Sheets and Gmail for Overdue invoices?", "Done: your taf sends reminders on weekdays."}

	// Each app approved once, a key and its Card in one: every gate passes.
	g := byName(gradeConnectBar(barRun{asks: []connectAsk{sheets, gmail, qonto},
		approvals: []approvalSeen{{Grant: "grt_s"}, {Grant: "grt_g"}, {Grant: "grt_q", Draft: "drf_1", Key: true}}, statements: clean}))
	for _, name := range []string{oneApprovalPerApp, noRawErrorShown, noLoopAfterRefusal} {
		if !g[name].Pass {
			t.Errorf("%s: %+v", name, g[name])
		}
	}
	// An app, then its Card in an approval of its own: still one each.
	if r := byName(gradeConnectBar(barRun{asks: []connectAsk{sheets}, approvals: []approvalSeen{{Grant: "grt_s"}, {Grant: "grt_s", Draft: "drf_2"}}, statements: clean}))[oneApprovalPerApp]; !r.Pass {
		t.Errorf("an app and its Card: %+v", r)
	}

	// Two APIs of one vendor, described apart (a spreadsheet and a mailbox on
	// the fake vendor), are two apps, each approved once; one asked twice isn't.
	sheet := connectAsk{API: true, APIURL: "https://vendor.example/sheets/openapi.json", Grant: "grt_sh", Declared: true}
	mail := connectAsk{API: true, APIURL: "https://vendor.example/mail/openapi.json", Grant: "grt_mb", Declared: true}
	if r := byName(gradeConnectBar(barRun{asks: []connectAsk{sheet, mail}, approvals: []approvalSeen{{Grant: "grt_sh", Draft: "drf_1", Key: true},
		{Grant: "grt_mb", Draft: "drf_1", Key: true}}, statements: clean}))[oneApprovalPerApp]; !r.Pass {
		t.Errorf("two APIs of one vendor: %+v", r)
	}
	sheet2 := sheet
	sheet2.Grant = "grt_sh2"
	if r := byName(gradeConnectBar(barRun{asks: []connectAsk{sheet, sheet2}, approvals: []approvalSeen{{Grant: "grt_sh", Key: true},
		{Grant: "grt_sh2", Key: true}}, statements: clean}))[oneApprovalPerApp]; r.Pass || !strings.Contains(r.Why, "api vendor.example/sheets") {
		t.Errorf("the spreadsheet asked twice: %+v", r)
	}

	// Job 3: LinkedIn approved five times, across new grants for the same app.
	li := func(grant string) connectAsk {
		return connectAsk{Entry: "connector:managed:linkedin", Uses: []string{"LINKEDIN_CREATE_POST"}, Grant: grant}
	}
	five := []connectAsk{li("grt_1"), li("grt_2"), li("grt_3")}
	r := byName(gradeConnectBar(barRun{asks: five, approvals: []approvalSeen{{Grant: "grt_1"}, {Grant: "grt_2"}, {Grant: "grt_3"}}, statements: clean}))[oneApprovalPerApp]
	if r.Pass || !strings.Contains(r.Why, "connector:managed:linkedin: 3 approval(s)") {
		t.Errorf("one app approved three times: %+v", r)
	}
	// A reused grant counts for the app of the request that made it.
	reused := []connectAsk{li("grt_1"), {Reuse: "grt_1", Uses: []string{"LINKEDIN_GET_COMPANY"}, Grant: "grt_1b"}}
	if r := byName(gradeConnectBar(barRun{asks: reused, approvals: []approvalSeen{{Grant: "grt_1"}, {Grant: "grt_1b"}}, statements: clean}))[oneApprovalPerApp]; r.Pass {
		t.Errorf("a reuse asked for approval again: %+v", r)
	}

	// The words the owner saw today, each the run's own error: quoted from
	// it, they fail, read from the run whatever the source's wording.
	errs := []string{
		"planque answered 400: tools[0]: fixed argument \"it\" must be a dotted path with a JSON value",
		"planque answered 409: read the app's tool catalog before selecting this action",
		"LinkedIn refused it (it answered 403): Forbidden. You don't have permission to access organization ACLs. Ensure you have the scope.",
		"tafs cloud answered 409: this card was answered already and changed in the meantime",
	}
	for _, said := range []string{
		"Coach couldn't connect the app yet. Why: fixed argument it must be a dotted path with a JSON value",
		"Why: read the app's tool catalog before selecting this action",
		"You don't have permission to access organization ACLs, it says.",
		// A wording no backstop knows, quoted from an error the run got.
		"Sorry: this card was answered already and changed in the meantime.",
	} {
		if r := byName(gradeConnectBar(barRun{statements: append(clean, said), errors: errs}))[noRawErrorShown]; r.Pass || !strings.Contains(r.Why, "an error's words") {
			t.Errorf("%q: %+v", said, r)
		}
	}
	// What an error quotes back of the request (a tool's title) is Setup's own.
	quoting := []string{`planque answered 400: tool "Create a post on your own profile as you" needs a resource_argument`}
	if r := byName(gradeConnectBar(barRun{statements: []string{"Approve: Create a post on your own profile as you."}, errors: quoting}))[noRawErrorShown]; !r.Pass {
		t.Errorf("a title an error quoted back: %+v", r)
	}
	// The backstop: wrappers and codes, which never come as an error result.
	for _, said := range []string{
		"[tafs coach lines] [connection refused] Coach couldn't connect the app yet.",
		"[taf Overdue invoices first check] it found nothing.",
		"[result r_1] failed: the card was declined",
		"LinkedIn refused it (it answered 403). Ensure you have the scope.",
		"The server said HTTP 401.",
		"[tafs time zone] The account's time zone is Europe/Paris.\nThe owner says: hi",
		"It failed: sign_in_not_confirmed",
	} {
		r := byName(gradeConnectBar(barRun{statements: append(clean, said)}))[noRawErrorShown]
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
		if r := byName(gradeConnectBar(barRun{statements: []string{said}, errors: errs}))[noRawErrorShown]; !r.Pass {
			t.Errorf("%q: %+v", said, r)
		}
	}

	// A refused request sent again as it was is a loop; changed, it isn't.
	refused := connectAsk{Entry: "connector:managed:linkedin", Uses: []string{"LINKEDIN_CREATE_POST", "LINKEDIN_GET_COMPANY"}, Refused: "it answered 403"}
	same := refused
	same.Refused = ""
	if r := byName(gradeConnectBar(barRun{asks: []connectAsk{refused, same}, statements: clean}))[noLoopAfterRefusal]; r.Pass || !strings.Contains(r.Why, "asked again as it was") {
		t.Errorf("a refused request sent again: %+v", r)
	}
	changed := connectAsk{Entry: "connector:managed:linkedin", Uses: []string{"LINKEDIN_CREATE_POST"}, Grant: "grt_9"}
	if r := byName(gradeConnectBar(barRun{asks: []connectAsk{refused, changed}, statements: clean}))[noLoopAfterRefusal]; !r.Pass {
		t.Errorf("a changed request after a refusal: %+v", r)
	}
	// A pin fixed after planque refused it: the same app and uses, not a loop.
	pin := func(tools string) []session.Event {
		return []session.Event{callEvent(t, "c_"+tools[:4], map[string]any{"action": "request", "entry": "connector:managed:linkedin",
			"uses": []string{"LINKEDIN_CREATE_POST"}, "tools": json.RawMessage(tools)})}
	}
	bad := connectAsksOf(pin(`[{"tool":"LINKEDIN_CREATE_POST","title":"Post","fixed_arguments":{"it":"PUBLIC"}}]`))[0]
	bad.Refused = "fixed argument isn't a dotted path"
	fixed := connectAsksOf(pin(`[{"tool":"LINKEDIN_CREATE_POST","title":"Post as you","fixed_arguments":{"visibility":"PUBLIC"}}]`))[0]
	if r := byName(gradeConnectBar(barRun{asks: []connectAsk{bad, fixed}, statements: clean}))[noLoopAfterRefusal]; !r.Pass {
		t.Errorf("a pin fixed after a refusal: %+v", r)
	}
	// The same pin sent again, its title reworded: still the same request.
	again := connectAsksOf(pin(`[{"tool":"LINKEDIN_CREATE_POST","title":"A post","fixed_arguments":{"it":"PUBLIC"}}]`))[0]
	if r := byName(gradeConnectBar(barRun{asks: []connectAsk{bad, again}, statements: clean}))[noLoopAfterRefusal]; r.Pass {
		t.Errorf("a refused pin sent again: %+v", r)
	}
	// Tools added to a connected grant: refused, then sent again as they were.
	tools := toolsAsksOf([]session.Event{
		callEvent(t, "t1", map[string]any{"action": "tools", "grant": "grt_li", "tools": []map[string]any{{"tool": "LINKEDIN_GET_COMPANY", "title": "Read the page"}}}),
		resultEvent(t, "t1", "planque answered 409: confirm this connection before reading its tool catalog", true),
		callEvent(t, "t2", map[string]any{"action": "tools", "grant": "grt_li", "tools": []map[string]any{{"tool": "LINKEDIN_GET_COMPANY", "title": "Read the company page"}}}),
	})
	if r := byName(gradeConnectBar(barRun{toolsAsks: tools, statements: clean}))[noLoopAfterRefusal]; r.Pass || !strings.Contains(r.Why, "tools for grt_li") {
		t.Errorf("a refused tools call sent again: %+v", r)
	}
	// The same approval shown twice is a loop.
	if r := byName(gradeConnectBar(barRun{asks: []connectAsk{sheets}, approvals: []approvalSeen{{Grant: "grt_s"}, {Grant: "grt_s"}}, statements: clean}))[noLoopAfterRefusal]; r.Pass || !strings.Contains(r.Why, "grt_s shown 2 times") {
		t.Errorf("an approval shown again: %+v", r)
	}

	// Nothing connected: the counts are skipped, the words still checked.
	none := byName(gradeConnectBar(barRun{statements: clean}))
	if !none[oneApprovalPerApp].Skipped || !none[noLoopAfterRefusal].Skipped || !none[noRawErrorShown].Pass {
		t.Errorf("nothing connected: %+v", none)
	}
}
