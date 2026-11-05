package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/kiff/agents/run"
)

// waitBody is what POST /agents/{id}/runs/{run_id}/waits/{call}/answer
// takes: the event's payload that completes the call the run waits on,
// and whether the run goes on in the background. The caller (tafs cloud,
// which matched the event to the run by grant, event and key) speaks for
// the vendor, not for a person: no requester.
type waitBody struct {
	Payload    json.RawMessage `json:"payload"`
	Background bool            `json:"background,omitempty"`
}

// maxWaitPayload bounds an event's payload as a call's result.
const maxWaitPayload = 64 << 10

// answerWait records the event that completes a call the run waits on
// (run.KindEvent), by the call's id, and continues the run: in this
// invocation, streamed, or in the background. A wait already answered, run
// out, or on a run that went on is 409 wait_not_open.
func (s *Server) answerWait(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := s.caller(w, r)
	if err != nil {
		return
	}
	var body waitBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if len(body.Payload) == 0 || !json.Valid(body.Payload) {
		s.fail(w, r, bad("payload is the event's data, as JSON"))
		return
	}
	if len(body.Payload) > maxWaitPayload {
		s.fail(w, r, bad("payload is at most 64 KB"))
		return
	}
	rn, err := s.agentRun(ctx, c, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	call := r.PathValue("call")
	if _, ok := openWait(rn, call, s.now()); !ok {
		s.fail(w, r, conflict("wait_not_open", "no call of this run waits for an event by that id: it was answered, its wait ran out, or the run went on"))
		return
	}
	next, err := run.AnswerWait(ctx, s.Runs, c.Tenant, rn.ID, call, body.Payload, s.now())
	switch {
	case errors.Is(err, run.ErrNotOpen):
		s.fail(w, r, conflict("wait_not_open", "the wait was answered meanwhile, or the run went on: read it"))
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
	if !resolved(next, s.now()) {
		writeJSON(w, http.StatusAccepted, s.viewRun(next)) // it waits for KIFF, a card, or another event too
		return
	}
	s.goOn(w, r, c, next, body.Background)
}

// openWait is rn's event requirement for call that still waits.
func openWait(rn run.Run, call string, now time.Time) (run.Requirement, bool) {
	if rn.Status != run.Paused {
		return run.Requirement{}, false
	}
	for _, q := range rn.Requirements {
		if q.Event() && q.CallID == call && !q.Resolved(now) {
			return q, true
		}
	}
	return run.Requirement{}, false
}
