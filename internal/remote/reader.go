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

	// readErr is the last non-EOF error the source gave, kept because
	// nothing else will ever see it.
	//
	// When a request body stops early, Go's transport truncates the request
	// and the server answers on its own terms -- for an S3 server, a 400
	// IncompleteBody, because it was promised a Content-Length it did not
	// receive. The SDK surfaces that response, and the read error that
	// actually caused it is gone. An unreadable file then reports as a
	// protocol complaint about byte counts, which is a sentence nobody can
	// act on. Put reads this back to say what really happened.
	readErr error
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
	if err != nil && err != io.EOF {
		p.readErr = err
	}
	return n, err
}

// sent reports how far the body was read and what stopped it, for an error
// message that would otherwise only be able to say the server was unhappy.
func (p *progressReader) sent() (int64, error) { return p.pos, p.readErr }

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
// The SDK refuses cleanly -- "failed to rewind transport stream for retry,
// request stream is not seekable" -- which is what the test asserts against
// the unfixed code.
//
// Scope, honestly: this was found while chasing repeated bursts of
// IncompleteBody 400s out of the e2e suite's twenty-thousand-file upload,
// and it is NOT established that this is their cause. IncompleteBody means a
// body shorter than its own Content-Length reached the server, and a rewind
// the SDK refuses never reaches the server at all. So the two may be
// unrelated and the burst may still be waiting to be explained. This is
// fixed on its own merits: an upload that cannot be retried is a real defect
// whatever else is true.
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

// counter is what Put reads back off a wrapped body when a request fails.
type counter interface {
	sent() (int64, error)
}

// withProgress wraps body so bytes are counted as they go out, keeping the
// body seekable when it already was.
//
// It wraps even when there is no progress callback. The count and the read
// error are worth having on their own: they are the difference between "the
// server rejected this" and "this file stopped being readable four megabytes
// in".
func withProgress(body io.Reader, cb func(n int64)) io.Reader {
	if s, ok := body.(io.Seeker); ok {
		return &progressSeeker{progressReader: progressReader{r: body, cb: cb}, s: s}
	}
	return &progressReader{r: body, cb: cb}
}
