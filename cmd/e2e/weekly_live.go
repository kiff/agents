package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kiff/agents/cmd/internal/testaccount"
)

// liveWeeklyCloud is tafs cloud's component route as the owner's phone calls
// it: the phone's KIFF session as bearer, its signed read of its grants on
// each call (planque takes each once), and the report signed as a setup call
// to KIFF (kiff.request.v1 over the request, in KIFF-Device-Signature).
type liveWeeklyCloud struct {
	t     *test
	coach string // Coach's taf id on tafs cloud
}

func (c *liveWeeklyCloud) client(sign bool) *client {
	t := c.t
	return &client{Base: strings.TrimRight(t.cfg.cloud, "/"), HTTP: t.hc, Auth: func(req *http.Request, body []byte) {
		req.Header.Set("Authorization", "Bearer "+t.cfg.session)
		if sign {
			req.Header.Set("KIFF-Device-Signature", deviceSignature(t.pk, t.tenant, req, body, time.Now()))
		}
	}}
}

// deviceSignature is KIFF's setup-call signature over a request, as the
// phone makes it (testaccount.KIFFSession's scheme).
func deviceSignature(pk *ecdsa.PrivateKey, tenant string, req *http.Request, body []byte, now time.Time) string {
	at := now.UTC().Format(time.RFC3339)
	sum := sha256.Sum256(body)
	path := req.URL.Path
	if req.URL.RawQuery != "" {
		path += "?" + req.URL.RawQuery
	}
	msg := strings.Join([]string{"kiff.request.v1", tenant, req.Method, path, hex.EncodeToString(sum[:]), at}, "\n")
	digest := sha256.Sum256([]byte(msg))
	sig, _ := ecdsa.SignASN1(rand.Reader, pk, digest[:])
	return "t=" + at + ",v1=" + base64.StdEncoding.EncodeToString(sig)
}

func (c *liveWeeklyCloud) componentPath(component string) string {
	return "/v1/tafs/" + url.PathEscape(c.coach) + "/components/" + url.PathEscape(component) + "/approval"
}

func (c *liveWeeklyCloud) decide(ctx context.Context, cl *client, path string, body map[string]any) (weeklyDecision, error) {
	proof, err := c.t.phone.GrantsProof(ctx)
	if err != nil {
		return weeklyDecision{}, err
	}
	a, err := cl.DoWith(ctx, http.MethodPost, path, body, http.Header{"Planque-Device-Signature": {proof}})
	if err != nil {
		return weeklyDecision{}, err
	}
	if err := a.Err(); err != nil {
		return weeklyDecision{}, err
	}
	var d weeklyDecision
	if err := json.Unmarshal([]byte(a.Raw), &d); err != nil {
		return weeklyDecision{}, fmt.Errorf("tafs cloud's answer: %w", err)
	}
	if d.Ask != nil {
		c.t.out.hide(d.Ask.ID, "<an ask>")
	}
	return d, nil
}

func (c *liveWeeklyCloud) Approval(ctx context.Context, component string, body map[string]any) (weeklyDecision, error) {
	return c.decide(ctx, c.client(false), c.componentPath(component), body)
}

func (c *liveWeeklyCloud) Report(ctx context.Context, component, ask string, body map[string]any) (weeklyDecision, error) {
	return c.decide(ctx, c.client(true), c.componentPath(component)+"/"+url.PathEscape(ask), body)
}

// liveWeeklyPhone is the phone's own calls: planque's grant as it reads it,
// KIFF's draft, and the calls an ask names.
type liveWeeklyPhone struct{ t *test }

func (p liveWeeklyPhone) Grant(ctx context.Context, id string) (string, []weeklyTool, error) {
	g, err := p.t.phone.Grant(ctx, id)
	if err != nil {
		return "", nil, err
	}
	p.t.out.hide(g.ID, "<a grant>")
	var tools []weeklyTool
	for _, tl := range g.Request.Tools {
		tools = append(tools, weeklyTool{Name: catalogName(tl), Op: tl.Tool})
	}
	return g.Digest, tools, nil
}

// catalogName is a grant tool's catalog name: its name without the grant's
// suffix ("_" and four letters or digits).
func catalogName(tl testaccount.GrantTool) string {
	n := tl.Name
	if i := strings.LastIndex(n, "_"); i > 0 && suffixed.MatchString(n[i:]) {
		return n[:i]
	}
	return n
}

func (p liveWeeklyPhone) DraftVersion(ctx context.Context, draft string) (string, error) {
	p.t.out.hide(draft, "<a draft>")
	return p.t.draftVersion(ctx, draft)
}

func (p liveWeeklyPhone) ConfirmDraft(ctx context.Context, draft, version string) error {
	d, err := p.t.kiff.Do(ctx, http.MethodPost, "/v1/me/agent-drafts/"+url.PathEscape(draft)+"/confirm", map[string]string{"version": version})
	if err != nil {
		return err
	}
	return d.Err()
}

// ApproveConnect sends planque's approve body as tafs cloud built it. A grant
// planque already holds the sign-in of connects at once; one signed in at the
// provider is confirmed with planque's one-use code. A vendor's sign-in page
// is never opened: the weekly proof clicks no consent page (kiff-qa 05:27),
// so a connection that needs one ends not connected.
func (p liveWeeklyPhone) ApproveConnect(ctx context.Context, grant string, body json.RawMessage) (string, error) {
	path := p.t.phone.AccountPath() + "/grants/" + url.PathEscape(grant) + "/approve"
	a, err := p.t.phone.Planque.Do(ctx, http.MethodPost, path, body)
	if err != nil {
		return "", err
	}
	if err := a.Err(); err != nil {
		return "", err
	}
	status, _ := a.Body["status"].(string)
	link, _ := a.Body["sign_in_url"].(string)
	code, _ := a.Body["confirm"].(string)
	switch {
	case link != "":
		return "sign_in_needed", nil
	case status == "signed_in" && code != "":
		c, err := p.t.phone.Planque.Do(ctx, http.MethodPost, p.t.phone.AccountPath()+"/grants/"+url.PathEscape(grant)+"/confirm", map[string]string{"confirm": code})
		if err != nil {
			return "", err
		}
		if err := c.Err(); err != nil {
			return "", err
		}
		st, _ := c.Body["status"].(string)
		return st, nil
	case status == "":
		return "", errors.New("planque's approve answered no status")
	}
	return status, nil
}
