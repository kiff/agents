package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kiff/agents/eval"
)

// -regrade grades the taf's answers in a transcript a run saved again, with
// this head's judges, and runs nothing else: TafConcise over its answers, and
// AfterApproval over the answer after the approved hold, when the transcript
// has one. It's how a change to a judge or to the taf's rule is shown on the
// transcripts that failed (Evals 42146041790 and 42176052141). Its verdicts
// may quote the chat, so it never runs in CI.

// tafLine is one of the taf's answers as the transcript wrote it: what the
// owner sent, with what came of a hold, and the answer.
type tafLine struct{ owner, said string }

// readTafLines is the taf's answers in a transcript, in order, up to the line
// that ends them: tafAnswersEnd, or the verdict a run that failed writes there
// instead ("taf  not concise: …", "taf  after approval: …"). What follows, such
// as the improve case's chat, isn't the taf's chat the judges read.
func readTafLines(transcript string) []tafLine {
	var out []tafLine
	for _, line := range strings.Split(transcript, "\n") {
		if strings.HasPrefix(line, "taf  ") {
			if len(out) > 0 {
				break
			}
			continue
		}
		rest, ok := strings.CutPrefix(line, "owner: ")
		if !ok {
			continue
		}
		if owner, said, ok := strings.Cut(rest, " → taf: "); ok {
			out = append(out, tafLine{owner: owner, said: said})
		}
	}
	return out
}

// heldApproved is how the transcript says the owner approved a held call, after
// what they sent.
const heldApproved = " → held; the owner approved it on the phone, signed"

// approvedAfterCheck is checkedThenApproved's chat in lines: the check the
// owner asked for and its answer, then the refund they asked for, held and
// approved, and its answer.
func approvedAfterCheck(lines []tafLine) (check, checked, ask, text string, ok bool) {
	for i := 0; i+1 < len(lines); i++ {
		if !strings.HasPrefix(lines[i].owner, "Check charge ch_e2e_checked_") {
			continue
		}
		if ask, held := strings.CutSuffix(lines[i+1].owner, heldApproved); held {
			return lines[i].owner, lines[i].said, ask, lines[i+1].said, true
		}
	}
	return "", "", "", "", false
}

// regraded is what the judges said of a transcript again.
type regraded struct {
	answers       int
	concise       eval.Grade
	afterApproval *eval.Grade // nil when the transcript has no approved hold after a check
}

func (r regraded) pass() bool {
	return r.concise.Pass && (r.afterApproval == nil || r.afterApproval.Pass)
}

func (r regraded) print(w io.Writer) {
	verdict := func(g eval.Grade) string {
		s := "pass"
		if !g.Pass {
			s = "FAIL: " + g.Why
		}
		if g.Split > 0 {
			s += fmt.Sprintf(" (%d of %d judges said otherwise: %s)", g.Split, eval.Votes, g.Why)
		}
		return s
	}
	fmt.Fprintf(w, "concise, over %d answers: %s\n", r.answers, verdict(r.concise))
	if r.afterApproval != nil {
		fmt.Fprintf(w, "after the approved hold: %s\n", verdict(*r.afterApproval))
	} else {
		fmt.Fprintln(w, "after the approved hold: not in this transcript")
	}
}

// regrade asks the judges about the taf's answers in transcript, as the run
// asked them: TafConcise over tafAnswers, and AfterApproval over
// afterApprovalChat.
func regrade(ctx context.Context, j *eval.Judge, transcript string) (regraded, error) {
	lines := readTafLines(transcript)
	if len(lines) == 0 {
		return regraded{}, errors.New("-regrade: the transcript has no answer of the taf's (its lines read \"owner: … → taf: …\")")
	}
	asked, said := make([]string, len(lines)), make([]string, len(lines))
	for i, l := range lines {
		asked[i], said[i] = l.owner, l.said
	}
	r := regraded{answers: len(lines)}
	var err error
	if r.concise, err = j.AskMajority(ctx, TafConcise, tafAnswers(asked, said)); err != nil {
		return regraded{}, err
	}
	if check, checked, ask, text, ok := approvedAfterCheck(lines); ok {
		g, err := j.AskMajority(ctx, AfterApproval, afterApprovalChat(check, checked, ask, text))
		if err != nil {
			return regraded{}, err
		}
		r.afterApproval = &g
	}
	return r, nil
}
