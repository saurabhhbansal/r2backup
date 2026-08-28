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
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 && p.cb != nil {
		p.cb(int64(n))
	}
	return n, err
}
