package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/kiff/agents/tool"
)

// The years a number can be dated in: from 2000 to 2100, as seconds or
// milliseconds since 1970. An id or an amount outside them is never a date.
var (
	firstDay = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	lastDay  = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

// dated is a vendor's answer with each time it gives as a number shown as a
// date beside it: "1793892934 (2026-11-05)", in UTC. The number stays as it
// came; the run reads the date instead of working it out (kiff/tafs#476: "the
// due date on both was 1793892934"). A number is dated only when the tool's
// result schema says its field is a time, or else when the field's name reads
// as one (created_at, due_date, expires, period_start, period.end, timestamp),
// and only between 2000 and 2100: never any number that only looks like a
// time, such as an id or an amount. text is compact JSON; anything else, or
// an answer with nothing to date, comes back as it is.
func dated(text string, units map[string]time.Duration) string {
	t := strings.TrimSpace(text)
	if t == "" || (t[0] != '{' && t[0] != '[') || !json.Valid([]byte(t)) {
		return text
	}
	type frame struct {
		object, key bool   // an object, and its next string is a key
		name        string // the object's key being read, or the array's
	}
	var (
		stack []frame
		at    []int    // where a date goes: just after its number
		dates []string // what goes there
	)
	dec := json.NewDecoder(strings.NewReader(t))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return text
		}
		top := len(stack) - 1
		// The name a value is read under: its key, with the object's own
		// key before it when it has one ("period" then "start").
		name, parent := "", ""
		if top >= 0 {
			name = stack[top].name
			if top > 0 {
				parent = stack[top-1].name
			}
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{':
				stack = append(stack, frame{object: true, key: true})
			case '[':
				stack = append(stack, frame{name: name})
			default:
				stack = stack[:top]
				if top > 0 && stack[top-1].object {
					stack[top-1].key = true
				}
			}
			continue
		case string:
			if top >= 0 && stack[top].object && stack[top].key {
				stack[top].name, stack[top].key = v, false
				continue
			}
		case json.Number:
			if top >= 0 && stack[top].object {
				if d, ok := dateOf(v, name, parent, units); ok {
					at, dates = append(at, int(dec.InputOffset())), append(dates, d)
				}
			}
		}
		if top >= 0 && stack[top].object {
			stack[top].key = true
		}
	}
	if len(at) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for i, off := range at {
		b.WriteString(t[last:off])
		b.WriteString(" (" + dates[i] + ")")
		last = off
	}
	b.WriteString(t[last:])
	return b.String()
}

// dateOf is n's date, when n is a time under name: by the result schema's
// word for name when it has one, else by the name itself, read with its
// parent's ("period" and "start" read as period_start).
func dateOf(n json.Number, name, parent string, units map[string]time.Duration) (string, bool) {
	unit, known := units[name]
	if !known {
		if !timeName(name) && (parent == "" || !timeName(parent+"_"+name)) {
			return "", false
		}
	}
	f, err := n.Float64()
	if err != nil {
		return "", false
	}
	for _, u := range []time.Duration{time.Second, time.Millisecond} {
		if known && u != unit {
			continue
		}
		// In range before it's a time: a number past 2100 never converts.
		ns := f * float64(u)
		if ns >= float64(firstDay.UnixNano()) && ns < float64(lastDay.UnixNano()) {
			return time.Unix(0, int64(ns)).UTC().Format("2006-01-02"), true
		}
	}
	return "", false
}

// timeName reports whether a field's name reads as a time: *_at, a word
// "date", created, due…, expires…, period_…, or timestamp, in snake_case or
// camelCase. Words, not letters: "update" and "candidate" aren't dates.
func timeName(name string) bool {
	words := snakeWords(name)
	if len(words) == 0 {
		return false
	}
	first, last := words[0], words[len(words)-1]
	switch {
	case len(words) > 1 && last == "at":
		return true
	case slices.ContainsFunc(words, func(w string) bool { return w == "date" || w == "dates" || w == "datetime" || w == "timestamp" }):
		return true
	case first == "created" || first == "due" || first == "expires" || first == "expiry" || first == "expiration":
		return true
	case first == "period" && len(words) > 1:
		return true
	}
	return false
}

// snakeWords is a field's name as lowercase words: "dueDate" and "due_date"
// are "due", "date".
func snakeWords(name string) []string {
	var words []string
	var w []rune
	runes := []rune(name)
	flush := func() {
		if len(w) > 0 {
			words = append(words, strings.ToLower(string(w)))
			w = w[:0]
		}
	}
	for i, r := range runes {
		switch {
		case r == '_' || r == '-' || r == '.' || r == ' ':
			flush()
		case unicode.IsUpper(r) && i > 0 && (unicode.IsLower(runes[i-1]) || i+1 < len(runes) && unicode.IsLower(runes[i+1])):
			flush()
			w = append(w, r)
		default:
			w = append(w, r)
		}
	}
	flush()
	return words
}

// unitHint is a schema description that says its number is a time: seconds
// or milliseconds since the Unix epoch, or a Unix timestamp.
var unitHint = regexp.MustCompile(`(?i)\b(unix|epoch|(milli)?seconds since)\b`)

// timeUnits is what a tool's result schema says of its fields that are
// times: each property's name it marks as one (format unix-time, timestamp
// or date-time on a number, or a unit in its description), with its unit.
// The schema comes first; a field it doesn't mark is read by its name.
func timeUnits(t tool.Tool) map[string]time.Duration {
	s, ok := t.(tool.ResultSchemer)
	if !ok {
		return nil
	}
	raw := s.ResultSchema()
	if len(raw) == 0 || !bytes.Contains(raw, []byte(`"properties"`)) {
		return nil
	}
	var schema any
	if json.Unmarshal(raw, &schema) != nil {
		return nil
	}
	units := map[string]time.Duration{}
	var walk func(v any, depth int)
	walk = func(v any, depth int) {
		m, ok := v.(map[string]any)
		if !ok || depth > 12 {
			return
		}
		if props, ok := m["properties"].(map[string]any); ok {
			for name, p := range props {
				if u, ok := schemaUnit(p); ok {
					units[name] = u
				}
				walk(p, depth+1)
			}
		}
		for _, k := range []string{"items", "additionalProperties"} {
			walk(m[k], depth+1)
		}
		for _, k := range []string{"anyOf", "oneOf", "allOf"} {
			if list, ok := m[k].([]any); ok {
				for _, s := range list {
					walk(s, depth+1)
				}
			}
		}
	}
	walk(schema, 0)
	return units
}

// schemaUnit is a property schema's time unit, when it marks a number as a
// time.
func schemaUnit(p any) (time.Duration, bool) {
	m, ok := p.(map[string]any)
	if !ok {
		return 0, false
	}
	number := false
	switch t := m["type"].(type) {
	case string:
		number = t == "integer" || t == "number"
	case []any:
		number = slices.Contains(t, any("integer")) || slices.Contains(t, any("number"))
	}
	if !number {
		return 0, false
	}
	desc, _ := m["description"].(string)
	unit := time.Second
	if strings.Contains(strings.ToLower(desc), "millisecond") {
		unit = time.Millisecond
	}
	switch f, _ := m["format"].(string); {
	case f == "unix-time" || f == "timestamp" || f == "date-time" || f == "unix-timestamp":
		return unit, true
	case unitHint.MatchString(desc):
		return unit, true
	}
	return 0, false
}
