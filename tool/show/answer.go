package show

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Answerable are the types that wait for an answer: showing one, the run
// waits for it (tool.StateAwaiting), and it comes back as the call's result,
// never as text in the chat. The others are shown and the run goes on. The
// owner answers each of them but a result, which the service that hands the
// owner over answers with what came of it (AnsweredBy).
var Answerable = []string{"confirm", "questions", "approval", "sign_in", "result"}

// ByService are the types a service answers in the owner's place, naming
// itself (Answered.AnsweredBy); the owner answers the others.
var ByService = []string{"result"}

// Outcome is a result's answer: what came of handing the owner over. Exactly
// one is set: TafID, the new taf's id; Version, the improved taf's new
// version; or Failed, why neither was made.
type Outcome struct {
	TafID   string `json:"taf_id,omitempty"`
	Version int    `json:"version,omitempty"`
	Failed  string `json:"failed,omitempty"`
}

// MaxOwn bounds the owner's own words in an answer.
const MaxOwn = 2000

// Answered is a card's answer as the run reads it, the show call's result:
// Answer as the app sent it, checked against the card, and Words, the owner's
// choice as the card showed it. Wrote is set instead when the owner wrote a
// message rather than answer the card: Answer is then null.
type Answered struct {
	Answer json.RawMessage `json:"answer"`
	Words  string          `json:"words"`
	Wrote  string          `json:"wrote,omitempty"`
	// AnsweredBy names the service that answered in the owner's place (a
	// result, ByService); empty when the owner answered.
	AnsweredBy string `json:"answered_by,omitempty"`
}

// QuestionAnswer is one question's answer on a questions card: one of its
// options' values, several on a multi question, or the owner's own words.
type QuestionAnswer struct {
	Value  *string  `json:"value,omitempty"`
	Values []string `json:"values,omitempty"`
	Own    *string  `json:"own,omitempty"`
}

// Answer checks answer against c, the card it answers, and returns it as the
// run reads it. Each type takes its own answer:
//   - confirm: one of the options' values, as a string;
//   - questions: {"<question>": {"value": …} | {"values": […]} | {"own": "…"}},
//     every question answered, a value only among its options;
//   - approval: "approved", "declined", or "failed: <why>" (the phone's
//     approval didn't go through);
//   - sign_in: "signed in", "not now", or "failed: <why>";
//   - result: an Outcome, {"taf_id": …} | {"version": n} | {"failed": "<why>"}.
func Answer(c Component, answer json.RawMessage) (Answered, error) {
	if !slices.Contains(Answerable, c.Type) {
		return Answered{}, fmt.Errorf("a %s isn't answered", c.Type)
	}
	switch c.Type {
	case "questions":
		return answerQuestions(c, answer)
	case "result":
		return answerResult(c, answer)
	}
	var s string
	if err := json.Unmarshal(answer, &s); err != nil {
		return Answered{}, fmt.Errorf("a %s's answer is a string", c.Type)
	}
	failed := func(word string) (Answered, error) {
		why := strings.TrimSpace(strings.TrimPrefix(s, "failed:"))
		if why == "" || utf8.RuneCountInString(why) > MaxOwn {
			return Answered{}, errors.New(`"failed: " is followed by why, at most 2000 characters`)
		}
		return Answered{Answer: answer, Words: word + " · it didn't go through: " + why}, nil
	}
	switch c.Type {
	case "confirm":
		i := slices.IndexFunc(c.Options, func(o Option) bool { return o.Value == s })
		if i < 0 {
			return Answered{}, fmt.Errorf("%q isn't one of the card's options", s)
		}
		return Answered{Answer: answer, Words: c.Options[i].Label}, nil
	case "approval":
		switch {
		case s == "approved":
			return Answered{Answer: answer, Words: "Approved"}, nil
		case s == "declined":
			return Answered{Answer: answer, Words: "Declined"}, nil
		case strings.HasPrefix(s, "failed:"):
			return failed("Approve")
		}
		return Answered{}, errors.New(`an approval's answer is "approved", "declined" or "failed: <why>"`)
	default: // sign_in
		switch {
		case s == "signed in":
			return Answered{Answer: answer, Words: "Signed in"}, nil
		case s == "not now":
			return Answered{Answer: answer, Words: cmpOr(c.No, "Not now")}, nil
		case strings.HasPrefix(s, "failed:"):
			return failed("Sign in")
		}
		return Answered{}, errors.New(`a sign_in's answer is "signed in", "not now" or "failed: <why>"`)
	}
}

