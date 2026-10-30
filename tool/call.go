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
