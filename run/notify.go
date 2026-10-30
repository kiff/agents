package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Notify is where a run is announced when it ends or pauses (kiff/tafs#206):
// URL, one the server allows, gets Note. Ref is the caller's own value for
// the run, which it checks the note by: the note is unsigned, and it carries
// only what the caller reads back from the run itself.
type Notify struct {
	URL string `json:"url"`
	Ref string `json:"ref"`
}

// Note is what a Notify URL is sent. A PAUSED run's note names the cards it
// waits on (Cards), so a caller that answers some of them (a result's
// hand-over) knows which to read the run back for.
type Note struct {
	Run    string     `json:"run"`
	Agent  string     `json:"agent"`
	Status Status     `json:"status"`
	Ref    string     `json:"ref"`
	Cards  []NoteCard `json:"cards,omitempty"`
	// State is set when the status alone doesn't say where the work stands:
	// "retrying" for a crashed run waiting for its restart Try of Of, at
	// NextAt; "needs_you" for one past its last restart. Reason says why, in
	// words an app shows as they are.
	State  string    `json:"state,omitempty"`
	Reason string    `json:"reason,omitempty"`
	Try    int       `json:"try,omitempty"`
	Of     int       `json:"of,omitempty"`
	NextAt time.Time `json:"next_at,omitzero"`
}

// NoteCard is a card a paused run waits on: its id and type, as shown.
type NoteCard struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// waitingCards are the cards r waits on, unanswered, in order.
func waitingCards(r Run) []NoteCard {
	var out []NoteCard
	for _, q := range r.Requirements {
		var c NoteCard
		if q.Card() && q.Answered.IsZero() && json.Unmarshal(q.Input, &c) == nil && c.ID != "" {
			out = append(out, c)
		}
	}
	return out
}

// NotifyWait bounds one note: a note that can't be sent in it is dropped, and
// the caller reads the run as it would without one.
var NotifyWait = 3 * time.Second

// Notifying is runs, announcing each run that has a Notify once per pause and
// once at its end: on the Put that moves it to PAUSED or a final status, from
// whichever path (the run itself, a cancel, the sweep). A note that fails
// never fails the Put: it's kept on the run (NoteTries, NoteRetry), and the
// sweep sends it again (Again) after each of NoteWaits, until Notify takes it.
type Notifying struct {
	Runs
	// Client sends the notes; nil is one that follows no redirect, so a note
	// goes only to the URL the server allowed.
	Client *http.Client
	// Log, when set, hears of a note that failed.
	Log func(msg string, err error)
	// Now is the clock a kept note's next try is set by; nil is time.Now.
	Now func() time.Time
}

// NoteWaits is how long a note Notify didn't take waits before each of its
// tries again: the same waits as a step's. Past the last, it's given up, and
// the caller reads the run as it would without one.
var NoteWaits = [...]time.Duration{5 * time.Minute, 20 * time.Minute, time.Hour}

// Put is Runs.Put, then the note when next is a new pause or the end.
func (n Notifying) Put(ctx context.Context, prev, next Run) error {
	if err := n.Runs.Put(ctx, prev, next); err != nil {
		return err
	}
	if next.Notify.URL == "" || next.Status == prev.Status || !noted(next.Status) {
		return nil
	}
	ctx = context.WithoutCancel(ctx)
	if err := n.send(ctx, next); err != nil {
		n.log("run: a note was not sent; it's kept and sent again", err)
		n.keep(ctx, next, 1)
	}
	return nil
}

// Again sends r's kept note again, from the sweep: as the run is now, so a
// run that moved on since sends its new state. Taken, the note is no longer
// kept; not taken, it waits for its next try, or is given up past the last.
func (n Notifying) Again(ctx context.Context, r Run) error {
	if r.Notify.URL == "" || !noted(r.Status) {
		return n.keep(ctx, r, 0) // nothing to say: a run at work is noted when it next stops
	}
	if err := n.send(ctx, r); err != nil {
		if r.NoteTries >= len(NoteWaits) {
			n.log("run: a note was given up after its last try", err)
			return n.keep(ctx, r, 0)
		}
		n.log("run: a note was not sent again; it's kept", err)
		return n.keep(ctx, r, r.NoteTries+1)
	}
	return n.keep(ctx, r, 0)
}

// noted reports whether a run in status s is announced: paused, or ended.
func noted(s Status) bool { return s == Paused || s.Final() }

// keep stores r's note as owed for its tries-th time (0: not owed), as r was
// read. A run that moved on since is left as it is: its own Put noted it.
func (n Notifying) keep(ctx context.Context, r Run, tries int) error {
	now := time.Now
	if n.Now != nil {
		now = n.Now
	}
	next := r.clone()
	next.NoteTries, next.NoteRetry = tries, time.Time{}
	if tries > 0 {
		next.NoteRetry = now().UTC().Add(NoteWaits[tries-1])
	} else {
		next.NoteTries = 0
	}
	if next.NoteTries == r.NoteTries && next.NoteRetry.Equal(r.NoteRetry) {
		return nil
	}
	next.Version, next.Updated = r.Version+1, now().UTC()
	err := n.Runs.Put(ctx, r, next)
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (n Notifying) log(msg string, err error) {
	if n.Log != nil {
		n.Log(msg, err)
	}
}

func (n Notifying) send(ctx context.Context, r Run) error {
	note := Note{Run: r.ID, Agent: r.Agent, Status: r.Status, Ref: r.Notify.Ref}
	if r.Status == Paused {
		note.Cards = waitingCards(r)
	}
	w := r.Work()
	note.State, note.Reason, note.Try, note.Of, note.NextAt = w.State, w.Reason, w.Try, w.Of, w.NextAt
	body, err := json.Marshal(note)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, NotifyWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Notify.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c := n.Client
	if c == nil {
		c = noRedirects
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("run %s: the note got %s", r.ID, resp.Status)
	}
	return nil
}

var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
