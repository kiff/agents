package run

import (
	"bytes"
	"context"
	"encoding/json"
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
// never fails the Put.
type Notifying struct {
	Runs
	// Client sends the notes; nil is one that follows no redirect, so a note
	// goes only to the URL the server allowed.
	Client *http.Client
	// Log, when set, hears of a note that failed.
	Log func(msg string, err error)
}

// Put is Runs.Put, then the note when next is a new pause or the end.
func (n Notifying) Put(ctx context.Context, prev, next Run) error {
	if err := n.Runs.Put(ctx, prev, next); err != nil {
		return err
	}
	if next.Notify.URL == "" || next.Status == prev.Status || !(next.Status == Paused || next.Status.Final()) {
		return nil
	}
	if err := n.send(context.WithoutCancel(ctx), next); err != nil && n.Log != nil {
		n.Log("run: a note was not sent", err)
	}
	return nil
}

func (n Notifying) send(ctx context.Context, r Run) error {
	note := Note{Run: r.ID, Agent: r.Agent, Status: r.Status, Ref: r.Notify.Ref}
	if r.Status == Paused {
		note.Cards = waitingCards(r)
	}
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
