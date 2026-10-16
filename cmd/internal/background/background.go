// Package background sends a stored run to the agents function to execute:
// an asynchronous invoke whose event names the run and carries nothing else.
// cmd/lambda sends its own background runs this way, and cmd/kiffalerts the
// paused runs KIFF's answers resolved; cmd/lambda executes both.
package background

import (
	"context"
	"encoding/json"
	"fmt"

	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

// Event is what the agents function is sent: the run to execute. The run is
// already stored; the event names it and carries nothing else, so a repeated
// or made-up event can only execute a run that exists, once.
type Event struct {
	Run struct {
		Tenant string `json:"tenant"`
		ID     string `json:"id"`
	} `json:"run"`
}

// Invoker is the part of the Lambda client Invoke uses.
type Invoker interface {
	Invoke(context.Context, *lambdasvc.InvokeInput, ...func(*lambdasvc.Options)) (*lambdasvc.InvokeOutput, error)
}

// Invoke has function execute the tenant's run in another invocation. It
// returns once Lambda has queued the event: any answer but 202 is an error.
func Invoke(api Invoker, function string) func(ctx context.Context, tenant, id string) error {
	return func(ctx context.Context, tenant, id string) error {
		var e Event
		e.Run.Tenant, e.Run.ID = tenant, id
		payload, err := json.Marshal(e)
		if err != nil {
			return err
		}
		out, err := api.Invoke(ctx, &lambdasvc.InvokeInput{
			FunctionName:   &function,
			InvocationType: lambdatypes.InvocationTypeEvent,
			Payload:        payload,
		})
		if err != nil {
			return fmt.Errorf("background: queueing a run: %w", err)
		}
		if out.StatusCode != 202 {
			return fmt.Errorf("background: queueing a run: status %d", out.StatusCode)
		}
		return nil
	}
}
