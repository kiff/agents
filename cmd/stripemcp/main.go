// Command stripemcp serves eval/stripemcp: the eval cases' Stripe
// operations as a remote MCP server, for KIFF's gateway to sit in front of.
//
//	STRIPE_API_KEY=sk_test_… STRIPEMCP_TOKEN=… stripemcp -addr :8090   # locally
//	stripemcp                                                           # on Lambda, behind a function URL
//
// STRIPE_API_KEY must be a test-mode key. STRIPEMCP_TOKEN is the bearer
// token the caller sends: the key KIFF holds for this connection.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"

	"github.com/kiff/agents/eval/stripemcp"
	"github.com/kiff/agents/server/lambdaurl"
)

func main() {
	addr := flag.String("addr", "", "serve here instead of on Lambda, e.g. :8090")
	flag.Parse()
	s, err := stripemcp.New(os.Getenv("STRIPE_API_KEY"), os.Getenv("STRIPEMCP_TOKEN"))
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
