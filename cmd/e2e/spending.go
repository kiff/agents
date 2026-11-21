package main

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/kiff/agents/model"
)

// spending counts what every model turn of a chat cost, at the price its
// request carries: Coach's turns and the tafs' alike, whatever the mode. The
// Eval's reports showed only the seeded cases' costs and Coach's own turns,
// so a night's spend on the chats couldn't be read back (kiff/agents' cost
// audit, 21 Nov).
type spending struct {
	model.Model
	spent atomic.Int64
}

func (s *spending) Turn(ctx context.Context, req model.Request, onText func(string)) (model.Reply, error) {
	r, err := s.Model.Turn(ctx, req, onText)
	s.spent.Add(r.Cost(req.Price))
	return r, err
}

// chatCost writes what the chat cost in all, its model turns and its judge,
// to the private transcript only: the log is public, and a run's cost has no
// place there.
func (t *test) chatCost(m *spending) {
	if t.cfg.transcript == nil {
		return
	}
	var judge int64
	if t.cfg.judge != nil {
		judge = t.cfg.judge.Spent()
	}
	turns := m.spent.Load()
	fmt.Fprintf(t.cfg.transcript, "chat cost $%.4f: model turns $%.4f, judge $%.4f\n",
		float64(turns+judge)/1e6, float64(turns)/1e6, float64(judge)/1e6)
}
