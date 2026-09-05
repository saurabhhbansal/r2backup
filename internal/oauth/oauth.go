// Package oauth signs a person in to Cloudflare from the terminal, with
// nothing to copy and nothing to paste.
//
// The shape is the one every command-line tool converges on, for the same
// reason. Cloudflare will only hand an approval back to a URL, and a terminal
// is not a URL -- so r2b briefly becomes one. It opens a web server on this
// machine's loopback address, sends the person's browser to Cloudflare, and
// waits. They click Allow, Cloudflare redirects the browser to that local
// address, and the approval lands in this process. The server then shuts down.
// It exists for a few seconds, is reachable only from this machine, and
// serves exactly one request.
//
// What this package deliberately does not do is keep anything. The token it
// returns is meant to be used once during setup -- to find the person's
// account and make their bucket -- and then handed to Revoke. r2backup's
// unattended promise is built on the R2 keys, which do not expire; a
// Cloudflare login that has to be renewed in a browser is the last thing a
// 3am scheduled run should depend on. So the sign-in is scaffolding, and the
// package is written to make throwing it away the easy path rather than an
// act of discipline. See internal/creds for what is kept instead, and why.
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Cloudflare's OAuth endpoints, as published by its own discovery document at
// https://dash.cloudflare.com/.well-known/openid-configuration. They are
// written out rather than fetched at run time: a sign-in that begins with a
// network round trip to learn three URLs that have not changed is a slower
// sign-in with one more thing to fail, and if Cloudflare ever moves them a
// hardcoded constant fails loudly here instead of quietly somewhere else.
const (
	DefaultAuthURL   = "https://dash.cloudflare.com/oauth2/auth"
	DefaultTokenURL  = "https://dash.cloudflare.com/oauth2/token"
	DefaultRevokeURL = "https://dash.cloudflare.com/oauth2/revoke"
)

// DefaultPorts are the loopback ports the callback server will try, in order.
//
// There is more than one because a redirect URL has to match what was
// registered with Cloudflare character for character, which rules out asking
// the OS for any free port. A single fixed port would mean sign-in simply
// fails whenever something else on the machine happens to hold it -- an
// unlucky collision with no path forward for the person hitting it. Three
// registered alternatives make that vanishingly unlikely while keeping every
// redirect URL exact.
//
// Every port here must also be registered on the Cloudflare OAuth client as
// http://127.0.0.1:<port>/callback. Adding one here without adding it there
// produces an invalid_request at the consent screen.
var DefaultPorts = []int{53682, 53683, 53684}

// callbackPath is the path component of the redirect URL. It is part of the
// registered string, so it is a constant rather than anything configurable.
const callbackPath = "/callback"

// Sentinel errors a caller can match with errors.Is.
var (
	// ErrDenied means the person reached the consent screen and said no.
	// It is not a failure and callers should not present it as one -- the
	// right response is to fall back to entering keys by hand.
	ErrDenied = errors.New("oauth: sign-in was declined")

	// ErrPortsBusy means every port in Ports was already taken. Sign-in
	// cannot proceed, because an unregistered port would be rejected by
	// Cloudflare anyway.
	ErrPortsBusy = errors.New("oauth: no free loopback port for the sign-in callback")

	// ErrMismatchedState means the callback that arrived did not carry the
	// state value we sent. Something other than the authorization we began
	// is talking to our listener, so the exchange is abandoned without
	// redeeming the code.
	ErrMismatchedState = errors.New("oauth: sign-in response did not match the request")

	// ErrNoToken means Cloudflare returned success with a body that had no
	// access token in it -- a shape this client does not understand.
	ErrNoToken = errors.New("oauth: no access token in the response")
)

// Config describes one client's sign-in. The zero value is not usable;
// ClientID and Scopes must be set.
type Config struct {
	// ClientID identifies this application to Cloudflare. It is not a
	// secret: a public client ships no secret at all, which is why PKCE
	// carries the weight instead, so this being readable in the binary is
	// the design rather than a leak.
	ClientID string

	// Scopes are the permissions to request, as Cloudflare's scope
	// identifiers. They must be a subset of what the client is registered
	// for; asking for more than was registered is rejected at the consent
	// screen rather than silently narrowed.
	Scopes []string

	// AuthURL, TokenURL and RevokeURL default to the Default* constants
	// when empty. Tests point them at a local server.
	AuthURL   string
	TokenURL  string
	RevokeURL string

	// Ports defaults to DefaultPorts when empty.
	Ports []int

	// OpenBrowser defaults to the platform opener. Tests replace it, both
	// to avoid launching a real browser and to drive the callback.
	OpenBrowser func(rawURL string) error

	// HTTPClient defaults to one with a timeout. The default http.Client
	// has none, and a token exchange that hangs forever would hang the
	// whole sign-in with no way out but Ctrl-C.
	HTTPClient *http.Client
}

