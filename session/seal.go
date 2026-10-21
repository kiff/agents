package session

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
)

// Sealer seals what a session keeps that only its runner may read
// (KeptRead), under the tenant's own key. aad binds a sealed body to where
// it is stored: moved to another tenant, session, run or place, it doesn't
// open.
type Sealer interface {
	Seal(ctx context.Context, tenant string, aad, plain []byte) ([]byte, error)
	Open(ctx context.Context, tenant string, aad, sealed []byte) ([]byte, error)
}

// SealedBody is a sealed event's Body as stored: still JSON, so every Store
// and every reader of events carries it, and nothing in it is readable.
type SealedBody struct {
	Sealed []byte `json:"sealed"`
}

// ErrNotSealed means an event that should be sealed isn't.
var ErrNotSealed = errors.New("session: the event's body is not sealed")

// aad is what a sealed event is bound to.
func aad(tenant string, e Event) []byte {
	return []byte("kiff/agents sealed event v1\x00" + tenant + "\x00" + e.Session + "\x00" + e.Run + "\x00" + strconv.FormatInt(e.Seq, 10) + "\x00" + string(e.Kind))
}

// seal is e with its Body sealed.
func seal(ctx context.Context, s Sealer, tenant string, e Event) (Event, error) {
	sealed, err := s.Seal(ctx, tenant, aad(tenant, e), e.Body)
	if err != nil {
		return e, err
	}
	e.Body, err = json.Marshal(SealedBody{Sealed: sealed})
	e.seal = false
	return e, err
}

// OpenEvent is a sealed event's body, in clear.
func OpenEvent(ctx context.Context, s Sealer, tenant string, e Event) ([]byte, error) {
	var b SealedBody
	if err := json.Unmarshal(e.Body, &b); err != nil || len(b.Sealed) == 0 {
		return nil, ErrNotSealed
	}
	return s.Open(ctx, tenant, aad(tenant, e), b.Sealed)
}

// KeySealer seals with AES-256-GCM under each tenant's data key, which Keys
// gives (made once per tenant, and kept by Keys' own store). A tenant's key
// is asked for once per KeySealer, then held in memory.
type KeySealer struct {
	// Keys is the tenant's 32-byte data key.
	Keys func(ctx context.Context, tenant string) ([]byte, error)

	mu    sync.Mutex
	aeads map[string]cipher.AEAD
}

func (k *KeySealer) aead(ctx context.Context, tenant string) (cipher.AEAD, error) {
	if tenant == "" {
		return nil, errors.New("session: sealing needs a tenant")
	}
	k.mu.Lock()
	a, ok := k.aeads[tenant]
	k.mu.Unlock()
	if ok {
		return a, nil
	}
	key, err := k.Keys(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("session: the tenant's sealing key: %w", err)
	}
	if len(key) != 32 {
		return nil, errors.New("session: a sealing key is 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if a, err = cipher.NewGCM(block); err != nil {
		return nil, err
	}
	k.mu.Lock()
	if k.aeads == nil {
		k.aeads = map[string]cipher.AEAD{}
	}
	k.aeads[tenant] = a
	k.mu.Unlock()
	return a, nil
}

// Seal is the nonce, then plain sealed under the tenant's key with aad.
func (k *KeySealer) Seal(ctx context.Context, tenant string, aad, plain []byte) ([]byte, error) {
	a, err := k.aead(ctx, tenant)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, a.NonceSize(), a.NonceSize()+len(plain)+a.Overhead())
	_, _ = rand.Read(nonce) // crypto/rand.Read never fails on supported platforms
	return a.Seal(nonce, nonce, plain, aad), nil
}

// Open is what Seal sealed, if aad is what it was sealed with.
func (k *KeySealer) Open(ctx context.Context, tenant string, aad, sealed []byte) ([]byte, error) {
	a, err := k.aead(ctx, tenant)
	if err != nil {
		return nil, err
	}
	if len(sealed) < a.NonceSize() {
		return nil, errors.New("session: a sealed body too short to open")
	}
	return a.Open(nil, sealed[:a.NonceSize()], sealed[a.NonceSize():], aad)
}

// MemoryKeys is a Keys for tests and local runs: a random key per tenant,
// for the life of the process.
func MemoryKeys() func(ctx context.Context, tenant string) ([]byte, error) {
	var mu sync.Mutex
	keys := map[string][]byte{}
	return func(_ context.Context, tenant string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if k, ok := keys[tenant]; ok {
			return k, nil
		}
		k := make([]byte, 32)
		_, _ = rand.Read(k)
		keys[tenant] = k
		return k, nil
	}
}
