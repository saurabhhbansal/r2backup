package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/account"
	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/creds"
)

// When the keys an account kept from before PINs cannot be saved sealed, the
// sign-in is not saved either, so signing in again (which hands the keys
// back) retries the move instead of leaving them where only the server can
// read them.
func TestASignInWhoseOldKeysCannotBeSealedIsNotSaved(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/keys" && r.Method == http.MethodPut && fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	t.Setenv(EnvAccountAPI, srv.URL)

	s := &account.Session{
		Token:      "a.b.c",
		SealKey:    make([]byte, 32),
		LegacyKeys: &account.StoredKeys{Primary: &creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: "tok"}},
	}
	if err := finishSignIn(context.Background(), s); err == nil {
		t.Fatal("sign-in finished although the keys were not saved")
	}
	if tok, _ := account.LoadToken(); tok != "" {
		t.Fatal("the session was saved although the keys were not")
	}

	fail.Store(false)
	if err := finishSignIn(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if tok, _ := account.LoadToken(); tok != "a.b.c" {
		t.Fatalf("session not saved after the keys were: %q", tok)
	}
}
