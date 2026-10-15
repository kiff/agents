// Package chat is the owner talking to an agent. A conversation is one chat
// session, and each message the owner sends is a run in it, started and
// executed by a server.Runner: stored, claimed, recorded, and then ended, or
// paused when KIFF holds one of its calls. The Runner carries the
// conversation on from the session's finished runs: what the owner and the
// agent said in them (Conversation), not what tools returned.
package chat

import (
	"context"
	"encoding/json"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/session"
)

// Chat is the owner's conversation with one agent.
type Chat struct {
	Runner        server.Runner
	Tenant, Agent string
	// Session is the conversation's session: empty until the first message
	// opens it, then every message's.
	Session string
}

// Send runs text, the owner's message, as a new run in the conversation, and
// streams it to emit (nil: none). A message sent while another run of the
// conversation is paused is a run of its own. It returns the run as stored
// where it stopped; a run that failed is stored ERROR, and its cause is
// returned too.
func (c *Chat) Send(ctx context.Context, text string, emit func(agent.Event)) (run.Run, error) {
	r, err := c.Runner.Start(ctx, server.Start{Tenant: c.Tenant, Agent: c.Agent, Session: c.Session, Trigger: run.ByOwner, Input: text})
	if err != nil {
		return r, err
	}
	c.Session = r.Session
	return c.Runner.Execute(ctx, c.Tenant, r.ID, emit)
}

// Conversation turns the owner's and the agent's messages among events into
// model messages, joining consecutive ones of the same role. Tool calls and
// their results are left out, so what it returns is always valid to send.
func Conversation(events []session.Event) []model.Message {
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
