// Package anthropic is the model adapter for Anthropic's Claude API, through
// its official Go SDK.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/kiff/agents/model"
)

// Model runs turns on the Claude API.
type Model struct {
	client  sdk.Client
	windows sync.Map // model id → its window
}

// New returns a Model. Without options the SDK reads ANTHROPIC_API_KEY from
// the environment.
func New(opts ...option.RequestOption) *Model {
	return &Model{client: sdk.NewClient(opts...)}
}

// FastBeta is the beta that turns on fast output (Request.Fast).
const FastBeta = "fast-mode-2026-02-01"

// Turn streams one turn and returns it whole. A fast turn the API turns
// away for its fast limits (a 429, or overloaded) before anything streamed
// runs again at once at standard speed, so someone waiting gets an answer,
// only slower; the SDK's own retries would wait instead.
func (m *Model) Turn(ctx context.Context, req model.Request, onText func(string)) (model.Reply, error) {
	r, streamed, err := m.turn(ctx, req, onText)
	if err != nil && req.Fast && !streamed && busy(err) {
		req.Fast = false
		r, _, err = m.turn(ctx, req, onText)
	}
	return r, err
}

// busy says the API turned a call away for its limits or its load.
func busy(err error) bool {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode == 529) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "rate_limit_error") || strings.Contains(msg, "overloaded_error")
}

