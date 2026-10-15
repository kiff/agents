package secretsmanager_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/kiff/agents/registry/secretsmanager"
)

// store is Secrets Manager holding one secret per name. In cmd/, it is
// sm.NewFromConfig(cfg).
type store map[string]string

func (s store) GetSecretValue(_ context.Context, in *sm.GetSecretValueInput, _ ...func(*sm.Options)) (*sm.GetSecretValueOutput, error) {
	v, ok := s[aws.ToString(in.SecretId)]
	if !ok {
		return nil, &types.ResourceNotFoundException{}
	}
	return &sm.GetSecretValueOutput{Name: in.SecretId, SecretString: aws.String(v)}, nil
}

func Example() {
	ctx := context.Background()
	secrets := secretsmanager.New(store{"agents/t1/kiff/rae": "kiff-agent-key"}, "agents/")

	// The agent's definition holds KeyRef "kiff/rae"; the key is t1's secret.
	name, _ := secrets.Name("t1", "kiff/rae")
	key, err := secrets.Key(ctx, "t1", "kiff/rae")
	fmt.Println(name, key, err)

	// The same reference, for another tenant, is that tenant's secret.
	_, err = secrets.Key(ctx, "t2", "kiff/rae")
	fmt.Println(errors.Is(err, secretsmanager.ErrNotFound))

	// No reference leaves its tenant.
	_, err = secrets.Key(ctx, "t2", "../t1/kiff/rae")
	fmt.Println(err != nil)
	// Output:
	// agents/t1/kiff/rae kiff-agent-key <nil>
	// true
	// true
}
