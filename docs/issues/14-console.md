# The console: what #14 delivers

**Issue:** [#14](https://github.com/kiff/agents/issues/14) · **Status:** in progress. The operators' run routes and operator sign-in are built; the rest of `/ops`, the console pages, stored eval runs and the deploy are to come.
**PRs:** #65 (runs across tenants, the `/ops` run routes), #67 (operator sign-in)

## What it's for

Step 3. Operators (the people building and running tafs, not a business
owner) need to see what agents do: every run, one run's timeline as it
happens, evals and cost. The owner's surface is the phone; this is the
operators'.

## Delivered

**Operators read runs across tenants (#65).** Served only when
`Server.Operator` is set: it verifies an operator, and the routes never take a
tenant's caller (a tenant's request is 401 there). They change nothing.

```go
srv := &server.Server{ /* … */
	Operator: func(r *http.Request) error { /* KIFF's sign-in, operators only */ },
}
```

```http
GET /ops/runs?limit=20             every run, newest first
GET /ops/runs/paused?limit=20      PAUSED runs, newest first, with what KIFF holds
GET /ops/tenants/t1/runs/5c4c…     one run
GET /ops/tenants/t1/runs/5c4c…/events

200 {"runs": [{"tenant": "t1", "id": "5c4c…", "agent": "rae", "status": "PAUSED",
               "requirements": [{"call_id": "toolu_01", "tool": "stripe_refund",
                                 "exception": "exc_123", "expires": "…"}], …}]}
```

- A run is named by its tenant in the path: run IDs are a tenant's own.
  (ARCHITECTURE.md planned `/ops/runs/{id}`.)
- A run shows what the tenant routes show, plus `tenant`; never its paused
  turn, claim, lease or version. Events are the run's as kept: what the keep
  rules dropped is gone.
- `Runs.ListAll` and `Runs.ListPaused` are the port's two lists across
  tenants, for these routes only. DynamoDB keeps them as partitions `A` (every
  run, with the run's TTL) and `PA` (written when a run pauses, deleted when
  it's claimed again or deleted; an entry left behind is never listed).

| Rule | Guarded by |
|---|---|
| all runs newest first, across tenants; paused ones only while paused; deleted and expired runs gone | `runtest` `OperatorsListAcrossTenants` (memory, DynamoDB) |
| the paused entry goes on a claim and on delete; one left behind lists nothing | `store/dynamodb` `TestPausedListItems` |
| operators list across tenants, paused apart, with a bounded `limit`; no one else, a tenant's caller included, gets in | `server` `TestOperatorsReadRunsAcrossTenants` |
| one run with its tenant, never its paused turn or claim; its kept events; another tenant's path is 404 | `TestOperatorsReadARun` |
| no `Operator`, no `/ops` | `TestNoOperatorNoOps` |

**Operators sign in with KIFF (#67).** The console has its own URL; a
browser can't reach the agents server's IAM function URL. Operators sign in
through KIFF's identity provider (the Clerk instance app.kiff.dev uses), where
the console is registered as an OpenID Connect app, and only the subjects in
`Operators` get in. `console.SignIn` does it with the standard library:
discovery, the authorization code flow with PKCE (S256), and RS256 ID tokens
checked against the issuer's keys. No new dependency.

```go
in, err := console.New(console.Config{
	Issuer:   "https://clerk.kiff.dev",            // KIFF's issuer, as its ID tokens name it
	ClientID: clientID, ClientSecret: clientSecret, // the console's registration there
	BaseURL:  "https://console.example",            // callback: BaseURL + "/console/callback"
	Operators: []string{"user_2abc…"},              // the operators' subjects
	CookieKey: key,                                 // 32 random bytes or more
})
mux := http.NewServeMux()
in.Routes(mux)                                       // /console/login, /console/callback, POST /console/logout
mux.Handle("GET /console", in.Require(runsPage))     // pages: signed-in operators only
```

```text
GET /console/login     → 302 issuer/authorize?response_type=code&client_id=…&code_challenge=…&state=…&nonce=…
GET /console/callback  → code exchanged with the PKCE verifier; ID token checked → session cookie, 303 /console
```

- **The ID token** must be RS256, signed by a key the issuer lists (2048
  bits or more; an unknown key ID reads the keys again), for this client,
  from this issuer, current (a minute's skew), with this sign-in's nonce, and
  name an operator's subject. Anything else: 401, or 403 for someone who
  isn't an operator.
- **The sign-in in progress** (state, nonce, verifier) lives 10 minutes in a
  signed, HttpOnly cookie scoped to the callback.
- **The session** is a signed, HttpOnly, SameSite=Lax cookie for 8 hours
  (`SessionTTL`). It's checked on every request, and the subject must still
  be in `Operators`, so removing an operator takes effect at once. Signing out
  is a POST. Without a session a page redirects to sign-in, and anything else
  is 401.
- The base URL, the issuer and every endpoint its discovery document names
  are absolute https URLs; plain http only on loopback, for local use. A
  redirect to anything else isn't followed. So the client secret, the code
  and the verifier never cross plain http, and keys are never fetched over
  it (OIDC Discovery §3).

| Rule | Guarded by |
|---|---|
| sign-in end to end: the issuer asked with PKCE, state and nonce; the code exchanged with the verifier; the session lets the operator in until it ends | `console` `TestAnOperatorSignsIn` |
| someone signed in who isn't an operator: 403, no session | `TestSomeoneElseIsNotLetIn` |
| an ID token from another issuer, for another client, expired, from the future, with another nonce, no subject, another or unlisted key, `none` or HS256, a tampered claim, or not a JWT: no session | `TestAnIDTokenThatDoesntCheck` |
| the callback's state, cookie, age, the issuer's refusal, the code and the verifier | `TestACallbackThatDoesntCheck` |
| a forged session, no session, a POST without one, signing out, an operator removed since | `TestSessions` |
| a key under 2048 bits isn't trusted | `TestAShortKeyIsNotTrusted` |
| an http or relative endpoint in the discovery document, or keys redirected to http, signs no one in | `TestTheIssuerIsReachedOnlyOverHTTPS` |
| issuer (https), client, base URL (https), cookie key, operators (none empty) are required | `TestConfigNeeds` |

**To set up:** register the console as an OAuth app in KIFF's Clerk instance
with redirect URL `<BaseURL>/console/callback`, then put its client ID and
secret, the operators' subjects and a cookie key in the console's secrets.

## Still to deliver

- **The rest of the operator routes**, read-only, across tenants: a run's
  events as SSE while it runs, `/ops/sessions/{id}`, `/ops/agents`,
  `/ops/agents/{id}/memories`, `/ops/schedules`, `/ops/evals`, `/ops/usage`. They are separate from the
  tenant-scoped routes tafs cloud calls, so the tenant rule there holds
  unchanged.
- **`console`** at `/console`, in Go, templ, htmx and plain CSS, with no
  Node. First pages:
  - **Runs:** every run, paused runs first, with what KIFF holds.
  - **A run:** its live timeline, with KIFF's decisions linked to
    app.kiff.dev, not copied.
  - **Evals.**
- **Stored eval runs.** From here on each eval case is an ordinary run in an
  eval session, and `GET /evals`, `GET /evals/{id}` and
  `GET /usage?agent_id=&from=&to=` (runs, tokens and cost per agent per day)
  join the API.

**Rules it keeps:** sign-in is KIFF's, limited to operators. The console
changes nothing; switching a schedule or an agent goes through tafs. It never
shows what the keep rules dropped.

## What's ready for it

| Needs | State |
|---|---|
| Runs with a status, requirements and cost | Built ([#12](12-runs.md)) |
| Sessions and events per run | Built ([#1](01-sessions.md), [#12](12-runs.md)) |
| Following a run's events live | Built for tenants (`resume`, [#13](13-server.md)); the `/ops` version reuses it |
| Grading a run of this runtime | Built (`eval.Load`, [#3](03-evals.md)) |
| Listing runs across tenants | Built: `Runs.ListAll`, `Runs.ListPaused` (#65) |
| Usage per agent per day | Not built: needs an aggregate the stores don't keep |
| Operator sign-in through KIFF | Built: `console.SignIn` (#67) |