// turn streams one turn; streamed says whether any of it came.
func (m *Model) turn(ctx context.Context, req model.Request, onText func(string)) (model.Reply, bool, error) {
	params, err := toParams(req)
	if err != nil {
		return model.Reply{}, false, err
	}
	var opts []option.RequestOption
	if req.Fast {
		opts = append(opts, option.WithHeaderAdd("anthropic-beta", FastBeta), option.WithJSONSet("speed", "fast"),
			option.WithMaxRetries(0))
	}
	start := time.Now()
	stream := m.client.Messages.NewStreaming(ctx, params, opts...)
	defer stream.Close()
	var msg sdk.Message
	var first time.Duration
	thinking := 0
	for stream.Next() {
		ev := stream.Current()
		if err := msg.Accumulate(ev); err != nil {
			return model.Reply{}, first != 0, fmt.Errorf("anthropic: %w", err)
		}
		d, ok := ev.AsAny().(sdk.ContentBlockDeltaEvent)
		if !ok {
			continue
		}
		if first == 0 {
			first = time.Since(start)
		}
		switch t := d.Delta.AsAny().(type) {
		case sdk.ThinkingDelta:
			thinking += len(t.Thinking)
		case sdk.TextDelta:
			if onText != nil {
				onText(t.Text)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return model.Reply{}, first != 0, fmt.Errorf("anthropic: %w", err)
	}
	r := fromMessage(msg)
	r.FirstToken, r.ThinkingBytes = first, thinking
	// The API says the speed it ran at in usage.speed; a fast call it
	// accepted without saying ran fast.
	r.Fast = req.Fast
	if speed, ok := msg.Usage.JSON.ExtraFields["speed"]; ok && speed.Valid() {
		r.Fast = speed.Raw() == `"fast"`
	}
	return r, true, nil
}

// Window is the model's context window, from the Models API. It asks once
// per model id, without retries and for at most windowWait, and remembers
// the answer, or a failure that wasn't the caller giving up: the window is
// for the report, never worth slowing a turn for.
func (m *Model) Window(ctx context.Context, id string) (int64, error) {
	if w, ok := m.windows.Load(id); ok {
		return w.(window).tokens, w.(window).err
	}
	var w window
	ask, cancel := context.WithTimeout(ctx, windowWait)
	defer cancel()
	info, err := m.client.Models.Get(ask, id, sdk.ModelGetParams{}, option.WithMaxRetries(0))
	switch {
	case err != nil && ctx.Err() != nil: // the caller's turn was cancelled: ask again next time
		return 0, fmt.Errorf("anthropic: %w", err)
	case err != nil:
		w.err = fmt.Errorf("anthropic: %w", err)
	case info.MaxInputTokens <= 0:
		w.err = fmt.Errorf("anthropic: model %s has no context window", id)
	default:
		w.tokens = info.MaxInputTokens
	}
	m.windows.Store(id, w)
	return w.tokens, w.err
}

// windowWait is the longest Window waits for the Models API.
var windowWait = 5 * time.Second

type window struct {
	tokens int64
	err    error
}

func toParams(req model.Request) (sdk.MessageNewParams, error) {
	p := sdk.MessageNewParams{Model: sdk.Model(req.Model), MaxTokens: req.MaxTokens}
	if req.Effort != "" {
		p.OutputConfig = sdk.OutputConfigParam{Effort: sdk.OutputConfigEffort(req.Effort)}
	}
	if req.System != "" {
		p.System = []sdk.TextBlockParam{{Text: req.System}}
	}
	for _, msg := range req.Messages {
		var blocks []sdk.ContentBlockParamUnion
		for _, b := range msg.Blocks {
			switch b.Kind {
			case model.Text:
				blocks = append(blocks, sdk.NewTextBlock(b.Text))
			case model.ToolUse:
				blocks = append(blocks, sdk.NewToolUseBlock(b.CallID, b.Input, b.Tool))
			case model.ToolResult:
				blocks = append(blocks, sdk.NewToolResultBlock(b.CallID, b.Result, b.IsError))
			case model.Thinking: // back exactly as it came: dropping or editing it breaks the turn
				blocks = append(blocks, sdk.NewThinkingBlock(b.Signature, b.Text))
			case model.RedactedThinking:
				blocks = append(blocks, sdk.NewRedactedThinkingBlock(b.Data))
			case model.Server: // back exactly as it came, like thinking
				var u sdk.ContentBlockParamUnion
				if err := json.Unmarshal([]byte(b.Data), &u); err != nil {
					return p, fmt.Errorf("anthropic: a server block: %w", err)
				}
				blocks = append(blocks, u)
			default:
				return p, fmt.Errorf("anthropic: unknown block kind %q", b.Kind)
			}
		}
		if msg.Role == model.Assistant {
			p.Messages = append(p.Messages, sdk.NewAssistantMessage(blocks...))
		} else {
			p.Messages = append(p.Messages, sdk.NewUserMessage(blocks...))
		}
	}
	deferred := false
	for _, t := range req.Tools {
		// The whole schema goes through: properties and required in their
		// fields, every other root keyword ($defs, additionalProperties, …)
		// as an extra field. "type" is always object.
		var schema map[string]any
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
			return p, fmt.Errorf("anthropic: tool %s: input schema: %w", t.Name, err)
		}
		in := sdk.ToolInputSchemaParam{Properties: schema["properties"]}
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				if s, ok := r.(string); ok {
					in.Required = append(in.Required, s)
				}
			}
		}
		for k, v := range schema {
			if k == "properties" || k == "required" || k == "type" {
				continue
			}
			if in.ExtraFields == nil {
				in.ExtraFields = map[string]any{}
			}
			in.ExtraFields[k] = v
		}
		tp := sdk.ToolParam{Name: t.Name, InputSchema: in}
		if t.Description != "" {
			tp.Description = sdk.String(t.Description)
		}
		if t.Defer {
			tp.DeferLoading = sdk.Bool(true)
			deferred = true
		}
		p.Tools = append(p.Tools, sdk.ToolUnionParam{OfTool: &tp})
	}
	if deferred { // the model finds a deferred tool with this search, which is never deferred
		p.Tools = append(p.Tools, sdk.ToolUnionParam{OfToolSearchToolRegex20251119: &sdk.ToolSearchToolRegex20251119Param{
			Type: sdk.ToolSearchToolRegex20251119TypeToolSearchToolRegex20251119}})
	}
	if req.NoToolUse && len(p.Tools) > 0 { // with no tools there's nothing to forbid, and the API refuses a choice alone
		p.ToolChoice = sdk.ToolChoiceUnionParam{OfNone: &sdk.ToolChoiceNoneParam{}}
	}
	if req.Cache {
		markCache(&p, req.CacheLong, req.HistoryEnd)
	}
	return p, nil
}

