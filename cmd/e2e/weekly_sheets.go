package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kiff/agents/cmd/internal/testaccount"
)

// The Sheets dry run (-weekly-sheets, by hand only): a new Coach chat on tafs
// cloud asks to connect Google Sheets, an app connected by a sign-in, and is
// driven as the owner's phone would until Coach shows its approval card. It
// checks what can be checked without the sign-in's consent page: Coach asked
// before it asked for the connection, no step failed and no connection was
// refused, and planque lists the grant the card names, so the card can be
// drawn. The card is never approved. Then a second new chat starts, for its
// first message to be read (it must carry nothing of the first chat).
var weeklySheets = weeklyFamily{
	Name: "sheets",
	Ask:  "Connect Google Sheets so a taf can read my orders sheet and add a row",
	Fact: "The sheet is called Orders. Read its rows, and add one row only after I approve it.",
	// A sign-in connects it: there's no sandbox key, and no tool is approved.
	CapUSD:  3,
	MaxTurn: 8,
}

// weeklySheetsSecond is the second new chat's first words: nothing about any app.
const weeklySheetsSecond = "What can you help me with?"

// sheetsResult is what the Sheets dry run saw.
type sheetsResult struct {
	weeklyResult
	// Questions are the questions Coach asked before its approval card: each
	// question on a questions card, and each turn it asked in words.
	Questions int
	// Card is whether an approval or sign-in card came, and Listed whether
	// planque lists the grant it names.
	Card, Listed bool
	Failed       []string
	Refused      int
	// Second is when the second chat started, for its first message to be
	// read on kiff/agents' record (no id goes to the log).
	Second time.Time
}

var (
	errNoQuestions = errors.New("Coach asked nothing before its approval card")
	errFailedStep  = errors.New("a step failed in the chat")
	errRefused     = errors.New("the connection server refused a connection in the chat")
	errNotListed   = errors.New("planque doesn't list the grant the approval card names")
	errHandedOver  = errors.New("Coach handed a taf over before any approval card")
)

// driveToCard is the dry run's chat: the owner's request, then Coach's
// questions and choices answered as in driveCoach, until Coach shows an
// approval or sign-in card. The card is checked, never approved.
func driveToCard(ctx context.Context, chat weeklyChat, phone weeklyPhone, f weeklyFamily, wait func(context.Context) error) (sheetsResult, error) {
	var r sheetsResult
	session, err := chat.Start(ctx, f.Ask)
	if err != nil {
		return r, fmt.Errorf("starting Coach's chat: %w", err)
	}
	limit := int64(f.CapUSD * 1e6)
	for r.Turns = 1; r.Turns <= f.MaxTurn; r.Turns++ {
		th, err := settled(ctx, chat, session, wait)
		if err != nil {
			return r, err
		}
		r.Failed, r.Refused = th.Failed, th.Refused
		if r.Spent = th.Spent; r.Spent >= limit {
			return r, fmt.Errorf("%w: US$%.2f spent of US$%.2f", errCostCap, float64(r.Spent)/1e6, f.CapUSD)
		}
		if len(th.Failed) > 0 {
			return r, fmt.Errorf("%w: %s", errFailedStep, strings.Join(th.Failed, "; "))
		}
		if th.Refused > 0 {
			return r, errRefused
		}
		notices := map[string]bool{}
		for _, c := range th.Cards {
			if c.Session == session && c.Card.Type == "notice" {
				notices[c.Card.ID] = true
			}
		}
		acted := false
		for _, c := range th.Cards {
			if c.Answered || c.Session != session {
				continue
			}
			switch c.Card.Type {
			case "approval", "sign_in":
				r.Card = true
				if r.Questions == 0 {
					return r, errNoQuestions
				}
				if c.Card.Grant == "" {
					return r, fmt.Errorf("%w: the card names no grant", errNotListed)
				}
				asked, err := phone.Asked(ctx, c.Card.Grant)
				if err != nil {
					return r, fmt.Errorf("reading the setup agent's asked grants: %w", err)
				}
				if !asked {
					return r, errNotAsked
				}
				// Recorded before it's read, so the cleanup takes it away either way.
				r.Grants = append(r.Grants, c.Card.Grant)
				if _, _, err := phone.Grant(ctx, c.Card.Grant); err != nil {
					return r, fmt.Errorf("%w: %v", errNotListed, err)
				}
				r.Listed = true
				return r, nil
			case "questions":
				r.Questions += len(c.Card.Questions)
				raw := strings.TrimPrefix(answerQuestions(c.Card, f.Fact), "[questions "+c.Card.ID+"] ")
				if err := chat.Answer(ctx, c.Card.ID, json.RawMessage(raw), session); err != nil {
					return r, err
				}
			case "confirm":
				pick, err := scriptedChoice(c.Card, notices)
				if err != nil {
					return r, err
				}
				r.Taps = append(r.Taps, weeklyTap{Card: c.Card.ID, Words: pick.Label})
				b, _ := json.Marshal(pick.Value)
				if err := chat.Answer(ctx, c.Card.ID, b, session); err != nil {
					return r, err
				}
			case "result":
				// Kept for the cleanup, which archives a taf this run made.
				if c.Card.Taf != nil {
					r.Taf = c.Card.Taf.Agent
				}
				return r, errHandedOver
			default:
				continue
			}
			acted = true
			break
		}
		if !acted {
			// Coach asked in words: a question, answered with the owner's one fact.
			r.Questions++
			if err := chat.Send(ctx, session, f.Fact); err != nil {
				return r, err
			}
		}
	}
	return r, errNoCard
}

