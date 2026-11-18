package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/kiff/agents/cmd/internal/testaccount"
	"github.com/kiff/agents/drafts"
	draftsapi "github.com/kiff/agents/drafts/kiffapi"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
	tooldrafts "github.com/kiff/agents/tool/drafts"
	"github.com/kiff/agents/tool/kiff"
	"github.com/kiff/agents/tool/planque"
)

// liveProof is a proof's steps on the test account: the setup agent's key
// asks planque and submits the draft, as Setup's own tools do; the test
// phone gives the key, signs and refuses, as the app does; the taf's key,
// claimed from the draft, calls through KIFF's gateway.
type liveProof struct {
	t      *test
	run    string
	setup  string // Setup's agent id, never swept
	claims *drafts.Service
	gw     *kiff.Client // the taf's gateway client, once its key is claimed
	draft  string
}

func newLiveProof(t *test, setup string) *liveProof {
	t.keys = memory.NewDrafts()
	return &liveProof{t: t, run: t.run, setup: setup,
		claims: &drafts.Service{Store: t.keys, Keys: t.keys, KIFF: draftsapi.New(kiff.APIURL, t.hc)}}
}

// planqueAsSetup is planque as the setup agent reaches it, by its key.
func (p *liveProof) planqueAsSetup() *client {
	return &client{Base: strings.TrimRight(p.t.cfg.planque, "/"), HTTP: p.t.hc, Auth: bearerAuth(p.t.cfg.setupKey)}
}

// askedGrant is a grant the setup agent asked for, with its agent and its tools.
type askedProofGrant struct {
	ID     string `json:"id"`
	Agent  string `json:"agent"`
	Status string `json:"status"`
	Tools  []struct {
		Name string `json:"name"`
		Tool string `json:"tool"`
	} `json:"tools"`
}

func (p *liveProof) asked(ctx context.Context) ([]askedProofGrant, error) {
	a, err := p.planqueAsSetup().Do(ctx, http.MethodGet, p.t.phone.AccountPath()+"/asked-grants", nil)
	if err != nil {
		return nil, err
	}
	if err := a.Err(); err != nil {
		return nil, fmt.Errorf("planque asked-grants: %w", err)
	}
	var out struct {
		Grants []askedProofGrant `json:"grants"`
	}
	if err := json.Unmarshal([]byte(a.Raw), &out); err != nil {
		return nil, fmt.Errorf("planque asked-grants: %w", err)
	}
	return out.Grants, nil
}

func (p *liveProof) Sweep(ctx context.Context, app proofApp) (int, error) {
	n := 0
	gs, err := p.asked(ctx)
	if err != nil {
		return 0, err
	}
	for _, g := range gs {
		if !proofSwept(app.Name, g.Agent, p.setup) {
			continue
		}
		p.t.out.hide(g.ID, "<a grant>")
		if err := p.t.phone.Disconnect(ctx, g.ID); err != nil {
			return n, fmt.Errorf("disconnecting an earlier proof's grant: %w", err)
		}
		n++
	}
	there, setup, err := testaccount.PhoneAccess(ctx, p.t.kiff)
	if err != nil {
		return n, err
	}
	var left []string
	for a := range there {
		if proofSwept(app.Name, a, setup) {
			left = append(left, a)
		}
	}
	if len(left) > 0 {
		c, err := testaccount.Sweep(ctx, p.t.kiff, left)
		if err != nil {
			return n, err
		}
		n += len(left) - c.Left()
	}
	return n, nil
}

func (p *liveProof) Connect(ctx context.Context, app proofApp, name string, tools []string) (proofGrant, error) {
	key := p.t.phone.VendorKey
	if key == "" {
		return proofGrant{}, errors.New("no sandbox key for the app in this job")
	}
	pq := planque.New(strings.TrimRight(p.t.cfg.planque, "/"), p.t.tenant, p.t.cfg.setupKey, p.t.hc)
	in, _ := json.Marshal(map[string]any{"action": "request", "entry": app.Entry, "name": name, "taf_name": name, "uses": tools})
	r, err := pq.Call(ctx, in)
	if err != nil {
		return proofGrant{}, err
	}
	if r.IsError {
		return proofGrant{}, errors.New("planque refused the request: " + r.Text)
	}
	gs, err := p.asked(ctx)
	if err != nil {
		return proofGrant{}, err
	}
	var asked *askedProofGrant
	for i := range gs {
		if strings.HasPrefix(gs[i].Agent, name) {
			asked = &gs[i]
		}
	}
	if asked == nil {
		return proofGrant{}, errors.New("planque lists no grant for the proof's agent")
	}
	p.t.out.hide(asked.ID, "<a grant>")
	p.t.out.hide(asked.Agent, "<the proof's agent>")
	g := proofGrant{ID: asked.ID, Agent: asked.Agent, Names: map[string]string{}}
	pg, err := p.t.phone.Grant(ctx, g.ID)
	if err != nil {
		return g, err
	}
	for _, c := range tools {
		for _, gt := range pg.Request.Tools {
			if tafNameOf(gt.Name, c) {
				g.Names[c] = gt.Name
			}
		}
	}
	return g, nil
}

// suffixed is a catalog tool's name as planque gives it a taf: the name, or
// the name and "_" and four letters or digits of the grant.
var suffixed = regexp.MustCompile(`^_[a-z0-9]{4}$`)

func tafNameOf(name, catalog string) bool {
	return name == catalog || (strings.HasPrefix(name, catalog) && suffixed.MatchString(name[len(catalog):]))
}