// Token is a Cloudflare access token and when it stops working.
//
// There is no refresh token here, on purpose. This package does not ask for
// offline access, so Cloudflare does not issue one, so there is nothing to
// accidentally persist. The absence is the feature: it makes "keep this
// around" a thing a future caller would have to go out of its way to build
// rather than something it can drift into.
type Token struct {
	AccessToken string
	ExpiresAt   time.Time
}

// Authorize runs the whole sign-in and returns a token.
//
// It blocks until the person finishes in their browser, ctx is cancelled, or
// something fails. Callers should give ctx a deadline generous enough for a
// real human -- finding a password manager, a 2FA prompt, picking the right
// account out of several -- because the cost of being too impatient here is
// making someone start over.
//
// The returned token should be passed to Revoke when the caller is done with
// it. See the package comment.
func (c Config) Authorize(ctx context.Context) (*Token, error) {
	if strings.TrimSpace(c.ClientID) == "" {
		return nil, errors.New("oauth: no client ID configured")
	}
	if len(c.Scopes) == 0 {
		return nil, errors.New("oauth: no scopes requested")
	}

	listener, port, err := c.listen()
	if err != nil {
		return nil, err
	}
	defer listener.Close()

	verifier, err := newPKCE()
	if err != nil {
		return nil, err
	}
	state, err := newState()
	if err != nil {
		return nil, err
	}

	redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", port, callbackPath)

	// Buffered by one so the handler can hand off its result and return --
	// and so finishing the HTTP response is never blocked on this function
	// still being around to receive. Without the buffer, a cancelled ctx
	// would leave the person's browser hanging on a request nobody reads.
	results := make(chan callbackResult, 1)
	server := &http.Server{
		Handler:           c.handler(state, results),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		// A short deadline, not ctx: ctx may already be cancelled, and the
		// only thing left to finish is writing a small page to a browser
		// on this same machine.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	open := c.OpenBrowser
	if open == nil {
		open = openBrowser
	}
	if err := open(c.authorizeURL(redirectURI, state, verifier.challenge)); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-results:
		if res.err != nil {
			return nil, res.err
		}
		return c.exchange(ctx, res.code, verifier.verifier, redirectURI)
	}
}

// listen binds the first port in Ports that is free.
//
// It binds 127.0.0.1 explicitly rather than letting the OS choose. On a
// laptop the difference is invisible; on a shared or server machine it is the
// difference between a listener only this computer can reach and one the
// whole network can, and for the few seconds it holds an authorization in
// flight that is worth being exact about.
func (c Config) listen() (net.Listener, int, error) {
	ports := c.Ports
	if len(ports) == 0 {
		ports = DefaultPorts
	}
	for _, port := range ports {
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		return l, port, nil
	}
	return nil, 0, ErrPortsBusy
}

// authorizeURL builds the URL the browser is sent to.
func (c Config) authorizeURL(redirectURI, state, challenge string) string {
	q := url.Values{
		"client_id":             {c.ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {strings.Join(c.Scopes, " ")},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return c.authEndpoint() + "?" + q.Encode()
}

// callbackResult is what the one served request produces.
type callbackResult struct {
	code string
	err  error
}

// handler serves the single redirect Cloudflare sends the browser to.
func (c Config) handler(state string, results chan<- callbackResult) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		// The state check comes first, before anything in the request is
		// trusted or acted on. A callback that fails it is not ours, so we
		// neither read a code out of it nor say anything specific back to
		// whoever sent it.
		if q.Get("state") != state {
			writePage(w, http.StatusBadRequest, "Something went wrong",
				"This sign-in did not match the one r2backup started. Close this tab and run the command again.")
			send(results, callbackResult{err: ErrMismatchedState})
			return
		}
		if errCode := q.Get("error"); errCode != "" {
			// access_denied is the person clicking Cancel, which is a
			// choice rather than a fault, so it gets its own error and a
			// page that does not apologise for them.
			if errCode == "access_denied" {
				writePage(w, http.StatusOK, "Sign-in cancelled",
					"Nothing was changed. You can close this tab.")
				send(results, callbackResult{err: ErrDenied})
				return
			}
			desc := q.Get("error_description")
			if desc == "" {
				desc = errCode
			}
			writePage(w, http.StatusBadRequest, "Cloudflare refused the sign-in", desc)
			send(results, callbackResult{err: fmt.Errorf("oauth: %s: %s", errCode, desc)})
			return
		}
		code := q.Get("code")
		if code == "" {
			writePage(w, http.StatusBadRequest, "Something went wrong",
				"Cloudflare's reply had no authorization in it. Close this tab and run the command again.")
			send(results, callbackResult{err: errors.New("oauth: callback carried no code")})
			return
		}
		writePage(w, http.StatusOK, "Signed in",
			"You can close this tab and go back to your terminal.")
		send(results, callbackResult{code: code})
	})
	// Browsers ask for /favicon.ico unprompted, and some open a speculative
	// connection to the root. Neither is the callback, and neither should
	// be mistaken for one, so everything else gets a plain 404 and is not
	// reported to the waiting Authorize.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	return mux
}

