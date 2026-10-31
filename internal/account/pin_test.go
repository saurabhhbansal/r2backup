package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/creds"
)

func TestValidPIN(t *testing.T) {
	for _, ok := range []string{"000000", "123456", "999999"} {
		if err := ValidPIN(ok); err != nil {
			t.Errorf("ValidPIN(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "12345", "1234567", "12a456", " 12345", "١٢٣٤٥٦"} {
		if ValidPIN(bad) == nil {
			t.Errorf("ValidPIN(%q) = nil, want an error", bad)
		}
	}
}

// pinServer is just enough of the Worker's PIN protocol to check what the
// client sends and what it does with the answers.
type pinServer struct {
	salt, proof string
	kdf         json.RawMessage
	sealed      map[string]string
	sealedRaw   []byte
	tries       int
}

const share = "c2hhcmUtc2hhcmUtc2hhcmUtc2hhcmUtc2hhcmUtMzI"

func (p *pinServer) serve(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/auth/verify":
			out := map[string]any{"pin_token": "pt", "pin_set": p.proof != ""}
			if p.proof != "" {
				out["salt"], out["kdf_params"] = p.salt, p.kdf
			}
			_ = json.NewEncoder(w).Encode(out)
		case "/auth/pin/setup":
			var in struct {
				Salt  string          `json:"salt"`
				KDF   json.RawMessage `json:"kdf_params"`
				Proof string          `json:"proof"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			p.salt, p.kdf, p.proof = in.Salt, in.KDF, in.Proof
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "full", "key_share": share,
				"legacy_keys": map[string]any{"primary": map[string]string{"provider": "drime", "drime_token": "old"}}})
		case "/auth/pin":
			var in struct{ Proof string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			if auth != "Bearer pt" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if in.Proof != p.proof {
				p.tries++
				if p.tries >= 5 {
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{"error":"too many attempts","retry_after":900}`))
					return
				}
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"wrong pin","tries_left":` + string(rune('0'+5-p.tries)) + `}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "full", "key_share": share})
		case "/keys":
			if auth != "Bearer full" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"pin required"}`))
				return
			}
			if r.Method == http.MethodPut {
				var in struct {
					Sealed map[string]string `json:"sealed"`
				}
				raw := new(bytes.Buffer)
				_, _ = raw.ReadFrom(r.Body)
				p.sealedRaw = raw.Bytes()
				_ = json.Unmarshal(raw.Bytes(), &in)
				p.sealed = in.Sealed
				_, _ = w.Write([]byte(`{"ok":true}`))
				return
			}
			if p.sealed == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sealed": p.sealed, "updated_at": 1})
		}
	}))
}

func TestPINSetupThenSignInOpensTheSameKeys(t *testing.T) {
	ps := &pinServer{}
	srv := ps.serve(t)
	defer srv.Close()
	c := NewClient(srv.URL, nil)
	ctx := context.Background()

	ch, err := c.Verify(ctx, "a@b.com", "123456")
	if err != nil || ch.PINSet {
		t.Fatalf("Verify: %+v %v", ch, err)
	}
	s1, err := c.SetupPIN(ctx, ch.PINToken, "135790")
	if err != nil {
		t.Fatal(err)
	}
	if s1.LegacyKeys == nil || s1.LegacyKeys.Primary.DrimeToken != "old" {
		t.Errorf("legacy keys not handed back: %+v", s1.LegacyKeys)
	}
	if strings.Contains(ps.proof, "135790") || len(s1.SealKey) != SealKeySize {
		t.Fatalf("proof %q, key %d bytes", ps.proof, len(s1.SealKey))
	}

	want := StoredKeys{
		Primary: &creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: "drime-secret"},
		Second:  &creds.Credentials{AccountID: "a", AccessKeyID: "k", SecretAccessKey: "s3-secret", Bucket: "b"},
	}
	if err := c.PutKeys(ctx, s1.Token, s1.SealKey, want); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ps.sealedRaw, []byte("drime-secret")) || bytes.Contains(ps.sealedRaw, []byte("s3-secret")) {
		t.Fatal("the server was sent the keys in the clear")
	}

	// Another computer: same PIN, same key.
	ch2, err := c.Verify(ctx, "a@b.com", "123456")
	if err != nil || !ch2.PINSet {
		t.Fatalf("Verify: %+v %v", ch2, err)
	}
	s2, err := c.VerifyPIN(ctx, ch2, "135790")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s1.SealKey, s2.SealKey) {
		t.Fatal("the same PIN made a different key")
	}
	got, err := c.GetKeys(ctx, s2.Token, s2.SealKey)
	if err != nil {
		t.Fatal(err)
	}
	if *got.Primary != *want.Primary || *got.Second != *want.Second {
		t.Fatalf("got %+v", got)
	}

	// A different key does not open them.
	other := bytes.Repeat([]byte{1}, SealKeySize)
	if _, err := c.GetKeys(ctx, s2.Token, other); !errors.Is(err, ErrDecryptFailed) {
		t.Fatalf("wrong key: %v, want ErrDecryptFailed", err)
	}
}

func TestWrongPINsAndTheLock(t *testing.T) {
	ps := &pinServer{}
	srv := ps.serve(t)
	defer srv.Close()
	c := NewClient(srv.URL, nil)
	ctx := context.Background()
	ch, _ := c.Verify(ctx, "a@b.com", "1")
	if _, err := c.SetupPIN(ctx, ch.PINToken, "111111"); err != nil {
		t.Fatal(err)
	}
	ch, _ = c.Verify(ctx, "a@b.com", "1")
	if _, err := c.VerifyPIN(ctx, ch, "12345"); err == nil {
		t.Fatal("a five-digit PIN was sent")
	}
	var wrong *WrongPINError
	for i := 0; i < 4; i++ {
		if _, err := c.VerifyPIN(ctx, ch, "222222"); !errors.As(err, &wrong) {
			t.Fatalf("try %d: %v, want WrongPINError", i, err)
		}
	}
	if wrong.TriesLeft != 1 {
		t.Errorf("tries left = %d, want 1", wrong.TriesLeft)
	}
	var locked *PINLockedError
	if _, err := c.VerifyPIN(ctx, ch, "222222"); !errors.As(err, &locked) || locked.RetryAfterSeconds != 900 {
		t.Fatalf("fifth: %v, want PINLockedError 900", err)
	}
	if !strings.Contains(locked.Error(), "15 minutes") {
		t.Errorf("lock message = %q", locked.Error())
	}
}

func TestPINRequiredIsItsOwnError(t *testing.T) {
	ps := &pinServer{}
	srv := ps.serve(t)
	defer srv.Close()
	_, err := NewClient(srv.URL, nil).GetKeys(context.Background(), "legacy", make([]byte, SealKeySize))
	if !errors.Is(err, ErrPINRequired) {
		t.Fatalf("err = %v, want ErrPINRequired", err)
	}
}

func TestKeyFileRoundTrip(t *testing.T) {
	want := StoredKeys{Primary: &creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: "tok"}}
	if _, err := ExportKeys("short", want); err == nil {
		t.Fatal("a short passphrase was accepted")
	}
	if _, err := ExportKeys("long enough", StoredKeys{}); err == nil {
		t.Fatal("an empty export was accepted")
	}
	data, err := ExportKeys("long enough", want)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"tok"`)) {
		t.Fatal("the key file holds the token in the clear")
	}
	if _, err := ImportKeys("wrong passphrase", data); !errors.Is(err, ErrDecryptFailed) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if _, err := ImportKeys("long enough", []byte(`{"hello":1}`)); !errors.Is(err, ErrNotKeyFile) {
		t.Fatalf("not a key file: %v", err)
	}
	got, err := ImportKeys("long enough", data)
	if err != nil {
		t.Fatal(err)
	}
	if *got.Primary != *want.Primary {
		t.Fatalf("got %+v", got.Primary)
	}
}
