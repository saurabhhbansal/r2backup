package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Resumable uploads.
//
// A multipart upload is the only kind that can be resumed, because it is the
// only kind the server remembers between requests: parts already accepted
// stay on the server under an upload id until the upload is completed or
// aborted. Everything here exists to keep hold of that id.
//
// The SDK's own manager.Uploader cannot do this. It creates an upload id,
// uses it, and on any failure aborts it -- deliberately, because it has
// nowhere to write the id down. So a nine-gigabyte file interrupted at eight
// gigabytes started again from zero, every time, and on a connection bad
// enough to interrupt it once it would very likely never finish at all. That
// is the failure this file removes.
//
// What is stored is the upload id, the part size, the parts already accepted,
// and the size and modification time of the file they came from. The last two
// are what make resuming safe: parts already on the server describe the bytes
// the file held when they were cut, and if the file has changed since, they
// describe nothing anyone wants. Then the old upload is abandoned rather than
// continued.

// PartRecord is one part the server has accepted.
type PartRecord struct {
	Number int32  `json:"n"`
	ETag   string `json:"etag"`
	Size   int64  `json:"size"`
}

// Upload is a multipart upload that has been started and not finished.
type Upload struct {
	Key      string `json:"key"`
	UploadID string `json:"upload_id"`
	PartSize int64  `json:"part_size"`

	// Size and ModTime describe the file as it was when this upload began.
	// A file that no longer matches them cannot be resumed into: the parts
	// already sent are from a version of it nobody is asking for.
	Size    int64 `json:"size"`
	ModTime int64 `json:"mod_time"` // Unix nanoseconds.

	Parts     []PartRecord `json:"parts"`
	StartedAt int64        `json:"started_at"` // Unix nanoseconds.
}

// Done is how many bytes of this upload the server already holds.
func (u Upload) Done() int64 {
	var n int64
	for _, p := range u.Parts {
		n += p.Size
	}
	return n
}

// ResumeStore is where in-progress uploads are written down.
//
// It is an interface here and a bbolt bucket in internal/index, for the same
// reason the engine declares its own: this package is tested against a real
// object store and should not need the index to be one of them.
type ResumeStore interface {
	// Resumable returns the unfinished upload for key, if there is one.
	Resumable(key string) (Upload, bool, error)
	// SaveResumable writes an upload's state. It is called once when the
	// upload starts and again after each part is accepted, because the
	// process can stop between any two of them.
	SaveResumable(u Upload) error
	// ForgetResumable drops the record, on completion or on abandonment.
	ForgetResumable(key string) error
	// AllResumable lists every record, for the sweep that abandons the ones
	// nothing is going to finish.
	AllResumable() ([]Upload, error)
}

// ResumeMaxAge is how long an unfinished upload is kept before the sweep
// abandons it.
//
// It is not free to keep: R2 bills the storage of parts that belong to an
// upload nobody has completed, and they are invisible in the dashboard's
// object listing, so an abandoned one is a charge with nothing to show for
// it. A week is long enough to survive a holiday and short enough that a
// file the user has given up on does not bill forever.
const ResumeMaxAge = 7 * 24 * time.Hour

// putResumable uploads body as key in parts, continuing a previous attempt
// when there is one to continue.
//
// src must be a ReaderAt or a ReadSeeker; the engine hands this an *os.File,
// which is both. Without one of them there is no way to send part seven
// without first sending parts one to six, and Put falls back to the
// non-resumable path rather than pretending.
func (c *Client) putResumable(ctx context.Context, key string, src io.Reader, size, modTime int64,
	meta map[string]string, contentType string, onBytes func(int64)) error {

	partSize := partSizeFor(size, c.partSize())
	prev, resuming := c.findResumable(ctx, key, size, modTime, partSize)

	upload := prev
	if !resuming {
		id, err := c.createMultipart(ctx, key, meta, contentType)
		if err != nil {
			return err
		}
		upload = Upload{
			Key: key, UploadID: id, PartSize: partSize,
			Size: size, ModTime: modTime, StartedAt: time.Now().UnixNano(),
		}
		if err := c.saveResume(upload); err != nil {
			return err
		}
	}

	have := make(map[int32]PartRecord, len(upload.Parts))
	for _, p := range upload.Parts {
		have[p.Number] = p
	}
	// Bytes the server already holds are reported once, up front. Without
	// this a resumed upload draws a progress bar that starts at zero and
	// finishes early, and the run's total -- which counts every byte of
	// every changed file -- would never be reached.
	if done := upload.Done(); done > 0 && onBytes != nil {
		onBytes(done)
	}

	total := int32((size + partSize - 1) / partSize)
	if total == 0 {
		total = 1 // a zero-byte file is still one (empty) part
	}
	var todo []int32
	for n := int32(1); n <= total; n++ {
		if _, ok := have[n]; !ok {
			todo = append(todo, n)
		}
	}

	if len(todo) > 0 {
		accepted, err := c.uploadParts(ctx, upload, src, size, todo, onBytes)
		// Whatever was accepted is saved even when the run as a whole
		// failed. That is the entire point: the next attempt starts from
		// here rather than from nothing.
		for _, p := range accepted {
			have[p.Number] = p
		}
		upload.Parts = sortedParts(have)
		if serr := c.saveResume(upload); serr != nil && err == nil {
			err = serr
		}
		if err != nil {
			return fmt.Errorf("remote: multipart put %q: %w", key, err)
		}
	}

	if err := c.completeMultipart(ctx, key, upload.UploadID, sortedParts(have)); err != nil {
		return fmt.Errorf("remote: multipart put %q: %w", key, err)
	}
	return c.forgetResume(key)
}

