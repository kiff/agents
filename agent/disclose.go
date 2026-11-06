package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Disclosure is the line every call that writes to someone outside the
// company carries, saying an AI agent wrote it. The runtime puts it in the
// call, so it never depends on the model remembering (Eval 42193895936: a
// dispute's evidence went to the card network without it).
type Disclosure struct {
	// Line is the sentence as sent: "Written by an AI agent for the company."
	Line string `json:"line"`
	// In is where it goes: for each tool that writes outside, the free-text
	// fields of its input, by dotted path, in order of preference.
	In []DisclosureIn `json:"in"`
}

// DisclosureIn is one tool's place for the line: Tool by its name, with or
// without the part KIFF adds to it for a connection; Fields by dotted path.
type DisclosureIn struct {
	Tool   string   `json:"tool"`
	Fields []string `json:"fields"`
}

// MaxDisclosureLine bounds the line: one sentence.
const MaxDisclosureLine = 300

// Check reports what's wrong with d, if anything.
func (d *Disclosure) Check() error {
	if d == nil {
		return nil
	}
	if strings.TrimSpace(d.Line) == "" || len(d.Line) > MaxDisclosureLine || strings.ContainsAny(d.Line, "\n\r") {
		return fmt.Errorf("a disclosure's line is one sentence of 1 to %d bytes", MaxDisclosureLine)
	}
	if len(d.In) == 0 {
		return errors.New("a disclosure names the tools it goes in")
	}
	for _, in := range d.In {
		if in.Tool == "" || len(in.Fields) == 0 {
			return errors.New("a disclosure's tool has a name and at least one field")
		}
		for _, f := range in.Fields {
			for _, part := range strings.Split(f, ".") {
				if part == "" {
					return fmt.Errorf("a disclosure's field is a dotted path, not %q", f)
				}
			}
		}
	}
	return nil
}

// fields is where the line goes in a call of tool, or nil when it goes in
// none: a tool named as given, or with one word of KIFF's for a connection
// after it or a vendor's before it ("update_dispute_k3f9",
// "stripe_update_dispute"); never another tool whose name only starts the
// same ("send_test_mode_off" for "send", agentgabo on #352).
func (d *Disclosure) fields(tool string) []string {
	if d == nil {
		return nil
	}
	for _, in := range d.In {
		after, isAfter := strings.CutPrefix(tool, in.Tool+"_")
		before, isBefore := strings.CutSuffix(tool, "_"+in.Tool)
		if tool == in.Tool || isAfter && word(after) || isBefore && word(before) {
			return in.Fields
		}
	}
	return nil
}

// word is one lowercase word of letters and digits, at most 16: a
// connection's part or a vendor's name, never more of a tool's name.
func word(s string) bool {
	if s == "" || len(s) > 16 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// Into is input as the call sends it, with the line: at the end of the first
// of the tool's fields the call carries as text, or as the first field when
// the call carries its parent and none of them. Unchanged when one of them
// says it already, when the call carries none of them nor the first's
// parent (it sends nothing to disclose), and for any other tool.
func (d *Disclosure) Into(tool string, input json.RawMessage) json.RawMessage {
	paths := d.fields(tool)
	if len(paths) == 0 {
		return input
	}
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber() // an amount stays as the model wrote it
	var call map[string]any
	if dec.Decode(&call) != nil || call == nil {
		return input
	}
	var first map[string]any // the parent of the first text field found
	var key string
	for _, path := range paths {
		parent, last := parentOf(call, path)
		if parent == nil {
			continue
		}
		s, ok := parent[last].(string)
		if !ok {
			continue
		}
		if strings.Contains(s, d.Line) {
			return input
		}
		if first == nil {
			first, key = parent, last
		}
	}
	switch {
	case first != nil && strings.TrimSpace(first[key].(string)) == "":
		first[key] = d.Line
	case first != nil:
		first[key] = strings.TrimRight(first[key].(string), " \n") + "\n\n" + d.Line
	default:
		parent, last := parentOf(call, paths[0])
		if parent == nil {
			return input
		}
		parent[last] = d.Line
	}
	// As the model wrote it: no HTML escapes, which change the text sent but not its value (agentgabo on #352).
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if enc.Encode(call) != nil {
		return input
	}
	return bytes.TrimRight(out.Bytes(), "\n")
}

// parentOf is the object holding path's last part in call, and that part;
// nil when an object on the way isn't there.
func parentOf(call map[string]any, path string) (map[string]any, string) {
	parts := strings.Split(path, ".")
	at := call
	for _, p := range parts[:len(parts)-1] {
		next, ok := at[p].(map[string]any)
		if !ok {
			return nil, ""
		}
		at = next
	}
	return at, parts[len(parts)-1]
}