// markCache sets up to three of the API's four cache breakpoints. The prompt
// renders tools, then system, then messages, and a breakpoint caches
// everything before it: one on the system prompt (or the last tool, without
// one) caches the prefix every turn shares; one on the history's last message
// (historyEnd) caches the earlier runs, which the next run renders the same
// way and reads back; one on the last message's last block caches the
// conversation, which this run's next turn reads back and extends.
//
// long keeps the shared prefix an hour: an owner's next message often comes
// after more than five minutes. The conversation's breakpoints stay five
// minutes; the API wants the longer one first, as it is here.
func markCache(p *sdk.MessageNewParams, long bool, historyEnd int) {
	cc := sdk.NewCacheControlEphemeralParam()
	shared := cc
	if long {
		shared.TTL = sdk.CacheControlEphemeralTTLTTL1h
	}
	if n := len(p.System); n > 0 {
		p.System[n-1].CacheControl = shared
	} else if n := len(p.Tools); n > 0 && p.Tools[n-1].OfTool != nil {
		p.Tools[n-1].OfTool.CacheControl = shared
	}
	if len(p.Messages) == 0 {
		return
	}
	if historyEnd > 0 && historyEnd < len(p.Messages) {
		markLast(p.Messages[historyEnd-1].Content, cc)
	}
	markLast(p.Messages[len(p.Messages)-1].Content, cc)
}

// markLast puts cc on the last block of blocks that takes a breakpoint.
func markLast(blocks []sdk.ContentBlockParamUnion, cc sdk.CacheControlEphemeralParam) {
	for i := len(blocks) - 1; i >= 0; i-- { // thinking blocks take no breakpoint
		b := &blocks[i]
		switch {
		case b.OfText != nil:
			b.OfText.CacheControl = cc
		case b.OfToolUse != nil:
			b.OfToolUse.CacheControl = cc
		case b.OfToolResult != nil:
			b.OfToolResult.CacheControl = cc
		default:
			continue
		}
		return
	}
}

func fromMessage(msg sdk.Message) model.Reply {
	r := model.Reply{Usage: model.Usage{
		InputTokens:  msg.Usage.InputTokens,
		OutputTokens: msg.Usage.OutputTokens,
		CacheRead:    msg.Usage.CacheReadInputTokens,
		// The hour's writes, priced apart; the rest kept five minutes.
		CacheWrite:   msg.Usage.CacheCreationInputTokens - msg.Usage.CacheCreation.Ephemeral1hInputTokens,
		CacheWrite1h: msg.Usage.CacheCreation.Ephemeral1hInputTokens,
	}}
	for _, c := range msg.Content {
		switch b := c.AsAny().(type) {
		case sdk.TextBlock:
			r.Blocks = append(r.Blocks, model.Block{Kind: model.Text, Text: b.Text})
		case sdk.ToolUseBlock:
			r.Blocks = append(r.Blocks, model.Block{Kind: model.ToolUse, CallID: b.ID, Tool: b.Name, Input: b.Input})
		case sdk.ThinkingBlock:
			r.Blocks = append(r.Blocks, model.Block{Kind: model.Thinking, Text: b.Thinking, Signature: b.Signature})
		case sdk.RedactedThinkingBlock:
			r.Blocks = append(r.Blocks, model.Block{Kind: model.RedactedThinking, Data: b.Data})
		case sdk.ServerToolUseBlock, sdk.ToolSearchToolResultBlock:
			// A tool search the API ran: the next turn needs it back, in place.
			if raw, err := json.Marshal(c.ToParam()); err == nil {
				r.Blocks = append(r.Blocks, model.Block{Kind: model.Server, Data: string(raw)})
			}
		}
	}
	switch msg.StopReason {
	case sdk.StopReasonEndTurn:
		r.Stop = model.EndTurn
	case sdk.StopReasonToolUse:
		r.Stop = model.CallTools
	case sdk.StopReasonMaxTokens:
		r.Stop = model.MaxTokens
	case sdk.StopReasonRefusal:
		r.Stop, r.Refusal = model.Refused, string(msg.StopDetails.Category)
	default:
		r.Stop = model.Other
	}
	return r
}
