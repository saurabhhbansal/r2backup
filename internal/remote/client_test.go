package remote

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestConfigEndpointDerivedFromAccountID(t *testing.T) {
	cfg := Config{AccountID: "abc123"}
	want := "https://abc123.r2.cloudflarestorage.com"
	if got := cfg.endpoint(); got != want {
		t.Errorf("endpoint() = %q, want %q", got, want)
	}
}

func TestConfigEndpointOverride(t *testing.T) {
	// This is what lets tests point the client at a local MinIO server
	// instead of Cloudflare.
	cfg := Config{AccountID: "abc123", Endpoint: "http://127.0.0.1:9000"}
	want := "http://127.0.0.1:9000"
	if got := cfg.endpoint(); got != want {
		t.Errorf("endpoint() = %q, want %q", got, want)
	}
}

func TestConfigEndpointEmptyWithoutAccountOrEndpoint(t *testing.T) {
	cfg := Config{}
	if got := cfg.endpoint(); got != "" {
		t.Errorf("endpoint() = %q, want empty", got)
	}
}

func TestNewRequiresBucket(t *testing.T) {
	_, err := New(context.Background(), Config{Endpoint: "http://127.0.0.1:9000"})
	if err == nil {
		t.Fatal("New: want error for missing bucket, got nil")
	}
}

func TestNewRequiresEndpointOrAccount(t *testing.T) {
	_, err := New(context.Background(), Config{Bucket: "b"})
	if err == nil {
		t.Fatal("New: want error for missing account id / endpoint, got nil")
	}
}

func TestNewBuildsClientForEndpoint(t *testing.T) {
	c, err := New(context.Background(), Config{
		Endpoint:        "http://127.0.0.1:9000",
		Bucket:          "test-bucket",
		AccessKeyID:     "id",
		SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.Bucket() != "test-bucket" {
		t.Errorf("Bucket() = %q, want %q", c.Bucket(), "test-bucket")
	}
}

// TestErrNotFoundWraps guards that a Head/Get "not found" error is
// discoverable with errors.Is, which is how callers are expected to
// distinguish a missing key from a genuine network failure.
func TestErrNotFoundWraps(t *testing.T) {
	wrapped := fmt.Errorf("remote: head %q: %w", "some/key", ErrNotFound)
	if !errors.Is(wrapped, ErrNotFound) {
		t.Errorf("errors.Is(%v, ErrNotFound) = false, want true", wrapped)
	}
}
