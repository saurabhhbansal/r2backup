package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeCloudflare stands in for the token endpoint. It records what the
// exchange actually sent, which is where the PKCE and redirect_uri
// assertions come from -- those are the two fields that are easy to get
// subtly wrong and impossible to notice by hand.
type fakeCloudflare struct {
	*httptest.Server
	gotForm url.Values
	status  int
	body    string
}

func newFakeCloudflare(t *testing.T) *fakeCloudflare {
	t.Helper()
	f := &fakeCloudflare{status: http.StatusOK,
		body: `{"access_token":"tok-abc","token_type":"bearer","expires_in":3600}`}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		f.gotForm = form
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
	}))
	t.Cleanup(f.Close)
	return f
}

// browserThatApproves returns an OpenBrowser stand-in that behaves like a
// person who clicks Allow: it reads the authorization URL, then calls back to
// the loopback listener with the state it was given.
func browserThatApproves(t *testing.T, code string) func(string) error {
	t.Helper()
	return browserThatReplies(t, func(state string) url.Values {
		return url.Values{"code": {code}, "state": {state}}
	})
}

// browserThatReplies builds the callback query from the state it saw, so a
// test can send back anything a real redirect might carry.
func browserThatReplies(t *testing.T, reply func(state string) url.Values) func(string) error {
	t.Helper()
	return func(rawURL string) error {
		u, err := url.Parse(rawURL)
		if err != nil {
			return err
		}
		q := u.Query()
		callback, err := url.Parse(q.Get("redirect_uri"))
		if err != nil {
			return err
		}
		callback.RawQuery = reply(q.Get("state")).Encode()
		go func() {
			resp, err := http.Get(callback.String())
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
}

func testConfig(t *testing.T, f *fakeCloudflare, open func(string) error) Config {
	t.Helper()
	return Config{
		ClientID:    "client-123",
		Scopes:      []string{"r2:write", "memberships:read"},
		AuthURL:     "https://example.invalid/auth",
		TokenURL:    f.URL,
		RevokeURL:   f.URL,
		OpenBrowser: open,
	}
}

func TestAuthorizeReturnsToken(t *testing.T) {
	f := newFakeCloudflare(t)
	cfg := testConfig(t, f, browserThatApproves(t, "code-xyz"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tok, err := cfg.Authorize(ctx)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if tok.AccessToken != "tok-abc" {
		t.Errorf("access token = %q, want %q", tok.AccessToken, "tok-abc")
	}
	if tok.ExpiresAt.IsZero() {
		t.Error("ExpiresAt not set from expires_in")
	}
	if got := f.gotForm.Get("code"); got != "code-xyz" {
		t.Errorf("exchanged code = %q, want %q", got, "code-xyz")
	}
	if got := f.gotForm.Get("grant_type"); got != "authorization_code" {
		t.Errorf("grant_type = %q", got)
	}
	// A public client authenticates with client_id alone; a secret here
	// would mean the registration and this code had drifted apart.
	if f.gotForm.Get("client_id") != "client-123" {
		t.Errorf("client_id = %q", f.gotForm.Get("client_id"))
	}
	if f.gotForm.Get("client_secret") != "" {
		t.Error("a client secret was sent; this is a public client")
	}
}

// The verifier sent at exchange must hash to the challenge sent at
// authorization. If these ever drift apart Cloudflare rejects every sign-in,
// so it is worth asserting rather than trusting.
func TestAuthorizeVerifierMatchesChallenge(t *testing.T) {
	f := newFakeCloudflare(t)
	var challenge string
	open := func(rawURL string) error {
		u, _ := url.Parse(rawURL)
		challenge = u.Query().Get("code_challenge")
		if got := u.Query().Get("code_challenge_method"); got != "S256" {
			t.Errorf("code_challenge_method = %q, want S256", got)
		}
		return browserThatApproves(t, "code-xyz")(rawURL)
	}
	cfg := testConfig(t, f, open)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cfg.Authorize(ctx); err != nil {
		t.Fatalf("Authorize: %v", err)
	}

	verifier := f.gotForm.Get("code_verifier")
	if verifier == "" {
		t.Fatal("no code_verifier sent")
	}
	sum := sha256.Sum256([]byte(verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); want != challenge {
		t.Errorf("sha256(verifier) = %q, but challenge was %q", want, challenge)
	}
}

// The redirect_uri at exchange must be byte-identical to the one used at
// authorization, and both must be the registered loopback form.
func TestAuthorizeRedirectURIIsConsistentAndLoopback(t *testing.T) {
	f := newFakeCloudflare(t)
	var authRedirect string
	open := func(rawURL string) error {
		u, _ := url.Parse(rawURL)
		authRedirect = u.Query().Get("redirect_uri")
		return browserThatApproves(t, "code-xyz")(rawURL)
	}
	cfg := testConfig(t, f, open)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cfg.Authorize(ctx); err != nil {
		t.Fatalf("Authorize: %v", err)
	}

	if !strings.HasPrefix(authRedirect, "http://127.0.0.1:") {
		t.Errorf("redirect_uri = %q, want a 127.0.0.1 loopback URL", authRedirect)
	}
	if !strings.HasSuffix(authRedirect, callbackPath) {
		t.Errorf("redirect_uri = %q, want it to end in %q", authRedirect, callbackPath)
	}
	if got := f.gotForm.Get("redirect_uri"); got != authRedirect {
		t.Errorf("redirect_uri at exchange = %q, at authorization = %q; these must match exactly", got, authRedirect)
	}
}

func TestAuthorizeSendsScopesAndResponseType(t *testing.T) {
	f := newFakeCloudflare(t)
	var q url.Values
	open := func(rawURL string) error {
		u, _ := url.Parse(rawURL)
		q = u.Query()
		return browserThatApproves(t, "code-xyz")(rawURL)
	}
	cfg := testConfig(t, f, open)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cfg.Authorize(ctx); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got := q.Get("response_type"); got != "code" {
		t.Errorf("response_type = %q, want code", got)
	}
	if got := q.Get("scope"); got != "r2:write memberships:read" {
		t.Errorf("scope = %q, want space-separated scopes", got)
	}
	if q.Get("state") == "" {
		t.Error("no state sent")
	}
}

// A callback carrying the wrong state is somebody else's, so the code in it
// must never be redeemed.
func TestAuthorizeRejectsMismatchedState(t *testing.T) {
	f := newFakeCloudflare(t)
	open := browserThatReplies(t, func(string) url.Values {
		return url.Values{"code": {"code-xyz"}, "state": {"not-the-state-we-sent"}}
	})
	cfg := testConfig(t, f, open)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cfg.Authorize(ctx)
	if !errors.Is(err, ErrMismatchedState) {
		t.Fatalf("err = %v, want ErrMismatchedState", err)
	}
	if f.gotForm != nil {
		t.Error("the token endpoint was called despite a bad state")
	}
}

func TestAuthorizeReportsDenial(t *testing.T) {
	f := newFakeCloudflare(t)
	open := browserThatReplies(t, func(state string) url.Values {
		return url.Values{"error": {"access_denied"}, "state": {state}}
	})
	cfg := testConfig(t, f, open)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cfg.Authorize(ctx)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("err = %v, want ErrDenied", err)
	}
}

func TestAuthorizeReportsCloudflareError(t *testing.T) {
	f := newFakeCloudflare(t)
	open := browserThatReplies(t, func(state string) url.Values {
		return url.Values{
			"error":             {"invalid_scope"},
			"error_description": {"the scope was not registered"},
			"state":             {state},
		}
	})
	cfg := testConfig(t, f, open)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cfg.Authorize(ctx)
	if err == nil {
		t.Fatal("want an error")
	}
	// The description is the only part a person can act on, so it has to
	// survive into the message rather than being flattened to the code.
	if !strings.Contains(err.Error(), "the scope was not registered") {
		t.Errorf("err = %v, want it to carry Cloudflare's description", err)
	}
}

func TestAuthorizeSurfacesTokenEndpointError(t *testing.T) {
	f := newFakeCloudflare(t)
	f.status = http.StatusBadRequest
	f.body = `{"error":"invalid_grant","error_description":"code already used"}`
	cfg := testConfig(t, f, browserThatApproves(t, "code-xyz"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cfg.Authorize(ctx)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "code already used") {
		t.Errorf("err = %v, want Cloudflare's description", err)
	}
}

func TestAuthorizeErrorsWhenTokenMissing(t *testing.T) {
	f := newFakeCloudflare(t)
	f.body = `{"token_type":"bearer"}`
	cfg := testConfig(t, f, browserThatApproves(t, "code-xyz"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cfg.Authorize(ctx)
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("err = %v, want ErrNoToken", err)
	}
}

// A browser that never comes back must not hang forever; the caller's
// context is the way out.
func TestAuthorizeHonoursContext(t *testing.T) {
	f := newFakeCloudflare(t)
	silent := func(string) error { return nil }
	cfg := testConfig(t, f, silent)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := cfg.Authorize(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestAuthorizePropagatesNoBrowser(t *testing.T) {
	f := newFakeCloudflare(t)
	cfg := testConfig(t, f, func(string) error { return ErrNoBrowser })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cfg.Authorize(ctx)
	if !errors.Is(err, ErrNoBrowser) {
		t.Fatalf("err = %v, want ErrNoBrowser", err)
	}
}

func TestAuthorizeReportsAllPortsBusy(t *testing.T) {
	f := newFakeCloudflare(t)
	cfg := testConfig(t, f, browserThatApproves(t, "code-xyz"))

	// Hold the only port the config is allowed to use.
	l, port := grabPort(t)
	defer l.Close()
	cfg.Ports = []int{port}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cfg.Authorize(ctx)
	if !errors.Is(err, ErrPortsBusy) {
		t.Fatalf("err = %v, want ErrPortsBusy", err)
	}
}

// A busy first port should be stepped over rather than ending the sign-in.
func TestAuthorizeFallsBackToNextPort(t *testing.T) {
	f := newFakeCloudflare(t)
	busy, busyPort := grabPort(t)
	defer busy.Close()
	free, freePort := grabPort(t)
	free.Close() // free it again; we only wanted a port number nothing holds

	var used string
	open := func(rawURL string) error {
		u, _ := url.Parse(rawURL)
		used = u.Query().Get("redirect_uri")
		return browserThatApproves(t, "code-xyz")(rawURL)
	}
	cfg := testConfig(t, f, open)
	cfg.Ports = []int{busyPort, freePort}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cfg.Authorize(ctx); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !strings.Contains(used, ":"+itoa(freePort)+"/") {
		t.Errorf("redirect_uri = %q, want it to use the free port %d", used, freePort)
	}
}

func TestRevokeSendsToken(t *testing.T) {
	f := newFakeCloudflare(t)
	cfg := testConfig(t, f, nil)
	if err := cfg.Revoke(context.Background(), "tok-abc"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got := f.gotForm.Get("token"); got != "tok-abc" {
		t.Errorf("revoked token = %q", got)
	}
}

// Revoking nothing is not an error: setup calls Revoke on a cleanup path that
// can run before a token was ever obtained.
func TestRevokeEmptyTokenIsNoOp(t *testing.T) {
	f := newFakeCloudflare(t)
	cfg := testConfig(t, f, nil)
	if err := cfg.Revoke(context.Background(), ""); err != nil {
		t.Fatalf("Revoke(\"\") = %v, want nil", err)
	}
	if f.gotForm != nil {
		t.Error("an empty token still called the endpoint")
	}
}

func TestConfigRequiresClientIDAndScopes(t *testing.T) {
	ctx := context.Background()
	if _, err := (Config{Scopes: []string{"r2:write"}}).Authorize(ctx); err == nil {
		t.Error("want an error with no client ID")
	}
	if _, err := (Config{ClientID: "x"}).Authorize(ctx); err == nil {
		t.Error("want an error with no scopes")
	}
}

// error_description arrives on a URL anything local can construct, and it is
// rendered into the page, so it must not be able to carry markup in.
func TestCallbackPageEscapesDescription(t *testing.T) {
	rec := httptest.NewRecorder()
	writePage(rec, http.StatusBadRequest, "Refused", `<script>alert(1)</script>`)
	body := rec.Body.String()
	if strings.Contains(body, "<script>") {
		t.Errorf("description was not escaped: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("want the escaped form in the page: %s", body)
	}
}

func TestPKCEValuesAreFreshAndWellFormed(t *testing.T) {
	a, err := newPKCE()
	if err != nil {
		t.Fatalf("newPKCE: %v", err)
	}
	b, err := newPKCE()
	if err != nil {
		t.Fatalf("newPKCE: %v", err)
	}
	if a.verifier == b.verifier {
		t.Error("two verifiers came back identical")
	}
	// RFC 7636 puts the verifier between 43 and 128 characters.
	if len(a.verifier) < 43 || len(a.verifier) > 128 {
		t.Errorf("verifier length = %d, outside RFC 7636's 43..128", len(a.verifier))
	}
	if strings.ContainsAny(a.verifier, "+/=") {
		t.Errorf("verifier %q is not base64url without padding", a.verifier)
	}
	if strings.ContainsAny(a.challenge, "+/=") {
		t.Errorf("challenge %q is not base64url without padding", a.challenge)
	}
}

func TestStateIsFresh(t *testing.T) {
	a, err := newState()
	if err != nil {
		t.Fatalf("newState: %v", err)
	}
	b, err := newState()
	if err != nil {
		t.Fatalf("newState: %v", err)
	}
	if a == b {
		t.Error("two state values came back identical")
	}
	if a == "" {
		t.Error("empty state")
	}
}

// grabPort binds a loopback port and returns the listener holding it. Callers
// either keep it open (to make that port busy) or close it immediately (to
// borrow a port number nothing is using).
func grabPort(t *testing.T) (net.Listener, int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return l, l.Addr().(*net.TCPAddr).Port
}

func itoa(n int) string { return strconv.Itoa(n) }
