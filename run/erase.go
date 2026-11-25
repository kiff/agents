package run

// Erase is r with what was written in it taken out, for a chat its person deleted: what they asked (Input, Aside, the
// files they sent), what they wrote while it went (Steering), what it answered (Output, Error), the calls' inputs and
// results (Requirements, NotAgain, Done) and its turns (Paused, Acting). What stays is the record that it ran and what
// it cost: its ids, status, times, turns and tokens, cost and meter, the tools it called and KIFF's ids for them
// (operations, holds, invocations), and how each held call was answered. Erasing an erased run changes nothing.
func (r Run) Erase() Run {
	e := r
	e.Erased = true
	e.Input, e.Aside, e.Output, e.Error = "", "", "", ""
	e.Attachments, e.AsideTools = nil, nil
	e.Paused, e.Acting = nil, nil
	e.Steering = nil
	for i := range r.Steering {
		st := r.Steering[i]
		st.Text, st.Key = "", ""
		e.Steering = append(e.Steering, st)
	}
	e.Requirements = nil
	for _, q := range r.Requirements {
		q.Input, q.Result, q.Key = nil, nil, ""
		e.Requirements = append(e.Requirements, q)
	}
	e.NotAgain = nil
	for _, c := range r.NotAgain {
		c.Input = nil
		e.NotAgain = append(e.NotAgain, c)
	}
	e.Done = nil
	for _, d := range r.Done {
		d.Input = nil
		e.Done = append(e.Done, d)
	}
	return e
}
