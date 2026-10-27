package drime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

const (
	// defaultThreshold is the size above which a file goes up in parts. The
	// same 16 MiB as R2, for the same reason: an interruption never costs
	// more than one part of re-sending.
	defaultThreshold = 16 << 20
	defaultPartSize  = 16 << 20
	minPartSize      = 5 << 20 // S3's floor, which Drime's storage enforces
	maxParts         = 10000
	partConcurrency  = 4
)

// uploadIDSep joins Drime's upload id and storage key into the one UploadID
// string the shared resume record has room for. Neither ever contains a
// newline.
const uploadIDSep = "\n"

func packUploadID(id, s3key string) string { return id + uploadIDSep + s3key }

func unpackUploadID(s string) (id, s3key string, ok bool) {
	id, s3key, ok = strings.Cut(s, uploadIDSep)
	return id, s3key, ok && id != "" && s3key != ""
}

func partSizeFor(size, base int64) int64 {
	if base < minPartSize {
		base = minPartSize
	}
	if size <= 0 || (size+base-1)/base <= maxParts {
		return base
	}
	ps := (size + maxParts - 1) / maxParts
	const mib = int64(1 << 20)
	if rem := ps % mib; rem != 0 {
		ps += mib - rem
	}
	return ps
}

func contentTypeFor(name, given string) string {
	if given != "" {
		return given
	}
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// progress counts bytes handed to the connection, reporting each byte once
// however many times a retry rewinds and re-reads it.
type progress struct {
	r    io.Reader
	cb   func(int64)
	pos  int64
	high *int64
	mu   *sync.Mutex
}

func (p *progress) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 && p.cb != nil {
		p.pos += int64(n)
		p.mu.Lock()
		if p.pos > *p.high {
			d := p.pos - *p.high
			*p.high = p.pos
			p.mu.Unlock()
			p.cb(d)
		} else {
			p.mu.Unlock()
		}
	}
	return n, err
}

