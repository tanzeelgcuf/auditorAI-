package storage

// MinIO/S3 file storage (doc 12 §1). Presigned-URL upload flow: the client PUTs
// file bytes directly to storage, then confirms — the API verifies bytes landed
// before triggering ingestion. MinIO is S3-compatible so the same client serves
// both.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
func (c *Client) ObjectExists(ctx context.Context, key string) (bool, error) {
	_, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return false, nil // treat as absent; S3 errors here are 404/403
	}
	return true, nil
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

// PutObject writes bytes (used by tests / seed-demo).
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
