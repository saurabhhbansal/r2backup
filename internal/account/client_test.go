package account

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequestCodeSuccess(t *testing.T) {
	var gotPath, gotEmail string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body struct {
			Email string `json:"email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		gotEmail = body.Email
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	c := NewClient(server.URL, nil)
	if err := c.RequestCode(context.Background(), "user@example.com"); err != nil {
		t.Fatalf("RequestCode: %v", err)
	}
	if gotPath != "/auth/request" {
		t.Errorf("path = %q, want /auth/request", gotPath)
	}
	if gotEmail != "user@example.com" {
		t.Errorf("email = %q, want user@example.com", gotEmail)
	}
}

func TestRequestCodeIdenticalForUnknownAndKnownEmail(t *testing.T) {
	// The client can't observe anything the Worker doesn't expose, but it
	// must not itself introduce a distinction the Worker doesn't make --
	// e.g. by treating some status code specially only for certain emails.
	// This test pins that RequestCode's outcome depends only on the HTTP
	// response, never on the email string itself.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	c := NewClient(server.URL, nil)
	err1 := c.RequestCode(context.Background(), "definitely-not-registered@example.com")
	err2 := c.RequestCode(context.Background(), "probably-registered@example.com")
	if err1 != nil || err2 != nil {
		t.Fatalf("RequestCode errors = %v, %v; want nil, nil", err1, err2)
	}
}

func TestVerifySuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/verify" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token":"the-jwt"}`))
	}))
	defer server.Close()

	c := NewClient(server.URL, nil)
	token, err := c.Verify(context.Background(), "user@example.com", "123456")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if token != "the-jwt" {
		t.Errorf("token = %q, want the-jwt", token)
	}
}

func TestVerifyWrongCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid or expired code"}`))
	}))
	defer server.Close()

	c := NewClient(server.URL, nil)
	_, err := c.Verify(context.Background(), "user@example.com", "000000")
	if !errors.Is(err, ErrInvalidCode) {
		t.Errorf("err = %v, want ErrInvalidCode", err)
	}
}

func TestVerifyExpiredCode(t *testing.T) {
	// The Worker reports an expired code with the exact same 400 body as a
	// wrong one (see worker/src/handlers/auth.ts) -- there is deliberately
	// no separate signal to distinguish them, so the client can't either.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid or expired code"}`))
	}))
	defer server.Close()

	c := NewClient(server.URL, nil)
	_, err := c.Verify(context.Background(), "user@example.com", "123456")
	if !errors.Is(err, ErrInvalidCode) {
		t.Errorf("err = %v, want ErrInvalidCode", err)
	}
}

func TestRequestCodeRateLimited(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"too many requests"}`))
	}))
	defer server.Close()

	c := NewClient(server.URL, nil)
	err := c.RequestCode(context.Background(), "hammered@example.com")
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("err = %v, want ErrRateLimited", err)
	}
}

func TestServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal error"}`))
	}))
	defer server.Close()

	c := NewClient(server.URL, nil)
	err := c.RequestCode(context.Background(), "user@example.com")
	if !errors.Is(err, ErrServer) {
		t.Errorf("err = %v, want ErrServer", err)
	}
}

func TestGetVaultNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no vault stored"}`))
	}))
	defer server.Close()

	c := NewClient(server.URL, nil)
	_, err := c.GetVault(context.Background(), "a-token")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGetVaultUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer server.Close()

	c := NewClient(server.URL, nil)
	_, err := c.GetVault(context.Background(), "a-bad-token")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

