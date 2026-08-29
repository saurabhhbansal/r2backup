package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/devices":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.Method == http.MethodGet && r.URL.Path == "/devices":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"devices":[{"device_name":"Laptop","os":"windows","last_seen":42}]}`))
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
}
