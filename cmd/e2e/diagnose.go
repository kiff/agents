package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
)

// recordingModel is a diagnostic, only on an Eval branch: every request of a
// continued run (its last message carries the record note) is written whole,
// with the reply, to resumed-requests.jsonl, which the workflow keeps in the
// private report store only.
type recordingModel struct {
	model.Model
	mu sync.Mutex
}

func (m *recordingModel) Turn(ctx context.Context, req model.Request, onText func(string)) (model.Reply, error) {
	reply, err := m.Model.Turn(ctx, req, onText)
	if n := len(req.Messages); n > 0 && continued(req.Messages[n-1]) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if f, ferr := os.OpenFile("resumed-requests.jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); ferr == nil {
			_ = json.NewEncoder(f).Encode(map[string]any{"request": req, "reply": reply})
			_ = f.Close()
		}
	}
	return reply, err
}

func continued(m model.Message) bool {
	for _, b := range m.Blocks {
		if b.Kind == model.Text && strings.HasPrefix(b.Text, agent.RecordNote) {
			return true
		}
	}
	return false
}
