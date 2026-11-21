package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/kiff/agents/keys"
)

// KeyStore is where the tafs' KIFF keys are written (keys.Moving).
type KeyStore interface {
	Put(ctx context.Context, tenant, ref, key string) error
	Remove(ctx context.Context, tenant, ref string) error
}

// keyWriter refuses a caller whose role isn't one of KeyWriters: only tafs
// cloud writes keys, never another caller of the API, and never a run.
func (s *Server) keyWriter(c Caller) error {
	if c.Role == "" || !slices.Contains(s.KeyWriters, c.Role) {
		return errors.New("server: this caller may not write keys")
	}
	return nil
}

// putKey is PUT /tenant/keys/{ref...}: the caller's tenant's key by ref,
// replacing any. The tenant is the signed one, never the body's: the body
// holds the key alone.
func (s *Server) putKey(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	if err := s.keyWriter(c); err != nil {
		return 0, nil, errors.Join(ErrUnauthorized, err)
	}
	ref := r.PathValue("ref")
	if err := keys.Valid(c.Tenant, ref); err != nil {
		return 0, nil, bad("%v", err)
	}
	var in struct {
		Key string `json:"key"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || dec.Decode(new(json.RawMessage)) != io.EOF {
		return 0, nil, bad("the body is {\"key\": …} and nothing else")
	}
	if strings.TrimSpace(in.Key) == "" {
		return 0, nil, bad("no key")
	}
	if err := s.Keys.Put(ctx, c.Tenant, ref, in.Key); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

// removeKey is DELETE /tenant/keys/{ref...}: the caller's tenant's key by
// ref, removed for good. None there is not an error.
func (s *Server) removeKey(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	if err := s.keyWriter(c); err != nil {
		return 0, nil, errors.Join(ErrUnauthorized, err)
	}
	ref := r.PathValue("ref")
	if err := keys.Valid(c.Tenant, ref); err != nil {
		return 0, nil, bad("%v", err)
	}
	if err := s.Keys.Remove(ctx, c.Tenant, ref); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

// AlertSecretStore is where tafs cloud writes each tenant's KIFF alert
// secret, the one kiffalerts checks that tenant's alerts with
// (dynamodb.AlertSecrets).
type AlertSecretStore interface {
	Put(ctx context.Context, tenant, secret string) error
	Remove(ctx context.Context, tenant string) error
}

// putAlertSecret is PUT /tenant/alert-secret: the caller's tenant's alert
// secret, replacing any. Like a key: only a key writer, the tenant the
// signed one, the body the secret alone.
func (s *Server) putAlertSecret(ctx context.Context, c Caller, r *http.Request) (int, any, error) {
	if err := s.keyWriter(c); err != nil {
		return 0, nil, errors.Join(ErrUnauthorized, err)
	}
	if err := keys.Valid(c.Tenant, "alert"); err != nil {
		return 0, nil, bad("%v", err)
	}
	var in struct {
		Secret string `json:"secret"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || dec.Decode(new(json.RawMessage)) != io.EOF {
		return 0, nil, bad("the body is {\"secret\": …} and nothing else")
	}
	if strings.TrimSpace(in.Secret) == "" {
		return 0, nil, bad("no secret")
	}
	if err := s.AlertSecrets.Put(ctx, c.Tenant, in.Secret); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

// removeAlertSecret is DELETE /tenant/alert-secret: the caller's tenant's
// alert secret, removed. None there is not an error.
func (s *Server) removeAlertSecret(ctx context.Context, c Caller, _ *http.Request) (int, any, error) {
	if err := s.keyWriter(c); err != nil {
		return 0, nil, errors.Join(ErrUnauthorized, err)
	}
	if err := keys.Valid(c.Tenant, "alert"); err != nil {
		return 0, nil, bad("%v", err)
	}
	if err := s.AlertSecrets.Remove(ctx, c.Tenant); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}
