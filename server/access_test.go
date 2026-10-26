package server_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/kiff/agents/run"
)

// #221: a member reads their sessions with an agent, and writes into
// their runs, only when the account says they may use it; overseeing it
// reads and writes nothing. No answer is no. A run the runner refuses for it says so, and nothing more.
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

	for who, want := range map[string]int{"ana": 200, "maria": 403, "bo": 403, "cy": 403} {
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

// Another person's sessions are served only with a read grant the
// account resolves for exactly that read: their list (titles and times),
// or one session they hold. Without one, a caller reads only its own, and
// a grant for anything else is refused as no oversight.
func TestAnothersSessionsNeedAReadGrant(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.server.Access = func(context.Context, string, string, string) (run.Access, error) { return run.Access{Use: true}, nil }
	grants := map[string]run.ReadGrant{
		"list": {Reader: "ada", Owner: "ana", Purpose: "follow_up"},
		"chat": {Reader: "ada", Owner: "ana", Session: "", Purpose: "fix"},
		"bos":  {Reader: "ada", Owner: "bo", Purpose: "fix"},
	}
	f.server.ResolveReadGrant = func(_ context.Context, tenant, agent, grant string) (run.ReadGrant, error) {
		g, ok := grants[grant]
		if !ok || tenant != "t1" || agent != "payouts" {
			return run.ReadGrant{}, errors.New("KIFF answered 403")
		}
		return g, nil
	}
	f.restart()

	code, body := f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "go", "background": true, "requester": map[string]any{"kind": "member", "id": "ana"}})
	f.want(202, code, body, "ana's run")
	sess, _ := body["session"].(string)
	g := grants["chat"]
	g.Session = sess
	grants["chat"] = g

	read := func(grant, path string) (int, map[string]any) {
		f.header = http.Header{"Kiff-Read-Grant": {grant}}
		defer func() { f.header = nil }()
		return f.do("t1", "GET", path, nil)
	}
	// The list grant lists Ana's sessions, and opens none of them.
	code, body = read("list", "/sessions?agent_id=payouts&owner=member:ana&reader=member:ada")
	f.want(200, code, body, "Ana's list, read by Ada")
	if list, _ := body["sessions"].([]any); len(list) != 1 {
		t.Errorf("Ana's list = %v", body)
	}
	code, body = read("list", "/sessions/"+sess+"?owner=member:ana&reader=member:ada")
	f.want(403, code, body, "a chat opened with the list's grant")
	if body["need"] != "oversee" {
		t.Errorf("the refusal = %v", body)
	}
	// The chat's grant opens that chat, and lists nothing.
	code, body = read("chat", "/sessions/"+sess+"?owner=member:ana&reader=member:ada")
	f.want(200, code, body, "Ana's chat, read by Ada")
	code, body = read("chat", "/sessions?agent_id=payouts&owner=member:ana&reader=member:ada")
	f.want(403, code, body, "the list read with a chat's grant")
	// A grant for Bo's work reads nothing of Ana's, one KIFF doesn't vouch
	// for reads nothing, and the owner must hold the session.
	code, body = read("bos", "/sessions?agent_id=payouts&owner=member:ana&reader=member:ada")
	f.want(403, code, body, "Ana's list with a grant for Bo's")
	code, body = read("forged", "/sessions?agent_id=payouts&owner=member:ana&reader=member:ada")
	f.want(403, code, body, "a grant KIFF refuses")
	code, body = read("bos", "/sessions/"+sess+"?owner=member:bo&reader=member:ada")
	f.want(404, code, body, "Ana's chat as Bo's")
	// A read grant reads a member's sessions only.
	code, body = read("list", "/sessions?agent_id=payouts&reader=member:ada")
	f.want(400, code, body, "a read grant with no owner")

	// The grant reads for the person KIFF granted it to, named by reader:
	// copied into another person's request, it reads nothing.
	code, body = read("list", "/sessions?agent_id=payouts&owner=member:ana&reader=member:max")
	f.want(403, code, body, "Ada's grant used by Max")
	code, body = read("list", "/sessions?agent_id=payouts&owner=member:ana")
	f.want(400, code, body, "a read grant with no reader")

	// Where read grants aren't taken, one is refused, not ignored.
	f.server.ResolveReadGrant = nil
	f.restart()
	code, body = read("list", "/sessions?agent_id=payouts&owner=member:ana&reader=member:ada")
	f.want(403, code, body, "a read grant where none is taken")
}
