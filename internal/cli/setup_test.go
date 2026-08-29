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
type fakeWorker struct {
	mu       sync.Mutex
	code     string
	token    string
	vault    *account.EncryptedVault
	devices  []string
	requests []string
}

func newFakeWorker(t *testing.T) (*fakeWorker, func()) {
	t.Helper()
	w := &fakeWorker{code: "123456", token: "tok-abc"}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.requests = append(w.requests, r.Method+" "+r.URL.Path)

		authed := r.Header.Get("Authorization") == "Bearer "+w.token
		switch {
		case r.URL.Path == "/auth/request":
			rw.WriteHeader(http.StatusOK)
			_, _ = rw.Write([]byte(`{"ok":true}`))
		case r.URL.Path == "/auth/verify":
			var in struct{ Email, Code string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.Code != w.code {
				// 400, matching worker/src/handlers/auth.ts: the client maps
				// a 400 on this path -- and only this path -- to
				// ErrInvalidCode, so a fake that answered 401 would test a
				// retry loop the real server never reaches.
				rw.WriteHeader(http.StatusBadRequest)
				_, _ = rw.Write([]byte(`{"error":"invalid or expired code"}`))
				return
			}
			_ = json.NewEncoder(rw).Encode(map[string]string{"token": w.token})
		case r.URL.Path == "/vault" && r.Method == http.MethodGet:
			if !authed {
				rw.WriteHeader(http.StatusUnauthorized)
				return
			}
			if w.vault == nil {
				rw.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(rw).Encode(w.vault)
		case r.URL.Path == "/vault" && r.Method == http.MethodPut:
			if !authed {
				rw.WriteHeader(http.StatusUnauthorized)
				return
			}
			var v account.EncryptedVault
			if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
				rw.WriteHeader(http.StatusBadRequest)
				return
			}
			w.vault = &v
			rw.WriteHeader(http.StatusOK)
			_, _ = rw.Write([]byte(`{"ok":true}`))
		case r.URL.Path == "/devices" && r.Method == http.MethodPost:
			if !authed {
				rw.WriteHeader(http.StatusUnauthorized)
				return
			}
			var in struct {
				DeviceName string `json:"device_name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			w.devices = append(w.devices, in.DeviceName)
			rw.WriteHeader(http.StatusOK)
			_, _ = rw.Write([]byte(`{"ok":true}`))
		default:
			rw.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Setenv(EnvAccountAPI, srv.URL)
	return w, srv.Close
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
	token, err := signIn(context.Background(), newPrompter(opts), accountClient())
	if err != nil {
		t.Fatalf("signIn: %v", err)
	}
	if token != "" {
		t.Fatalf("token = %q, want empty: an account is optional", token)
	}
	if !strings.Contains(said(opts), "stay on this computer") {
		t.Errorf("declining an account should say what that means; got:\n%s", said(opts))
	}
}

func TestSignInForgivesAMistypedCode(t *testing.T) {
	w, stop := newFakeWorker(t)
	defer stop()
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())

	// A wrong code should cost a keystroke, not the whole command: a user who
	// has to re-run `setup` also has to wait for a second email.
	opts := conversation("me@example.com", "000000", "123456")
	token, err := signIn(context.Background(), newPrompter(opts), accountClient())
	if err != nil {
		t.Fatalf("signIn: %v", err)
	}
	if token != w.token {
		t.Fatalf("token = %q, want %q", token, w.token)
	}
	if !strings.Contains(said(opts), "That code is not right") {
		t.Errorf("the retry should be explained; got:\n%s", said(opts))
	}
	// And the token is cached, so `setup --keys` later does not re-mail.
	if got, _ := account.LoadToken(); got != w.token {
		t.Errorf("cached token = %q, want %q", got, w.token)
	}
}

// TestCredentialsReachTheSecondComputer is the whole feature in one test: what
// the first computer stores is exactly what the second one gets back, with
// nothing but an email address and a password carried between them.
func TestCredentialsReachTheSecondComputer(t *testing.T) {
	w, stop := newFakeWorker(t)
	defer stop()
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	ctx := context.Background()

	want := creds.Credentials{
		AccountID:       "acct-1",
		AccessKeyID:     "key-1",
		SecretAccessKey: "secret-1",
		Bucket:          "backups",
	}

	// First computer: store them.
	push := conversation("hunter2hunter2", "hunter2hunter2")
	if err := pushCredentials(ctx, newPrompter(push), accountClient(), w.token, want); err != nil {
		t.Fatalf("pushCredentials: %v", err)
	}
	if w.vault == nil {
		t.Fatal("nothing was stored")
	}
	// The server must hold ciphertext, not credentials. If the secret key is
	// findable in what was uploaded, the encryption is decorative.
	blob, _ := json.Marshal(w.vault)
	if bytes.Contains(blob, []byte("secret-1")) {
		t.Fatal("the stored vault contains the plaintext secret key")
	}

	// Second computer: a fresh data dir, nothing local at all.
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	a, err := openApp()
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer a.close()

	pull := conversation("hunter2hunter2")
	done, err := pullCredentials(ctx, a, newPrompter(pull), accountClient(), w.token)
	if err != nil {
		t.Fatalf("pullCredentials: %v", err)
	}
	if !done {
		t.Fatal("pullCredentials reported nothing stored, but the vault is there")
	}
	got, err := a.creds.Load()
	if err != nil {
		t.Fatalf("load credentials on the second computer: %v", err)
	}
	if got != want {
		t.Fatalf("second computer got %+v, want %+v", got, want)
	}
}

func TestPullForgivesAMistypedPassword(t *testing.T) {
	w, stop := newFakeWorker(t)
	defer stop()
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	ctx := context.Background()

	push := conversation("hunter2hunter2", "hunter2hunter2")
	if err := pushCredentials(ctx, newPrompter(push), accountClient(), w.token,
		creds.Credentials{AccountID: "a", AccessKeyID: "k", SecretAccessKey: "s", Bucket: "b"}); err != nil {
		t.Fatalf("pushCredentials: %v", err)
	}

	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	a, err := openApp()
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer a.close()

	pull := conversation("wrongwrong", "hunter2hunter2")
	done, err := pullCredentials(ctx, a, newPrompter(pull), accountClient(), w.token)
	if err != nil {
		t.Fatalf("pullCredentials: %v", err)
	}
	if !done {
		t.Fatal("a corrected password should still unlock the vault")
	}
	if !strings.Contains(said(pull), "does not open it") {
		t.Errorf("the retry should be explained; got:\n%s", said(pull))
	}
}

// A wrong password is not a data-loss event and must not read like one.
func TestAGivenUpPasswordPointsAtTheWayOut(t *testing.T) {
	w, stop := newFakeWorker(t)
	defer stop()
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	ctx := context.Background()

	push := conversation("hunter2hunter2", "hunter2hunter2")
	if err := pushCredentials(ctx, newPrompter(push), accountClient(), w.token,
		creds.Credentials{AccountID: "a", AccessKeyID: "k", SecretAccessKey: "s", Bucket: "b"}); err != nil {
		t.Fatalf("pushCredentials: %v", err)
	}

	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	a, err := openApp()
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer a.close()

	pull := conversation("no", "nope", "still no")
	if _, err := pullCredentials(ctx, a, newPrompter(pull), accountClient(), w.token); err == nil {
		t.Fatal("three wrong passwords should give up")
	} else {
		msg := err.Error()
		if !strings.Contains(msg, "Nothing is lost") || !strings.Contains(msg, "setup --keys") {
			t.Errorf("the message should say nothing is lost and name the way out; got:\n%s", msg)
		}
	}
}

// The first computer on an account: the vault is genuinely absent, which is
// not an error and must not be reported as one.
func TestFirstComputerIsToldWhyItMustTypeTheKeys(t *testing.T) {
	w, stop := newFakeWorker(t)
	defer stop()
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())

	a, err := openApp()
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer a.close()

	opts := conversation()
	done, err := pullCredentials(context.Background(), a, newPrompter(opts), accountClient(), w.token)
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
