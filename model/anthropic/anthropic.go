// Package anthropic is the model adapter for Anthropic's Claude API, through
// its official Go SDK.
package anthropic

import (
	"context"
	"encoding/base64"
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

// BindingBeta lets a request say what happens to a thinking block whose
// conversation changed since it was made: a run clears tool results it has
// read (agent.ClearAfter), and every thinking block after a cleared result is
// then bound to a conversation the request no longer sends. Dropped, the run
// goes on without that reasoning; otherwise the API refuses the turn, after
// the run's calls have already gone (kiff/tafs, the 859 device test).
const BindingBeta = "thinking-binding-controls-2026-08-01"

// UpdatesBeta lets a request ask for the model's progress notes between tool
// calls, with its reasoning hidden (model.ThinkingUpdates): thinking display
// "updates". Without it the API refuses that display.
const UpdatesBeta = "thinking-display-updates-2026-08-18"

// interrupted is the note the API puts where a turn that stopped short left
// work unfinished. It says nothing to someone waiting, so it isn't shown.
const interrupted = "This part of the response was interrupted before it finished."

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
	// The turn's betas go in one anthropic-beta header, comma-separated, as
	// the API reads them: two lines of it may be read as one.
	var betas []string
	if replaysThinking(req) {
		if params.Thinking.OfAdaptive == nil && params.Thinking.OfEnabled == nil {
			// block_binding needs a thinking type: a model that left a thinking
			// block thinks unasked, so adaptive is what it runs anyway.
			params.Thinking = sdk.ThinkingConfigParamUnion{OfAdaptive: &sdk.ThinkingConfigAdaptiveParam{}}
		}
		betas = append(betas, BindingBeta)
		opts = append(opts, option.WithJSONSet("thinking.block_binding.prefix_mismatch_behavior", "drop_block"))
	}
	updates := req.Thinking == model.ThinkingUpdates
	if updates {
		betas = append(betas, UpdatesBeta)
	}
	if req.Fast {
		betas = append(betas, FastBeta)
		opts = append(opts, option.WithJSONSet("speed", "fast"), option.WithMaxRetries(0))
	}
	if len(betas) > 0 {
		opts = append(opts, option.WithHeader("anthropic-beta", strings.Join(betas, ",")))
	}
	start := time.Now()
	stream := m.client.Messages.NewStreaming(ctx, params, opts...)
	defer stream.Close()
	var msg sdk.Message
	var first time.Duration
	thinking := 0
	// Under updates, every thinking block with text is one progress note,
	// shown whole when its block ends; an empty one shows nothing.
	var note strings.Builder
	for stream.Next() {
		ev := stream.Current()
		if err := msg.Accumulate(ev); err != nil {
			return model.Reply{}, first != 0, fmt.Errorf("anthropic: %w", err)
		}
		var d sdk.ContentBlockDeltaEvent
		switch e := ev.AsAny().(type) {
		case sdk.ContentBlockDeltaEvent:
			d = e
		case sdk.ContentBlockStopEvent:
			if s := strings.TrimSpace(note.String()); s != "" && s != interrupted && req.OnUpdate != nil {
				req.OnUpdate(s)
			}
			note.Reset()
			continue
		default:
			continue
		}
		if first == 0 {
			first = time.Since(start)
		}
		switch t := d.Delta.AsAny().(type) {
		case sdk.ThinkingDelta:
			thinking += len(t.Thinking)
			if updates {
				note.WriteString(t.Thinking)
			} else if req.Thinking != "" && req.OnThinking != nil && t.Thinking != "" {
				req.OnThinking(t.Thinking)
			}
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

// replaysThinking reports whether req sends back a thinking block.
func replaysThinking(req model.Request) bool {
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			if b.Kind == model.Thinking || b.Kind == model.RedactedThinking {
				return true
			}
		}
	}
	return false
}

func toParams(req model.Request) (sdk.MessageNewParams, error) {
	p := sdk.MessageNewParams{Model: sdk.Model(req.Model), MaxTokens: req.MaxTokens}
	if req.Effort != "" {
		p.OutputConfig = sdk.OutputConfigParam{Effort: sdk.OutputConfigEffort(req.Effort)}
	}
	switch req.Thinking {
	case model.ThinkingSummarized:
		// Adaptive, as the model runs with no thinking set, with its summary shown.
		p.Thinking = sdk.ThinkingConfigParamUnion{OfAdaptive: &sdk.ThinkingConfigAdaptiveParam{Display: sdk.ThinkingConfigAdaptiveDisplaySummarized}}
	case model.ThinkingUpdates:
		// Adaptive, with only the progress notes shown (UpdatesBeta). The SDK
		// names this display on its beta types only; the value is the same.
		p.Thinking = sdk.ThinkingConfigParamUnion{OfAdaptive: &sdk.ThinkingConfigAdaptiveParam{Display: sdk.ThinkingConfigAdaptiveDisplay(sdk.BetaThinkingConfigAdaptiveDisplayUpdates)}}
	}
	if req.System != "" {
		p.System = []sdk.TextBlockParam{{Text: req.System}}
	}
	unsent := unsentServer(req)
	for _, msg := range req.Messages {
		var blocks []sdk.ContentBlockParamUnion
		for _, b := range msg.Blocks {
			if id, _, ok := serverPart(b); ok && unsent[id] {
				continue
			}
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
			case model.File:
				fb, err := fileBlock(b)
				if err != nil {
					return p, err
				}
				blocks = append(blocks, fb)
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
		if len(blocks) == 0 && len(msg.Blocks) > 0 { // only a server tool left out: the turns still alternate
			blocks = append(blocks, sdk.NewTextBlock("[a server tool, no longer offered]"))
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
			Type: sdk.ToolSearchToolRegex20251119TypeToolSearchToolRegex20251119}}) // named searchTool
	}
	if w := req.Web; w != nil {
		// The versions without code execution: their results come back as
		// plain search and fetch blocks, which the next turn sends back.
		search := sdk.WebSearchTool20250305Param{AllowedDomains: w.Domains}
		fetch := sdk.WebFetchTool20250910Param{AllowedDomains: w.Domains}
		if w.MaxUses > 0 {
			search.MaxUses, fetch.MaxUses = sdk.Int(int64(w.MaxUses)), sdk.Int(int64(w.MaxUses))
		}
		if w.MaxFetchTokens > 0 {
			fetch.MaxContentTokens = sdk.Int(int64(w.MaxFetchTokens))
		}
		p.Tools = append(p.Tools, sdk.ToolUnionParam{OfWebSearchTool20250305: &search}, sdk.ToolUnionParam{OfWebFetchTool20250910: &fetch})
	}
	if req.NoToolUse && len(p.Tools) > 0 { // with no tools there's nothing to forbid, and the API refuses a choice alone
		p.ToolChoice = sdk.ToolChoiceUnionParam{OfNone: &sdk.ToolChoiceNoneParam{}}
	}
	if req.Cache {
		markCache(&p, req.CacheLong, req.HistoryEnd)
	}
	return p, nil
}

