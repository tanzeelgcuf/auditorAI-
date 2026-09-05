package storage

// MinIO/S3 file storage (doc 12 §1). Presigned-URL upload flow: the client PUTs
// file bytes directly to storage, then confirms — the API verifies bytes landed
// before triggering ingestion. MinIO is S3-compatible so the same client serves
// both.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type Client struct {
	s3        *s3.Client
	bucket    string
	presign   *s3.PresignClient
}

// New builds an S3-compatible client from env. Endpoint may be MinIO (local) or AWS.
func New() (*Client, error) {
	endpoint := envOr("S3_ENDPOINT", "http://minio:9000")
	bucket := envOr("S3_BUCKET", "ai-auditor")
	accessKey := envOr("S3_ACCESS_KEY", "minioadmin")
	secretKey := envOr("S3_SECRET_KEY", "minioadmin")
	region := envOr("AWS_REGION", "us-east-1")

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, err
	}

	s3c := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true // required for MinIO
	})

	return &Client{s3: s3c, bucket: bucket, presign: s3.NewPresignClient(s3c)}, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// PresignUpload returns a PUT URL for a storage key, valid for ttl.
func (c *Client) PresignUpload(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := c.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}, func(o *s3.PresignOptions) { o.Expires = ttl })
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

// ObjectExists HEADs an object to confirm bytes actually landed.
//
// A GENUINE absence returns (false, nil). Anything else — connection refused,
// DNS failure, expired or wrong credentials, missing bucket — returns an error,
// because the two cases need different answers to the client and the previous
// version could not tell them apart: it returned `false, nil` for every error,
// so HandleConfirmUpload told a user whose upload had succeeded that their
// "upload did not complete" (400) whenever MinIO was unreachable. That sends the
// firm to debug their own network while the actual fault is server-side.
func (c *Client) ObjectExists(ctx context.Context, key string) (bool, error) {
	_, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true, nil
	}
	// HeadObject reports absence as types.NotFound; GetObject-style callers see
	// NoSuchKey. Both are matched so the semantics do not depend on which the
	// S3-compatible implementation chooses to send (MinIO and AWS differ).
	var notFound *s3types.NotFound
	var noSuchKey *s3types.NoSuchKey
	if errors.As(err, &notFound) || errors.As(err, &noSuchKey) {
		return false, nil
	}
	return false, fmt.Errorf("head %s: %w", key, err)
}

// StreamObject downloads an object's bytes (used by ingestion).
func (c *Client) StreamObject(ctx context.Context, key string) ([]byte, error) {
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	defer out.Body.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(out.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// PutObject writes bytes. This is the production path for the direct multipart
// upload handler (internal/documents.HandleUpload), not just tests/seed-demo:
// the presigned flow has the client PUT straight to storage, but the multipart
// flow reads the bytes into the API process and they must be written here or the
// source_documents row points at an object that does not exist.
func (c *Client) PutObject(ctx context.Context, key string, data []byte) error {
	_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return err
	}
	return nil
}

// EnsureBucketExists creates the bucket if absent (idempotent, best-effort).
func (c *Client) EnsureBucketExists(ctx context.Context) error {
	_, err := c.s3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(c.bucket)})
	if err != nil {
		slog.Debug("bucket may already exist", "error", err)
	}
	return nil
}
