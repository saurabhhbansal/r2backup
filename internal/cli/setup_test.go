package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/account"
	"github.com/saurabhhbansal/r2backup/internal/creds"
)

// fakeWorker stands in for the account service. It is deliberately a real
// HTTP server rather than a stubbed account.Client: the two-computer flow is
// the thing under test, and half of it is what actually crosses the wire.
//
// It speaks the PIN protocol of worker/src/handlers/pin.ts: a correct code
// returns a PIN-step token, the PIN (or its first setting) returns a session
// and a key share, and /keys holds sealed bytes it never opens.
type fakeWorker struct {
	mu       sync.Mutex
	code     string
	pinToken string
	token    string
	// legacyToken is a session from before PINs: refused everywhere with
	// "pin required" until it sets one.
	legacyToken string

	pinSet bool
	salt   string
	kdf    json.RawMessage
	proof  string
	share  string

	wrong    int               // wrong PINs in a row; five locks the account

	sealed   map[string]string // {ciphertext, nonce}; nil when nothing stored
	devices  []string
	requests []string
}

func newFakeWorker(t *testing.T) (*fakeWorker, func()) {
	t.Helper()
	w := &fakeWorker{
		code: "123456", pinToken: "pin-tok", token: "tok-abc", legacyToken: "legacy-tok",
		share: "c2hhcmUtc2hhcmUtc2hhcmUtc2hhcmUtc2hhcmUtMzI",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.requests = append(w.requests, r.Method+" "+r.URL.Path)
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		reply := func(status int, v any) {
			rw.WriteHeader(status)
			_ = json.NewEncoder(rw).Encode(v)
		}
		session := func() {
			reply(http.StatusOK, map[string]string{"token": w.token, "key_share": w.share})
		}
		// Every endpoint past sign-in, as authenticate() in middleware.ts.
		authed := func() bool {
			switch {
			case bearer == w.token && w.pinSet:
				return true
			case bearer == w.legacyToken && !w.pinSet:
				reply(http.StatusForbidden, map[string]string{"error": "pin required"})
			default:
				reply(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			}
			return false
		}

		switch {
		case r.URL.Path == "/auth/request":
			reply(http.StatusOK, map[string]bool{"ok": true})
		case r.URL.Path == "/auth/verify":
			var in struct{ Email, Code string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.Code != w.code {
				// 400, matching worker/src/handlers/auth.ts: the client maps
				// a 400 on this path -- and only this path -- to
				// ErrInvalidCode.
				reply(http.StatusBadRequest, map[string]string{"error": "invalid or expired code"})
				return
			}
			out := map[string]any{"pin_token": w.pinToken, "pin_set": w.pinSet}
			if w.pinSet {
				out["salt"], out["kdf_params"] = w.salt, w.kdf
			}
			reply(http.StatusOK, out)
		case r.URL.Path == "/auth/pin/setup":
			if bearer != w.pinToken && bearer != w.legacyToken {
				reply(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
			if w.pinSet {
				reply(http.StatusConflict, map[string]string{"error": "pin already set"})
				return
			}
			var in struct {
				Salt  string          `json:"salt"`
				KDF   json.RawMessage `json:"kdf_params"`
				Proof string          `json:"proof"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			w.pinSet, w.salt, w.kdf, w.proof = true, in.Salt, in.KDF, in.Proof
			session()
		case r.URL.Path == "/auth/pin":
			if bearer != w.pinToken {
				reply(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
			if w.wrong >= 5 {
				reply(http.StatusTooManyRequests, map[string]any{"error": "locked", "retry_after": 900})
				return
			}
			var in struct{ Proof string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.Proof != w.proof {
				w.wrong++
				if w.wrong >= 5 {
					reply(http.StatusTooManyRequests, map[string]any{"error": "locked", "retry_after": 900})
					return
				}
				reply(http.StatusUnauthorized, map[string]any{"error": "wrong pin", "tries_left": 5 - w.wrong})
				return
			}
			w.wrong = 0
			session()
		case r.URL.Path == "/auth/pin/reset":
			// Forgot the PIN: a new one, and the keys locked with the old
			// one are gone.
			if bearer != w.pinToken {
				reply(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
			var in struct {
				Salt  string          `json:"salt"`
				KDF   json.RawMessage `json:"kdf_params"`
				Proof string          `json:"proof"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			w.pinSet, w.salt, w.kdf, w.proof, w.wrong, w.sealed = true, in.Salt, in.KDF, in.Proof, 0, nil
			session()
		case r.URL.Path == "/keys" && r.Method == http.MethodGet:
			if !authed() {
				return
			}
			if w.sealed == nil {
				reply(http.StatusNotFound, map[string]string{"error": "no keys stored"})
				return
			}
			reply(http.StatusOK, map[string]any{"sealed": w.sealed, "updated_at": 1})
		case r.URL.Path == "/keys" && r.Method == http.MethodPut:
			if !authed() {
				return
			}
			var in struct {
				Sealed map[string]string `json:"sealed"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Sealed == nil {
				reply(http.StatusBadRequest, map[string]string{"error": "invalid request"})
				return
			}
			w.sealed = in.Sealed
			reply(http.StatusOK, map[string]bool{"ok": true})
		case r.URL.Path == "/devices" && r.Method == http.MethodPost:
			if !authed() {
				return
			}
			var in struct {
				DeviceName string `json:"device_name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			w.devices = append(w.devices, in.DeviceName)
			reply(http.StatusOK, map[string]bool{"ok": true})
		case r.URL.Path == "/devices" && r.Method == http.MethodGet:
			if !authed() {
				return
			}
			reply(http.StatusOK, map[string]any{"devices": []any{}})
		default:
			rw.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Setenv(EnvAccountAPI, srv.URL)
	return w, srv.Close
}

// testPIN is the PIN the tests' accounts use.
const testPIN = "246810"

// storeKeys sets up the fake account the way a first computer would have:
// a PIN, and k sealed under it.
func storeKeys(t *testing.T, k account.StoredKeys) {
	t.Helper()
	ctx := context.Background()
	c := accountClient()
	ch, err := c.Verify(ctx, "me@example.com", "123456")
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.SetupPIN(ctx, ch.PINToken, testPIN)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PutKeys(ctx, s.Token, s.SealKey, k); err != nil {
		t.Fatal(err)
	}
}

// conversation drives a prompter from a script of typed lines.
func conversation(lines ...string) *Options {
	return &Options{
		Out: &bytes.Buffer{},
		Err: &bytes.Buffer{},
		In:  strings.NewReader(strings.Join(lines, "\n") + "\n"),
	}
}

func said(opts *Options) string { return opts.Out.(*bytes.Buffer).String() }

func TestSignInSkipsTheAccountOnABlankEmail(t *testing.T) {
	_, stop := newFakeWorker(t)
	defer stop()
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())

	opts := conversation("")
	s, err := signIn(context.Background(), newPrompter(opts), accountClient())
	if err != nil {
		t.Fatalf("signIn: %v", err)
	}
	if s != nil {
		t.Fatalf("session = %+v, want none: an account is optional", s)
	}
	if !strings.Contains(said(opts), "stay on this computer") {
		t.Errorf("declining an account should say what that means; got:\n%s", said(opts))
	}
	if accountMode() != accountModeLocal {
		t.Errorf("mode = %q, want local", accountMode())
	}
}

func TestSignInForgivesAMistypedCodeAndSetsAPIN(t *testing.T) {
	w, stop := newFakeWorker(t)
	defer stop()
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())

	// A wrong code should cost a keystroke, not the whole command: a user who
	// has to re-run `setup` also has to wait for a second email. A new
	// account then chooses its PIN, and a mistyped confirmation is asked
	// again.
	opts := conversation("me@example.com", "000000", "123456", "12345", testPIN, "999999", testPIN, testPIN)
	s, err := signIn(context.Background(), newPrompter(opts), accountClient())
	if err != nil {
		t.Fatalf("signIn: %v\n%s", err, said(opts))
	}
	if s.Token != w.token {
		t.Fatalf("token = %q, want %q", s.Token, w.token)
	}
	for _, want := range []string{"That code is not right", "6 digits", "did not match"} {
		if !strings.Contains(said(opts), want) {
			t.Errorf("output is missing %q; got:\n%s", want, said(opts))
		}
	}
	// The session is cached, key and all, so `setup --keys` later does not
	// re-mail.
	if got, _ := account.LoadToken(); got != w.token {
		t.Errorf("cached token = %q, want %q", got, w.token)
	}
	if _, err := account.LoadSealKey(); err != nil {
		t.Errorf("seal key not saved: %v", err)
	}
	if len(w.devices) == 0 {
		t.Error("signing in did not register the computer")
	}
}

// TestCredentialsReachTheSecondComputer is the whole feature in one test: what
// the first computer stores is exactly what the second one gets back, with
// nothing but an email address and a PIN carried between them.
func TestCredentialsReachTheSecondComputer(t *testing.T) {
	w, stop := newFakeWorker(t)
	defer stop()
	ctx := context.Background()

	want := creds.Credentials{
		AccountID:       "acct-1",
		AccessKeyID:     "key-1",
		SecretAccessKey: "secret-1",
		Bucket:          "backups",
	}

	// First computer: sign in, choosing the PIN, and store them.
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	first := conversation("me@example.com", "123456", testPIN, testPIN)
	s, err := signIn(ctx, newPrompter(first), accountClient())
	if err != nil {
		t.Fatalf("signIn: %v", err)
	}
	a1, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a1.close()
	if err := offerToStore(ctx, a1, newPrompter(conversation()), accountClient(), s, want); err != nil {
		t.Fatalf("offerToStore: %v", err)
	}
	if w.sealed == nil {
		t.Fatal("nothing was stored")
	}
	// The server must hold ciphertext, not credentials. If the secret key is
	// findable in what was uploaded, the encryption is decorative.
	blob, _ := json.Marshal(w.sealed)
	if bytes.Contains(blob, []byte("secret-1")) {
		t.Fatal("the stored keys contain the plaintext secret key")
	}

	// Second computer: a fresh data dir, nothing local at all. A wrong PIN
	// is forgiven.
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	a, err := openApp()
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer a.close()

	second := conversation("me@example.com", "123456", "111111", testPIN)
	s2, err := signIn(ctx, newPrompter(second), accountClient())
	if err != nil {
		t.Fatalf("signIn on the second computer: %v\n%s", err, said(second))
	}
	if !strings.Contains(said(second), "PIN is not right") {
		t.Errorf("the wrong PIN should be explained; got:\n%s", said(second))
	}
	done, err := pullCredentials(ctx, a, newPrompter(second), accountClient(), s2)
	if err != nil {
		t.Fatalf("pullCredentials: %v", err)
	}
	if !done {
		t.Fatal("pullCredentials reported nothing stored, but the keys are there")
	}
	got, err := a.creds.Load()
	if err != nil {
		t.Fatalf("load credentials on the second computer: %v", err)
	}
	if got != want {
		t.Fatalf("second computer got %+v, want %+v", got, want)
	}
	// The README promises that setup says which protection a computer got.
	if !strings.Contains(said(second), "guarded by") {
		t.Errorf("pullCredentials did not say what protects the secret; got:\n%s", said(second))
	}
}

// A computer signed in before PINs is asked to choose one, and its session
// then works.
func TestASessionFromBeforePINsSetsOne(t *testing.T) {
	w, stop := newFakeWorker(t)
	defer stop()
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	if err := account.SaveToken(w.legacyToken); err != nil {
		t.Fatal(err)
	}
	opts := conversation(testPIN, testPIN)
	s, err := signIn(context.Background(), newPrompter(opts), accountClient())
	if err != nil {
		t.Fatalf("signIn: %v\n%s", err, said(opts))
	}
	if s.Token != w.token || !w.pinSet {
		t.Fatalf("session %q, pin set %v", s.Token, w.pinSet)
	}
	if !strings.Contains(said(opts), "Choose one") {
		t.Errorf("should say why it asks; got:\n%s", said(opts))
	}
}

// The first computer on an account: nothing is stored, which is not an error
// and must not be reported as one.
func TestFirstComputerIsToldWhyItMustTypeTheKeys(t *testing.T) {
	_, stop := newFakeWorker(t)
	defer stop()
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())

	a, err := openApp()
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer a.close()

	s, err := signIn(context.Background(), newPrompter(conversation("me@example.com", "123456", testPIN, testPIN)), accountClient())
	if err != nil {
		t.Fatal(err)
	}
	opts := conversation()
	done, err := pullCredentials(context.Background(), a, newPrompter(opts), accountClient(), s)
	if err != nil {
		t.Fatalf("an empty account is not an error: %v", err)
	}
	if done {
		t.Fatal("pullCredentials claimed to have found credentials that were never stored")
	}
	if !strings.Contains(said(opts), "first computer") {
		t.Errorf("the user should be told why they are being asked; got:\n%s", said(opts))
	}
}
