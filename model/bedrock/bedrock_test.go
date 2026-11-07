package bedrock

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kiff/agents/model"
)

type fake struct {
	got model.Request
	err error
}

func (f *fake) Turn(_ context.Context, req model.Request, _ func(string)) (model.Reply, error) {
	f.got = req
	return model.Reply{Usage: model.Usage{InputTokens: 10, OutputTokens: 5}}, f.err
}

// A turn goes to the model's EU profile, without what Bedrock doesn't serve,
// and says where it ran and at what price.
func TestATurnRunsOnTheEUProfile(t *testing.T) {
	f := &fake{}
	m := &Model{inner: f, Region: "eu-west-1"}
	r, err := m.Turn(context.Background(), model.Request{Model: "claude-sonnet-5-5", Fast: true, Web: &model.Web{Domains: []string{"stripe.com"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.got.Model != "eu.anthropic.claude-sonnet-5-5" || f.got.Fast || f.got.Web != nil {
		t.Fatalf("sent %+v", f.got)
	}
	if r.Source != model.SourceBedrock || r.Region != "eu-west-1" || r.PriceFactor != GeoFactor {
		t.Fatalf("reply %+v", r)
	}
	if _, err := m.Turn(context.Background(), model.Request{Model: "claude-haiku-5-5"}, nil); !errors.Is(err, ErrNoProfile) {
		t.Fatalf("an unserved model: %v", err)
	}
	if w, err := m.Window(context.Background(), "claude-opus-5-5"); err != nil || w != 1_000_000 {
		t.Fatalf("window %d, %v", w, err)
	}
}

// No AWS credential reaches a log line or a run record through an error's
// text: key ids, a session token, a SigV4 Authorization (qa-kiff's list).
func TestAnErrorCarriesNoAWSCredential(t *testing.T) {
	cause := errors.New(`POST "https://bedrock-runtime.eu-west-1.amazonaws.com/model/x/invoke": 403 Forbidden ` +
		`Authorization: AWS4-HMAC-SHA256 Credential=ASIAABCDEFGHIJKLMNOP/20261107/eu-west-1/bedrock/aws4_request, SignedHeaders=host;x-amz-date, Signature=0123456789abcdef ` +
		`X-Amz-Security-Token: IQoJb3JpZ2luX2VjEJr//secret+token== key AKIAABCDEFGHIJKLMNOP`)
	f := &fake{err: cause}
	_, err := (&Model{inner: f, Region: "eu-west-1"}).Turn(context.Background(), model.Request{Model: "claude-opus-5-5"}, nil)
	for _, secret := range []string{"ASIAABCDEFGHIJKLMNOP", "AKIAABCDEFGHIJKLMNOP", "IQoJb3JpZ2luX2VjEJr", "Signature=0123456789abcdef", "Credential="} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error still carries %q: %v", secret, err)
		}
	}
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "403 Forbidden") {
		t.Fatalf("the error lost its kind or its words: %v", err)
	}
	if Scrub(nil) != nil {
		t.Fatal("nil scrubbed to an error")
	}
	plain := errors.New("throttled")
	if Scrub(plain) != plain {
		t.Fatal("an error with nothing to scrub was rewrapped")
	}
}
