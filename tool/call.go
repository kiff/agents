package tool

import "context"

type callKey struct{}

// WithCall marks ctx with the model's ID for the call a tool is making, set
// by the agent's loop, never by the model: KIFF's gateway tool keys the
// call's operation by it.
func WithCall(ctx context.Context, callID string) context.Context {
	return context.WithValue(ctx, callKey{}, callID)
}

// CallFrom is the call ID ctx is marked with, or "".
func CallFrom(ctx context.Context) string {
	id, _ := ctx.Value(callKey{}).(string)
	return id
}

type outcomesKey struct{}

// Outcomes says how the run's latest call to a tool ended, by the tool's
// name: the agent's words for it (agent.OutcomeDone, OutcomeRefused, …), or
// "" when the run hasn't called it. Set by the agent's loop, never by the
// model; a tool reads it to check what the model claims about a call.
type Outcomes func(tool string) string

// WithOutcomes marks ctx with the run's call outcomes.
func WithOutcomes(ctx context.Context, o Outcomes) context.Context {
	return context.WithValue(ctx, outcomesKey{}, o)
}

// OutcomesFrom is the outcomes ctx is marked with; one that knows no call
// when it has none.
func OutcomesFrom(ctx context.Context) Outcomes {
	if o, ok := ctx.Value(outcomesKey{}).(Outcomes); ok && o != nil {
		return o
	}
	return func(string) string { return "" }
}
