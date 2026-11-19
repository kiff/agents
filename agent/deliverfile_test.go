package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/files"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/deliverfile"
)

// A file a run delivers is the run's own: it gets no Called line and no
// count, beside a write that gets both.
func TestADeliveredFileIsNeitherCalledNorCounted(t *testing.T) {
	svc := &files.Service{Store: memory.NewFiles(), Blobs: memory.NewBlobs()}
	deliver := deliverfile.New(svc, "t1", "rae", "r1", nil)
	create := &seqTool{name: "sheets__create", access: tool.Write, results: []tool.Result{{Text: `{"spreadsheetId":"2xYz"}`, State: "forwarded"}}}
	m := &script{replies: []model.Reply{
		use("c1", deliverfile.Name, `{"name":"week","media_type":"text/csv","content":"a,b\n1,2\n"}`),
		use("c2", "sheets__create", `{}`),
		say("Delivered the week's table and made the sheet."),
	}}
	store, r := start(t, session.KindRun)
	out, _, err := agent.Run(context.Background(), newAgent(m, deliver, create), r, nil, "Do the week.", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.End(context.Background()); err != nil {
		t.Fatal(err)
	}
	if kept, _ := svc.List(context.Background(), "t1", files.Filter{Run: "r1"}); len(kept) != 1 {
		t.Fatalf("delivered %d files, want 1", len(kept))
	}
	if _, ok := out.Count()[deliverfile.Name]; ok || len(out.Count()) != 1 {
		t.Errorf("the run's count: %+v, want sheets__create only", out.Count())
	}
	_, events, err := store.Read(context.Background(), "t1", r.Session().ID)
	if err != nil {
		t.Fatal(err)
	}
	var called []string
	for _, e := range events {
		if e.Kind == session.Called {
			var b agent.CalledBody
			if err := json.Unmarshal(e.Body, &b); err != nil {
				t.Fatal(err)
			}
			called = append(called, b.Tool)
		}
	}
	if len(called) != 1 || called[0] != "sheets__create" {
		t.Errorf("Called lines for %v, want sheets__create only", called)
	}
}
