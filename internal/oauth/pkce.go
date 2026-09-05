package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// pkce is one authorization attempt's proof that the process which asked for
// a code is the same process now redeeming it.
//
// The problem it solves is specific to a program like this one. A public
// client ships no secret -- r2b is a binary anyone can download and read, so
// there is nowhere to put one -- which means Cloudflare cannot tell r2b apart
// from any other program claiming the same client ID. Without PKCE, anything
// on the machine that managed to intercept the redirect (a browser extension,
// another local listener that won the port first) could redeem the code for a
// real token, because redeeming it would need nothing the interceptor does
// not already have.
//
// PKCE closes that by making the redemption require something that never
// travels with the code: the verifier stays in this process's memory, and only
// its hash goes out over the wire. An interceptor ends up holding a code it
// cannot spend.
type pkce struct {
	// verifier is the secret. It is sent only to the token endpoint, over
	// TLS, at the moment the code is redeemed.
	verifier string
	// challenge is sha256(verifier), which is what goes in the browser-visible
	// authorization URL. Deriving the verifier back out of it is the thing
	// SHA-256 is for.
	challenge string
}

// newPKCE generates a fresh verifier and its challenge.
//
// The verifier is 32 random bytes rendered as base64url, which lands at 43
// characters -- inside RFC 7636's 43-to-128 range, and 256 bits of entropy
// either way. There is no reason to go longer; the limit exists for URLs,
// and the security does not improve past the hash's own width.
func newPKCE() (pkce, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand failing is not a condition worth a recovery path --
		// on every platform this runs on it means the OS entropy source is
		// gone, and nothing else this program does would be trustworthy
		// either. Report it and let the caller abandon the sign-in.
		return pkce{}, fmt.Errorf("oauth: no randomness available: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return pkce{
		verifier:  verifier,
		challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// MinStateLen is the shortest state Cloudflare will accept.
//
// Not in any documentation -- it came from asking the live authorization
// endpoint, which rejects a shorter one outright with "The state is missing or
// does not have enough characters and is therefore considered too weak.
// Request parameter 'state' must be at least be 8 characters long to ensure
// sufficient entropy."
//
// newState produces far more than this and there is no reason to go near the
// floor. It is written down because the failure it causes is a bad one to
// debug: every sign-in breaks at once, at the consent screen, with an error
// that never reaches this process.
const MinStateLen = 8

// newState generates the CSRF value that ties the callback we receive back to
// the authorization we started.
//
// It is not the same job as the PKCE verifier and one does not cover for the
// other. PKCE proves the redeemer is the requester; state proves the callback
// arriving at our listener belongs to the request we made, rather than being
// one an attacker walked our browser into. Both are cheap, and the loopback
// listener is open to anything else running on the machine, so both are worth
// having.
func newState() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("oauth: no randomness available: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
