// Package netwait holds a run while the network is gone, instead of letting
// every file in flight burn its retries and fail.
//
// A laptop whose lid is closed mid-backup, a Wi-Fi that drops, a router being
// restarted: none of these mean anything is wrong with the files or with the
// storage. The run should stand still until the network is back and then carry
// on, the way it does when someone presses pause. Before this, each file being
// sent at that moment tried six times over about a minute, then failed, and
// the run went on failing the next ones just as fast -- 736 files of one run,
// reported as "will be retried next run".
//
// It works the way package pause does: a Gate travels in the run's context,
// and whoever hits a network error -- a worker, a request's retry loop -- trips
// it and waits on it. One prober then checks, with backoff, whether the
// storage's host can be reached again, and reopens the gate when it can. A
// run that has waited too long (MaxWait of awake time in one outage) stops
// waiting: the files still to go fail at once with ErrNoNetwork, so a run on a
// machine that has really gone offline ends instead of hanging.
package netwait

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"syscall"
)

// ErrNoNetwork is what a file fails with once the run has given up waiting
// for the network to come back.
var ErrNoNetwork = errors.New("no network: the storage could not be reached")

// Windows socket errors, by number, so they are recognised on any platform a
// test runs on. They arrive as syscall.Errno inside an *os.SyscallError inside
// an *net.OpError, but a client that re-wraps the error can lose the OpError
// and keep the errno.
const (
	wsaENETDOWN     = 10050
	wsaENETUNREACH  = 10051
	wsaENETRESET    = 10052
	wsaECONNABORTED = 10053
	wsaECONNRESET   = 10054
	wsaETIMEDOUT    = 10060
	wsaECONNREFUSED = 10061
	wsaEHOSTDOWN    = 10064
	wsaEHOSTUNREACH = 10065
)

// networkMessages are the tell-tale words of a network failure, for an error
// that reached us flattened to text (a client that formats with %v rather
// than %w). Each names a connection or a lookup, never an HTTP answer.
var networkMessages = []string{
	"no such host",
	"connection reset",
	"connection refused",
	"connection was aborted",
	"connection aborted",
	"established connection was aborted",
	"forcibly closed by the remote host",
	"network is unreachable",
	"network is down",
	"host is down",
	"no route to host",
	"broken pipe",
	"wsarecv",
	"wsasend",
	"tls handshake timeout",
	"i/o timeout",
	"server closed idle connection",
	"http2: client connection lost",
	"use of closed network connection",
	"temporary failure in name resolution",
	"dial tcp",
}

// IsNetworkError reports whether err means the network itself failed -- the
// host could not be looked up or reached, or a connection broke -- rather
// than the storage answering with an error. An HTTP error response is never
// one: the network was evidently working to carry it.
//
// A cancelled context is not one either. A run being stopped cancels its
// requests, and those must not be mistaken for an outage.
func IsNetworkError(err error) bool {
	if IsConnectionError(err) {
		return true
	}
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, m := range networkMessages {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// IsConnectionError is IsNetworkError without reading the message: only an
// error that carries this computer's own network failure in its chain. A
// service that reports, as text, a connection that broke somewhere else --
// the relay saying its own fetch was reset -- is not one.
func IsConnectionError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	// io.ErrUnexpectedEOF is a response body that stopped short of the
	// length it promised: the connection went partway through it.
	if errors.Is(err, ErrNoNetwork) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && isNetworkErrno(errno) {
		return true
	}
	// A request that went out and came back with nothing: the connection
	// closed before a response, or a deadline passed waiting for one.
	var ue *url.Error
	if errors.As(err, &ue) {
		if errors.Is(ue.Err, io.EOF) || errors.Is(ue.Err, io.ErrUnexpectedEOF) ||
			errors.Is(ue.Err, context.DeadlineExceeded) || ue.Timeout() {
			return true
		}
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func isNetworkErrno(e syscall.Errno) bool {
	switch e {
	case syscall.ECONNRESET, syscall.ECONNABORTED, syscall.ECONNREFUSED,
		syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ENETDOWN,
		syscall.ETIMEDOUT, syscall.EPIPE:
		return true
	}
	switch uintptr(e) {
	case wsaENETDOWN, wsaENETUNREACH, wsaENETRESET, wsaECONNABORTED, wsaECONNRESET,
		wsaETIMEDOUT, wsaECONNREFUSED, wsaEHOSTDOWN, wsaEHOSTUNREACH:
		return true
	}
	return false
}
