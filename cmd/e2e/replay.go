package main

import (
	"fmt"
	"strings"
)

// Replays: setups that failed a real owner, run again on the fake vendor with
// the owner's words and answers as they were, graded by the bar for connecting
// an app (connectbar.go) with the rest. Each is red until the bar holds.
//
//	intros  a list in a spreadsheet and introductions drafted in a mailbox,
//	        two apps by one key each (13 Nov: three approvals and a second
//	        sign-in for two apps, and a sign-in that went through read as a
//	        failure)
//
// The run goes on to the result: the taf made, its Card agreed. Nothing runs
// after it.
type replay struct {
	taf   string                   // the taf's name, as the owner gives it
	ask   func(root string) string // the owner's first message, the vendor's root in it
	facts []string                 // when Setup asks in words: the owner's answers, then agreement
	// none: the job has no test or live mode to choose.
	none bool
}

var replays = map[string]replay{
	"intros": {
		taf: "Introduction drafts",
		ask: func(root string) string {
			return fmt.Sprintf("Each weekday morning, read my list of companies in my spreadsheet. For each one I have not written to yet, write a "+
				"short personal introduction as a draft in my mailbox, using only the reason I noted for that company. Up to 10 drafts a day. "+
				"Never send anything yourself: every send waits for me. Skip anyone I already wrote to or who asked not to be contacted, and "+
				"tell me who you skipped. The list is the sheet %s in my spreadsheet, whose API is described at %s/sheets/openapi.json, "+
				"its operations start at %s/sheets/v1, and its key goes in the X-Sheets-Key header. My mailbox is %s, whose API is described "+
				"at %s/mail/openapi.json, its operations start at %s/mail/v1, and its key goes in the X-Mail-Key header. "+
				"Call the taf Introduction drafts.", replaySheet, root, root, replayMailbox, root, root)
		},
		facts: []string{
			"Keep your own record of whom you wrote to, and check my sent mail too. Run once now, then on weekdays at 8:00, Paris time. " +
				"Each draft starts with a line saying it was drafted by tafs, my AI assistant. Yes, you can connect both.",
			"Yes, that's right. Go ahead.",
		},
		none: true,
	},
}

// The fake vendor's list of companies and the mailbox it's written from.
const (
	replaySheet   = "sh_eval"
	replayMailbox = "mb_eval"
)

// vendorRoot is the fake vendor's root, from its MCP server's address (…/mcp).
func vendorRoot(vendor string) string {
	return strings.TrimSuffix(strings.TrimRight(vendor, "/"), "/mcp")
}
