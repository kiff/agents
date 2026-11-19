// Package delivered gives the Eval's runners (cmd/e2e, cmd/eval) the
// deliver_file tool the Lambda gives a run (cmd/lambda), on stores in memory:
// a taf whose definition sets files (kiff/tafs's do) then builds and runs in a
// test as it does live, instead of failing its build with registry.ErrNoFiles.
package delivered

import (
	"context"

	"github.com/kiff/agents/files"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/deliverfile"
)

// Tool is registry.Builder's DeliverFile over files kept in memory, each
// filed under the chat of its run, as runs keeps it.
func Tool(runs run.Runs) func(tenant, agent, runID string) tool.Tool {
	svc := &files.Service{Store: memory.NewFiles(), Blobs: memory.NewBlobs()}
	return func(tenant, agent, runID string) tool.Tool {
		return deliverfile.New(svc, tenant, agent, runID, func(ctx context.Context, tenant, runID string) (string, error) {
			r, err := runs.Get(ctx, tenant, runID)
			return r.Session, err
		})
	}
}
