package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Links let bytes move between two stores without passing through this
// computer: the source hands out a temporary download link, the destination
// temporary upload links, and something else -- Ultra fast mode's Worker --
// carries the bytes from one to the other.

// ErrNoLink means a store cannot hand out a link for this object -- it is
// packed inside a bundle, say, or the store has no such thing. The caller
// copies it the ordinary way instead.
var ErrNoLink = errors.New("remote: no link for this object")

// DownloadLink is a temporary URL an object can be fetched from with no
// other credentials, Range requests included.
type DownloadLink struct {
	URL         string
	Size        int64
	Metadata    Metadata
	ContentType string
	// Expires is when URL stops working.
	Expires time.Time
}

// LinkReader is a Store that can hand out download links.
type LinkReader interface {
	DownloadLink(ctx context.Context, key string) (*DownloadLink, error)
}

// LinkUpload describes an object to upload by link.
type LinkUpload struct {
	Key         string
	Size        int64
	Metadata    Metadata
	ContentType string
	// PartSize is how big each part is for an object sent in parts; zero
	// is DefaultLinkPartSize. An object no bigger than one part goes up in
	// one request.
	PartSize int64
}

// DefaultLinkPartSize is the part size for uploads by link. It is bigger than
// the 16 MiB a direct upload uses: an upload by link is never resumed from
// this computer, and each part is one request to the relay.
const DefaultLinkPartSize = 64 << 20

// UploadLink is one request of an upload by link: PUT Length bytes, starting
// at Offset of the object, to URL with Header.
type UploadLink struct {
	URL    string
	Header http.Header
	Offset int64
	Length int64
}

// UploadPlan is an upload by link in progress. Parts are sent in any order,
// then the plan is finished with the ETag each part's upload answered.
type UploadPlan struct {
	Key string
	// UploadID is set for an upload in parts, and empty for a single PUT.
	UploadID string
	Size     int64
	Parts    []UploadLink
}

// LinkWriter is a Store that can be written to by link.
type LinkWriter interface {
	UploadLinks(ctx context.Context, in LinkUpload) (*UploadPlan, error)
	// FinishUpload completes plan once every part is sent. etags are in
	// part order.
	FinishUpload(ctx context.Context, plan *UploadPlan, etags []string) error
	// AbandonUpload gives up on a plan whose parts did not all go.
	AbandonUpload(ctx context.Context, plan *UploadPlan) error
}

// linkTTL is how long an upload link works. Generous, because the parts of
// one big file are sent a few at a time.
const linkTTL = 6 * time.Hour

// signedHeader keeps the headers a presigned request was signed with that
// the sender must repeat. Host and Content-Length are the sender's own.
func signedHeader(h http.Header) http.Header {
	out := http.Header{}
	for k, v := range h {
		switch strings.ToLower(k) {
		case "host", "content-length":
			continue
		}
		out[k] = v
	}
	return out
}