// findResumable decides whether a previous attempt can be continued, and
// cleans up after one that cannot.
//
// The server is asked rather than trusted to agree with us: our record is
// written after each part is accepted, so a process killed between the
// acceptance and the write leaves the server holding a part we have no note
// of. ListParts is one request and it is authoritative, which is worth more
// than the request costs.
func (c *Client) findResumable(ctx context.Context, key string, size, modTime, partSize int64) (Upload, bool) {
	if c.resume == nil {
		return Upload{}, false
	}
	prev, ok, err := c.resume.Resumable(key)
	if err != nil || !ok {
		return Upload{}, false
	}

	// The file underneath has changed, or we would cut it into different
	// parts than last time. Either way the parts on the server are of no
	// use, and leaving them there is a bill for nothing.
	if prev.Size != size || prev.ModTime != modTime || prev.PartSize != partSize ||
		time.Since(time.Unix(0, prev.StartedAt)) > ResumeMaxAge {
		c.abandon(ctx, prev)
		return Upload{}, false
	}

	parts, err := c.listParts(ctx, key, prev.UploadID)
	if err != nil {
		// A vanished upload id is the ordinary case here -- the bucket has
		// a lifecycle rule, or someone aborted it -- and means only that
		// there is nothing to resume.
		if isNoSuchUpload(err) {
			_ = c.forgetResume(key)
		}
		return Upload{}, false
	}
	prev.Parts = parts
	return prev, true
}

// uploadParts sends the missing parts, returning every one the server
// accepted -- including when it later returns an error, so the caller can
// write down the progress that was made before it failed.
func (c *Client) uploadParts(ctx context.Context, u Upload, src io.Reader, size int64,
	todo []int32, onBytes func(int64)) ([]PartRecord, error) {

	ra, hasReaderAt := src.(io.ReaderAt)
	concurrency := uploadConcurrency
	if !hasReaderAt {
		// One part at a time, because a plain seeker has one cursor. The
		// engine always passes an *os.File, so this is the fallback rather
		// than the path.
		concurrency = 1
	}

	var (
		mu       sync.Mutex
		accepted []PartRecord
		firstErr error
		stopped  atomic.Bool
	)

	work := make(chan int32)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range work {
				// Checked before starting, never during. On a connection
				// that has genuinely gone there is no point beginning part
				// nine, but a part already on the wire is allowed to
				// finish: it may be seconds from being accepted, and an
				// accepted part is progress that survives to the next
				// attempt. Cancelling the context instead -- which is what
				// this did first -- threw that away, and on a fast local
				// failure it threw away every part in flight.
				if stopped.Load() {
					continue
				}
				offset := int64(n-1) * u.PartSize
				length := min64(u.PartSize, size-offset)
				if length < 0 {
					length = 0
				}
				body, err := partReader(ra, src, offset, length)
				var rec PartRecord
				if err == nil {
					rec, err = c.uploadPart(ctx, u.Key, u.UploadID, n, body, length, onBytes)
				}
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					stopped.Store(true)
				} else {
					accepted = append(accepted, rec)
				}
				mu.Unlock()
			}
		}()
	}
	for _, n := range todo {
		if stopped.Load() {
			break
		}
		work <- n
	}
	close(work)
	wg.Wait()

	sort.Slice(accepted, func(i, j int) bool { return accepted[i].Number < accepted[j].Number })
	return accepted, firstErr
}

// partReader returns a reader over one part's bytes.
func partReader(ra io.ReaderAt, src io.Reader, offset, length int64) (io.Reader, error) {
	if ra != nil {
		return io.NewSectionReader(ra, offset, length), nil
	}
	s, ok := src.(io.Seeker)
	if !ok {
		return nil, errors.New("source is neither readable at an offset nor seekable")
	}
	if _, err := s.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	return io.LimitReader(src, length), nil
}

