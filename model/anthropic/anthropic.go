// Package anthropic is the model adapter for Anthropic's Claude API, through
// its official Go SDK.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/kiff/agents/model"
)

// Model runs turns on the Claude API.
type Model struct {
	client sdk.Client
}

// New returns a Model. Without options the SDK reads ANTHROPIC_API_KEY from
// the environment.
func New(opts ...option.RequestOption) *Model {
	return &Model{client: sdk.NewClient(opts...)}
}

// Turn streams one turn and returns it whole.
func (m *Model) Turn(ctx context.Context, req model.Request, onText func(string)) (model.Reply, error) {
	params, err := toParams(req)
	if err != nil {
		return model.Reply{}, err
	}
	stream := m.client.Messages.NewStreaming(ctx, params)
	defer stream.Close()
	var msg sdk.Message
	for stream.Next() {
		ev := stream.Current()
		if err := msg.Accumulate(ev); err != nil {
			return model.Reply{}, fmt.Errorf("anthropic: %w", err)
		}
		if onText == nil {
			continue
		}
		if d, ok := ev.AsAny().(sdk.ContentBlockDeltaEvent); ok {
			if t, ok := d.Delta.AsAny().(sdk.TextDelta); ok {
				onText(t.Text)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return model.Reply{}, fmt.Errorf("anthropic: %w", err)
	}
	return fromMessage(msg), nil
}

func toParams(req model.Request) (sdk.MessageNewParams, error) {
	p := sdk.MessageNewParams{Model: sdk.Model(req.Model), MaxTokens: req.MaxTokens}
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
		p.Tools = append(p.Tools, sdk.ToolUnionParam{OfTool: &tp})
	}
	if req.NoToolUse && len(p.Tools) > 0 { // with no tools there's nothing to forbid, and the API refuses a choice alone
		p.ToolChoice = sdk.ToolChoiceUnionParam{OfNone: &sdk.ToolChoiceNoneParam{}}
	}
	return p, nil
}

func fromMessage(msg sdk.Message) model.Reply {
	r := model.Reply{Usage: model.Usage{
		InputTokens:  msg.Usage.InputTokens,
		OutputTokens: msg.Usage.OutputTokens,
		CacheRead:    msg.Usage.CacheReadInputTokens,
		CacheWrite:   msg.Usage.CacheCreationInputTokens,
	}}
	for _, c := range msg.Content {
		switch b := c.AsAny().(type) {
		case sdk.TextBlock:
			r.Blocks = append(r.Blocks, model.Block{Kind: model.Text, Text: b.Text})
		case sdk.ToolUseBlock:
			r.Blocks = append(r.Blocks, model.Block{Kind: model.ToolUse, CallID: b.ID, Tool: b.Name, Input: b.Input})
		}
	}
	switch msg.StopReason {
	case sdk.StopReasonEndTurn:
		r.Stop = model.EndTurn
	case sdk.StopReasonToolUse:
		r.Stop = model.CallTools
	case sdk.StopReasonMaxTokens:
		r.Stop = model.MaxTokens
	default:
		r.Stop = model.Other
	}
	return r
}
