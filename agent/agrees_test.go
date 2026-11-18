package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool/show"
)

const (
	rulesNotice  = `{"type":"notice","id":"n1","tone":"info","text":"Refunds up to 50 a call","rules":[{"tool":"refund_charge","over":"ask"}]}`
	refusedRules = `{"type":"notice","id":"n1","tone":"loud","text":"Refunds up to 50 a call","rules":[{"tool":"refund_charge","over":"ask"}]}`
	otherNotice  = `{"type":"notice","id":"n2","tone":"info","text":"Refunds up to 50 a call","rules":[{"tool":"refund_charge","over":"ask"}]}`
	plainNotice  = `{"type":"notice","id":"n1","tone":"info","text":"Asking planque."}`
	agreeToN1    = `{"type":"confirm","id":"c1","text":"Agree to these rules?","options":[{"value":"agree","label":"Agree"},{"value":"change","label":"Change something"}],"agrees":"n1"}`
	plainConfirm = `{"type":"confirm","id":"c2","text":"Go on?","options":[{"value":"yes","label":"Yes"},{"value":"no","label":"No"}]}`
)

// calls is one reply that calls show once per input, in that order: one batch.
func calls(inputs ...string) model.Reply {
	var blocks []model.Block
	for i, in := range inputs {
		blocks = append(blocks, model.Block{Kind: model.ToolUse, CallID: "s" + string(rune('a'+i)), Tool: "show", Input: json.RawMessage(in)})
	}
	return model.Reply{Blocks: blocks, Stop: model.CallTools, Usage: model.Usage{InputTokens: 1000, OutputTokens: 100}}
}

// shownIn is what one run, from replies, left shown in the session (each
// component's id and type), what each show call answered, and the own_refused
// records the session kept for a confirm refused as unshown.
func shownIn(t *testing.T, store *memory.Store, r *session.Recording, replies ...model.Reply) (shown []string, answers []string, unshown int) {
	shown, answers, unshown, _ = shownAndMade(t, store, r, replies...)
	return shown, answers, unshown
}

