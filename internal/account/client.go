package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/creds"
)

// Sentinel errors a caller can match with errors.Is. They mirror the
// Worker's own distinctions (see worker/src/handlers/auth.go) but say
// nothing more than the Worker itself is willing to say -- in particular
// there is no ErrNoSuchAccount, because the Worker never reports one.
var (
	// ErrInvalidCode covers a wrong code, an expired one, and one that has
	// already burned through its attempt budget -- the Worker collapses
	// all three into the same response, and this client preserves that.
	ErrInvalidCode = errors.New("account: invalid or expired code")
	// ErrRateLimited means the Worker's per-email or per-IP limit rejected
	// the request. The caller should back off before retrying.
	ErrRateLimited = errors.New("account: rate limited, try again later")
	// ErrUnauthorized means the token is missing, malformed, expired, or
	// signed with a key the Worker doesn't recognise.
	ErrUnauthorized = errors.New("account: unauthorized")
	// ErrNotFound means an authenticated request found nothing (e.g. no
	// vault has ever been stored for this account).
	ErrNotFound = errors.New("account: not found")
	// ErrServer wraps any 5xx response.
	ErrServer = errors.New("account: server error")
	// ErrBadResponse means the response was valid HTTP with a 2xx status,
	// but its body was not JSON this client's types could make sense of --
	// in practice, so far, always the same class of bug: the Worker and
	// this binary are on separate release schedules, so the field a struct
	// here expects and the field a deployed Worker actually sends can
	// disagree even though neither side, in isolation, is behaving wrongly
	// by its own version's contract. The original decode error is wrapped
	// in (via %w below), not discarded, so anything that wants the full
	// diagnostic -- a bug report, a future --verbose flag -- can still get
	// it with errors.Unwrap or fmt's %+v. Nothing that only needs to tell a
	// person what to do has to look at it, which is the point: a raw
	// encoding/json message ("cannot unmarshal string into Go struct
	// field...") is not something anyone signing in can act on.
	ErrBadResponse = errors.New("account: could not understand the server's response")
)

// Client speaks to the account Worker over plain HTTP(S) JSON. It holds no
// state about who is signed in -- that is TokenStore's job -- so a Client
// can be shared across goroutines and reused across an entire process's
// lifetime.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient builds a Client against baseURL (e.g.
// "https://r2backup-account.cloudflaredd.workers.dev"). A nil http.Client is replaced with one
// carrying a sane timeout, so a caller who doesn't care about transport
// details can't accidentally hang forever on a stalled connection.
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{baseURL: baseURL, httpClient: httpClient}
}

// RequestCode asks the server to email a sign-in code to email. It returns
// nil whether or not an account already exists for that address -- the
// Worker deliberately gives no way to tell the two apart (see the comment
// on GENERIC_OK in worker/src/handlers/auth.ts), so neither can this
// client. A non-nil error here means the request itself failed (rate
// limited, network error, server error), never "no such account".
func (c *Client) RequestCode(ctx context.Context, email string) error {
	_, err := c.do(ctx, http.MethodPost, "/auth/request", "", map[string]string{"email": email}, nil)
	return err
}

