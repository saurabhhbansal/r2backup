package remote

import (
	"context"
	"fmt"
	"sort"
	"strings"
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

// ListPrefixes returns the immediate child "folders" under prefix -- the
// CommonPrefixes an S3 LIST reports when it is given a delimiter -- with the
// prefix and the trailing "/" trimmed off each one.
//
// It exists so a computer that has never backed anything up can find out what
// is in the bucket. Everything else here reads objects because it already
// knows which ones it wants; this is the one question ("what is there?") that
// cannot be answered from local state, and answering it by listing every
// object under "machines/" would download a key for all 60,000 files to learn
// a handful of names. A delimited list stops at the first "/" and pays for
// pages of names instead.
func (c *Client) ListPrefixes(ctx context.Context, prefix string) ([]string, error) {
	var names []string
	paginator := s3.NewListObjectsV2Paginator(c.s3, &s3.ListObjectsV2Input{
		Bucket:    aws.String(c.bucket),
		Prefix:    aws.String(prefix),
		Delimiter: aws.String("/"),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("remote: list prefixes %q: %w", prefix, err)
		}
		for _, cp := range page.CommonPrefixes {
			name := strings.TrimSuffix(strings.TrimPrefix(aws.ToString(cp.Prefix), prefix), "/")
			if name != "" {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names, nil
}
