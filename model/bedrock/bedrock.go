// Package bedrock runs turns on Claude in Amazon Bedrock: bedrock-runtime's
// InvokeModel, through the EU cross-region inference profiles
// ("eu.anthropic.claude-sonnet-5-5"), signed with the caller's own AWS role.
// It is the backup model.Sourced falls back to, off unless configured.
package bedrock

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	sdkbedrock "github.com/anthropics/anthropic-sdk-go/bedrock"
	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/kiff/agents/model"
	"github.com/kiff/agents/model/anthropic"
)

// GeoFactor is what a turn through an EU profile costs against Anthropic's
// list price: AWS prices geographic profiles at the standard rate, about 10%
// over its global ones, which are at Anthropic's list.
const GeoFactor = 1.1

// Profiles are the models the backup serves, by the id a definition names,
// as their EU inference profile. Routed from an EU region, an "eu." profile
// stays in the EU (Frankfurt, Stockholm, Milan, Spain, Ireland, Paris).
var Profiles = map[string]string{
	"claude-sonnet-5-5": "eu.anthropic.claude-sonnet-5-5",
	"claude-opus-5-5":   "eu.anthropic.claude-opus-5-5",
}

// Windows are their context windows: Bedrock serves no Models API.
var Windows = map[string]int64{
	"claude-sonnet-5-5": 1_000_000,
	"claude-opus-5-5":   1_000_000,
}

// Model runs a turn through Bedrock. Region is the caller's, the one its
// endpoint is in; the profile picks where in the EU the turn runs, which
// Bedrock's reply doesn't say.
type Model struct {
	inner  model.Model
	Region string
}

// New is the backup in cfg's region, signed with cfg's credentials (the
// function's own role, from the default chain).
func New(cfg aws.Config) *Model {
	return &Model{inner: anthropic.New(sdkbedrock.WithConfig(cfg)), Region: cfg.Region}
}

// ErrNoProfile is a model the backup doesn't serve.
var ErrNoProfile = errors.New("bedrock: the backup serves only Claude Sonnet 5.5 and Opus 5.5")

func (m *Model) Turn(ctx context.Context, req model.Request, onText func(string)) (model.Reply, error) {
	profile, ok := Profiles[req.Model]
	if !ok {
		return model.Reply{}, fmt.Errorf("%w, not %q", ErrNoProfile, req.Model)
	}
	req.Model = profile
	req.Fast = false // Bedrock has no fast output
	req.Web = nil    // nor Anthropic's web search and fetch
	r, err := m.inner.Turn(ctx, req, onText)
	r.Source, r.Region, r.PriceFactor = model.SourceBedrock, m.Region, GeoFactor
	return r, Scrub(err)
}

// Window is the model's context window, from Windows.
func (m *Model) Window(_ context.Context, id string) (int64, error) {
	if w, ok := Windows[id]; ok {
		return w, nil
	}
	return 0, fmt.Errorf("%w, not %q", ErrNoProfile, id)
}

// credentials are the AWS secrets a signed request carries, as they could
// show in an error's text: a SigV4 Authorization header (its credential,
// signed headers and signature), the same in a presigned URL's query, a
// session token, and key ids.
var credentials = []*regexp.Regexp{
	regexp.MustCompile(`(?i)AWS4-HMAC-SHA256\s+Credential=[^\s",]+(?:,\s*SignedHeaders=[^\s",]+)?(?:,\s*Signature=[0-9a-f]+)?`),
	regexp.MustCompile(`(?i)X-Amz-(?:Credential|Signature|Security-Token)=[^&\s"]+`),
	regexp.MustCompile(`(?i)x-amz-security-token:\s*[^\s"]+`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`),
}

// Scrub is err with any AWS credential in its text taken out, so a log line
// or a run record never carries one. Nil stays nil, and an error with none
// is returned as it is. Log the scrubbed error only: it unwraps to the
// original, whose text still has them, so what errors.As finds through it
// (the SDK's API error) is for reading its status, never for logging.
func Scrub(err error) error {
	if err == nil {
		return nil
	}
	clean := err.Error()
	for _, re := range credentials {
		clean = re.ReplaceAllString(clean, "[redacted]")
	}
	if clean == err.Error() {
		return err
	}
	return scrubbed{msg: clean, err: err}
}

// scrubbed keeps the error's kind (errors.Is, errors.As) with clean words.
type scrubbed struct {
	msg string
	err error
}

func (e scrubbed) Error() string { return e.msg }
func (e scrubbed) Unwrap() error { return e.err }