// offersSearch: req offers a deferred tool, so toParams offers the tool search
// beside it.
func offersSearch(req model.Request) bool {
	for _, t := range req.Tools {
		if t.Defer {
			return true
		}
	}
	return false
}

// unsentServer are the ids of the server tool calls in req's messages that
// can't go back as they came: both the call and its result are left out, as
// the API refuses either alone. A call goes back only while the request
// offers its tool, and a tool search only while it also offers every tool
// its result found. A run's tools can change between turns (Setup's reads
// end once the owner has decided, and with them its last deferred tool; web
// search is offered on connect and repair turns only), and the API then
// refuses a result whose tool isn't offered (kiff/agents#293). Any server
// tool's call and its result go back together and in order, or neither
// does: a result with no call before it, or a call whose result never came,
// is left out with its other half (a resumed run can lose one with a turn
// cut from its front).
func unsentServer(req model.Request) map[string]bool {
	offered := map[string]bool{}
	for _, t := range req.Tools {
		offered[t.Name] = true
	}
	server := map[string]bool{}
	if offersSearch(req) {
		server[searchTool] = true
	}
	if req.Web != nil {
		server["web_search"], server["web_fetch"] = true, true
	}
	unsent := map[string]bool{}
	called, answered := map[string]bool{}, map[string]bool{}
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			id, result, ok := serverPart(b)
			if !ok {
				continue
			}
			if result {
				if !called[id] {
					unsent[id] = true
				}
				answered[id] = true
				continue
			}
			called[id] = true
			if !server[serverName(b)] {
				unsent[id] = true
			}
		}
	}
	for id := range called {
		if !answered[id] {
			unsent[id] = true
		}
	}
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			id, ok := searchResult(b)
			if !ok {
				continue
			}
			var v struct {
				Content struct {
					References []struct {
						ToolName string `json:"tool_name"`
					} `json:"tool_references"`
				} `json:"content"`
			}
			if json.Unmarshal([]byte(b.Data), &v) != nil {
				continue
			}
			for _, r := range v.Content.References {
				if !offered[r.ToolName] {
					unsent[id] = true
				}
			}
		}
	}
	return unsent
}

