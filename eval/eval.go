// Package eval grades what an agent did. A Case names the run to make and the
// checks to grade it with; a check reads the run's session events, in code or
// through a model as judge. Grading gives a score per case and what the run
// cost, in total and per item worked.
//
// Grading reads only session events, so the same cases grade any runtime that
// records them: today's tafs runtime on Managed Agents (eval/managed) and the
// runtime in this repository.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
)

// Run is one recorded run of an agent: its session, where it is, and its
// events, in order.
type Run struct {
	Session session.Session
	Status  run.Status
	Events  []session.Event
}

// Grade is one check's verdict on a run. A skipped check isn't counted in the
// score, and says why it was skipped.
type Grade struct {
	Pass    bool   `json:"pass"`
	Skipped bool   `json:"skipped,omitempty"`
	Why     string `json:"why,omitempty"`
}

// Check grades one thing a good run does.
type Check struct {
	Name string
	// Grade reads the run. j is nil when no judge is configured; a check that
	// needs one returns a skipped Grade.
	Grade func(ctx context.Context, r Run, j *Judge) (Grade, error)
}

// Case is a run to grade: what the agent is asked, how many items the run
// works (for the cost per item), and the checks. Smoke cases run on every PR
// that touches the runtime; the rest run nightly.
type Case struct {
	Name   string
	Job    string
	Input  string
	Items  int
	Smoke  bool
	Checks []Check
}

// CheckResult is a check's name and its grade.
type CheckResult struct {
	Name string `json:"name"`
	Grade
}

// Result is a case graded on one run. Score is the share of graded checks
// that passed, from 0 to 1. Costs are in millionths of a US dollar: the run's,
// the run's per item, and the judge's, kept apart so judging never counts
// against the agent.
type Result struct {
	Case          string        `json:"case"`
	Session       string        `json:"session"`
	Score         float64       `json:"score"`
	Passed        int           `json:"passed"`
	Graded        int           `json:"graded"`
	Checks        []CheckResult `json:"checks"`
	CostMicroUSD  int64         `json:"cost_micro_usd"`
	PerItemMicro  int64         `json:"cost_per_item_micro_usd"`
	JudgeMicroUSD int64         `json:"judge_cost_micro_usd"`
	Context       Context       `json:"context"`
	// Report is what the agent said last, which the checks read: kept so a
	// check that fails on it ("the report doesn't name …") can be read.
	Report string `json:"report,omitempty"`
}

