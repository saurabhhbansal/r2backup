package netwait

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The two errors from the run that prompted this, built the way the standard
// library builds them on Windows, then wrapped the way drime and the engine
// wrap them.
func TestIsNetworkError_TheReportedShapes(t *testing.T) {
	aborted := &net.OpError{
		Op: "read", Net: "tcp",
		Source: &net.TCPAddr{IP: net.IPv4(192, 168, 1, 20), Port: 50123},
		Addr:   &net.TCPAddr{IP: net.IPv4(104, 26, 0, 1), Port: 443},
		Err:    os.NewSyscallError("wsarecv", syscall.Errno(10053)),
	}
	lookup := &url.Error{Op: "Post", URL: "https://app.drime.cloud/api/v1/uploads", Err: &net.OpError{
		Op: "dial", Net: "tcp",
		Err: &net.DNSError{Err: "no such host", Name: "app.drime.cloud", IsNotFound: true},
	}}
	cases := map[string]error{
		"wsarecv aborted, decoding the upload's answer": fmt.Errorf("upload a/b.txt: %w",
			fmt.Errorf("drime: upload: unexpected response: %w", aborted)),
		"lookup failed": fmt.Errorf("upload a/b.txt: %w", lookup),
	}
	for name, err := range cases {
		if !IsNetworkError(err) {
			t.Errorf("%s: %q not recognised", name, err)
		}
	}
	if want := `Post "https://app.drime.cloud/api/v1/uploads": dial tcp: lookup app.drime.cloud: no such host`; lookup.Error() != want {
		t.Errorf("built %q, the report had %q", lookup.Error(), want)
	}
	if !strings.Contains(aborted.Error(), "wsarecv") {
		t.Errorf("built %q", aborted.Error())
	}
}

// The same messages flattened to text, as a client formatting with %v
// leaves them.
func TestIsNetworkError_Flattened(t *testing.T) {
	for _, s := range []string{
		"drime: upload: unexpected response: read tcp 192.168.1.20:50123->104.26.0.1:443: wsarecv: An established connection was aborted by the software in your host machine.",
		`Post "https://app.drime.cloud/api/v1/uploads": dial tcp: lookup app.drime.cloud: no such host`,
		"write tcp 10.0.0.2:5000->1.2.3.4:443: wsasend: An existing connection was forcibly closed by the remote host.",
		"read: connection reset by peer",
		"net/http: TLS handshake timeout",
		"dial tcp 1.2.3.4:443: connect: network is unreachable",
	} {
		if !IsNetworkError(errors.New(s)) {
			t.Errorf("%q not recognised", s)
		}
	}
}

func TestIsNetworkError_Errnos(t *testing.T) {
	for _, e := range []syscall.Errno{10053, 10054, 10051, 10060, 10061, syscall.ECONNRESET, syscall.ECONNREFUSED} {
		if !IsNetworkError(fmt.Errorf("send: %w", os.NewSyscallError("write", e))) {
			t.Errorf("errno %d not recognised", e)
		}
	}
	if !IsNetworkError(fmt.Errorf("x: %w", net.ErrClosed)) {
		t.Error("net.ErrClosed not recognised")
	}
}

// Not outages: the storage answering, a file problem, a run being stopped.
func TestIsNetworkError_NotNetwork(t *testing.T) {
	for name, err := range map[string]error{
		"nil":               nil,
		"http answer":       errors.New("drime: Too Many Requests (HTTP 429)"),
		"forbidden":         fmt.Errorf("upload a: %w", errors.New("drime: Forbidden (HTTP 403)")),
		"missing file":      fmt.Errorf("open a: %w", os.ErrNotExist),
		"file changed":      errors.New("upload a: file changed again during upload; giving up after one retry"),
		"cancelled":         context.Canceled,
		"cancelled request": &url.Error{Op: "Post", URL: "https://x/", Err: context.Canceled},
		"cancelled dial": &url.Error{Op: "Post", URL: "https://x/", Err: &net.OpError{
			Op: "dial", Net: "tcp", Err: context.Canceled}},
		"bad json": fmt.Errorf("drime: unexpected response: %w", &json.SyntaxError{}),
	} {
		if IsNetworkError(err) {
			t.Errorf("%s: %v taken for a network failure", name, err)
		}
	}
}

// Real failures from the standard library, not hand-built ones.
func TestIsNetworkError_Real(t *testing.T) {
	// A port nothing listens on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	_, err = http.Post("http://"+addr+"/", "text/plain", strings.NewReader("x"))
	if err == nil || !IsNetworkError(err) {
		t.Errorf("refused: %v not recognised", err)
	}

	// A lookup that cannot succeed (.invalid is reserved for that).
	_, err = http.Get("http://r2backup-test.invalid/")
	if err == nil || !IsNetworkError(err) {
		t.Errorf("lookup: %v not recognised", err)
	}

	// A server that hangs up without answering: EOF on a fresh connection.
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, _ := w.(http.Hijacker).Hijack()
		c.Close()
	}))
	defer hang.Close()
	_, err = http.Get(hang.URL)
	if err == nil || !IsNetworkError(err) {
		t.Errorf("hang-up: %v not recognised", err)
	}

	// A connection cut halfway through the answer.
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"fileEntry":`))
		w.(http.Flusher).Flush()
		c, _, _ := w.(http.Hijacker).Hijack()
		c.Close()
	}))
	defer cut.Close()
	resp, err := http.Get(cut.URL)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	err = json.NewDecoder(resp.Body).Decode(&v)
	resp.Body.Close()
	if err == nil || !IsNetworkError(fmt.Errorf("drime: upload: unexpected response: %w", err)) {
		t.Errorf("cut body: %v not recognised", err)
	}

	// The storage answering with an error is not one.
	answer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer answer.Close()
	resp, err = http.Get(answer.URL)
	if err != nil {
		t.Fatalf("an HTTP answer came back as %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// A dial that times out.
	d := net.Dialer{Timeout: time.Nanosecond}
	if _, err := d.Dial("tcp", "10.255.255.1:443"); err != nil && !IsNetworkError(err) {
		t.Errorf("dial timeout: %v not recognised", err)
	}
}