// TestGetVaultDecodesBothKDFParamsWireShapes is the test for the bug a user
// actually hit: sign-in failing with "account: decode response: json:
// cannot unmarshal string into Go struct field EncryptedVault.kdf_params".
//
// worker/src/handlers/vault.ts stores kdf_params as a JSON string in D1
// (handlePutVault re-serialises it -- see the comment there) and, until the
// matching worker fix, handed that stored string straight back on GET
// /vault instead of parsing it first. Querying the live production D1
// confirmed the exact shape a real, already-signed-up account's vault row
// travels the wire in today:
//
//	{"salt":"...","ciphertext":"...","nonce":"...",
//	 "kdf_params":"{\"time\":1,\"memory_kib\":65536,\"threads\":4,\"key_len\":32}",
//	 "updated_at":1788036275}
//
// -- kdf_params double-encoded, plus an updated_at field the Go struct has
// no field for at all. This test builds exactly that shape (byte-for-byte
// modulo the actual ciphertext, which this test supplies its own copy of so
// it can prove the round trip by decrypting the result, something a fixture
// built from someone else's real vault could never do) alongside the fixed
// worker's plain-object shape, and asserts GetVault turns both into a
// KDFParams a real passphrase still opens. Losing tolerance for the first
// shape would break every client already in the wild the moment it talks to
// a server that hasn't redeployed yet, or reads a vault row nobody has
// rewritten -- which per the production query is exactly this user's row.
func TestGetVaultDecodesBothKDFParamsWireShapes(t *testing.T) {
	const passphrase = "correct horse battery staple"
	plaintext := []byte(`{"account_id":"abc","access_key_id":"AKIA...","secret_access_key":"shh","bucket":"my-bucket"}`)

	sealed, err := Encrypt(passphrase, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	saltB64 := base64.StdEncoding.EncodeToString(sealed.Salt)
	nonceB64 := base64.StdEncoding.EncodeToString(sealed.Nonce)
	ctB64 := base64.StdEncoding.EncodeToString(sealed.Ciphertext)
	kdfJSON, err := json.Marshal(sealed.KDFParams)
	if err != nil {
		t.Fatalf("marshal kdf_params: %v", err)
	}

	// The production shape: kdf_params double-encoded as a JSON string, plus
	// the updated_at column the Go struct doesn't declare a field for --
	// confirmed against the live vaults table, so this is not a guess.
	stringified := fmt.Sprintf(
		`{"salt":%q,"ciphertext":%q,"nonce":%q,"kdf_params":%q,"updated_at":1788036275}`,
		saltB64, ctB64, nonceB64, string(kdfJSON),
	)
	// The fixed shape: kdf_params as a plain object, same extra column.
	object := fmt.Sprintf(
		`{"salt":%q,"ciphertext":%q,"nonce":%q,"kdf_params":%s,"updated_at":1788036275}`,
		saltB64, ctB64, nonceB64, string(kdfJSON),
	)

	for name, body := range map[string]string{
		"stringified kdf_params (today's production shape)": stringified,
		"object kdf_params (the fixed worker shape)":        object,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()

			c := NewClient(server.URL, nil)
			got, err := c.GetVault(context.Background(), "a-token")
			if err != nil {
				t.Fatalf("GetVault: %v", err)
			}

			plain, err := Decrypt(passphrase, got)
			if err != nil {
				t.Fatalf("Decrypt on a vault fetched over this wire shape: %v", err)
			}
			if string(plain) != string(plaintext) {
				t.Errorf("plaintext = %q, want %q", plain, plaintext)
			}
		})
	}
}

func TestPutAndGetVaultRoundTrip(t *testing.T) {
	var stored EncryptedVault
	var gotAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.Method {
		case http.MethodPut:
			if err := json.NewDecoder(r.Body).Decode(&stored); err != nil {
				t.Fatalf("decode PUT body: %v", err)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case http.MethodGet:
			encoded, _ := json.Marshal(stored)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(encoded)
		}
	}))
	defer server.Close()

	c := NewClient(server.URL, nil)
	ev, err := Encrypt("a passphrase", []byte("r2 credentials json"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	if err := c.PutVault(context.Background(), "session-token", ev); err != nil {
		t.Fatalf("PutVault: %v", err)
	}
	if gotAuth != "Bearer session-token" {
		t.Errorf("Authorization header = %q, want Bearer session-token", gotAuth)
	}

	got, err := c.GetVault(context.Background(), "session-token")
	if err != nil {
		t.Fatalf("GetVault: %v", err)
	}

	plaintext, err := Decrypt("a passphrase", got)
	if err != nil {
		t.Fatalf("Decrypt round-tripped vault: %v", err)
	}
	if string(plaintext) != "r2 credentials json" {
		t.Errorf("plaintext = %q, want %q", plaintext, "r2 credentials json")
	}
}

func TestRegisterDeviceAndListDevices(t *testing.T) {
	// A real Unix-seconds timestamp, not the literal 42 this test used to
	// use: 42 renders as 1 Jan 1970 whether it's read as seconds or
	// milliseconds, so it could never have caught the two callers (see
	// internal/cli/account.go and internal/cli/dashboard_ops.go) that once
	// decoded this field with time.UnixMilli instead of time.Unix.
	wantLastSeen := time.Date(2024, time.March, 15, 9, 30, 0, 0, time.UTC).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/devices":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.Method == http.MethodGet && r.URL.Path == "/devices":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"devices":[{"device_name":"Laptop","os":"windows","last_seen":%d}]}`, wantLastSeen)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	c := NewClient(server.URL, nil)
	if err := c.RegisterDevice(context.Background(), "token", "Laptop", "windows"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	devices, err := c.ListDevices(context.Background(), "token")
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(devices) != 1 || devices[0].DeviceName != "Laptop" || devices[0].OS != "windows" {
		t.Errorf("devices = %+v, want one Laptop/windows entry", devices)
	}
	if devices[0].LastSeen != wantLastSeen {
		t.Errorf("LastSeen = %d, want %d (the field is Unix seconds, decoded straight off the wire)", devices[0].LastSeen, wantLastSeen)
	}
}
