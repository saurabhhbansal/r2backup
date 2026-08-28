package remote

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ListEntry is one object found under a prefix.
type ListEntry struct {
	Key     string
	Size    int64
	ETag    string
	ModTime time.Time
}

// List returns every object under prefix, following pagination until S3
// reports no more pages.
//
// A single ListObjectsV2 call returns at most 1,000 keys; a real backed-up
// dataset routinely has 60,000+ objects, so anything that stops at the
// first page silently drops most of the bucket.
func (c *Client) List(ctx context.Context, prefix string) ([]ListEntry, error) {
	var entries []ListEntry

	paginator := s3.NewListObjectsV2Paginator(c.s3, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("remote: list %q: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			e := ListEntry{Key: aws.ToString(obj.Key)}
			if obj.Size != nil {
				e.Size = *obj.Size
			}
			if obj.ETag != nil {
				e.ETag = *obj.ETag
			}
			if obj.LastModified != nil {
				e.ModTime = *obj.LastModified
			}
			entries = append(entries, e)
		}
	}
	return entries, nil
}
