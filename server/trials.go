package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/kiff/agents/registry"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/session"
)

// An agent's open trial (kiff/tafs#173): the owner chats with one of its
// versions under that version's trial Card in KIFF, while the live version
// keeps its schedule and Cards. One per agent. When it ends, its chat is
// deleted and its version keeps one line about it, with KIFF's counts.
//
//	PUT    /agents/{id}/trial           {n, draft} → {"trial"}; the same again is the same trial
//	GET    /agents/{id}/trial           {"trial"}
//	POST   /agents/{id}/trial/messages  {input} → {"run", "trial"}, at once; the turn runs in the background
//	DELETE /agents/{id}/trial           {why, calls, held, refused} → {"ended"}
//
// SweepTrials, from the function's sweep, ends trials idle for
// registry.TrialIdle.

// openTrialBody is what PUT /agents/{id}/trial takes.
type openTrialBody struct {
	N int `json:"n"`
	// Draft is the KIFF draft confirmed as the trial's Card: its calls go
	// under it (KIFF-Trial).
	Draft string `json:"draft"`
	// Requester is the person who opens it: its chat is theirs alone.
	Requester *run.Requester `json:"requester,omitempty"`
}

// endTrialBody is what DELETE /agents/{id}/trial takes: why it ended, and
// KIFF's counts of its calls, which the version keeps.
type endTrialBody struct {
	Why     string `json:"why"`
	Calls   int    `json:"calls"`
	Held    int    `json:"held"`
	Refused int    `json:"refused"`
}

// openTrial opens a trial of version n, against its trial Card's draft. An
// open trial of another version or draft is a conflict: end it first, with
// its counts, so nothing about it is lost.
func (s *Server) openTrial(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	var body openTrialBody
	if err := decode(r, &body); err != nil {
		return 0, nil, err
	}
	if body.N < 1 || body.Draft == "" {
		return 0, nil, bad("a trial needs its version (n) and its trial Card's draft")
	}
	q, err := requesterOf(body.Requester)
	if err != nil {
		return 0, nil, err
	}
	id := r.PathValue("id")
	vs, err := s.Agents.Versions(ctx, c.Tenant, id)
	if err != nil {
		return 0, nil, err
	}
	if !slices.ContainsFunc(vs, func(v registry.Version) bool { return v.N == body.N }) {
		return 0, nil, registry.ErrNoVersion
	}
	if open, ok, err := s.Agents.Trial(ctx, c.Tenant, id); err != nil || ok {
		return s.openAlready(open, ok, err, body)
	}
	// The trial's chat is chosen now, before its first message: two first
	// messages at once share it, and ending the trial deletes the one chat.
	d, err := s.Agents.GetVersion(ctx, c.Tenant, id, body.N)
	if err != nil {
		return 0, nil, err
	}
	rec, err := session.NewRecorderAt(s.Sessions, d.Keep, s.now)
	if err != nil {
		return 0, nil, err
	}
	sess, err := rec.OpenOwned(ctx, c.Tenant, id, session.KindTrial, q.Key())
	if err != nil {
		return 0, nil, err
	}
	now := s.now()
	t := registry.Trial{Tenant: c.Tenant, Agent: id, ID: run.NewID(), N: body.N, Draft: body.Draft, Session: sess.ID, Started: now, Used: now}
	if err := s.Agents.OpenTrial(ctx, t); err != nil {
		// Another open got there first, or the agent is gone: its chat isn't this one.
		if derr := s.Sessions.Delete(context.WithoutCancel(ctx), c.Tenant, sess.ID); derr != nil {
			err = errors.Join(err, fmt.Errorf("removing the chat it opened: %w", derr))
		}
		if errors.Is(err, registry.ErrTrialOpen) {
			open, ok, rerr := s.Agents.Trial(ctx, c.Tenant, id)
			return s.openAlready(open, ok, rerr, body)
		}
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"trial": viewTrial(t)}, nil
}

// openAlready answers an open of a trial when the agent has one: the same
// version and draft is the same trial; another is a conflict, ended first.
func (s *Server) openAlready(open registry.Trial, ok bool, err error, body openTrialBody) (int, any, error) {
	if err != nil {
		return 0, nil, err
	}
	if !ok { // ended meanwhile: the owner opens it again
		return 0, nil, conflictError{"trial_changed", "the trial changed while it was opened: try again"}
	}
	if open.N == body.N && open.Draft == body.Draft {
		return http.StatusOK, map[string]any{"trial": viewTrial(open)}, nil
	}
	return 0, nil, conflictError{"trial_open", fmt.Sprintf("this agent is already trying version %d: end that trial first", open.N)}
}

