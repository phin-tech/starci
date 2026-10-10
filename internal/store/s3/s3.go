// Package s3 is a result store in an S3-compatible bucket, for sharing
// results between machines. Cloudflare R2 is the one it has been run against.
package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/step"
)

// Store keeps one small JSON object per result.
type Store struct {
	client *minio.Client
	bucket string
	prefix string
}

// Open returns the store described by location, written as
//
//	s3://bucket/prefix?endpoint=host[&region=name]
//
// endpoint is the S3 API host, for R2 "<account>.r2.cloudflarestorage.com".
// Credentials come from the usual places: AWS_ACCESS_KEY_ID and
// AWS_SECRET_ACCESS_KEY, or a shared credentials file named by
// AWS_SHARED_CREDENTIALS_FILE.
func Open(location string) (*Store, error) {
	u, err := url.Parse(location)
	if err != nil || u.Scheme != "s3" || u.Host == "" {
		return nil, fmt.Errorf("a shared store is written s3://bucket/prefix?endpoint=host, got %q", location)
	}
	endpoint := u.Query().Get("endpoint")
	if endpoint == "" {
		return nil, fmt.Errorf("shared store %q needs ?endpoint=host", location)
	}
	region := u.Query().Get("region")
	if region == "" {
		region = "auto"
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds: credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{},
			&credentials.FileAWSCredentials{},
		}),
		Secure: u.Query().Get("insecure") == "",
		Region: region,
	})
	if err != nil {
		return nil, err
	}
	return &Store{client: client, bucket: u.Host, prefix: strings.Trim(u.Path, "/")}, nil
}

func (s *Store) object(key string) string {
	name := key[:2] + "/" + key + ".json"
	if s.prefix != "" {
		name = s.prefix + "/" + name
	}
	return name
}

// Requests are short and bounded: a slow store should cost a step being run
// again, not a run that hangs.
const timeout = 15 * time.Second

func (s *Store) Get(ctx context.Context, key string) (plan.Result, bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	obj, err := s.client.GetObject(ctx, s.bucket, s.object(key), minio.GetObjectOptions{})
	if err != nil {
		return plan.Result{}, false
	}
	defer obj.Close()
	data, err := io.ReadAll(io.LimitReader(obj, 4<<20))
	if err != nil {
		return plan.Result{}, false
	}
	var r plan.Result
	if json.Unmarshal(data, &r) != nil || r.Status != "passed" {
		return plan.Result{}, false
	}
	return r, true
}

func (s *Store) Put(ctx context.Context, key string, r plan.Result) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err = s.client.PutObject(ctx, s.bucket, s.object(key), bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/json"})
	if err != nil {
		return fmt.Errorf("shared store: %w", err)
	}
	return nil
}

func (s *Store) blob(digest string) string {
	name := "outputs/" + digest[:2] + "/" + digest + ".tar"
	if s.prefix != "" {
		name = s.prefix + "/" + name
	}
	return name
}

// An output can be large, so moving one is given far longer than a result.
const blobTimeout = 10 * time.Minute

func (s *Store) HasBlob(ctx context.Context, digest string) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := s.client.StatObject(ctx, s.bucket, s.blob(digest), minio.StatObjectOptions{})
	return err == nil
}

func (s *Store) GetBlob(ctx context.Context, digest, dest string) error {
	ctx, cancel := context.WithTimeout(ctx, blobTimeout)
	defer cancel()
	err := s.client.FGetObject(ctx, s.bucket, s.blob(digest), dest, minio.GetObjectOptions{})
	if minio.ToErrorResponse(err).Code == "NoSuchKey" {
		return step.ErrNoBlob
	}
	if err != nil {
		return fmt.Errorf("shared store: %w", err)
	}
	return nil
}

func (s *Store) PutBlob(ctx context.Context, digest, src string) error {
	if s.HasBlob(ctx, digest) {
		return nil // the same content, already there
	}
	ctx, cancel := context.WithTimeout(ctx, blobTimeout)
	defer cancel()
	_, err := s.client.FPutObject(ctx, s.bucket, s.blob(digest), src, minio.PutObjectOptions{ContentType: "application/x-tar"})
	if err != nil {
		return fmt.Errorf("shared store: %w", err)
	}
	return nil
}

// Check confirms the store can be read and written, so that a mistake in its
// address or credentials is found when a worker starts and not by every step
// quietly running again.
func (s *Store) Check(ctx context.Context) error {
	probe := strings.Repeat("0", 64)
	if err := s.Put(ctx, probe, plan.Result{Name: "probe", Status: "probe"}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := s.client.StatObject(ctx, s.bucket, s.object(probe), minio.StatObjectOptions{}); err != nil {
		return fmt.Errorf("shared store: wrote a probe but could not read it back: %w", err)
	}
	return nil
}
