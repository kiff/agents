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
	// Spent is what the run has cost so far, in millionths of a dollar.
	Spent(ctx context.Context) (int64, error)
}

// weeklyThread is Coach's chat as tafs cloud gives it.
type weeklyThread struct {
	Working bool
	Cards   []weeklyThreadCard
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
	Name    string
	Ask     string
	Fact    string
	Tools   []string
	CapUSD  float64
	MaxTurn int
}

// weeklyResult is what came of a weekly proof's chat.
type weeklyResult struct {
	Approvals []string // tafs cloud's answers to Coach, one per tap
	Taf       string   // the taf Coach handed over
	Turns     int
	Spent     int64
}

var (
	errNoCard   = errors.New("no approval card in the turns allowed")
	errCostCap  = errors.New("cost cap")
	errInWords  = errors.New("a card asks for agreement in words")
	approvalish = regexp.MustCompile(`(?i)^\s*(yes|ok|okay|approve[ds]?|agree[ds]?|confirm(ed)?|accept(ed)?|go ahead|sure)\b`)
)

// driveCoach is the weekly proof's chat: the owner's request, then each of
// Coach's cards answered as the owner's phone does, until Coach hands a taf
// over. The scripted turns answer questions and choices only, never with an
// approval in words (kiff-qa (b)): the only approval is the component route
// (approveCard). The cost is read after each turn, and the run stops before
// the next one would pass the cap (kiff-qa (d)).
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
		if r.Spent, err = chat.Spent(ctx); err != nil {
			return r, fmt.Errorf("reading the cost: %w", err)
		}
		if r.Spent >= limit {
			return r, fmt.Errorf("%w: US$%.2f spent of US$%.2f", errCostCap, float64(r.Spent)/1e6, f.CapUSD)
		}
		acted := false
		for _, c := range th.Cards {
			if c.Answered || c.Session != session {
				continue
			}
			switch c.Card.Type {
			case "approval", "sign_in":
				answer, err := approveCard(ctx, cloud, phone, weeklyShown{ID: c.Card.ID, Session: session, Grant: c.Card.Grant, Draft: c.Card.Draft, Tools: c.Tools, SignsCard: c.Signs}, tenant, f.Tools)
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
				pick, err := scriptedChoice(c.Card)
				if err != nil {
					return r, err
				}
				b, _ := json.Marshal(pick)
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

// scriptedChoice is the owner's pick on a choices card: its first option,
// unless every option is a way of agreeing, which only the component route
// may do.
func scriptedChoice(c component) (string, error) {
	for _, o := range c.Options {
		if !approvalish.MatchString(o.Label) && !approvalish.MatchString(o.Value) {
			return o.Value, nil
		}
	}
	return "", errInWords
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
