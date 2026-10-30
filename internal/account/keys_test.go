package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/creds"
)

// The keys round-trip through the Worker's wire shape: PUT {"keys": ...},
// GET {"keys": ..., "updated_at": n}.
func TestKeysRoundTrip(t *testing.T) {
	var stored json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/keys" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("unexpected %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		switch r.Method {
		case http.MethodPut:
			var body struct {
				Keys json.RawMessage `json:"keys"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			stored = body.Keys
			_, _ = w.Write([]byte(`{"ok":true}`))
		case http.MethodGet:
			if stored == nil {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"no keys stored"}`))
				return
			}
			_, _ = w.Write([]byte(`{"keys":` + string(stored) + `,"updated_at":5}`))
		}
	}))
	defer server.Close()
	c := NewClient(server.URL, nil)
	ctx := context.Background()

	if _, err := c.GetKeys(ctx, "tok"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetKeys before any PUT: %v, want ErrNotFound", err)
	}
	want := StoredKeys{
		Primary: &creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: "t-1"},
		Second:  &creds.Credentials{AccountID: "a", AccessKeyID: "k", SecretAccessKey: "s", Bucket: "b"},
	}
	if err := c.PutKeys(ctx, "tok", want); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetKeys(ctx, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if *got.Primary != *want.Primary || *got.Second != *want.Second {
		t.Fatalf("got %+v / %+v", got.Primary, got.Second)
	}
}
