package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/chat"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/tool"
)

// MaxScan bounds how many of a session's runs a run looks back over for its
// history, newest first, past the latest summary.
const MaxScan = 2 * MaxHistory

// tokens estimates what text takes in the model's context: about four bytes
// a token, which overestimates most text slightly.
func tokens(msgs []model.Message) int {
	n := 0
	for _, m := range msgs {
		for _, b := range m.Blocks {
			n += len(b.Text) + len(b.Input) + len(b.Result)
		}
	}
	return n / 4
}

// earlier is one finished run of the session before the current one, and
// what the model reads of it.
type earlier struct {
	id   string
	msgs []model.Message
}

// past is what a run gets of its session before its input.
type past struct {
	// msgs is the conversation of the runs it gets in full, oldest first.
	msgs []model.Message
	// summary is the latest summary of the runs before msgs, if any.
	summary *session.SummaryBody
	// fold is the runs that no longer fit, after the summary: what a new
	// summary covers. Empty when everything fits, or nothing is summarised.
	fold []earlier
	// dropped: runs before msgs that neither msgs nor a summary covers.
	dropped bool
	// opening is the owner's first message in the session, when the run it
	// is in isn't in msgs.
	opening string
	// shown is the session's components, oldest first, for the Staters.
	shown []json.RawMessage
}

// past reads what current gets of its session: its finished runs before it
// (newest first, up to d.History of them and, when set, d.HistoryTokens of
// their messages), the latest summary of the ones before those, the kept
// reads still in their windows, and the session's components. Which runs
// came before is read from the runs themselves, not from their events: the
// keep rules may have dropped all of the current run's own while it was
// paused.
func (rn *Runner) past(ctx context.Context, sess session.Session, events []session.Event, current run.Run, d registry.Definition, tools []tool.Tool) (past, error) {
	var p past
	var order []string // the session's runs, as SortEvents orders them
	byRun := map[string][]session.Event{}
	for _, e := range events {
		if len(order) == 0 || order[len(order)-1] != e.Run {
			order = append(order, e.Run)
		}
		byRun[e.Run] = append(byRun[e.Run], e)
		if e.Kind == session.Component {
			p.shown = append(p.shown, e.Body)
		}
	}

	finished := map[string]bool{}
	final := func(id string) (bool, error) {
		if ok, seen := finished[id]; seen {
			return ok, nil
		}
		finished[id] = false
		if id == current.ID || slices.Contains(sess.Held, id) {
			return false, nil
		}
		r, err := rn.Runs.Get(ctx, sess.Tenant, id)
		if errors.Is(err, run.ErrNotFound) {
			return false, nil // its run is gone: deleted, or expired before its events
		}
		if err != nil {
			return false, err
		}
		finished[id] = r.Status.Final() && r.Created.Before(current.Created)
		return finished[id], nil
	}

	// The latest summary a finished run wrote, and where it ends.
	from := 0
	for i := len(order) - 1; i >= 0 && p.summary == nil; i-- {
		for _, e := range byRun[order[i]] {
			if e.Kind != session.Summary {
				continue
			}
			ok, err := final(order[i])
			if err != nil {
				return past{}, err
			}
			var s session.SummaryBody
			if ok && json.Unmarshal(e.Body, &s) == nil && s.Text != "" {
				if at := slices.Index(order, s.Through); at >= 0 {
					p.summary, from = &s, at+1
				}
			}
		}
	}

	// The finished runs after it, newest first, as long as they fit.
	limit := min(d.History, MaxHistory)
	var fit, rest []earlier
	size, scanned := 0, 0
	for i := len(order) - 1; i >= from && scanned < MaxScan; i-- {
		ok, err := final(order[i])
		if err != nil {
			return past{}, err
		}
		if !ok {
			continue
		}
		scanned++
		e := earlier{id: order[i], msgs: chat.Conversation(byRun[order[i]])}
		if len(rest) == 0 && len(fit) < limit && (d.HistoryTokens == 0 || size+tokens(e.msgs) <= d.HistoryTokens) {
			fit, size = append(fit, e), size+tokens(e.msgs)
			continue
		}
		rest = append(rest, e)
	}
	slices.Reverse(fit)
	slices.Reverse(rest)

	if len(rest) > 0 {
		if d.HistoryTokens > 0 && sess.Kind == session.KindChat {
			// Summarise down to half of each bound, so the next runs fit
			// without summarising again at once.
			for len(fit) > 1 && (len(fit) > max(limit/2, 1) || tokens(flatten(fit)) > d.HistoryTokens/2) {
				rest, fit = append(rest, fit[0]), fit[1:]
			}
			p.fold = rest
		} else {
			p.dropped = true
		}
	}
	p.msgs = flatten(fit)

	// The owner's first message, unless it is in what the run reads.
	if len(order) > 0 && (len(fit) == 0 || fit[0].id != order[0]) {
		for _, e := range byRun[order[0]] {
			var body agent.TextBody
			if e.Kind == session.OwnerMessage && json.Unmarshal(e.Body, &body) == nil && body.Text != "" {
				p.opening = body.Text
				break
			}
		}
	}
	if p.opening == "" && p.summary != nil {
		p.opening = p.summary.Opening // the first run's own events are gone
	}
	return p, nil
}