// GetVault fetches the stored ciphertext blob and KDF parameters for the
// account behind token. ErrNotFound means the account exists but has never
// stored a vault (e.g. this is the very first computer).
func (c *Client) GetVault(ctx context.Context, token string) (*EncryptedVault, error) {
	var out EncryptedVault
	_, err := c.do(ctx, http.MethodGet, "/vault", token, nil, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// PutVault stores vault under the account behind token, replacing whatever
// was stored before. The server treats ciphertext and nonce as opaque bytes
// -- see EncryptedVault -- and this call sends them exactly that way.
func (c *Client) PutVault(ctx context.Context, token string, vault *EncryptedVault) error {
	_, err := c.do(ctx, http.MethodPut, "/vault", token, vault, nil)
	return err
}

// StoredKeys is what an account keeps for its computers: where backups go,
// the second copy if there is one, and a Drime account mirrored from. They
// travel sealed with a key made from the PIN (see pin.go); the server cannot
// open them.
type StoredKeys struct {
	Primary *creds.Credentials `json:"primary,omitempty"`
	Second  *creds.Credentials `json:"second,omitempty"`
	Source  *creds.Credentials `json:"source,omitempty"`
}

// RegisterDevice records that name (running os) is one of this account's
// devices. Calling it again with the same name updates the existing entry
// rather than adding a duplicate.
func (c *Client) RegisterDevice(ctx context.Context, token, name, os string) error {
	body := map[string]string{"device_name": name, "os": os}
	_, err := c.do(ctx, http.MethodPost, "/devices", token, body, nil)
	return err
}

// RenameDevice records this device under a new name, replacing the entry it
// had under from. A server from before renames existed ignores from and
// simply adds the new name; the old entry then ages out of use.
func (c *Client) RenameDevice(ctx context.Context, token, from, to, os string) error {
	body := map[string]string{"device_name": to, "os": os, "previous_name": from}
	_, err := c.do(ctx, http.MethodPost, "/devices", token, body, nil)
	return err
}

// Device is one entry from ListDevices.
type Device struct {
	DeviceName string `json:"device_name"`
	OS         string `json:"os"`
	// LastSeen is Unix SECONDS, not milliseconds -- it is decoded straight
	// off what the Worker wrote with `Math.floor(Date.now() / 1000)` (see
	// worker/src/index.ts's ctx.now and worker/migrations/0001_init.sql's
	// plain INTEGER column). A caller reaching for time.UnixMilli here will
	// print every device as sometime in January 1970; use time.Unix instead.
	LastSeen int64 `json:"last_seen"`
}

// ListDevices returns every device registered under the account behind
// token, most recently seen first.
func (c *Client) ListDevices(ctx context.Context, token string) ([]Device, error) {
	var out struct {
		Devices []Device `json:"devices"`
	}
	_, err := c.do(ctx, http.MethodGet, "/devices", token, nil, &out)
	if err != nil {
		return nil, err
	}
	return out.Devices, nil
}

// do is the one place that builds a request, sends it, and turns the
// response into either a decoded value or a sentinel error. Every method
// above is a thin wrapper around this because the differences between them
// (path, method, whether a token or body is involved) are all data, not
// behavior.
func (c *Client) do(ctx context.Context, method, path, token string, body, out any) (*http.Response, error) {
	var reqBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("account: encode request: %w", err)
		}
		reqBody = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("account: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("account: request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp, fmt.Errorf("account: read response: %w", err)
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if out != nil {
			if err := json.Unmarshal(respBody, out); err != nil {
				return resp, fmt.Errorf("%w: %v", ErrBadResponse, err)
			}
		}
		return resp, nil
	case resp.StatusCode == http.StatusUnauthorized && path == "/auth/pin":
		var e struct {
			TriesLeft int `json:"tries_left"`
		}
		if json.Unmarshal(respBody, &e) == nil && e.TriesLeft > 0 {
			return resp, &WrongPINError{TriesLeft: e.TriesLeft}
		}
		return resp, ErrUnauthorized
	case resp.StatusCode == http.StatusUnauthorized:
		return resp, ErrUnauthorized
	case resp.StatusCode == http.StatusForbidden && bytes.Contains(respBody, []byte("pin required")):
		return resp, ErrPINRequired
	case resp.StatusCode == http.StatusConflict && path == "/auth/pin/setup":
		return resp, ErrPINAlreadySet
	case resp.StatusCode == http.StatusNotFound:
		return resp, ErrNotFound
	case resp.StatusCode == http.StatusTooManyRequests:
		var e struct {
			RetryAfter int `json:"retry_after"`
		}
		if json.Unmarshal(respBody, &e) == nil && e.RetryAfter > 0 {
			return resp, &PINLockedError{RetryAfterSeconds: e.RetryAfter}
		}
		return resp, ErrRateLimited
	case resp.StatusCode == http.StatusBadRequest && path == "/auth/verify":
		// The Worker's only 400 on this path is "invalid or expired code";
		// every other endpoint's 400s are client-side request shape bugs,
		// which is why this mapping is scoped to /auth/verify specifically.
		return resp, ErrInvalidCode
	case resp.StatusCode >= 500:
		return resp, fmt.Errorf("%w: status %d", ErrServer, resp.StatusCode)
	default:
		return resp, fmt.Errorf("account: unexpected status %d from %s %s: %s", resp.StatusCode, method, path, string(respBody))
	}
}