// uploadSmall sends a file in one request to /uploads.
func (c *Client) uploadSmall(ctx context.Context, parentID, leaf string, body io.Reader, size int64, contentType string, onBytes func(int64)) (item, error) {
	name := encodeName(leaf)

	// The form is built around the file rather than buffered with it, so the
	// exact Content-Length is known up front: Drime sits behind Cloudflare,
	// and a chunked upload of unknown length is one more thing to go wrong.
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	_ = mw.WriteField("parentId", parentID)
	_ = mw.WriteField("relativePath", name)
	if c.api.workspace != "" {
		_ = mw.WriteField("workspaceId", c.api.workspace)
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, escapeQuotes(name)))
	h.Set("Content-Type", contentTypeFor(leaf, contentType))
	if _, err := mw.CreatePart(h); err != nil {
		return item{}, err
	}
	headBytes := append([]byte(nil), head.Bytes()...)
	head.Reset()
	_ = mw.Close()
	tail := append([]byte(nil), head.Bytes()...)

	seeker, canRewind := body.(io.Seeker)
	var start int64
	if canRewind {
		var err error
		if start, err = seeker.Seek(0, io.SeekCurrent); err != nil {
			canRewind = false
		}
	}
	var high int64
	var mu sync.Mutex
	used := false
	r := request{
		method:      http.MethodPost,
		url:         c.api.url("/uploads", nil),
		contentType: mw.FormDataContentType(),
		length:      int64(len(headBytes)) + size + int64(len(tail)),
		newBody: func() (io.Reader, error) {
			if used && !canRewind {
				return nil, errors.New("drime: upload failed and the source cannot be read twice")
			}
			used = true
			file := &progress{r: io.LimitReader(body, size), cb: onBytes, high: &high, mu: &mu}
			return io.MultiReader(bytes.NewReader(headBytes), file, bytes.NewReader(tail)), nil
		},
	}
	if canRewind {
		r.rewind = func() error {
			_, err := seeker.Seek(start, io.SeekStart)
			return err
		}
	}
	resp, err := c.api.send(ctx, r)
	if err != nil {
		return item{}, err
	}
	defer resp.Body.Close()
	var res struct {
		FileEntry item `json:"fileEntry"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return item{}, fmt.Errorf("drime: upload: unexpected response: %w", err)
	}
	if res.FileEntry.ID == "" {
		return item{}, errors.New("drime: upload: no file entry in response")
	}
	return res.FileEntry, nil
}

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

func escapeQuotes(s string) string { return quoteEscaper.Replace(s) }

// partSource hands out one part's bytes at a time.
type partSource interface {
	part(n int32, partSize, size int64) (io.ReadSeeker, int64, error)
	parallel() bool
}

// readerAtSource reads parts out of order straight from the file, so parts
// can go up side by side and a resumed upload starts at the first missing
// one. It is what an *os.File gets.
type readerAtSource struct{ ra io.ReaderAt }

func (s readerAtSource) part(n int32, partSize, size int64) (io.ReadSeeker, int64, error) {
	off := int64(n-1) * partSize
	length := min(partSize, size-off)
	return io.NewSectionReader(s.ra, off, length), length, nil
}
func (readerAtSource) parallel() bool { return true }

// streamSource buffers parts in order from a plain reader.
type streamSource struct {
	r    io.Reader
	next int32
}

func (s *streamSource) part(n int32, partSize, size int64) (io.ReadSeeker, int64, error) {
	if n != s.next {
		return nil, 0, fmt.Errorf("drime: stream source asked for part %d out of order", n)
	}
	off := int64(n-1) * partSize
	length := min(partSize, size-off)
	buf := make([]byte, length)
	if _, err := io.ReadFull(s.r, buf); err != nil {
		return nil, 0, err
	}
	s.next++
	return bytes.NewReader(buf), length, nil
}
func (*streamSource) parallel() bool { return false }

type completedPart struct {
	ETag       string `json:"ETag"`
	PartNumber int32  `json:"PartNumber"`
}

// uploadLarge sends a file through Drime's multipart endpoints, which hand out
// presigned S3 part URLs. Progress is written to the resume store after each
// part, so an interrupted upload continues from the first part not sent.
func (c *Client) uploadLarge(ctx context.Context, key, parentID, leaf string, in remote.PutInput) (item, error) {
	var src partSource
	ra, isRA := in.Body.(io.ReaderAt)
	if isRA {
		src = readerAtSource{ra}
	} else {
		src = &streamSource{r: in.Body, next: 1}
	}
	size := in.Size
	partSize := partSizeFor(size, c.partSize)
	modTime := in.Metadata.ModTime.UnixNano()
	ctype := contentTypeFor(leaf, in.ContentType)

	up, resuming := remote.Upload{}, false
	if c.resume != nil && isRA {
		if prev, ok, err := c.resume.Resumable(key); err == nil && ok {
			_, _, valid := unpackUploadID(prev.UploadID)
			if valid && prev.Size == size && prev.ModTime == modTime && prev.PartSize == partSize {
				up, resuming = prev, true
			} else {
				c.abandon(ctx, prev)
			}
		}
	}

	it, err := c.sendParts(ctx, key, parentID, leaf, ctype, size, partSize, modTime, src, up, resuming, in.Progress)
	if err != nil && resuming && !errors.Is(err, context.Canceled) {
		// The server may have expired what we were resuming. One fresh try
		// before giving up -- the parts are a week old at most, and nothing
		// is lost by sending them again.
		var ae *apiError
		if errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 {
			c.abandon(ctx, up)
			return c.sendParts(ctx, key, parentID, leaf, ctype, size, partSize, modTime, src, remote.Upload{}, false, in.Progress)
		}
	}
	return it, err
}

func (c *Client) sendParts(ctx context.Context, key, parentID, leaf, ctype string, size, partSize, modTime int64,
	src partSource, up remote.Upload, resuming bool, onBytes func(int64)) (item, error) {

	name := encodeName(leaf)
	ext := strings.TrimPrefix(path.Ext(leaf), ".")
	if ext == "" {
		ext = "bin" // Drime refuses a multipart upload without one
	}

	if !resuming {
		req := struct {
			Filename     string      `json:"filename"`
			Mime         string      `json:"mime"`
			Size         int64       `json:"size"`
			Extension    string      `json:"extension"`
			ParentID     json.Number `json:"parentId"`
			RelativePath string      `json:"relativePath"`
			WorkspaceID  string      `json:"workspaceId,omitempty"`
		}{name, ctype, size, ext, json.Number(parentID), name, c.api.workspace}
		var res struct {
			UploadID string `json:"uploadId"`
			Key      string `json:"key"`
		}
		if err := c.api.call(ctx, http.MethodPost, "/s3/multipart/create", nil, req, &res, nil); err != nil {
			return item{}, fmt.Errorf("drime: start upload of %q: %w", key, err)
		}
		if res.UploadID == "" || res.Key == "" {
			return item{}, fmt.Errorf("drime: start upload of %q: no upload id in response", key)
		}
		up = remote.Upload{
			Key: key, UploadID: packUploadID(res.UploadID, res.Key), PartSize: partSize,
			Size: size, ModTime: modTime, StartedAt: time.Now().UnixNano(),
		}
		c.saveResume(up)
	}
	uploadID, s3key, _ := unpackUploadID(up.UploadID)

	total := int32((size + partSize - 1) / partSize)
	if total < 1 {
		total = 1
	}
	have := map[int32]remote.PartRecord{}
	for _, p := range up.Parts {
		if p.Number >= 1 && p.Number <= total && p.ETag != "" {
			have[p.Number] = p
		}
	}
	if onBytes != nil {
		var done int64
		for _, p := range have {
			done += p.Size
		}
		if done > 0 {
			onBytes(done)
		}
	}
	var todo []int32
	for n := int32(1); n <= total; n++ {
		if _, ok := have[n]; !ok {
			todo = append(todo, n)
		}
	}

	var mu sync.Mutex
	record := func(p remote.PartRecord) {
		mu.Lock()
		have[p.Number] = p
		up.Parts = sortedParts(have)
		snap := up
		mu.Unlock()
		c.saveResume(snap)
	}

	conc := partConcurrency
	if !src.parallel() {
		conc = 1
	}
	for len(todo) > 0 {
		batch := todo[:min(conc, len(todo))]
		todo = todo[len(batch):]
		urls, err := c.signParts(ctx, uploadID, s3key, batch)
		if err != nil {
			return item{}, err
		}
		bodies := make([]io.ReadSeeker, len(batch))
		lengths := make([]int64, len(batch))
		for i, n := range batch {
			body, length, err := src.part(n, partSize, size)
			if err != nil {
				return item{}, fmt.Errorf("drime: read part %d of %q: %w", n, key, err)
			}
			bodies[i], lengths[i] = body, length
		}
		var wg sync.WaitGroup
		errs := make([]error, len(batch))
		for i, n := range batch {
			body, length := bodies[i], lengths[i]
			wg.Add(1)
			go func(i int, n int32, body io.ReadSeeker, length int64) {
				defer wg.Done()
				etag, err := c.putPart(ctx, urls[n], body, length, onBytes)
				if err != nil {
					errs[i] = fmt.Errorf("drime: send part %d of %q: %w", n, key, err)
					return
				}
				record(remote.PartRecord{Number: n, ETag: etag, Size: length})
			}(i, n, body, length)
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return item{}, err
		}
	}

	parts := make([]completedPart, 0, len(have))
	for _, p := range sortedParts(have) {
		parts = append(parts, completedPart{ETag: p.ETag, PartNumber: p.Number})
	}
	complete := struct {
		UploadID string          `json:"uploadId"`
		Key      string          `json:"key"`
		Parts    []completedPart `json:"parts"`
	}{uploadID, s3key, parts}
	if err := c.api.call(ctx, http.MethodPost, "/s3/multipart/complete", nil, complete, nil, nil); err != nil {
		return item{}, fmt.Errorf("drime: finish upload of %q: %w", key, err)
	}

	entry := struct {
		ClientMime      string      `json:"clientMime"`
		ClientName      string      `json:"clientName"`
		Filename        string      `json:"filename"`
		Size            int64       `json:"size"`
		ClientExtension string      `json:"clientExtension"`
		ParentID        json.Number `json:"parentId"`
		RelativePath    string      `json:"relativePath"`
		WorkspaceID     string      `json:"workspaceId,omitempty"`
	}{ctype, name, path.Base(s3key), size, ext, json.Number(parentID), name, c.api.workspace}
	var res struct {
		FileEntry item `json:"fileEntry"`
	}
	if err := c.api.call(ctx, http.MethodPost, "/s3/entries", nil, entry, &res, nil); err != nil {
		return item{}, fmt.Errorf("drime: register upload of %q: %w", key, err)
	}
	if c.resume != nil {
		_ = c.resume.ForgetResumable(key)
	}
	if res.FileEntry.ID == "" {
		return item{}, fmt.Errorf("drime: register upload of %q: no file entry in response", key)
	}
	return res.FileEntry, nil
}

func (c *Client) signParts(ctx context.Context, uploadID, s3key string, parts []int32) (map[int32]string, error) {
	nums := make([]int, len(parts))
	for i, n := range parts {
		nums[i] = int(n)
	}
	req := struct {
		UploadID    string `json:"uploadId"`
		Key         string `json:"key"`
		PartNumbers []int  `json:"partNumbers"`
	}{uploadID, s3key, nums}
	var res struct {
		URLs []struct {
			URL        string `json:"url"`
			PartNumber int32  `json:"partNumber"`
		} `json:"urls"`
	}
	if err := c.api.call(ctx, http.MethodPost, "/s3/multipart/batch-sign-part-urls", nil, req, &res, nil); err != nil {
		return nil, fmt.Errorf("drime: get part upload addresses: %w", err)
	}
	out := make(map[int32]string, len(res.URLs))
	for _, u := range res.URLs {
		out[u.PartNumber] = u.URL
	}
	for _, n := range parts {
		if out[n] == "" {
			return nil, fmt.Errorf("drime: no upload address for part %d", n)
		}
	}
	return out, nil
}

func (c *Client) putPart(ctx context.Context, partURL string, body io.ReadSeeker, length int64, onBytes func(int64)) (string, error) {
	var high int64
	var mu sync.Mutex
	resp, err := c.api.send(ctx, request{
		method:      http.MethodPut,
		url:         partURL,
		contentType: "application/octet-stream",
		length:      length,
		noAuth:      true,
		newBody: func() (io.Reader, error) {
			return &progress{r: body, cb: onBytes, high: &high, mu: &mu}, nil
		},
		rewind: func() error {
			_, err := body.Seek(0, io.SeekStart)
			return err
		},
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", errors.New("storage accepted the part but returned no ETag")
	}
	return etag, nil
}

func (c *Client) abortUpload(ctx context.Context, packed string) error {
	id, s3key, ok := unpackUploadID(packed)
	if !ok {
		return nil
	}
	req := struct {
		UploadID string `json:"uploadId"`
		Key      string `json:"key"`
	}{id, s3key}
	err := c.api.call(ctx, http.MethodPost, "/s3/multipart/abort", nil, req, nil, nil)
	if err != nil && isNotFound(err) {
		return nil
	}
	return err
}

func (c *Client) abandon(ctx context.Context, u remote.Upload) {
	_ = c.abortUpload(ctx, u.UploadID)
	if c.resume != nil {
		_ = c.resume.ForgetResumable(u.Key)
	}
}

func (c *Client) saveResume(u remote.Upload) {
	if c.resume != nil {
		_ = c.resume.SaveResumable(u)
	}
}

func sortedParts(have map[int32]remote.PartRecord) []remote.PartRecord {
	out := make([]remote.PartRecord, 0, len(have))
	for _, p := range have {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
