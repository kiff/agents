package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func client() *s3.Client {
	return s3.New(s3.Options{Region: "eu-west-1", Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", "")})
}

// An upload URL is signed for its exact size, type and checksum: S3 refuses
// other bytes, and the app must send those headers as given.
func TestAnUploadIsPinned(t *testing.T) {
	b := New(client(), "attachments")
	data := []byte("hello")
	sum := sha256.Sum256(data)
	up, err := b.PresignPut(context.Background(), "t1/a1/att_1", "image/png", 5, hex.EncodeToString(sum[:]), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(up.URL)
	if err != nil || up.Method != "PUT" || !strings.HasSuffix(u.Path, "/t1/a1/att_1") || u.Query().Get("X-Amz-Expires") != "300" {
		t.Fatalf("upload = %+v", up)
	}
	signed := strings.ToLower(u.Query().Get("X-Amz-SignedHeaders"))
	for _, h := range []string{"content-length", "content-type", "x-amz-checksum-sha256"} {
		if !strings.Contains(signed, h) {
			t.Errorf("%s isn't signed: %s", h, signed)
		}
	}
	if up.Headers["Content-Type"] != "image/png" || up.Headers["Content-Length"] != "5" {
		t.Errorf("headers = %v", up.Headers)
	}
}

func TestADownloadIsNamed(t *testing.T) {
	b := New(client(), "attachments")
	got, err := b.PresignGet(context.Background(), "t1/a1/att_1", "factura marzo.pdf", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(got)
	if d := u.Query().Get("response-content-disposition"); d != "attachment; filename*=UTF-8''factura%20marzo.pdf" || u.Query().Get("X-Amz-Expires") != "60" {
		t.Fatalf("url = %s", got)
	}
}

type fake struct {
	API
	objects map[string][]byte
	deletes int
}

func (f *fake) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	b := f.objects[*in.Key]
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(b)), ContentLength: aws.Int64(int64(len(b)))}, nil
}

func (f *fake) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	f.deletes++
	for _, o := range in.Delete.Objects {
		delete(f.objects, *o.Key)
	}
	return &s3.DeleteObjectsOutput{}, nil
}

func (f *fake) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	var out []types.Object
	for k := range f.objects {
		if strings.HasPrefix(k, *in.Prefix) {
			out = append(out, types.Object{Key: aws.String(k)})
		}
	}
	return &s3.ListObjectsV2Output{Contents: out, IsTruncated: aws.Bool(false)}, nil
}

func TestGetRefusesMore(t *testing.T) {
	f := &fake{objects: map[string][]byte{"k": []byte("12345")}}
	b := &Blobs{api: f, bucket: "x"}
	if _, err := b.Get(context.Background(), "k", 4); err == nil {
		t.Fatal("read 5 bytes with a limit of 4")
	}
	if got, err := b.Get(context.Background(), "k", 5); err != nil || string(got) != "12345" {
		t.Fatalf("Get = %q, %v", got, err)
	}
}

func TestDeletePrefix(t *testing.T) {
	f := &fake{objects: map[string][]byte{"t1/a1/x": nil, "t1/a2/y": nil, "t2/a1/z": nil}}
	b := &Blobs{api: f, bucket: "x"}
	if err := b.DeletePrefix(context.Background(), "t1/"); err != nil {
		t.Fatal(err)
	}
	if len(f.objects) != 1 || f.objects["t2/a1/z"] != nil {
		if _, ok := f.objects["t2/a1/z"]; !ok || len(f.objects) != 1 {
			t.Fatalf("left %v", f.objects)
		}
	}
	if err := b.DeletePrefix(context.Background(), ""); err == nil {
		t.Fatal("deleted the whole bucket")
	}
}