// searchTool is the name of the tool search toParams offers beside deferred
// tools.
const searchTool = "tool_search_tool_regex"

// serverPart is the id of the server tool call b is part of, and whether b is
// its result: a call carries its id, a result the id of the call it answers.
func serverPart(b model.Block) (id string, result, ok bool) {
	if b.Kind != model.Server {
		return "", false, false
	}
	var v struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		ToolUseID string `json:"tool_use_id"`
	}
	if json.Unmarshal([]byte(b.Data), &v) != nil {
		return "", false, false
	}
	switch {
	case v.Type == "server_tool_use" && v.ID != "":
		return v.ID, false, true
	case v.ToolUseID != "":
		return v.ToolUseID, true, true
	}
	return "", false, false
}

// serverName is the tool a server call b names.
func serverName(b model.Block) string {
	var v struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal([]byte(b.Data), &v)
	return v.Name
}

// searchResult is the id of the tool search call b answers, when b is a
// tool search's result.
func searchResult(b model.Block) (string, bool) {
	if b.Kind != model.Server {
		return "", false
	}
	var v struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
	}
	if json.Unmarshal([]byte(b.Data), &v) != nil || v.Type != "tool_search_tool_result" {
		return "", false
	}
	return v.ToolUseID, true
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
		// The history's end keeps as long as the shared part: the next run of
		// the chat reads it back, and a person's next message often comes
		// after a pause longer than five minutes (kiff/agents#242).
		markLast(p.Messages[historyEnd-1].Content, shared)
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
		case b.OfImage != nil:
			b.OfImage.CacheControl = cc
		case b.OfDocument != nil:
			b.OfDocument.CacheControl = cc
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
		case sdk.ServerToolUseBlock, sdk.ToolSearchToolResultBlock, sdk.WebSearchToolResultBlock, sdk.WebFetchToolResultBlock:
			// A server tool the API ran: the next turn needs it back, in place.
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

// fileBlock is a file the owner attached, as the model reads it: a photo as
// an image, a PDF as a document, text as a plain-text document, each titled
// with the owner's name for it. A file whose bytes this turn doesn't carry
// is a line saying so, so the conversation still reads in order.
func fileBlock(b model.Block) (sdk.ContentBlockParamUnion, error) {
	if len(b.Bytes) == 0 {
		return sdk.NewTextBlock(fmt.Sprintf("[%s: attached earlier, no longer in view; the owner can send it again]", b.Name)), nil
	}
	switch b.MediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return sdk.NewImageBlockBase64(b.MediaType, base64.StdEncoding.EncodeToString(b.Bytes)), nil
	case "application/pdf":
		d := sdk.NewDocumentBlock(sdk.Base64PDFSourceParam{Data: base64.StdEncoding.EncodeToString(b.Bytes)})
		d.OfDocument.Title = sdk.String(b.Name)
		return d, nil
	case model.FileText:
		d := sdk.NewDocumentBlock(sdk.PlainTextSourceParam{Data: string(b.Bytes)})
		d.OfDocument.Title = sdk.String(b.Name)
		return d, nil
	}
	return sdk.ContentBlockParamUnion{}, fmt.Errorf("anthropic: a file of type %q", b.MediaType)
}
