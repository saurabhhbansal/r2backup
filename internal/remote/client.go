// Package remote wraps the S3 API for talking to Cloudflare R2 (and, in
// tests, an S3-compatible MinIO server standing in for it).
//
// The transfer loop is owned here rather than left to a generic sync tool
// because that is the only way an honest progress ETA is possible: every
// byte written to the wire passes through code this package controls, so
// the caller's progress tracker is fed real numbers instead of a guess.
package remote

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// region is the only region string R2 accepts. Unlike S3, an R2 bucket is
// not region-scoped -- "auto" is a literal the API expects, not a default
// standing in for an unset value.
const region = "auto"

const (
	// multipartThreshold is the size above which Put switches from a
	// single PutObject call to a multipart upload.
	multipartThreshold = 64 << 20 // 64 MiB

	// defaultPartSize is used for multipart uploads as long as it keeps the
	// part count under maxParts; see partSizeFor.
	defaultPartSize = 16 << 20 // 16 MiB

	// maxParts is R2's hard cap on parts in a single multipart upload.
	maxParts = 10000

	// uploadConcurrency is how many parts are in flight at once for a
	// multipart upload.
	uploadConcurrency = 4
)

// Config configures a Client.
type Config struct {
	// AccountID builds the default R2 endpoint,
	// https://<AccountID>.r2.cloudflarestorage.com.
	AccountID string
	// Endpoint, if set, is used verbatim instead of the derived R2
	// endpoint. This is how tests point the client at a local MinIO
	// server instead of Cloudflare.
	Endpoint string
	// Bucket is the bucket every operation targets.
	Bucket string

	AccessKeyID     string
	SecretAccessKey string

	// HTTPClient, if set, replaces the SDK's default HTTP client.
	HTTPClient *http.Client
}

func (c Config) endpoint() string {
	if c.Endpoint != "" {
		return c.Endpoint
	}
	if c.AccountID == "" {
		return ""
	}
	return fmt.Sprintf("https://%s.r2.cloudflarestorage.com", c.AccountID)
}

// Client is an S3 client wrapper configured for R2: region "auto",
// path-style addressing forced, and a retryer tuned for R2's throttling
// behavior.
type Client struct {
	s3       *s3.Client
	uploader *manager.Uploader
	bucket   string
}

// New builds a Client from cfg.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("remote: config: bucket is required")
	}
	endpoint := cfg.endpoint()
	if endpoint == "" {
		return nil, fmt.Errorf("remote: config: account id or endpoint is required")
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID, cfg.SecretAccessKey, "",
		)),
		// The SDK's newer default is to always attach a request checksum
		// when the operation supports one, using a chunked-trailer
		// encoding. R2 (and plenty of other S3-compatible servers) do not
		// all speak that dialect identically, so checksums are computed
		// only when a caller explicitly asks for one, not on every request
		// by default.
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		// The standard retryer already retries on 503/SlowDown -- S3's
		// throttling signal -- and on other transient failures; this only
		// tunes how many attempts and how long it is willing to back off.
		awsconfig.WithRetryer(func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.MaxAttempts = 6
				o.MaxBackoff = 30 * time.Second
			})
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("remote: load config: %w", err)
	}
	if cfg.HTTPClient != nil {
		awsCfg.HTTPClient = cfg.HTTPClient
	}

	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		// Sign requests with UNSIGNED-PAYLOAD instead of a SHA256 of the body.
		//
		// Two reasons, one correctness and one performance. Computing the hash
		// requires the SDK to read the body to the end and then seek back to
		// the start -- so a body that is not seekable, which is exactly what a
		// progress-counting wrapper around a file produces, fails outright with
		// "request stream is not seekable". And even when it succeeds it means
		// reading every file off disk twice: once to hash, once to send. Over a
		// 60,000-file backup that is double the disk I/O for no benefit, since
		// the connection is already TLS-protected and S3 accepts unsigned
		// payloads over HTTPS.
		o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
		// R2 does not return the checksums the SDK would like to validate, so
		// it logs a warning on every single object. Restoring 60,000 files
		// would print 60,000 lines of it over the user's progress bar, about
		// a condition that is expected and harmless here.
		o.DisableLogOutputChecksumValidationSkipped = true
		o.BaseEndpoint = aws.String(endpoint)
		// R2 and the MinIO server the tests run against only serve
		// path-style requests (bucket-in-path). Virtual-hosted-style would
		// put the bucket name in the hostname, which neither speaks, and
		// would also require per-bucket DNS/TLS that a self-hosted test
		// server does not have.
		o.UsePathStyle = true
		o.Region = region
	})

	uploader := manager.NewUploader(s3Client, func(u *manager.Uploader) {
		u.PartSize = defaultPartSize
		u.Concurrency = uploadConcurrency
		u.LeavePartsOnError = false
	})

	return &Client{s3: s3Client, uploader: uploader, bucket: cfg.Bucket}, nil
}

// Bucket returns the bucket this client is configured for.
func (c *Client) Bucket() string {
	return c.bucket
}
