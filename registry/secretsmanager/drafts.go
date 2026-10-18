package secretsmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/kiff/agents/drafts"
)

// WriteAPI is the part of the Secrets Manager client that writes, besides
// reading.
type WriteAPI interface {
	API
	CreateSecret(context.Context, *sm.CreateSecretInput, ...func(*sm.Options)) (*sm.CreateSecretOutput, error)
	PutSecretValue(context.Context, *sm.PutSecretValueInput, ...func(*sm.Options)) (*sm.PutSecretValueOutput, error)
}

// Drafts is drafts.Store on Secrets Manager: each submitted draft's record
// is a secret named prefix + tenant + "/" + draft, holding its claim secret
// until its key is claimed, then what the claim did.
//
// The prefix must be outside the agents' key prefix (Secrets.Prefix), so no
// agent's key reference can name a draft's claim secret.
type Drafts struct {
	api    WriteAPI
	Prefix string // such as "agent-drafts/"
}

var _ drafts.Store = (*Drafts)(nil)

// NewDrafts returns draft records under prefix.
func NewDrafts(api WriteAPI, prefix string) *Drafts { return &Drafts{api: api, Prefix: prefix} }

func (d *Drafts) name(tenant, draft string) (string, error) {
	if !segment(tenant) || !segment(draft) {
		return "", errors.New("secretsmanager: a tenant and a draft are each one name segment")
	}
	return d.Prefix + tenant + "/" + draft, nil
}

// Put writes the draft's record, replacing what was there.
func (d *Drafts) Put(ctx context.Context, tenant, draft string, r drafts.Record) error {
	name, err := d.name(tenant, draft)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(r)
	_, err = d.api.PutSecretValue(ctx, &sm.PutSecretValueInput{SecretId: aws.String(name), SecretString: aws.String(string(b))})
	var missing *types.ResourceNotFoundException
	if errors.As(err, &missing) {
		_, err = d.api.CreateSecret(ctx, &sm.CreateSecretInput{Name: aws.String(name), SecretString: aws.String(string(b)),
			Description: aws.String("a taf's KIFF draft: its claim secret until claimed (kiff/agents#91)")})
	}
	if err != nil {
		return fmt.Errorf("secretsmanager: writing the draft's record: %w", err)
	}
	return nil
}

// Get reads the draft's record; drafts.ErrNotFound if this tenant has none.
func (d *Drafts) Get(ctx context.Context, tenant, draft string) (drafts.Record, error) {
	name, err := d.name(tenant, draft)
	if err != nil {
		return drafts.Record{}, drafts.ErrNotFound
	}
	out, err := d.api.GetSecretValue(ctx, &sm.GetSecretValueInput{SecretId: aws.String(name)})
	var missing *types.ResourceNotFoundException
	if errors.As(err, &missing) {
		return drafts.Record{}, drafts.ErrNotFound
	}
	if err != nil {
		return drafts.Record{}, fmt.Errorf("secretsmanager: reading the draft's record: %w", err)
	}
	if aws.ToString(out.Name) != name {
		return drafts.Record{}, drafts.ErrNotFound
	}
	var r drafts.Record
	if err := json.Unmarshal([]byte(aws.ToString(out.SecretString)), &r); err != nil {
		return drafts.Record{}, errors.New("secretsmanager: the draft's record is not a record")
	}
	return r, nil
}

// Keys is drafts.Keys on Secrets Manager: agents' KIFF keys where
// Secrets reads them, created only where none is.
type Keys struct {
	Secrets *Secrets
	api     WriteAPI
}

var _ drafts.Keys = (*Keys)(nil)

// NewKeys returns keys written where s reads them.
func NewKeys(s *Secrets, api WriteAPI) *Keys { return &Keys{Secrets: s, api: api} }

func (k *Keys) Key(ctx context.Context, tenant, ref string) (string, bool, error) {
	key, err := k.Secrets.Key(ctx, tenant, ref)
	if errors.Is(err, ErrNotFound) {
		return "", false, nil
	}
	if deleting(err) {
		return "", false, drafts.ErrTaken
	}
	if err != nil {
		return "", false, err
	}
	return key, true, nil
}

// Create stores key under ref; drafts.ErrTaken if a secret is already
// there (Secrets Manager refuses to create one by an existing name).
func (k *Keys) Create(ctx context.Context, tenant, ref, key string) error {
	name, err := k.Secrets.Name(tenant, ref)
	if err != nil {
		return err
	}
	_, err = k.api.CreateSecret(ctx, &sm.CreateSecretInput{Name: aws.String(name), SecretString: aws.String(key),
		Description: aws.String("an agent's KIFF key, claimed from its confirmed draft (kiff/agents#91)")})
	var exists *types.ResourceExistsException
	if errors.As(err, &exists) || deleting(err) {
		return drafts.ErrTaken
	}
	if err != nil {
		return fmt.Errorf("secretsmanager: storing the key: %w", err)
	}
	return nil
}

// deleting is Secrets Manager refusing a secret scheduled for deletion (a
// removed agent's key, until its recovery window ends): its name is still
// taken, and it can be neither read nor created.
func deleting(err error) bool {
	var inv *types.InvalidRequestException
	return errors.As(err, &inv) && strings.Contains(strings.ToLower(inv.ErrorMessage()), "deletion")
}
