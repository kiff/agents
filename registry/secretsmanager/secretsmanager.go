// Package secretsmanager is registry.Secrets on AWS Secrets Manager: an
// agent's KIFF key is a secret named prefix + tenant + "/" + its reference.
//
// The prefix is configuration, never a constant here. A tenant is one name
// segment and a reference is one or more, so no reference reaches another
// tenant's secrets: whatever it says, the name it makes starts with its own
// tenant's segment.
package secretsmanager

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/kiff/agents/registry"
)

// API is the part of the Secrets Manager client the adapter uses.
type API interface {
	GetSecretValue(context.Context, *sm.GetSecretValueInput, ...func(*sm.Options)) (*sm.GetSecretValueOutput, error)
}

// maxName is the longest secret name Secrets Manager takes.
const maxName = 512

// ErrNotFound means the tenant has no secret by that reference.
var ErrNotFound = errors.New("secretsmanager: no such secret")

// Secrets resolves KIFF key references on Secrets Manager.
type Secrets struct {
	api API
	// Prefix starts every secret's name, such as "agents/".
	Prefix string
}

// New returns Secrets that read the secrets named under prefix.
func New(api API, prefix string) *Secrets {
	return &Secrets{api: api, Prefix: prefix}
}

var _ registry.Secrets = (*Secrets)(nil)

// Name is the secret that holds the tenant's key by ref, or an error when
// either can't be one name segment (the tenant) or a path of them (ref).
func (s *Secrets) Name(tenant, ref string) (string, error) {
	if !segment(tenant) {
		return "", fmt.Errorf("secretsmanager: a tenant is one segment of letters, digits or %q", allowed)
	}
	for _, p := range strings.Split(ref, "/") {
		if !segment(p) {
			return "", fmt.Errorf("secretsmanager: a key reference is segments of letters, digits or %q, split by '/'", allowed)
		}
	}
	name := s.Prefix + tenant + "/" + ref
	if len(name) > maxName {
		return "", fmt.Errorf("secretsmanager: the secret's name is longer than %d bytes", maxName)
	}
	return name, nil
}

// Key returns the tenant's KIFF key by ref: the secret's string, which must
// not be empty.
func (s *Secrets) Key(ctx context.Context, tenant, ref string) (string, error) {
	name, err := s.Name(tenant, ref)
	if err != nil {
		return "", err
	}
	out, err := s.api.GetSecretValue(ctx, &sm.GetSecretValueInput{SecretId: aws.String(name)})
	var missing *types.ResourceNotFoundException
	if errors.As(err, &missing) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("secretsmanager: reading the key: %w", err)
	}
	// Secrets Manager takes a name ending in '-' and six characters as part
	// of an ARN, and may answer with another secret: only this one will do.
	if aws.ToString(out.Name) != name {
		return "", ErrNotFound
	}
	key := strings.TrimSpace(aws.ToString(out.SecretString))
	if key == "" {
		return "", errors.New("secretsmanager: the secret holds no key")
	}
	return key, nil
}

// allowed is what a segment may hold besides letters and digits: what
// Secrets Manager allows in a name, but '/'.
const allowed = "_+=.@-"

func segment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, c := range []byte(s) {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte(allowed, c) >= 0) {
			return false
		}
	}
	return true
}
