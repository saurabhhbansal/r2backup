package remote

import "io"

// progressReader wraps an upload body so Put can report genuine progress.
// The callback only fires once bytes have actually been pulled off the
// reader -- which, since it sits between the caller's data and the HTTP
// request body, only happens as those bytes are handed to the connection --
// rather than when the caller merely queued them.
type progressReader struct {
	r  io.Reader
	cb func(n int64)

	// pos is where the underlying reader is, and high is the furthest
	// point already reported. They differ only after a rewind, and keeping
	// both is what stops a retried request from counting the same bytes a
	// second time -- see progressSeeker.
	pos  int64
	high int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.pos += int64(n)
		if p.cb != nil && p.pos > p.high {
			p.cb(p.pos - p.high)
			p.high = p.pos
		}
	}
	return n, err
}

// progressSeeker is a progressReader that has kept the body seekable.
//
// This matters far more than it looks. The AWS SDK retries a failed request
// by rewinding the body and sending it again, and it can only rewind a body
// that implements io.Seeker. progressReader does not, so wrapping a file in
// one hid the *os.File's own Seek and left the SDK holding a stream it could
// not replay. Every upload that hit a transient failure was then lost, which
// is why this was only ever seen in bursts, on a loaded runner, in a
// twenty-thousand-file backup: it takes a first failure to reach at all.
//
// What that costs depends on where the rewind is refused. Against MinIO the
// SDK gives up cleanly -- "failed to rewind transport stream for retry,
// request stream is not seekable", which is what the test asserts. Against
// R2 the same runs reported instead:
//
//	IncompleteBody: You did not provide the number of bytes specified by
//	the Content-Length HTTP header
//
// -- a short body sent against a Content-Length still describing the whole
// file. That second form has not been reproduced here and the exact path to
// it is not established; it is recorded because it is what the failure looks
// like in production, and both are the same unseekable body.
//
// The hazard was half-known. client.go already explains that a
// progress-counting wrapper around a file is not seekable, and swaps in
// UNSIGNED-PAYLOAD so that request *signing* does not need to rewind. Signing
// was only one of the two things that needed it.
//
// It is a separate type rather than a Seek method on progressReader because
// the interface has to be advertised only when it can be honoured: a
// *progressReader with a Seek that always fails is worse than one without,
// since the SDK would stop treating the request as unretryable and start
// treating it as a rewind that failed.
type progressSeeker struct {
	progressReader
	s io.Seeker
}

func (p *progressSeeker) Seek(offset int64, whence int) (int64, error) {
	n, err := p.s.Seek(offset, whence)
	if err != nil {
		return n, err
	}
	// Only the position moves. high stays where it was, so the bytes this
	// rewind is about to re-send are not counted twice -- a progress bar
	// must not report more bytes uploaded than the file has.
	p.pos = n
	return n, nil
}

// withProgress wraps body so bytes are counted as they go out, keeping the
// body seekable when it already was.
func withProgress(body io.Reader, cb func(n int64)) io.Reader {
	if cb == nil {
		return body
	}
	if s, ok := body.(io.Seeker); ok {
		return &progressSeeker{progressReader: progressReader{r: body, cb: cb}, s: s}
	}
	return &progressReader{r: body, cb: cb}
}