// shownAndMade is shownIn, with how each refused call streamed: "not_made"
// for one refused back unmade, "failed" for any other error.
func shownAndMade(t *testing.T, store *memory.Store, r *session.Recording, replies ...model.Reply) (shown []string, answers []string, unshown int, refused []string) {
	t.Helper()
	m := &script{replies: append(replies, say("Done."))}
	var streamed []agent.Event
	if _, _, err := agent.Run(context.Background(), newAgent(m, show.New()), r, nil, "Set it up.", func(e agent.Event) { streamed = append(streamed, e) }); err != nil {
		t.Fatal(err)
	}
	if err := r.End(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, events, err := store.Read(context.Background(), "t1", r.Session().ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == session.Component {
			var c struct{ ID, Type string }
			_ = json.Unmarshal(e.Body, &c)
			shown = append(shown, c.Type+":"+c.ID)
		}
		if e.Kind == session.OwnRefused {
			var b agent.OwnRefusedBody
			if json.Unmarshal(e.Body, &b) == nil && b.Error == agent.UnshownNotice {
				unshown++
			}
		}
	}
	for _, e := range streamed {
		if e.Kind == agent.CallResult && e.Result != nil {
			answers = append(answers, e.Result.Text)
			switch {
			case e.NotMade:
				refused = append(refused, "not_made")
			case e.Result.IsError:
				refused = append(refused, "failed")
			}
		}
	}
	return shown, answers, unshown, refused
}

func refusedUnshown(answers []string) bool {
	for _, a := range answers {
		if a == agent.UnshownNotice {
			return true
		}
	}
	return false
}

// A confirm that agrees to a rules notice is shown only once the notice was:
// earlier in the conversation, or earlier in the same batch. Otherwise it's
// refused, unshown, so the owner never sees "Agree?" for rules they weren't
// shown (Eval 44128040350's day, 17 Nov: a refused notice and its confirm in
// one batch).
func TestAConfirmAgreesOnlyToARulesNoticeTheConversationShowed(t *testing.T) {
	for name, tc := range map[string]struct {
		replies []model.Reply
		want    []string
		refused bool
	}{
		"the notice earlier in the run":         {[]model.Reply{calls(rulesNotice), calls(agreeToN1)}, []string{"notice:n1", "confirm:c1"}, false},
		"the notice earlier in the same batch":  {[]model.Reply{calls(rulesNotice, agreeToN1)}, []string{"notice:n1", "confirm:c1"}, false},
		"the notice refused, in the same batch": {[]model.Reply{calls(refusedRules, agreeToN1)}, nil, true},
		"no notice by that id":                  {[]model.Reply{calls(agreeToN1)}, nil, true},
		"the notice shown again without rules":  {[]model.Reply{calls(rulesNotice), calls(plainNotice, agreeToN1)}, []string{"notice:n1", "notice:n1"}, true},
		"a confirm that agrees to nothing":      {[]model.Reply{calls(plainConfirm)}, []string{"confirm:c2"}, false},
		"another notice's id":                   {[]model.Reply{calls(otherNotice), calls(agreeToN1)}, []string{"notice:n2"}, true},
		// The batch is checked in call order: a confirm before its notice is refused, and the notice still shows.
		"the confirm before its notice, in the same batch": {[]model.Reply{calls(agreeToN1, rulesNotice)}, []string{"notice:n1"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			store, r := start(t, session.KindChat)
			shown, answers, unshown := shownIn(t, store, r, tc.replies...)
			if strings.Join(shown, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("shown %v, want %v", shown, tc.want)
			}
			if refusedUnshown(answers) != tc.refused {
				t.Fatalf("refused %v, want %v: %q", refusedUnshown(answers), tc.refused, answers)
			}
			// A refused confirm leaves the record of why it didn't show.
			if (unshown == 1) != tc.refused {
				t.Fatalf("%d own_refused records for an unshown notice, refused %v", unshown, tc.refused)
			}
		})
	}
}

// The notice shown in an earlier run of the same conversation counts.
func TestAConfirmAgreesToANoticeAnEarlierRunShowed(t *testing.T) {
	store := memory.New()
	rec, err := session.NewRecorder(store, session.DefaultPolicy(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	s, err := rec.Open(context.Background(), "t1", "rae", session.KindChat)
	if err != nil {
		t.Fatal(err)
	}
	r1, err := rec.Start(context.Background(), s, "r1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if shown, _, _ := shownIn(t, store, r1, calls(rulesNotice)); len(shown) != 1 {
		t.Fatalf("first run shown %v", shown)
	}
	r2, err := rec.Start(context.Background(), s, "r2", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	shown, answers, _ := shownIn(t, store, r2, calls(agreeToN1))
	if strings.Join(shown, ",") != "notice:n1,confirm:c1" || refusedUnshown(answers) {
		t.Fatalf("second run: shown %v, answers %q", shown, answers)
	}
	// Another conversation's notice never counts.
	other, err := rec.Open(context.Background(), "t1", "rae", session.KindChat)
	if err != nil {
		t.Fatal(err)
	}
	r3, err := rec.Start(context.Background(), other, "r3", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if shown, answers, unshown := shownIn(t, store, r3, calls(agreeToN1)); len(shown) != 0 || !refusedUnshown(answers) || unshown != 1 {
		t.Fatalf("another conversation: shown %v, answers %q", shown, answers)
	}
}

// A confirm refused because its notice wasn't shown was never shown either:
// it streams as not made, so the owner's work line counts one failed step
// (the notice), not two (the demo of 18 Nov: "2 steps, 2 failed").
func TestAConfirmForAnUnshownNoticeIsNotMadeNotFailed(t *testing.T) {
	store, r := start(t, session.KindChat)
	_, _, unshown, refused := shownAndMade(t, store, r, calls(refusedRules, agreeToN1))
	if strings.Join(refused, ",") != "failed,not_made" || unshown != 1 {
		t.Fatalf("refused calls streamed as %v (%d unshown records), want the notice failed and its confirm not made", refused, unshown)
	}
	// Its notice shown, the confirm is made and shows.
	store, r = start(t, session.KindChat)
	shown, _, _, refused := shownAndMade(t, store, r, calls(rulesNotice, agreeToN1))
	if len(refused) != 0 || strings.Join(shown, ",") != "notice:n1,confirm:c1" {
		t.Fatalf("shown %v, refused %v", shown, refused)
	}
}
