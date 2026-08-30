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
	"errors"
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
	"github.com/aws/smithy-go"
)

// region is the only region string R2 accepts. Unlike S3, an R2 bucket is
// not region-scoped -- "auto" is a literal the API expects, not a default
// standing in for an unset value.
const region = "auto"

const (
	// defaultPartSize is used for multipart uploads as long as it keeps the
	// part count under maxParts; see partSizeFor.
	defaultPartSize = 16 << 20 // 16 MiB

	// multipartThreshold is the size above which Put switches from a single
	// PutObject call to a multipart upload.
	//
	// It is deliberately the same as defaultPartSize, which gives the
	// product one property worth stating plainly: an interruption can never
	// cost more than 16MiB of re-uploading, whatever the file's size. Above
	// the threshold a file goes up in parts, and parts already accepted
	// survive an interruption; at or below it, the whole file is 16MiB
	// anyway.
	//
	// It was 64MiB, which was the right choice while nothing could be
	// resumed -- a single PUT is one Class A operation and a multipart
	// upload is one per part plus two, and on a tool whose whole argument is
	// the operations budget that difference is the thing to protect. What
	// changed is that the parts are now worth something: they are what a
	// resumed upload starts from. A 40MiB file costs four operations instead
	// of one, and stops being sent from the beginning every time a
	// connection drops.
	multipartThreshold = defaultPartSize

	// maxParts is R2's hard cap on parts in a single multipart upload.
	maxParts = 10000

	// uploadConcurrency is how many parts are in flight at once for a
	// multipart upload.
	uploadConcurrency = 4

	// defaultMaxAttempts is how many times a failed request is tried before
	// it is reported. Six, with backoff, is roughly a minute of patience --
	// which is what a domestic connection dropping for a moment needs.
	defaultMaxAttempts = 6
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

	// Resume, if set, is where unfinished multipart uploads are recorded so
	// a later run can continue one. See ResumeStore.
	Resume ResumeStore

	// MultipartThreshold and PartSize override the defaults for how a large
	// object is cut up. Zero means the default.
	//
	// They are configuration rather than constants because the sizes that
	// matter here are not universal -- and, plainly, because the resume path
	// is worth testing without writing a 64MiB file for every case. S3's own
	// floor of 5MiB per part (bar the last) still applies; a smaller PartSize
	// is refused by the server, not by us.
	MultipartThreshold int64
	PartSize           int64

	// MaxRetryAttempts overrides how many times a failed request is tried.
	// Zero means the default. A test that means to fail does not want to
	// wait out six attempts and thirty seconds of backoff first.
	MaxRetryAttempts int
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

// retryIncompleteBody makes a short upload body worth another attempt.
//
// IncompleteBody is a 400, and the standard retryer treats every 4xx as the
// caller's fault and final. That is right for a malformed request and wrong
// for this one: it means the server was promised a Content-Length it did not
// receive, which happens when the *writer* stalls long enough for the server
// to stop waiting -- a slow disk, a starved CPU, a saturated link. Nothing
// about the request is wrong, and sending it again is exactly the answer.
//
// It is only safe to say that because the body is rewindable now; see
// progressSeeker in reader.go. Retrying an unrewindable body is how you turn
// one short send into six.
//
// A body that really is short -- a file truncated underneath the upload --
// fails all six attempts and is then reported with Put's own explanation of
// how far it got, so this costs a doomed upload some backoff and nothing
// else.
type retryIncompleteBody struct{}

func (retryIncompleteBody) IsErrorRetryable(err error) aws.Ternary {
	var api smithy.APIError
	if errors.As(err, &api) && api.ErrorCode() == "IncompleteBody" {
		return aws.TrueTernary
	}
	return aws.UnknownTernary
}

// Client is an S3 client wrapper configured for R2: region "auto",
// path-style addressing forced, and a retryer tuned for R2's throttling
// behavior.
type Client struct {
	s3       *s3.Client
	uploader *manager.Uploader
	bucket   string

	// resume, when set, is where in-progress multipart uploads are written
	// down so a later run can carry one on instead of starting it again.
	// nil keeps the old behaviour, which is what every test that does not
	// care about resuming gets.
	resume ResumeStore

	// multipartThresholdOverride and partSizeOverride are zero unless the
	// caller asked for something other than the package defaults.
	multipartThresholdOverride int64
	partSizeOverride           int64
}

// threshold is the size above which an object goes up in parts.
func (c *Client) threshold() int64 {
	if c.multipartThresholdOverride > 0 {
		return c.multipartThresholdOverride
	}
	return multipartThreshold
}

// partSize is the base part size, before partSizeFor grows it to stay under
// the part-count cap.
func (c *Client) partSize() int64 {
	if c.partSizeOverride > 0 {
		return c.partSizeOverride
	}
	return defaultPartSize
}

// UseResumeStore attaches the place unfinished multipart uploads are
// recorded. Without one, a large upload interrupted partway is started again
// from the beginning on the next run.
func (c *Client) UseResumeStore(s ResumeStore) { c.resume = s }

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
				o.MaxAttempts = defaultMaxAttempts
				if cfg.MaxRetryAttempts > 0 {
					o.MaxAttempts = cfg.MaxRetryAttempts
				}
				o.MaxBackoff = 30 * time.Second
				o.Retryables = append(o.Retryables, retryIncompleteBody{})
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
		// the start -- so a body that is not seekable fails outright with
		// "request stream is not seekable". And even when it succeeds it means
		// reading every file off disk twice: once to hash, once to send. Over a
		// 60,000-file backup that is double the disk I/O for no benefit, since
		// the connection is already TLS-protected and S3 accepts unsigned
		// payloads over HTTPS.
		//
		// This is not a licence to hand the SDK an unseekable body. Signing
		// is one of two things that rewind one; retrying is the other, and
		// nothing here can swap that away. See progressSeeker in reader.go,
		// which is what keeps the upload body seekable now.
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

	return &Client{
		s3: s3Client, uploader: uploader, bucket: cfg.Bucket, resume: cfg.Resume,
		multipartThresholdOverride: cfg.MultipartThreshold,
		partSizeOverride:           cfg.PartSize,
	}, nil
}

// Bucket returns the bucket this client is configured for.
func (c *Client) Bucket() string {
	return c.bucket
}