func (c *Client) uploadPart(ctx context.Context, key, uploadID string, n int32,
	body io.Reader, length int64, onBytes func(int64)) (PartRecord, error) {

	// The same wrapper the single-object path uses, for the same two
	// reasons: bytes are counted as they reach the wire, and the body stays
	// rewindable so the SDK can retry this part on its own.
	wrapped := withProgress(body, onBytes)
	out, err := c.s3.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		UploadId:      aws.String(uploadID),
		PartNumber:    aws.Int32(n),
		Body:          wrapped,
		ContentLength: aws.Int64(length),
	})
	if err != nil {
		return PartRecord{}, explain(fmt.Errorf("part %d: %w", n, err), wrapped, length)
	}
	return PartRecord{Number: n, ETag: aws.ToString(out.ETag), Size: length}, nil
}

func (c *Client) createMultipart(ctx context.Context, key string, meta map[string]string, contentType string) (string, error) {
	in := &s3.CreateMultipartUploadInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		Metadata: meta,
	}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	out, err := c.s3.CreateMultipartUpload(ctx, in)
	if err != nil {
		return "", fmt.Errorf("remote: start multipart %q: %w", key, err)
	}
	return aws.ToString(out.UploadId), nil
}

func (c *Client) completeMultipart(ctx context.Context, key, uploadID string, parts []PartRecord) error {
	completed := make([]types.CompletedPart, 0, len(parts))
	for _, p := range parts {
		completed = append(completed, types.CompletedPart{
			PartNumber: aws.Int32(p.Number), ETag: aws.String(p.ETag),
		})
	}
	_, err := c.s3.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(c.bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	return err
}

func (c *Client) listParts(ctx context.Context, key, uploadID string) ([]PartRecord, error) {
	var out []PartRecord
	var marker *string
	for {
		page, err := c.s3.ListParts(ctx, &s3.ListPartsInput{
			Bucket:           aws.String(c.bucket),
			Key:              aws.String(key),
			UploadId:         aws.String(uploadID),
			PartNumberMarker: marker,
		})
		if err != nil {
			return nil, err
		}
		for _, p := range page.Parts {
			out = append(out, PartRecord{
				Number: aws.ToInt32(p.PartNumber),
				ETag:   aws.ToString(p.ETag),
				Size:   aws.ToInt64(p.Size),
			})
		}
		if !aws.ToBool(page.IsTruncated) {
			break
		}
		marker = page.NextPartNumberMarker
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// AbandonStaleUploads aborts unfinished uploads nothing is going to finish
// and forgets them.
//
// Parts belonging to an upload that is never completed are billed and are
// invisible in an object listing, so without this a file the user gave up on
// keeps costing them money with nothing on screen to explain it. Called at
// the end of a run, where the handful of requests it costs is noise.
func (c *Client) AbandonStaleUploads(ctx context.Context) (int, error) {
	if c.resume == nil {
		return 0, nil
	}
	all, err := c.resume.AllResumable()
	if err != nil {
		return 0, err
	}
	var n int
	for _, u := range all {
		if time.Since(time.Unix(0, u.StartedAt)) <= ResumeMaxAge {
			continue
		}
		c.abandon(ctx, u)
		n++
	}
	return n, nil
}

// abandon aborts an upload on the server and drops our note of it. Failures
// are swallowed: this is tidying, and the caller is in the middle of
// something the user actually asked for.
func (c *Client) abandon(ctx context.Context, u Upload) {
	if u.UploadID != "" {
		_, _ = c.s3.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(c.bucket),
			Key:      aws.String(u.Key),
			UploadId: aws.String(u.UploadID),
		})
	}
	_ = c.forgetResume(u.Key)
}

func (c *Client) saveResume(u Upload) error {
	if c.resume == nil {
		return nil
	}
	if err := c.resume.SaveResumable(u); err != nil {
		return fmt.Errorf("remote: record part progress for %q: %w", u.Key, err)
	}
	return nil
}

func (c *Client) forgetResume(key string) error {
	if c.resume == nil {
		return nil
	}
	if err := c.resume.ForgetResumable(key); err != nil {
		return fmt.Errorf("remote: clear part progress for %q: %w", key, err)
	}
	return nil
}

func sortedParts(have map[int32]PartRecord) []PartRecord {
	out := make([]PartRecord, 0, len(have))
	for _, p := range have {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

func isNoSuchUpload(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == "NoSuchUpload"
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
