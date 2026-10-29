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
	"github.com/kiff/agents/attach"
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
// their results are left out, so what it returns is always valid to send;
// each call is there as a line of the agent's (session.Called).
func Conversation(events []session.Event) []model.Message {
	var msgs []model.Message
	for _, e := range events {
		var role model.Role
		var text string
		switch e.Kind {
		case session.OwnerMessage:
			role = model.User
		case session.AgentMessage, session.Report:
			role = model.Assistant
		case session.Called:
			// What a run called and what came of it, in words: the call's own
			// events are gone once the run ends, and without them a look-up
			// made in one message didn't count in the next (the R-3005 test).
			var body agent.CalledBody
			if json.Unmarshal(e.Body, &body) != nil || body.Tool == "" {
				continue
			}
			role, text = model.Assistant, agent.CalledLine(body)
		case session.Component:
			// What the agent showed stays in its history, so an answer
			// that names a component ("[approval connect-stripe] approved")
			// can be followed up: the grant it named, the options it gave.
			role, text = model.Assistant, "[shown] "+agent.Unforged(string(e.Body))
		default:
			continue
		}
		var blocks []model.Block
		if text == "" {
			var body agent.TextBody
			if json.Unmarshal(e.Body, &body) != nil || (body.Text == "" && len(body.Attachments) == 0) {
				continue
			}
			if body.Text != "" {
				// Only a Called event is a line of the record: anything
				// written to look like one, by the owner or the agent, isn't.
				blocks = append(blocks, model.Block{Kind: model.Text, Text: agent.Unforged(body.Text)})
			}
			// The files sent with an owner's message, by reference: the
			// runner loads their bytes for a turn (attach.Service.Load).
			if role == model.User {
				blocks = append(blocks, attach.Blocks(body.Attachments)...)
			}
		} else {
			blocks = []model.Block{{Kind: model.Text, Text: text}}
		}
		if len(blocks) == 0 {
			continue
		}
		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			msgs[n-1].Blocks = append(msgs[n-1].Blocks, blocks...)
			continue
		}
		msgs = append(msgs, model.Message{Role: role, Blocks: blocks})
	}
	// A conversation the model carries on ends with its own answer.
	if n := len(msgs); n > 0 && msgs[n-1].Role == model.User {
		msgs = msgs[:n-1]
	}
	return msgs
}
