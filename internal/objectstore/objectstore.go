package objectstore

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Options struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	UseSSL    bool
}

func (o Options) Configured() bool {
	return o.Endpoint != "" && o.Bucket != ""
}

type Client struct {
	objects *minio.Client
	bucket  string
}

// A nil client is the unconfigured case and every caller must handle it: archives are left
// attached rather than dropped, and results are stored inline regardless of size.
func New(ctx context.Context, opts Options) (*Client, error) {
	if !opts.Configured() {
		return nil, nil
	}

	objects, err := minio.New(opts.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(opts.AccessKey, opts.SecretKey, ""),
		Secure: opts.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("connect object storage: %w", err)
	}

	exists, err := objects.BucketExists(ctx, opts.Bucket)
	if err != nil {
		return nil, fmt.Errorf("check bucket %s: %w", opts.Bucket, err)
	}
	if !exists {
		if err := objects.MakeBucket(ctx, opts.Bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("create bucket %s: %w", opts.Bucket, err)
		}
	}
	return &Client{objects: objects, bucket: opts.Bucket}, nil
}

func (c *Client) Put(ctx context.Context, name string, body []byte, contentType string) error {
	_, err := c.objects.PutObject(ctx, c.bucket, name, bytes.NewReader(body), int64(len(body)),
		minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("upload %s: %w", name, err)
	}
	return nil
}

func (c *Client) Get(ctx context.Context, name string) ([]byte, error) {
	object, err := c.objects.GetObject(ctx, c.bucket, name, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", name, err)
	}
	defer object.Close()

	body, err := io.ReadAll(object)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	return body, nil
}

// Deleting an object that is already gone is not an error: the sweep may have crashed
// between removing the row and removing the object, and the retry must still make progress.
func (c *Client) Delete(ctx context.Context, names ...string) error {
	for _, name := range names {
		err := c.objects.RemoveObject(ctx, c.bucket, name, minio.RemoveObjectOptions{})
		if err != nil && minio.ToErrorResponse(err).Code != "NoSuchKey" {
			return fmt.Errorf("delete %s: %w", name, err)
		}
	}
	return nil
}
