package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// weeklyChat is Coach's chat on tafs cloud, as the owner's phone uses it.
type weeklyChat interface {
	// Start sends the owner's request in a new chat and returns its session.
	Start(ctx context.Context, text string) (string, error)
	// Send writes the owner's next message in the chat.
	Send(ctx context.Context, session, text string) error
	// Thread is the chat as it stands: whether Coach is at work, and its cards.
	Thread(ctx context.Context, session string) (weeklyThread, error)
	// Answer answers a card that waits for it, as data.
	Answer(ctx context.Context, component string, answer json.RawMessage, session string) error
}

// weeklyThread is Coach's chat as tafs cloud gives it.
type weeklyThread struct {
	Working bool
	Cards   []weeklyThreadCard
	// Spent is what the chat's model turns cost so far, in millionths of a
	// US dollar (the thread's usage.cost_micro_usd, from kiff/agents' runs).
	Spent int64
	// Failed are the chat's steps the app shows as failed (a step's failed or
	// error), by the words it shows; Refused counts the messages that carry a
	// connection the server refused (refused). A step not made (not_made) is
	// neither: nothing was sent, and the app shows it as not made.
	Failed  []string
	Refused int
}

// weeklyThreadCard is one card in the chat, and whether it's answered.
type weeklyThreadCard struct {
	Session  string
	Card     component
	Tools    []string
	Signs    *bool
	Answered bool
}

// weeklyFamily is one family's weekly proof: the request, worded as an owner
// would, the owner's one fact when Coach asks in words, and the tools the job
// needs.
type weeklyFamily struct {
	Name string
	Ask  string
	Fact string
	// Job is what the grant's tools must be.
	Job     weeklyJob
	CapUSD  float64
	MaxTurn int
}

// weeklyResult is what came of a weekly proof's chat.
type weeklyResult struct {
	Approvals []string // tafs cloud's answers to Coach, one per tap
	Grants    []string // the grants the taps approved
	Taf       string   // the taf Coach handed over
	Turns     int
	Spent     int64
	// Taps are the confirm cards the script tapped: the card, the option's
	// words, and the rules notice it agreed to, if any.
	Taps []weeklyTap
}

// weeklyTap is one tap on a confirm card.
type weeklyTap struct{ Card, Words, Agrees string }

var (
	errNoCard   = errors.New("no approval card in the turns allowed")
	errCostCap  = errors.New("cost cap")
	errInWords  = errors.New("a card asks for agreement with no notice shown")
	errNotAsked = errors.New("a card names a grant the setup agent didn't ask for")
	approvalish = regexp.MustCompile(`(?i)^\s*(yes|ok|okay|approve[ds]?|agree[ds]?|confirm(ed)?|accept(ed)?|go ahead|sure)\b`)
)

// driveCoach is the weekly proof's chat: the owner's request, then each of
// Coach's cards answered as the owner's phone does, until Coach hands a taf
// over. The scripted turns answer questions and choices only, never with an
// approval in words: the only approval is the component route
// (approveCard). The cost is checked after each turn, and the run stops once
// it has passed the cap (so one turn can take it past).
func driveCoach(ctx context.Context, chat weeklyChat, cloud weeklyCloud, phone weeklyPhone, f weeklyFamily, tenant string, wait func(context.Context) error) (weeklyResult, error) {
	var r weeklyResult
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
		if r.Spent = th.Spent; r.Spent >= limit {
			return r, fmt.Errorf("%w: US$%.2f spent of US$%.2f", errCostCap, float64(r.Spent)/1e6, f.CapUSD)
		}
		// The rules notices this chat showed, by id: what a confirm may agree to.
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
				// Only a grant the setup agent asked for is approved, and so taken
				// away after: any other is someone else's, and the run stops
				// before the phone acts. It's recorded before the phone acts on
				// it, so a connect that fails half-way is still taken away.
				if c.Card.Grant != "" {
					asked, err := phone.Asked(ctx, c.Card.Grant)
					if err != nil {
						return r, fmt.Errorf("reading the setup agent's asked grants: %w", err)
					}
					if !asked {
						return r, errNotAsked
					}
					r.Grants = append(r.Grants, c.Card.Grant)
				}
				answer, err := approveCard(ctx, cloud, phone, weeklyShown{ID: c.Card.ID, Session: session, Grant: c.Card.Grant, Draft: c.Card.Draft, Tools: c.Tools, SignsCard: c.Signs}, tenant, f.Job)
				if err != nil {
					return r, err
				}
				r.Approvals = append(r.Approvals, answer)
			case "questions":
				raw := strings.TrimPrefix(answerQuestions(c.Card, f.Fact), "[questions "+c.Card.ID+"] ")
				if err := chat.Answer(ctx, c.Card.ID, json.RawMessage(raw), session); err != nil {
					return r, err
				}
			case "confirm":
				pick, err := scriptedChoice(c.Card, notices)
				if err != nil {
					return r, err
				}
				tap := weeklyTap{Card: c.Card.ID, Words: pick.Label}
				if approvalish.MatchString(pick.Label) || approvalish.MatchString(pick.Value) {
					tap.Agrees = c.Card.Agrees
				}
				r.Taps = append(r.Taps, tap)
				b, _ := json.Marshal(pick.Value)
				if err := chat.Answer(ctx, c.Card.ID, b, session); err != nil {
					return r, err
				}
			case "result":
				if c.Card.Taf != nil {
					r.Taf = c.Card.Taf.Agent
				}
				if len(r.Approvals) == 0 {
					return r, errors.New("Coach handed a taf over with no approval through the component route")
				}
				return r, nil
			default:
				continue
			}
			acted = true
			break
		}
		if !acted {
			// Coach asked in words: the owner gives the one fact, never a yes.
			if err := chat.Send(ctx, session, f.Fact); err != nil {
				return r, err
			}
		}
	}
	if len(r.Approvals) == 0 {
		return r, errNoCard
	}
	return r, fmt.Errorf("no taf handed over in %d turns", f.MaxTurn)
}

// settled waits until Coach isn't at work in the chat.
func settled(ctx context.Context, chat weeklyChat, session string, wait func(context.Context) error) (weeklyThread, error) {
	for {
		th, err := chat.Thread(ctx, session)
		if err != nil {
			return th, fmt.Errorf("reading Coach's chat: %w", err)
		}
		if !th.Working {
			return th, nil
		}
		if err := wait(ctx); err != nil {
			return th, err
		}
	}
}

// scriptedChoice is the owner's tap on a confirm card. An
// agreeing option is tapped only when the card agrees to a rules notice this
// chat showed (its agrees names a notice among the chat's own cards), as the
// app does on the owner's tap. Otherwise the first option that doesn't agree;
// and when every option agrees, the run fails: the only approval of a write
// or a grant is the approval route.
func scriptedChoice(c component, notices map[string]bool) (option, error) {
	agreeing := func(o option) bool { return approvalish.MatchString(o.Label) || approvalish.MatchString(o.Value) }
	if c.Agrees != "" && notices[c.Agrees] {
		for _, o := range c.Options {
			if agreeing(o) {
				return o, nil
			}
		}
	}
	for _, o := range c.Options {
		if !agreeing(o) {
			return o, nil
		}
	}
	return option{}, errInWords
}

// pollWait is the wait between reads of a chat at work.
func pollWait(d time.Duration) func(context.Context) error {
	return func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
			return nil
		}
	}
}