func (p *liveProof) Draft(ctx context.Context, g proofGrant, label string, cards []map[string]any) (string, bool, string, error) {
	in, _ := json.Marshal(map[string]any{"action": "submit", "agent_id": g.Agent, "label": label, "cards": cards})
	r, err := tooldrafts.New(p.claims, p.t.tenant, p.t.cfg.setupKey).Call(ctx, in)
	if err != nil {
		return "", false, "", err
	}
	if r.IsError {
		return "", false, "", errors.New("KIFF refused the draft: " + r.Text)
	}
	var d drafts.Draft
	if err := json.Unmarshal([]byte(r.Text), &d); err != nil || d.ID == "" {
		return "", false, "", errors.New("the draft's answer has no id")
	}
	p.t.out.hide(d.ID, "<a draft>")
	p.draft = d.ID
	version, err := p.t.draftVersion(ctx, d.ID)
	if err != nil {
		return d.ID, false, "", err
	}
	checked, err := p.t.checkDraft(ctx, g.ID, d.ID, version)
	if err != nil {
		return d.ID, false, "", err
	}
	return d.ID, checked.OK, checked.Code, nil
}

func (p *liveProof) Approve(ctx context.Context, g proofGrant, draft string) error {
	pg, err := p.t.phone.Grant(ctx, g.ID)
	if err != nil {
		return err
	}
	if status, err := p.t.phone.ApproveAndSignIn(ctx, pg); err != nil {
		return fmt.Errorf("the phone's approval: %w", err)
	} else if !strings.Contains(status, "connected") {
		return fmt.Errorf("after the phone's approval the grant isn't connected: %s", status)
	}
	version, err := p.t.draftVersion(ctx, draft)
	if err != nil {
		return err
	}
	d, err := p.t.kiff.Do(ctx, http.MethodPost, "/v1/me/agent-drafts/"+url.PathEscape(draft)+"/confirm", map[string]string{"version": version})
	if err != nil || d.Err() != nil {
		return fmt.Errorf("confirming the draft in KIFF: %v %v", err, d.Err())
	}
	if ids, ok := d.Body["card_ids"].([]any); ok {
		for _, id := range ids {
			if s, ok := id.(string); ok {
				p.t.out.hide(s, "<a Card>")
			}
		}
	}
	agentID, err := p.claims.Claim(ctx, p.t.tenant, draft, "kiff/proof")
	if err != nil {
		return fmt.Errorf("claiming the taf's key: %w", err)
	}
	if agentID != g.Agent {
		return errors.New("the confirmed draft's agent isn't the grant's")
	}
	key, ok, err := p.t.keys.Key(ctx, p.t.tenant, "kiff/proof")
	if err != nil || !ok {
		return fmt.Errorf("the taf's key wasn't kept: %v", err)
	}
	p.t.out.hide(key, "<the taf's key>")
	p.gw = kiff.New(kiff.URL, key, p.t.hc)
	return nil
}

func (p *liveProof) Call(ctx context.Context, g proofGrant, name string, args map[string]any) (proofCall, error) {
	if p.gw == nil {
		return proofCall{}, errors.New("no taf key: the approval didn't finish")
	}
	if name == "" {
		return proofCall{}, errors.New("the grant has no such tool")
	}
	in, _ := json.Marshal(args)
	r, err := p.gw.Call(ctx, name, in)
	if err != nil {
		return proofCall{}, err
	}
	return callOf(r), nil
}

// callOf is what the proof keeps of a gateway call's result.
func callOf(r tool.Result) proofCall {
	c := proofCall{State: r.State, Count: -1}
	if c.State == "" && !r.IsError {
		c.State = tool.StateForwarded
	}
	if r.Hold != nil {
		c.Hold = r.Hold.Exception
	}
	if c.State != tool.StateForwarded {
		c.Text = r.Text
		return c
	}
	c.Items, c.Count = listOf([]byte(r.Text))
	return c
}

// listOf is a list read's items and their count: the result itself when
// it's a list, or the first list under items, data or results.
func listOf(raw []byte) ([]proofItem, int) {
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			return nil, -1
		}
		found := false
		for _, k := range []string{"items", "data", "results"} {
			if v, ok := obj[k]; ok && json.Unmarshal(v, &list) == nil {
				found = true
				break
			}
		}
		if !found {
			return nil, -1
		}
	}
	items := make([]proofItem, 0, len(list))
	for _, raw := range list {
		var it struct {
			ID    json.RawMessage `json:"id"`
			Name  string          `json:"name"`
			Title string          `json:"title"`
		}
		_ = json.Unmarshal(raw, &it)
		items = append(items, proofItem{ID: strings.Trim(string(it.ID), `"`), Name: firstOf(it.Name, it.Title)})
	}
	return items, len(items)
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func (p *liveProof) Refuse(ctx context.Context, hold string) (string, error) {
	p.t.out.hide(hold, "<a hold>")
	if err := p.t.answerHold(ctx, hold, "reject"); err != nil {
		return "", err
	}
	e, err := p.t.kiff.Do(ctx, http.MethodGet, "/v1/me/card-exceptions/"+url.PathEscape(hold), nil)
	if err != nil || e.Err() != nil {
		return "", fmt.Errorf("reading the hold after the refusal: %v %v", err, e.Err())
	}
	status, _ := e.Body["status"].(string)
	return status, nil
}

func (p *liveProof) Cleanup(ctx context.Context, g proofGrant) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var errs []error
	if g.ID != "" {
		if err := p.t.phone.Disconnect(ctx, g.ID); err != nil {
			errs = append(errs, fmt.Errorf("disconnecting the grant: %w", err))
		}
	}
	if g.Agent != "" {
		if _, err := testaccount.Sweep(ctx, p.t.kiff, []string{g.Agent}); err != nil {
			errs = append(errs, fmt.Errorf("archiving the agent: %w", err))
		}
	}
	return errors.Join(errs...)
}
