// Package chat is the owner talking to an agent. Each message is a chat
// session of its own, ended when the agent answers, so what tools returned is
// dropped at once under the default keep rules. Chats that follow one another
// within Window form one conversation: a new message carries all of it on, so
// the agent sees everything said, though not what tools returned.
package chat

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/session"
)

// Window is the longest pause within one conversation: a message carries on
// the chats before it while each followed the last within Window.
const Window = 30 * time.Minute

// MaxChats bounds how many earlier chats (one message each) a conversation
// carries on.
const MaxChats = 50

// EndTimeout bounds ending a chat's session, which runs even when the request
// that sent the message was cancelled.
const EndTimeout = 10 * time.Second

// History is the agent's current conversation as of now: every chat that
// followed the one before it within Window, the last within Window of now.
// It holds what the owner and the agent said, in order, and is nil when there
// is no conversation to carry on.
func History(ctx context.Context, store session.Store, tenant, agentID string, now time.Time) ([]model.Message, error) {
	sessions, err := store.List(ctx, tenant, agentID, 2*MaxChats)
	if err != nil {
		return nil, err
	}
	var chain []session.Session // newest first
	edge := now
	for _, s := range sessions {
		if s.Kind != session.KindChat {
			continue
		}
		if edge.Sub(s.Updated) > Window || len(chain) == MaxChats {
			break
		}
		chain = append(chain, s)
		edge = s.Started
	}
	var events []session.Event
	for _, s := range slices.Backward(chain) {
		_, evs, err := store.Read(ctx, tenant, s.ID)
		if errors.Is(err, session.ErrNotFound) {
			continue // deleted meanwhile
		}
		if err != nil {
			return nil, err
		}
		events = append(events, evs...)
	}
	return conversation(events), nil
}

// conversation turns owner and agent messages into model messages, joining
// consecutive ones of the same role.
func conversation(events []session.Event) []model.Message {
	var msgs []model.Message
	for _, e := range events {
		var role model.Role
		switch e.Kind {
		case session.OwnerMessage:
			role = model.User
		case session.AgentMessage, session.Report:
			role = model.Assistant
		default:
			continue
		}
		var body agent.TextBody
		if json.Unmarshal(e.Body, &body) != nil || body.Text == "" {
			continue
		}
		block := model.Block{Kind: model.Text, Text: body.Text}
		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			msgs[n-1].Blocks = append(msgs[n-1].Blocks, block)
			continue
		}
		msgs = append(msgs, model.Message{Role: role, Blocks: []model.Block{block}})
	}
	// A conversation the model carries on ends with its own answer.
	if n := len(msgs); n > 0 && msgs[n-1].Role == model.User {
		msgs = msgs[:n-1]
	}
	return msgs
}

// NotEnded is returned when a chat ran but its session couldn't be ended. The
// session takes no more events; Retry ends it, and drops what was kept only
// until the end.
type NotEnded struct {
	Recording *session.Recording
	Status    session.Status
	Err       error
}

func (e *NotEnded) Error() string { return "chat: the session was not ended: " + e.Err.Error() }
func (e *NotEnded) Unwrap() error { return e.Err }

// Retry ends the session again.
func (e *NotEnded) Retry(ctx context.Context) error { return e.Recording.End(ctx, e.Status) }

// Send runs one owner message as a chat session: it carries on the current
// conversation, streams the run to emit, and ends the session. Ending uses its
// own bounded context, so a cancelled request still ends it; if ending fails
// anyway, the error is a *NotEnded to retry.
func Send(ctx context.Context, rec *session.Recorder, store session.Store, a agent.Agent, text string, now time.Time, emit func(agent.Event)) (session.Session, agent.Outcome, error) {
	if err := a.Check(); err != nil {
		return session.Session{}, agent.Outcome{}, err
	}
	history, err := History(ctx, store, a.Tenant, a.ID, now)
	if err != nil {
		return session.Session{}, agent.Outcome{}, err
	}
	r, err := rec.Start(ctx, a.Tenant, a.ID, session.KindChat)
	if err != nil {
		return session.Session{}, agent.Outcome{}, err
	}
	out, _, runErr := agent.Run(ctx, a, r, history, text, emit)
	status := session.Ended
	if runErr != nil {
		status = session.Failed
	}
	endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), EndTimeout)
	defer cancel()
	if err := r.End(endCtx, status); err != nil {
		ne := &NotEnded{Recording: r, Status: status, Err: err}
		if runErr != nil {
			return r.Session(), out, errors.Join(runErr, ne)
		}
		return r.Session(), out, ne
	}
	return r.Session(), out, runErr
}