// Context is what the run's turns read, from its Usage events: the tokens
// they took as input in all, the share the cache served, the largest turn
// and its tool results, and the least room any turn left in the model's
// window (-1 when no turn knew its window).
type Context struct {
	Turns            int     `json:"turns"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CacheShare       float64 `json:"cache_share"`
	PeakPromptTokens int64   `json:"peak_prompt_tokens"`
	PeakResultsBytes int     `json:"peak_tool_results_bytes"`
	LeastRoomTokens  int64   `json:"least_room_tokens"`
}

func contextOf(events []session.Event) (Context, error) {
	c := Context{LeastRoomTokens: -1}
	for _, e := range events {
		if e.Kind != session.Usage {
			continue
		}
		var u session.UsageBody
		if err := json.Unmarshal(e.Body, &u); err != nil {
			return c, err
		}
		c.Turns++
		c.PromptTokens += u.Prompt()
		c.CacheReadTokens += u.CacheRead
		c.CacheWriteTokens += u.CacheWrite
		c.PeakPromptTokens = max(c.PeakPromptTokens, u.Prompt())
		if u.Context != nil {
			c.PeakResultsBytes = max(c.PeakResultsBytes, u.Context.ToolResultsBytes)
		}
		if room, ok := u.Room(); ok && (c.LeastRoomTokens < 0 || room < c.LeastRoomTokens) {
			c.LeastRoomTokens = room
		}
	}
	if c.PromptTokens > 0 {
		c.CacheShare = float64(c.CacheReadTokens) / float64(c.PromptTokens)
	}
	return c, nil
}

// Grade runs every check of c over r. A check's error stops grading: a
// result with a check missing would score as something it isn't.
func (c Case) Grade(ctx context.Context, r Run, j *Judge) (Result, error) {
	cost, err := session.Cost(r.Events)
	if err != nil {
		return Result{}, fmt.Errorf("eval: %s: cost: %w", c.Name, err)
	}
	res := Result{Case: c.Name, Session: r.Session.ID, CostMicroUSD: cost}
	if res.Report, err = r.Report(); err != nil {
		return Result{}, fmt.Errorf("eval: %s: report: %w", c.Name, err)
	}
	if res.Context, err = contextOf(r.Events); err != nil {
		return Result{}, fmt.Errorf("eval: %s: context: %w", c.Name, err)
	}
	if c.Items > 0 {
		res.PerItemMicro = (cost + int64(c.Items) - 1) / int64(c.Items)
	}
	var spent int64
	if j != nil {
		spent = j.Spent()
	}
	for _, ch := range c.Checks {
		g, err := ch.Grade(ctx, r, j)
		if err != nil {
			return Result{}, fmt.Errorf("eval: %s: %s: %w", c.Name, ch.Name, err)
		}
		res.Checks = append(res.Checks, CheckResult{Name: ch.Name, Grade: g})
		if g.Skipped {
			continue
		}
		res.Graded++
		if g.Pass {
			res.Passed++
		}
	}
	if res.Graded > 0 {
		res.Score = float64(res.Passed) / float64(res.Graded)
	}
	if j != nil {
		res.JudgeMicroUSD = j.Spent() - spent
	}
	return res, nil
}

// Lower lists the cases in got that score below their score in base. A case
// that base doesn't have can't be lower.
func Lower(base, got []Result) []string {
	was := map[string]float64{}
	for _, r := range base {
		was[r.Case] = r.Score
	}
	var lower []string
	for _, r := range got {
		if s, ok := was[r.Case]; ok && r.Score < s {
			lower = append(lower, fmt.Sprintf("%s: %.2f, was %.2f", r.Case, r.Score, s))
		}
	}
	slices.Sort(lower)
	return lower
}

// The bodies a run's events carry. They match what the run loop records, so
// one check reads either runtime.

// TextBody is the body of OwnerMessage, AgentMessage and Report events.
type TextBody struct {
	Text string `json:"text"`
}

// ToolCallBody is the body of a ToolCall event. Server is the MCP server the
// tool belongs to; empty for the agent's own tools (its file tools, its
// memory).
type ToolCallBody struct {
	CallID string          `json:"call_id"`
	Tool   string          `json:"tool"`
	Server string          `json:"server,omitempty"`
	Input  json.RawMessage `json:"input"`
}

// ToolResultBody is the body of a ToolResult event.
type ToolResultBody struct {
	CallID  string `json:"call_id"`
	Tool    string `json:"tool"`
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
}

// ErrorBody is the body of an Error event.
type ErrorBody struct {
	Message string `json:"message"`
}

// Calls returns the run's tool calls, in order.
func (r Run) Calls() ([]ToolCallBody, error) {
	var calls []ToolCallBody
	for _, e := range r.Events {
		if e.Kind != session.ToolCall {
			continue
		}
		var c ToolCallBody
		if err := json.Unmarshal(e.Body, &c); err != nil {
			return nil, fmt.Errorf("eval: event %d: %w", e.Seq, err)
		}
		calls = append(calls, c)
	}
	return calls, nil
}

// Results returns the run's tool results, by the id of the call they answer.
func (r Run) Results() (map[string]ToolResultBody, error) {
	results := map[string]ToolResultBody{}
	for _, e := range r.Events {
		if e.Kind != session.ToolResult {
			continue
		}
		var b ToolResultBody
		if err := json.Unmarshal(e.Body, &b); err != nil {
			return nil, fmt.Errorf("eval: event %d: %w", e.Seq, err)
		}
		results[b.CallID] = b
	}
	return results, nil
}

// Report returns the agent's report at the end of the run, or "" if it made
// none.
func (r Run) Report() (string, error) {
	for _, e := range slices.Backward(r.Events) {
		if e.Kind != session.Report {
			continue
		}
		var b TextBody
		if err := json.Unmarshal(e.Body, &b); err != nil {
			return "", fmt.Errorf("eval: event %d: %w", e.Seq, err)
		}
		return b.Text, nil
	}
	return "", nil
}