// flatten is the runs' conversations, one after another.
func flatten(runs []earlier) []model.Message {
	var msgs []model.Message
	for _, e := range runs {
		msgs = append(msgs, e.msgs...)
	}
	return msgs
}

// SummaryNote opens the summary of a chat's earlier part, as the model
// reads it before what follows.
const SummaryNote = "The earlier part of this conversation no longer fits, so you summarised it; your summary follows, then the conversation carries on from where it ends."

// DroppedNote stands for earlier runs a run doesn't get, when nothing
// summarises them: the agent knows something is missing instead of
// guessing.
const DroppedNote = "Earlier messages of this conversation are no longer shown to you. Don't guess what they said: ask the owner, or look up where things stand."

// OpeningNote introduces the owner's first message, when the run it is in
// isn't shown.
const OpeningNote = "The owner opened this conversation with:\n\n"

// StateNote opens what the agent's tools say of where things stand now.
const StateNote = "Where things stand now, as their own sources say (this is current; the conversation may be older):"

// NamesNote ends a chat run's instructions: its tools' results aren't kept for the
// chat's next messages, only what it writes, so it writes what a follow-up
// ("refund it") needs to act on.
const NamesNote = "What your tools return isn't kept for this chat's next messages: only what you write is. When you answer, name what the owner may act on next, with its id and amount (for example the charge id), so a follow-up such as \"refund it\" can act on it without looking it up again."

// lead is what comes before the conversation the run reads: the summary, or
// a note that earlier messages are missing, and the opening message.
func (p past) lead() []model.Block {
	var b []model.Block
	if p.summary != nil {
		b = append(b, model.Block{Kind: model.Text, Text: SummaryNote + "\n\n" + p.summary.Text})
	}
	if p.dropped {
		b = append(b, model.Block{Kind: model.Text, Text: DroppedNote})
	}
	if p.opening != "" && len(b) > 0 {
		b = append(b, model.Block{Kind: model.Text, Text: OpeningNote + p.opening})
	}
	return b
}

// history is the conversation the run reads before its input, led by what
// stands for its earlier part. With no conversation to lead, the lead goes
// into the preface instead.
func (p past) history() (msgs []model.Message, preface []model.Block) {
	lead := p.lead()
	msgs = slices.Clone(p.msgs)
	if len(lead) == 0 {
		return msgs, nil
	}
	if len(msgs) == 0 {
		return nil, lead
	}
	if msgs[0].Role == model.User {
		msgs[0] = model.Message{Role: model.User, Blocks: append(lead, msgs[0].Blocks...)}
	} else {
		msgs = append([]model.Message{{Role: model.User, Blocks: lead}}, msgs...)
	}
	return msgs, nil
}

// age is how long ago, in the coarsest unit that says it.
func age(d time.Duration) string {
	switch m := int(d.Minutes()); {
	case m < 1:
		return "less than a minute"
	case m == 1:
		return "1 minute"
	case m < 60:
		return fmt.Sprintf("%d minutes", m)
	case m < 120:
		return "1 hour"
	default:
		return fmt.Sprintf("%d hours", m/60)
	}
}

// state is what the agent's Staters say of the session's components now. A
// tool that fails says nothing: the agent can still ask it itself.
func state(ctx context.Context, tools []tool.Tool, shown []json.RawMessage) string {
	if len(shown) == 0 {
		return ""
	}
	var parts []string
	for _, t := range tools {
		s, ok := t.(tool.Stater)
		if !ok {
			continue
		}
		text, err := s.State(ctx, shown)
		if err != nil || strings.TrimSpace(text) == "" {
			continue
		}
		parts = append(parts, strings.TrimSpace(text))
	}
	if len(parts) == 0 {
		return ""
	}
	return StateNote + "\n\n" + strings.Join(parts, "\n\n")
}

// preface is what goes before the run's input, in the same message: what
// leads the conversation when there is none, where things stand, and the
// kept reads. It is never recorded: each run makes its own.
func (p past) preface(ctx context.Context, tools []tool.Tool, now time.Time) []model.Block {
	_, out := p.history()
	if s := state(ctx, tools, p.shown); s != "" {
		out = append(out, model.Block{Kind: model.Text, Text: s})
	}
	return out
}