// send delivers a result without blocking. The channel is buffered by one and
// only the first result matters -- a browser that retries the callback, or a
// stray request arriving behind the real one, must not wedge a handler.
func send(results chan<- callbackResult, res callbackResult) {
	select {
	case results <- res:
	default:
	}
}

// exchange redeems the authorization code for a token.
func (c Config) exchange(ctx context.Context, code, verifier, redirectURI string) (*Token, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {c.ClientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenEndpoint(),
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("oauth: build token request: %w", err)
	}
	// No Authorization header: this is a public client registered with
	// token_endpoint_auth_method "none", so client_id in the form is the
	// whole of its identity and PKCE is what actually proves anything.
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth: exchange the sign-in for a token: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		AccessToken      string `json:"access_token"`
		TokenType        string `json:"token_type"`
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	// A non-2xx from this endpoint still carries a JSON body describing
	// what was wrong, and that description is far more useful to a person
	// than the status code, so decode before judging the status.
	decodeErr := json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if body.Error != "" {
			desc := body.ErrorDescription
			if desc == "" {
				desc = body.Error
			}
			return nil, fmt.Errorf("oauth: Cloudflare rejected the sign-in: %s", desc)
		}
		return nil, fmt.Errorf("oauth: Cloudflare returned %s", resp.Status)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("oauth: could not read the token response: %w", decodeErr)
	}
	if body.AccessToken == "" {
		return nil, ErrNoToken
	}
	tok := &Token{AccessToken: body.AccessToken}
	if body.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	}
	return tok, nil
}

// Revoke tells Cloudflare to forget a token.
//
// Setup calls this when it is finished, which is the point: discarding the
// variable would leave a working credential alive on Cloudflare's side for
// its full lifetime, and this makes it dead the moment we stop needing it.
//
// A failure here is worth reporting but not worth failing setup over. The
// keys are already saved and the backup already works by the time this runs;
// the worst case is a token that expires on its own schedule instead of
// immediately, which is where every other tool leaves it anyway.
func (c Config) Revoke(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	form := url.Values{
		"token":     {token},
		"client_id": {c.ClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.revokeEndpoint(),
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("oauth: build revoke request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("oauth: revoke the sign-in: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("oauth: revoke returned %s", resp.Status)
	}
	return nil
}

func (c Config) authEndpoint() string {
	if c.AuthURL != "" {
		return c.AuthURL
	}
	return DefaultAuthURL
}

func (c Config) tokenEndpoint() string {
	if c.TokenURL != "" {
		return c.TokenURL
	}
	return DefaultTokenURL
}

func (c Config) revokeEndpoint() string {
	if c.RevokeURL != "" {
		return c.RevokeURL
	}
	return DefaultRevokeURL
}

func (c Config) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// writePage renders the one page a person sees in their browser.
//
// It is deliberately a self-contained string with no external anything: this
// server is alive for seconds and the tab may well be opened offline-ish, on
// a captive network, or by a browser that blocks third-party requests, and a
// sign-in confirmation that renders as unstyled fragments looks like a
// failure even when everything worked.
func writePage(w http.ResponseWriter, status int, heading, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// This page is served to a browser off a loopback address; there is no
	// reason for it to be framed, sniffed, or referred onward.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	fmt.Fprintf(w, pageTemplate, htmlEscape(heading), htmlEscape(detail))
}

const pageTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>r2backup</title>
<style>
  :root { color-scheme: light dark; }
  body {
    margin: 0; min-height: 100vh;
    display: flex; align-items: center; justify-content: center;
    font: 16px/1.5 system-ui, -apple-system, Segoe UI, Roboto, sans-serif;
    background: #fbfbfa; color: #1b1b1a;
  }
  main { max-width: 26rem; padding: 2rem; text-align: center; }
  h1 { font-size: 1.25rem; margin: 0 0 .5rem; }
  p { margin: 0; color: #5b5b57; }
  @media (prefers-color-scheme: dark) {
    body { background: #16161a; color: #ecebe8; }
    p { color: #a3a29d; }
  }
</style>
</head>
<body><main><h1>%s</h1><p>%s</p></main></body>
</html>
`

// htmlEscape escapes the few characters that matter for text dropped into an
// element body. Only error_description reaches this from outside the package,
// but it arrives on a URL anyone on this machine can construct, so it is not
// treated as trusted.
func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}