func (s *Server) getOpenTrial(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	t, ok, err := s.Agents.Trial(ctx, c.Tenant, r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	if !ok {
		return 0, nil, registry.ErrNoTrial
	}
	return http.StatusOK, map[string]any{"trial": viewTrial(t)}, nil
}

// trialMessage runs one turn of the trial's chat: version n's definition,
// in the trial's session, its calls under the trial Card's draft. The live
// version's runs are untouched. A trial idle for registry.TrialIdle takes
// no more: the sweep ends it.
func (s *Server) trialMessage(w http.ResponseWriter, r *http.Request) {
	c, err := s.caller(w, r)
	if err != nil {
		return
	}
	ctx := r.Context()
	var body trialBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Input == "" || len(body.Input) > MaxInput {
		s.fail(w, r, bad("the input is 1 to %d bytes", MaxInput))
		return
	}
	q, err := requesterOf(body.Requester)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.Background == nil {
		s.fail(w, r, unavailable("this server doesn't run runs in the background"))
		return
	}
	id := r.PathValue("id")
	t, ok, err := s.Agents.Trial(ctx, c.Tenant, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		s.fail(w, r, registry.ErrNoTrial)
		return
	}
	now := s.now()
	if t.Idle(now) {
		s.fail(w, r, conflictError{"trial_idle", "this trial went unused for a week, and is ending"})
		return
	}
	// Marked used only while it is still this trial: one that ended meanwhile,
	// or was replaced, takes no message and isn't brought back.
	if err := s.Agents.TouchTrial(ctx, c.Tenant, id, t.ID, now); err != nil {
		s.fail(w, r, err)
		return
	}
	t.Used = now
	// In the trial's own chat, chosen when it opened: if the trial ends now,
	// its chat is gone and the run can't start in it.
	rn, err := s.start(ctx, Start{Tenant: c.Tenant, Agent: id, Session: t.Session, Trigger: run.ByTrial, Input: body.Input, Requester: q, Version: t.N, Draft: t.Draft})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.toBackground(ctx, rn); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run": viewRun(rn), "trial": viewTrial(t)})
}

// closeTrial ends the agent's open trial: Install or Go back, from the
// owner, once KIFF has ended its trial Card.
func (s *Server) closeTrial(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	var body endTrialBody
	if err := decode(r, &body); err != nil {
		return 0, nil, err
	}
	switch body.Why {
	case registry.TrialInstalled, registry.TrialBack, registry.TrialIdled:
	default:
		return 0, nil, bad("why is %q, %q or %q", registry.TrialInstalled, registry.TrialBack, registry.TrialIdled)
	}
	if body.Calls < 0 || body.Held < 0 || body.Refused < 0 {
		return 0, nil, bad("the counts are KIFF's, and never below zero")
	}
	t, ok, err := s.Agents.Trial(ctx, c.Tenant, r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	if !ok {
		return 0, nil, registry.ErrNoTrial
	}
	sum, err := s.endTrial(ctx, t, body.Why, registry.TrialCounts{Calls: body.Calls, Held: body.Held, Refused: body.Refused})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"ended": sum}, nil
}

// endTrial keeps t's summary on its version, deletes its chat, and then
// removes the trial. Each step is safe to repeat: a retry after a failure
// finds the summary kept (by when the trial started) and doesn't keep it
// twice.
func (s *Server) endTrial(ctx context.Context, t registry.Trial, why string, n registry.TrialCounts) (registry.TrialSummary, error) {
	sum := registry.TrialSummary{Started: t.Started, Ended: s.now(), Why: why, Calls: n.Calls, Held: n.Held, Refused: n.Refused}
	vs, err := s.Agents.Versions(ctx, t.Tenant, t.Agent)
	if err != nil {
		return sum, err
	}
	i := slices.IndexFunc(vs, func(v registry.Version) bool { return v.N == t.N })
	if i < 0 {
		return sum, registry.ErrNoVersion
	}
	kept := slices.ContainsFunc(vs[i].Trials, func(k registry.TrialSummary) bool { return k.Started.Equal(t.Started) })
	if !kept {
		if err := s.Agents.AddTrialSummary(ctx, t.Tenant, t.Agent, t.N, sum); err != nil {
			return sum, err
		}
	}
	if t.Session != "" {
		if err := s.Sessions.Delete(ctx, t.Tenant, t.Session); err != nil {
			return sum, err
		}
	}
	return sum, s.Agents.EndTrial(ctx, t.Tenant, t.Agent, t.ID)
}

// SweepTrials ends every trial idle for registry.TrialIdle, and returns
// them: KIFF ends its trial Card first (EndKIFFTrial), and its counts are
// kept on the version. A trial KIFF can't end now is left for the next
// sweep, and its error returned with the others. Without EndKIFFTrial it
// ends none, since ending one only here would leave its Cards in KIFF.
func (s *Server) SweepTrials(ctx context.Context) ([]registry.Trial, error) {
	if s.EndKIFFTrial == nil {
		return nil, nil
	}
	all, err := s.Agents.Trials(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	ended := []registry.Trial{}
	var errs []error
	for _, t := range all {
		if !t.Idle(now) {
			continue
		}
		n, err := s.EndKIFFTrial(ctx, t)
		if err == nil {
			_, err = s.endTrial(ctx, t, registry.TrialIdled, n)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("trial of %s/%s: %w", t.Tenant, t.Agent, err))
			continue
		}
		ended = append(ended, t)
	}
	return ended, errors.Join(errs...)
}

// trialView is a trial as the API answers it: how many days unused end it,
// and when it ends if it stays unused, so no caller keeps its own copy.
type trialView struct {
	registry.Trial
	IdleDays int       `json:"idle_days"`
	Ends     time.Time `json:"ends"`
}

func viewTrial(t registry.Trial) trialView {
	return trialView{Trial: t, IdleDays: int(registry.TrialIdle / (24 * time.Hour)), Ends: t.Ends()}
}