// sheetsSummary is the dry run's result for the job's summary: each check,
// the turns and the spend. No id, key or app data.
func sheetsSummary(r sheetsResult, err error) string {
	var b strings.Builder
	outcome := "PASS"
	if err != nil {
		outcome = "FAIL: " + scrubWhy(err.Error())
	}
	yes := func(ok bool) string {
		if ok {
			return "yes"
		}
		return "no"
	}
	fmt.Fprintf(&b, "### Sheets dry run: %s\n\n", outcome)
	fmt.Fprintf(&b, "- questions Coach asked before its card: %d\n", r.Questions)
	fmt.Fprintf(&b, "- approval card shown: %s; its grant listed by planque: %s\n", yes(r.Card), yes(r.Listed))
	fmt.Fprintf(&b, "- failed steps: %d; refused connections: %d\n", len(r.Failed), r.Refused)
	if !r.Second.IsZero() {
		fmt.Fprintf(&b, "- second new chat started at %s UTC, for its first message to be read\n", r.Second.UTC().Format("15:04:05"))
	}
	fmt.Fprintf(&b, "- %d turns, US$%.2f of US$%.2f\n", r.Turns, float64(r.Spent)/1e6, weeklySheets.CapUSD)
	return b.String()
}

// runWeeklySheets is -weekly-sheets: the Sheets dry run on the test account,
// its grant taken away after, whatever happens.
func runWeeklySheets(ctx context.Context, stdout *redacting, env func(string) string) (retErr error) {
	ctx, cancel := context.WithTimeout(ctx, weeklyFamilyDeadline)
	defer cancel()
	t, chat, err := weeklyAccount(ctx, stdout, env, "")
	if err != nil {
		return err
	}
	// The tafs on the account before the run: one Coach hands over that was
	// already there isn't this run's to archive.
	before, _, err := testaccount.PhoneAccess(ctx, t.kiff)
	if err != nil {
		return err
	}
	var r sheetsResult
	defer func() {
		cleanCtx := context.WithoutCancel(ctx)
		for _, g := range r.Grants {
			stdout.hide(g, "<a grant>")
		}
		mine := weeklyCleanup(r.weeklyResult, before, stdout,
			func(g string) error { return t.phone.Disconnect(cleanCtx, g) },
			func(taf string) error {
				c, err := testaccount.Sweep(cleanCtx, t.kiff, []string{taf})
				if err == nil && c.Left() > 0 {
					err = errors.New("it's still on the account")
				}
				return err
			})
		fmt.Fprintf(stdout, "  cleanup: %d of this run's taken away\n", mine)
	}()
	defer func() { writeSummary(env("GITHUB_STEP_SUMMARY"), sheetsSummary(r, retErr)) }()
	fmt.Fprintln(stdout, "== Sheets dry run")
	r, err = driveToCard(ctx, chat, liveWeeklyPhone{t: t}, weeklySheets, pollWait(5*time.Second))
	fmt.Fprintf(stdout, "  turns %d, spent US$%.2f, questions %d, card %v, listed %v, failed %d, refused %d\n",
		r.Turns, float64(r.Spent)/1e6, r.Questions, r.Card, r.Listed, len(r.Failed), r.Refused)
	if err != nil {
		return err
	}
	// A second new chat, as a person opens one after: its first message is
	// read on kiff/agents' record afterwards (it must carry nothing of the first).
	r.Second = time.Now()
	second, err := chat.Start(ctx, weeklySheetsSecond)
	if err != nil {
		return fmt.Errorf("starting the second chat: %w", err)
	}
	if _, err := settled(ctx, chat, second, pollWait(5*time.Second)); err != nil {
		return fmt.Errorf("the second chat: %w", err)
	}
	fmt.Fprintf(stdout, "  second chat started at %s UTC: PASS on tafs cloud's side\n", r.Second.UTC().Format("15:04:05"))
	return nil
}
