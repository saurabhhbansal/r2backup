package remote

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Object is a streamed object plus the metadata a restore needs.
type Object struct {
	// Body is the object's content. The caller must Close it.
	Body        io.ReadCloser
	Metadata    Metadata
	Size        int64
	ContentType string
	ETag        string
}

// Get streams Key's content and metadata. The caller must Close Object.Body.
func (c *Client) Get(ctx context.Context, key string) (*Object, error) {
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("remote: get %q: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("remote: get %q: %w", key, err)
	}

	meta, err := MetadataFromS3(out.Metadata)
	if err != nil {
		out.Body.Close()
		return nil, fmt.Errorf("remote: get %q: %w", key, err)
	}

	obj := &Object{
		Body:     out.Body,
		Metadata: meta,
	}
	if out.ContentLength != nil {
		obj.Size = *out.ContentLength
	}
	if out.ContentType != nil {
		obj.ContentType = *out.ContentType
	}
	if out.ETag != nil {
		obj.ETag = *out.ETag
	}
	return obj, nil
}

// HeadResult is what Head learns about an object without fetching its body.
type HeadResult struct {
	Size int64
	ETag string
	// LastModified is S3's own record of when the object was written --
	// distinct from Metadata.ModTime, which is the file's modification
	// time on the machine that backed it up.
	LastModified time.Time
	Metadata     Metadata
}

// Head fetches an object's metadata and size without its body. It returns
// an error wrapping ErrNotFound if key does not exist.
func (c *Client) Head(ctx context.Context, key string) (*HeadResult, error) {
	out, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("remote: head %q: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("remote: head %q: %w", key, err)
	}

	meta, err := MetadataFromS3(out.Metadata)
	if err != nil {
		return nil, fmt.Errorf("remote: head %q: %w", key, err)
	}

	res := &HeadResult{Metadata: meta}
	if out.ContentLength != nil {
		res.Size = *out.ContentLength
	}
	if out.ETag != nil {
		res.ETag = *out.ETag
	}
	if out.LastModified != nil {
		res.LastModified = *out.LastModified
	}
	return res, nil
}