func answerResult(c Component, answer json.RawMessage) (Answered, error) {
	var o Outcome
	dec := json.NewDecoder(bytes.NewReader(answer))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return Answered{}, errors.New(`a result's answer is {"taf_id": "…"}, {"version": n} or {"failed": "<why>"}`)
	}
	set := 0
	for _, b := range []bool{o.TafID != "", o.Version != 0, o.Failed != ""} {
		if b {
			set++
		}
	}
	switch {
	case set != 1:
		return Answered{}, errors.New(`a result's answer sets exactly one of taf_id, version and failed`)
	case o.TafID != "":
		if c.Taf.Improves != "" {
			return Answered{}, errors.New("this result improves a taf: its answer is {version} or {failed}")
		}
		if !refPattern.MatchString(o.TafID) {
			return Answered{}, errors.New("taf_id is the new taf's tafs id")
		}
		return Answered{Answer: answer, Words: c.Taf.Name + " is set up"}, nil
	case o.Version != 0:
		if c.Taf.Improves == "" {
			return Answered{}, errors.New("this result sets up a new taf: its answer is {taf_id} or {failed}")
		}
		if o.Version < 1 {
			return Answered{}, errors.New("version is the taf's new version, from 1")
		}
		return Answered{Answer: answer, Words: fmt.Sprintf("Version %d of %s is ready to try", o.Version, c.Taf.Name)}, nil
	}
	why := strings.TrimSpace(o.Failed)
	if why == "" || utf8.RuneCountInString(why) > MaxOwn {
		return Answered{}, errors.New("failed says why, at most 2000 characters")
	}
	return Answered{Answer: answer, Words: "It didn't go through: " + why}, nil
}

func answerQuestions(c Component, answer json.RawMessage) (Answered, error) {
	var answers map[string]QuestionAnswer
	if err := json.Unmarshal(answer, &answers); err != nil || answers == nil {
		return Answered{}, errors.New(`a questions card's answer is {"<question>": {"value": …} | {"values": […]} | {"own": "…"}}`)
	}
	for id := range answers {
		if !slices.ContainsFunc(c.Questions, func(q Question) bool { return q.ID == id }) {
			return Answered{}, fmt.Errorf("the card has no question %q", id)
		}
	}
	var words []string
	for _, q := range c.Questions {
		a, ok := answers[q.ID]
		if !ok {
			return Answered{}, fmt.Errorf("question %q isn't answered", q.ID)
		}
		label := func(v string) (string, error) {
			i := slices.IndexFunc(q.Options, func(o Option) bool { return o.Value == v })
			if i < 0 {
				return "", fmt.Errorf("%q isn't one of question %q's options", v, q.ID)
			}
			return q.Options[i].Label, nil
		}
		set := 0
		for _, b := range []bool{a.Value != nil, a.Values != nil, a.Own != nil} {
			if b {
				set++
			}
		}
		if set != 1 {
			return Answered{}, fmt.Errorf("question %q takes one of value, values or own", q.ID)
		}
		switch {
		case a.Value != nil:
			l, err := label(*a.Value)
			if err != nil {
				return Answered{}, err
			}
			words = append(words, l)
		case a.Values != nil:
			if !q.Multi || len(a.Values) == 0 {
				return Answered{}, fmt.Errorf("question %q takes values only when it's multi, and at least one", q.ID)
			}
			var ls []string
			for _, v := range a.Values {
				l, err := label(v)
				if err != nil {
					return Answered{}, err
				}
				ls = append(ls, l)
			}
			words = append(words, strings.Join(ls, ", "))
		default:
			own := strings.TrimSpace(*a.Own)
			if own == "" || utf8.RuneCountInString(own) > MaxOwn {
				return Answered{}, fmt.Errorf("question %q's own words are 1 to 2000 characters", q.ID)
			}
			words = append(words, "In your words: “"+own+"”")
		}
	}
	return Answered{Answer: answer, Words: strings.Join(words, " · ")}, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
