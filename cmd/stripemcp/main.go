// Command stripemcp serves eval/stripemcp: the eval cases' Stripe
// operations as a remote MCP server, for KIFF's gateway to sit in front of.
//
//	STRIPE_API_KEY=sk_test_… STRIPEMCP_TOKEN=… stripemcp -addr :8090   # locally
//	stripemcp                                                           # on Lambda, behind a function URL
//
// STRIPE_API_KEY must be a test-mode key. STRIPEMCP_TOKEN is the bearer
// token the caller sends: the key KIFF holds for this connection. On Lambda
// both come from one Secrets Manager secret instead, named by
// STRIPEMCP_SECRET ({"stripe_api_key": "…", "token": "…"}, written by
// infra/eval-stripemcp.sh), so neither sits in the function's environment.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/kiff/agents/eval/stripemcp"
	"github.com/kiff/agents/server/lambdaurl"
)

func main() {
	addr := flag.String("addr", "", "serve here instead of on Lambda, e.g. :8090")
	flag.Parse()
	key, token, err := credentials(context.Background(), os.Getenv, readSecret)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	s, err := stripemcp.New(key, token)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *addr != "" {
		srv := &http.Server{Addr: *addr, Handler: s, ReadHeaderTimeout: 10 * time.Second}
		fmt.Fprintln(os.Stderr, "stripemcp on", *addr)
		if err := srv.ListenAndServe(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	lambda.Start(func(ctx context.Context, e events.LambdaFunctionURLRequest) (*events.LambdaFunctionURLStreamingResponse, error) {
		return lambdaurl.Serve(ctx, s, e, log)
	})
}

// credentials are the Stripe key and the bearer token: from the secret
// STRIPEMCP_SECRET names, both of them, when it's set; else from
// STRIPE_API_KEY and STRIPEMCP_TOKEN. A secret that can't be read, or lacks
// either, is an error, never a fall back to the environment.
func credentials(ctx context.Context, getenv func(string) string, read func(context.Context, string) (string, error)) (key, token string, err error) {
	name := strings.TrimSpace(getenv("STRIPEMCP_SECRET"))
	if name == "" {
		return getenv("STRIPE_API_KEY"), getenv("STRIPEMCP_TOKEN"), nil
	}
	raw, err := read(ctx, name)
	if err != nil {
		return "", "", fmt.Errorf("STRIPEMCP_SECRET can't be read: %w", err)
	}
	var v struct {
		Key   string `json:"stripe_api_key"`
		Token string `json:"token"`
	}
	if json.Unmarshal([]byte(raw), &v) != nil || strings.TrimSpace(v.Key) == "" || strings.TrimSpace(v.Token) == "" {
		return "", "", errors.New(`STRIPEMCP_SECRET must hold {"stripe_api_key": "…", "token": "…"}`)
	}
	return v.Key, v.Token, nil
}

// readSecret reads a Secrets Manager secret's current string.
func readSecret(ctx context.Context, name string) (string, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return "", err
	}
	out, err := sm.NewFromConfig(cfg).GetSecretValue(ctx, &sm.GetSecretValueInput{SecretId: aws.String(name)})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.SecretString), nil
}
