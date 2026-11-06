package kiff

import (
	"context"
	"crypto/sha256"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kiff/agents/tool"
)

// SessionTTL is the longest a gateway session is reused: a chat's next
// message skips the handshake (initialize) while its session is younger than
// this. The handshake was 486 ms of each run's 1.24 s start in the Eval after
// kiff/agents#359. Measured from the handshake, never extended by use.
const SessionTTL = 2 * time.Minute

// Sessions keeps each agent's gateway session for SessionTTL, so the next
// run of the same agent, with the same key, reuses it. A session is a cache
// of who the key is, so it is kept per gateway, tenant, agent and key, and
// never shared across any of them. A new key for the agent (a rotation)
// replaces it, and any refusal or failure from the gateway drops it: a
// revoked key or grant then opens a new one, or fails as it would have.
//
// Only the handshake is kept. Each run lists its tools afresh, so a Card that
// changed shows at once: Coach's own tools are given on the phone, straight
// to KIFF, where nothing here would hear of it.
type Sessions struct {
	// HTTP is the sessions' HTTP client; nil uses http.DefaultClient.
	HTTP *http.Client
	// Now is the clock; nil uses time.Now.
	Now func() time.Time

	mu sync.Mutex
	m  map[agentOf]*warmSession
}

// agentOf names one agent's session. Its key is in the session, by its
// SHA-256 only, never the key itself.
type agentOf struct{ gateway, tenant, agent string }

type warmSession struct {
	c   *Client
	key [sha256.Size]byte
	at  time.Time
	// dropped is set by the session's client when the gateway refuses or
	// fails it: a flag, never a lock, since the client may be holding its
	// own (a failed handshake). The next start opens a new session.
	dropped *atomic.Bool
}

// usable says the session may be reused at now.
func (w *warmSession) usable(now time.Time) bool {
	return fresh(w.at, now) && !w.dropped.Load()
}

// Tools lists the tools the gateway offers the agent, through its warm
// session or a new one. A warm session the gateway refuses is dropped, and
// the list is asked once more through a new session, so a session the
// gateway forgot never fails a run's start.
func (s *Sessions) Tools(ctx context.Context, gateway, tenant, agent, key string) ([]tool.Tool, error) {
	c, warm := s.client(gateway, tenant, agent, key)
	tools, err := c.Tools(ctx)
	if err != nil && warm {
		s.drop(agentOf{gateway, tenant, agent}, c)
		c, _ = s.client(gateway, tenant, agent, key)
		tools, err = c.Tools(ctx)
	}
	return tools, err
}

// client is the agent's session for key: its warm one, opened less than
// SessionTTL ago with the same key, or a new one in its place. warm says the
// session was opened before.
func (s *Sessions) client(gateway, tenant, agent, key string) (c *Client, warm bool) {
	now := s.now()
	of := agentOf{gateway, tenant, agent}
	sum := sha256.Sum256([]byte(key))
	// Nothing under s.mu waits on a client: its state is read atomically, so
	// one agent's handshake never holds up another agent's start.
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.m[of]; ok && w.key == sum && w.usable(now) {
		return w.c, w.c.ready.Load()
	}
	if s.m == nil {
		s.m = map[agentOf]*warmSession{}
	}
	// Sessions past their time, or dropped, go as new ones are opened: none is kept for long.
	for k, w := range s.m {
		if !w.usable(now) {
			delete(s.m, k)
		}
	}
	c = New(gateway, key, s.HTTP)
	dropped := new(atomic.Bool)
	c.refused = func() { dropped.Store(true) }
	s.m[of] = &warmSession{c: c, key: sum, at: now, dropped: dropped}
	return c, false
}

// drop forgets the agent's session, if it is still c.
func (s *Sessions) drop(of agentOf, c *Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.m[of]; ok && w.c == c {
		delete(s.m, of)
	}
}

func (s *Sessions) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// fresh says a session opened at is still young enough to reuse at now.
func fresh(at, now time.Time) bool {
	age := now.Sub(at)
	return age >= 0 && age < SessionTTL
}
