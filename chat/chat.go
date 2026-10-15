// Package chat is the owner talking to an agent. Each message is a chat
// session of its own, ended when the agent answers, so what tools returned is
// dropped at once under the default keep rules. A message within Window of
// the agent's last chat carries on that conversation: the agent sees what was
// said, though not what tools returned.
package chat

import (
	"context"
	"encoding/json"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/session"
)

// Window is how recent the last chat must be for a message to carry it on.
const Window = 30 * time.Minute

// listed bounds how many of an agent's sessions are read to find its last chat.
const listed = 20

// History is the conversation of the agent's last chat, when it was active
// within Window of now: what the owner and the agent said, in order. It's nil
// when there is none to carry on.
func History(ctx context.Context, store session.Store, tenant, agentID string, now time.Time) ([]model.Message, error) {
	sessions, err := store.List(ctx, tenant, agentID, listed)
	if err != nil {
		return nil, err
	}
	for _, s := range sessions {
		if s.Kind != session.KindChat {
			continue
		}
		if now.Sub(s.Updated) > Window {
			return nil, nil
		}
		_, events, err := store.Read(ctx, tenant, s.ID)
		if err != nil {
			return nil, err
		}
		return conversation(events), nil
	}
	return nil, nil
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

// Send runs one owner message as a chat session: it carries on the last chat
// within Window, streams the run to emit, and ends the session.
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
	if err := r.End(ctx, status); err != nil && runErr == nil {
		runErr = err
	}
	return r.Session(), out, runErr
}
