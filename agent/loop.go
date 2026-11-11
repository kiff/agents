package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/show"
)

// LoopPages is how many reads of the same list a run makes in a row before it
// asks its owner whether to read on: a read whose arguments change only in a
// cursor, page after page, with nothing else between.
const LoopPages = 10

// A loop card is the runtime's own confirm, shown in place of the read that
// made LoopPages: its id starts with LoopCard, and its answers are
// LoopContinue (LoopPages more, within the same cost cap) and LoopSummarise
// (one turn with no tool use, answering from what was read). Only the person
// answers it (run.AnswerCard); unanswered, it settles as LoopSummarise.
const (
	LoopCard      = "tafs-loop-"
	LoopContinue  = "tafs_loop_continue"
	LoopSummarise = "tafs_loop_summarise"
)

// LoopNudge is what a run is told when its owner chose, or didn't answer, to
// stop reading: one turn more, with no tool, to answer from what it read.
const LoopNudge = "[tafs] Your owner asked you to stop reading and answer now, from what you have read. Write your message as text, with no tool call, and say plainly what is left unread. A count of what you read is never the total: say how many you read, and that more remain."

// LoopRead is the result a loop card's call gets when the owner chose to read
// on: the page that call read wasn't kept, so the run reads it again.
const LoopRead = "[tafs] Your owner chose to read on. The page this call read wasn't kept: read it again, then go on. After 10 more pages you'll be asked again."

// cursorArgs are arguments named like a list's cursor: a page of the same
// read moves them, and nothing else.
var cursorArgs = map[string]bool{
	"starting_after": true, "ending_before": true, "cursor": true, "next_cursor": true, "page": true, "page_number": true,
	"offset": true, "after": true, "before": true, "page_token": true, "next_page_token": true, "pagetoken": true,
	"next": true, "skip": true, "since_id": true, "max_id": true, "marker": true, "continuation": true, "start": true,
}

// pages follows a run's reads of the same list, turn after turn.
type pages struct {
	shape string
	n     int
	prev  string         // the latest such read's result
	args  map[string]any // and its arguments
}

// shapeOf is a read call as pages compares it: its tool and its arguments
// without its cursor: those named like one (cursorArgs), KIFF's own (kiff_),
// and a string that changed since the previous page's call (was) to a value
// that page's result held in quotes (the last item's id, as a cursor). A
// filter that stays the same stays in the shape, whatever the results hold.
// Keys come out sorted, so the same arguments give the same shape. It also
// returns the call's arguments, for the next page's.
func shapeOf(name string, input json.RawMessage, prev string, was map[string]any) (string, map[string]any) {
	var args map[string]any
	if json.Unmarshal(input, &args) != nil {
		return name + "|" + string(input), nil
	}
	kept := make(map[string]any, len(args))
	for k, v := range args {
		lk := strings.ToLower(k)
		if cursorArgs[lk] || strings.HasPrefix(lk, "kiff_") {
			continue
		}
		if s, ok := v.(string); ok && len(s) >= 4 && prev != "" && was != nil && was[k] != v && strings.Contains(prev, `"`+s+`"`) {
			continue
		}
		kept[k] = v
	}
	raw, _ := json.Marshal(kept)
	return name + "|" + string(raw), args
}

// see counts a turn's calls and says which result, if any, gives way to a
// loop card: the read that made LoopPages of the same list in a row. A call
// that isn't a read, or a read of another list, starts the count again; so
// does a card, which also restarts it after the owner reads on.
func (p *pages) see(byName map[string]tool.Tool, calls []ToolResultBody, inputs []json.RawMessage) (int, bool) {
	at := -1
	for i, r := range calls {
		t := byName[r.Tool]
		if t == nil || t.Access() != tool.Read || acts(t, inputs[i]) {
			*p = pages{}
			continue
		}
		shape, args := shapeOf(r.Tool, inputs[i], p.prev, p.args)
		if shape != p.shape {
			*p = pages{shape: shape}
		}
		p.n++
		p.prev, p.args = r.Text, args
		if p.n >= LoopPages && at < 0 {
			at = i
		}
	}
	if at >= 0 {
		*p = pages{}
	}
	return at, at >= 0
}

// loopCard is the card a run shows its owner in place of the read r, its
// pages-th of the same list: words in English for an app without tafs cloud's,
// which word it from Loop.
func loopCard(r ToolResultBody, id string, pages int) tool.Result {
	c := show.Component{
		Type:  "confirm",
		ID:    id,
		Title: "Still reading",
		Text:  fmt.Sprintf("I've read %d pages of %s, and there's more. Read on, or answer with what's been read?", pages, r.Tool),
		Options: []show.Option{
			{Value: LoopContinue, Label: "Read on"},
			{Value: LoopSummarise, Label: "Answer with what's read"},
		},
		Waits: show.WaitsOwner,
		Loop:  &show.Loop{Tool: r.Tool, Pages: pages},
	}
	b, _ := json.Marshal(c)
	return tool.Result{Text: "shown: confirm " + id + ", waiting for the owner's answer", Component: b, State: tool.StateAwaiting}
}

// isLoopCard reports whether a component is a loop card.
func isLoopCard(component json.RawMessage) bool {
	var c struct {
		ID   string          `json:"id"`
		Loop json.RawMessage `json:"loop"`
	}
	return json.Unmarshal(component, &c) == nil && strings.HasPrefix(c.ID, LoopCard) && len(c.Loop) > 0
}
