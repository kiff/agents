package server_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/kiff/agents/run"
)

// #221: a member reads their sessions with an agent, and writes into
// their runs, only when the account says they may use or oversee it; no
// answer is no. A run the runner refuses for it says so, and nothing more.
func TestAMemberNeedsAccessToReadAndWrite(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	may := map[string]run.Access{"ana": {Use: true}, "maria": {Oversee: true}, "bo": {}}
	f.server.Access = func(_ context.Context, tenant, agent, subject string) (run.Access, error) {
		a, ok := may[subject]
		if !ok {
			return run.Access{}, errors.New("the account didn't answer")
		}
		return a, nil
	}
	f.restart()

	code, body := f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "go", "background": true, "requester": map[string]any{"kind": "member", "id": "ana"}})
	f.want(202, code, body, "ana's run")
	sess, _ := body["session"].(string)
	runID, _ := body["id"].(string)

	for who, want := range map[string]int{"ana": 200, "maria": 200, "bo": 403, "cy": 403} {
		code, body := f.do("t1", "GET", "/sessions?agent_id=payouts&owner=member:"+who, nil)
		f.want(want, code, body, who+"'s sessions")
		if want == 403 && (body["error"] != "no_access" || body["need"] != "use") {
			t.Errorf("%s's refusal = %v", who, body)
		}
	}
	code, body = f.do("t1", "GET", "/sessions/"+sess+"?owner=member:ana", nil)
	f.want(200, code, body, "ana reading her chat")
	// Ana loses the agent: her own chat, and her own run, are hers no more.
	may["ana"] = run.Access{}
	code, body = f.do("t1", "GET", "/sessions/"+sess+"?owner=member:ana", nil)
	f.want(403, code, body, "ana reading her chat without access")
	code, body = f.do("t1", "POST", "/agents/payouts/runs/"+runID+"/messages", map[string]any{"text": "and order 7", "requester": map[string]any{"kind": "member", "id": "ana"}})
	f.want(403, code, body, "ana writing into her run without access")
	// A door's sender, and no one, read as before.
	code, body = f.do("t1", "GET", "/sessions?agent_id=payouts&owner=door:d1", nil)
	f.want(200, code, body, "a door sender's sessions")
	code, body = f.do("t1", "GET", "/sessions?agent_id=payouts", nil)
	f.want(200, code, body, "every session")

	// The runner's refusal reaches the caller as the same answer.
	f.runner.refusing = fmt.Errorf("%w: not granted", run.ErrNoAccess)
	code, body = f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "go", "background": true, "requester": map[string]any{"kind": "member", "id": "bo"}})
	f.want(403, code, body, "bo's run")
	if body["error"] != "no_access" || body["need"] != "use" {
		t.Errorf("bo's refusal = %v", body)
	}
}
