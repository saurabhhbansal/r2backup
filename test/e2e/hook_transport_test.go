package e2e

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// firstReadHook calls fn exactly once, the first time Read is called, before
// any byte of the wrapped body is actually delivered. It exists to make "the
// file changed while this exact byte range was in flight" deterministic:
// firing synchronously from inside the read that internal/engine's Put call
// is blocked on guarantees the mutation lands strictly between the engine's
// pre-upload stat and its post-upload stat, with no timing window to race.
type firstReadHook struct {
	io.ReadCloser
	fn   func()
	once sync.Once
}

func (h *firstReadHook) Read(p []byte) (int, error) {
	h.once.Do(h.fn)
	return h.ReadCloser.Read(p)
}

// putHookTransport calls onAttempt(n) -- n starting at 1 -- the first time
// the body of the nth PUT request whose path contains keyFragment starts
// being read. Every other request passes through untouched.
//
// This is how the "modified mid-upload" tests reach into a real HTTP
// upload, made through a real remote.Client against a real MinIO server,
// without depending on wall-clock timing at all.
type putHookTransport struct {
	base        http.RoundTripper
	keyFragment string
	onAttempt   func(attempt int)
	attempts    int32
}

func (t *putHookTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPut && req.Body != nil && strings.Contains(req.URL.Path, t.keyFragment) {
		n := int(atomic.AddInt32(&t.attempts, 1))
		req.Body = &firstReadHook{ReadCloser: req.Body, fn: func() { t.onAttempt(n) }}
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// withPutHook returns a remote.Config mutator that installs a putHookTransport
// keyed on keyFragment (ordinarily the file's relative path, or a unique
// enough suffix of it) into the client's HTTPClient.
func withPutHook(keyFragment string, onAttempt func(attempt int)) func(*remote.Config) {
	return func(cfg *remote.Config) {
		cfg.HTTPClient = &http.Client{Transport: &putHookTransport{
			keyFragment: keyFragment,
			onAttempt:   onAttempt,
		}}
	}
}
