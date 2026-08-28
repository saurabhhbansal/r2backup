package remote

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Copy duplicates srcKey to dstKey with a single server-side CopyObject
// call: the bytes travel within R2 and never pass through this process.
// This is what moving an object to trash relies on -- a
// download-then-reupload would pull every trashed byte across the network
// (and back) for no reason, for every file ever deleted.
func (c *Client) Copy(ctx context.Context, srcKey, dstKey string) error {
	_, err := c.s3.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(dstKey),
		// CopySource is "bucket/key" and must itself be URL-encoded per
		// S3's API -- a raw key containing a space, "#", or "?" would
		// otherwise either break the header or get parsed as a version-id
		// query string, silently copying the wrong object.
		CopySource: aws.String(c.bucket + "/" + encodeCopySource(srcKey)),
		// Copy the source's own metadata rather than requiring the caller
		// to resupply it: it is the same object, just at a new key.
		MetadataDirective: types.MetadataDirectiveCopy,
	})
	if err != nil {
		return fmt.Errorf("remote: copy %q to %q: %w", srcKey, dstKey, err)
	}
	return nil
}

// encodeCopySource percent-encodes a key for use in CopySource. url.PathEscape
// on the whole key would also escape "/", which a nested key needs to keep
// literal, so each path segment is escaped on its own.
func encodeCopySource(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
