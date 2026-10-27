// Package s3 keeps attachments' bytes in one private S3 bucket
// (attach.Blobs). The app uploads and downloads through short-lived
// presigned URLs, so a file never passes through the API; the runner reads
// a file's bytes for a turn.
//
// An upload URL is signed for one key, its exact size, its media type and
// its SHA-256, so S3 itself refuses any other bytes there. The bucket is
// private, encrypted, and its lifecycle rule removes what no sweep did
// (infra/aws.sh).
package s3

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/kiff/agents/attach"
)

// API is the part of the S3 client the store uses.
type API interface {
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObjects(context.Context, *s3.DeleteObjectsInput, ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// Blobs is attach.Blobs on Bucket.
type Blobs struct {
	api     API
	presign *s3.PresignClient
	bucket  string
	now     func() time.Time
}

// New returns the blob store on bucket.
func New(client *s3.Client, bucket string) *Blobs {
	return &Blobs{api: client, presign: s3.NewPresignClient(client), bucket: bucket, now: time.Now}
}

var _ attach.Blobs = (*Blobs)(nil)

// PresignPut signs a PUT of exactly size bytes of mediaType, with that
// SHA-256, at key. The app sends the headers it returns, unchanged.
func (b *Blobs) PresignPut(ctx context.Context, key, mediaType string, size int64, sha string, ttl time.Duration) (attach.Upload, error) {
	raw, err := hex.DecodeString(sha)
	if err != nil {
		return attach.Upload{}, fmt.Errorf("s3: sha256: %w", err)
	}
	req, err := b.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: &b.bucket, Key: &key, ContentType: &mediaType, ContentLength: &size,
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(raw)),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return attach.Upload{}, err
	}
	headers := map[string]string{}
	for name, vs := range req.SignedHeader {
		if strings.EqualFold(name, "host") || len(vs) == 0 {
			continue
		}
		headers[name] = vs[0]
	}
	return attach.Upload{URL: req.URL, Method: req.Method, Headers: headers, Expires: b.now().Add(ttl)}, nil
}

// PresignGet signs a GET of key, downloaded under name.
func (b *Blobs) PresignGet(ctx context.Context, key, name string, ttl time.Duration) (string, error) {
	disposition := "attachment; filename*=UTF-8''" + url.PathEscape(name)
	req, err := b.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &b.bucket, Key: &key,
		ResponseContentDisposition: &disposition}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

// Get reads key, refusing more than max bytes.
func (b *Blobs) Get(ctx context.Context, key string, max int64) ([]byte, error) {
	out, err := b.api.GetObject(ctx, &s3.GetObjectInput{Bucket: &b.bucket, Key: &key})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	if out.ContentLength != nil && *out.ContentLength > max {
		return nil, fmt.Errorf("s3: %s is %d bytes, over %d", key, *out.ContentLength, max)
	}
	data, err := io.ReadAll(io.LimitReader(out.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("s3: %s is over %d bytes", key, max)
	}
	return data, nil
}

// Put writes data at key.
func (b *Blobs) Put(ctx context.Context, key, mediaType string, data []byte) error {
	_, err := b.api.PutObject(ctx, &s3.PutObjectInput{Bucket: &b.bucket, Key: &key, ContentType: &mediaType,
		Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data)))})
	return err
}

// Delete removes keys, a thousand at a time.
func (b *Blobs) Delete(ctx context.Context, keys ...string) error {
	for len(keys) > 0 {
		n := min(len(keys), 1000)
		objs := make([]types.ObjectIdentifier, n)
		for i, k := range keys[:n] {
			objs[i] = types.ObjectIdentifier{Key: aws.String(k)}
		}
		out, err := b.api.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &b.bucket,
			Delete: &types.Delete{Objects: objs, Quiet: aws.Bool(true)}})
		if err != nil {
			return err
		}
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return fmt.Errorf("s3: deleting %s: %s", aws.ToString(e.Key), aws.ToString(e.Message))
		}
		keys = keys[n:]
	}
	return nil
}

// DeletePrefix removes every key under prefix. An empty prefix is refused:
// it would be the whole bucket.
func (b *Blobs) DeletePrefix(ctx context.Context, prefix string) error {
	if prefix == "" || prefix == "/" {
		return errors.New("s3: refusing to delete the whole bucket")
	}
	var token *string
	for {
		out, err := b.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &b.bucket, Prefix: &prefix, ContinuationToken: token})
		if err != nil {
			return err
		}
		keys := make([]string, 0, len(out.Contents))
		for _, o := range out.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
		if err := b.Delete(ctx, keys...); err != nil {
			return err
		}
		if !aws.ToBool(out.IsTruncated) {
			return nil
		}
		token = out.NextContinuationToken
	}
}