// UploadLinks presigns the requests that upload in.Key.
func (c *Client) UploadLinks(ctx context.Context, in LinkUpload) (*UploadPlan, error) {
	if in.Key == "" || in.Size < 1 {
		return nil, fmt.Errorf("remote: upload links: need a key and a size")
	}
	md := in.Metadata
	md.Size = in.Size
	meta := md.ToS3()
	partSize := in.PartSize
	if partSize <= 0 {
		partSize = DefaultLinkPartSize
	}
	partSize = partSizeFor(in.Size, partSize)
	pc := s3.NewPresignClient(c.s3, s3.WithPresignExpires(linkTTL))
	plan := &UploadPlan{Key: in.Key, Size: in.Size}

	if in.Size <= partSize {
		put := &s3.PutObjectInput{
			Bucket: aws.String(c.bucket), Key: aws.String(in.Key),
			ContentLength: aws.Int64(in.Size), Metadata: meta,
		}
		if in.ContentType != "" {
			put.ContentType = aws.String(in.ContentType)
		}
		req, err := pc.PresignPutObject(ctx, put)
		if err != nil {
			return nil, fmt.Errorf("remote: sign upload of %q: %w", in.Key, err)
		}
		plan.Parts = []UploadLink{{URL: req.URL, Header: signedHeader(req.SignedHeader), Length: in.Size}}
		return plan, nil
	}

	create := &s3.CreateMultipartUploadInput{Bucket: aws.String(c.bucket), Key: aws.String(in.Key), Metadata: meta}
	if in.ContentType != "" {
		create.ContentType = aws.String(in.ContentType)
	}
	out, err := c.s3.CreateMultipartUpload(ctx, create)
	if err != nil {
		return nil, fmt.Errorf("remote: start upload of %q: %w", in.Key, err)
	}
	plan.UploadID = aws.ToString(out.UploadId)
	for off, n := int64(0), int32(1); off < in.Size; off, n = off+partSize, n+1 {
		length := min64(partSize, in.Size-off)
		req, err := pc.PresignUploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(c.bucket), Key: aws.String(in.Key), UploadId: out.UploadId,
			PartNumber: aws.Int32(n), ContentLength: aws.Int64(length),
		})
		if err != nil {
			_ = c.AbandonUpload(context.WithoutCancel(ctx), plan)
			return nil, fmt.Errorf("remote: sign part %d of %q: %w", n, in.Key, err)
		}
		plan.Parts = append(plan.Parts, UploadLink{URL: req.URL, Header: signedHeader(req.SignedHeader), Offset: off, Length: length})
	}
	return plan, nil
}

// FinishUpload completes an upload in parts. A single PUT is already done.
func (c *Client) FinishUpload(ctx context.Context, plan *UploadPlan, etags []string) error {
	if plan.UploadID == "" {
		return nil
	}
	if len(etags) != len(plan.Parts) {
		return fmt.Errorf("remote: finish %q: %d parts sent of %d", plan.Key, len(etags), len(plan.Parts))
	}
	parts := make([]types.CompletedPart, len(etags))
	for i, e := range etags {
		if e == "" {
			return fmt.Errorf("remote: finish %q: part %d has no ETag", plan.Key, i+1)
		}
		parts[i] = types.CompletedPart{ETag: aws.String(e), PartNumber: aws.Int32(int32(i + 1))}
	}
	_, err := c.s3.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(c.bucket), Key: aws.String(plan.Key), UploadId: aws.String(plan.UploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return fmt.Errorf("remote: finish upload of %q: %w", plan.Key, err)
	}
	return nil
}

// AbandonUpload aborts an upload in parts, so its parts are not kept.
func (c *Client) AbandonUpload(ctx context.Context, plan *UploadPlan) error {
	if plan.UploadID == "" {
		return nil
	}
	return c.AbortUpload(ctx, plan.Key, plan.UploadID)
}

var _ LinkWriter = (*Client)(nil)

// A Sub view passes links through to the store under it, keys prefixed.

func (v *sub) DownloadLink(ctx context.Context, key string) (*DownloadLink, error) {
	lr, ok := v.s.(LinkReader)
	if !ok {
		return nil, ErrNoLink
	}
	return lr.DownloadLink(ctx, v.k(key))
}

func (v *sub) UploadLinks(ctx context.Context, in LinkUpload) (*UploadPlan, error) {
	lw, ok := v.s.(LinkWriter)
	if !ok {
		return nil, ErrNoLink
	}
	in.Key = v.k(in.Key)
	return lw.UploadLinks(ctx, in)
}

func (v *sub) FinishUpload(ctx context.Context, plan *UploadPlan, etags []string) error {
	lw, ok := v.s.(LinkWriter)
	if !ok {
		return ErrNoLink
	}
	return lw.FinishUpload(ctx, plan, etags)
}

func (v *sub) AbandonUpload(ctx context.Context, plan *UploadPlan) error {
	lw, ok := v.s.(LinkWriter)
	if !ok {
		return ErrNoLink
	}
	return lw.AbandonUpload(ctx, plan)
}
