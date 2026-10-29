package show

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Answerable are the types the owner answers: showing one, the run waits for
// the answer (tool.StateAwaiting), which comes back as the call's result,
// never as text in the chat. The others are shown and the run goes on.
var Answerable = []string{"confirm", "questions", "approval", "sign_in"}

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
//   - sign_in: "signed in", "not now", or "failed: <why>".
func Answer(c Component, answer json.RawMessage) (Answered, error) {
	if !slices.Contains(Answerable, c.Type) {
		return Answered{}, fmt.Errorf("a %s isn't answered", c.Type)
	}
	if c.Type == "questions" {
		return answerQuestions(c, answer)
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
