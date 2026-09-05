package cfapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   string
}

// fakeCloudflare serves canned responses and records what was asked for.
type fakeCloudflare struct {
	*httptest.Server
	got    []recordedRequest
	status int
	body   string
}

func newFake(t *testing.T) *fakeCloudflare {
	t.Helper()
	f := &fakeCloudflare{status: http.StatusOK, body: `{"success":true,"errors":[],"result":[]}`}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.got = append(f.got, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Auth:   r.Header.Get("Authorization"),
			Body:   string(raw),
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
	}))
	t.Cleanup(f.Close)
	return f
}

func clientFor(f *fakeCloudflare) *Client {
	c := New("tok-abc")
	c.SetBaseURL(f.URL)
	return c
}

// The object scopes are the ones that could read someone's backed-up files.
// This token has no business with them, and the consent screen should not ask.
func TestScopesExcludeObjectAccess(t *testing.T) {
	for _, s := range Scopes {
		if strings.Contains(s, "bucket-item") {
			t.Errorf("Scopes asks for %q, which reaches inside a bucket", s)
		}
	}
	want := map[string]bool{
		"workers-r2.read":  false,
		"workers-r2.write": false,
		"memberships.read": false,
	}
	for _, s := range Scopes {
		if _, ok := want[s]; !ok {
			t.Errorf("unexpected scope %q", s)
		}
		want[s] = true
	}
	for s, seen := range want {
		if !seen {
			t.Errorf("missing scope %q", s)
		}
	}
}

func TestAccountsSendsBearerAndParsesResult(t *testing.T) {
	f := newFake(t)
	f.body = `{"success":true,"errors":[],"result":[
		{"id":"acc-1","name":"Personal"},
		{"id":"acc-2","name":"Work"}
	]}`

	accounts, err := clientFor(f).Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("len = %d, want 2", len(accounts))
	}
	if accounts[0].ID != "acc-1" || accounts[0].Name != "Personal" {
		t.Errorf("first account = %+v", accounts[0])
	}
	if got := f.got[0].Auth; got != "Bearer tok-abc" {
		t.Errorf("Authorization = %q, want a bearer token", got)
	}
	if got := f.got[0].Path; got != "/accounts" {
		t.Errorf("path = %q, want /accounts", got)
	}
}

// Someone with no account cannot be set up, and saying so plainly beats
// failing later with an empty picker.
func TestAccountsWithNoneIsItsOwnError(t *testing.T) {
	f := newFake(t)
	f.body = `{"success":true,"errors":[],"result":[]}`
	_, err := clientFor(f).Accounts(context.Background())
	if !errors.Is(err, ErrNoAccounts) {
		t.Fatalf("err = %v, want ErrNoAccounts", err)
	}
}

func TestBucketsUnwrapsTheBucketsArray(t *testing.T) {
	f := newFake(t)
	f.body = `{"success":true,"errors":[],"result":{"buckets":[
		{"name":"backups","location":"weur","storage_class":"Standard","creation_date":"2026-01-02T03:04:05Z"}
	]}}`

	buckets, err := clientFor(f).Buckets(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("Buckets: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("len = %d, want 1", len(buckets))
	}
	if buckets[0].Name != "backups" || buckets[0].StorageClass != "Standard" {
		t.Errorf("bucket = %+v", buckets[0])
	}
	if want := "/accounts/acc-1/r2/buckets"; f.got[0].Path != want {
		t.Errorf("path = %q, want %q", f.got[0].Path, want)
	}
}

// An account with no buckets yet is the normal state before setup, not an
// error -- the caller offers to make one.
func TestBucketsEmptyIsNotAnError(t *testing.T) {
	f := newFake(t)
	f.body = `{"success":true,"errors":[],"result":{"buckets":[]}}`
	buckets, err := clientFor(f).Buckets(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("Buckets: %v", err)
	}
	if len(buckets) != 0 {
		t.Errorf("buckets = %v, want none", buckets)
	}
}

func TestCreateBucketPostsTheName(t *testing.T) {
	f := newFake(t)
	f.body = `{"success":true,"errors":[],"result":{"name":"my-backups"}}`

	if err := clientFor(f).CreateBucket(context.Background(), "acc-1", "my-backups"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	req := f.got[0]
	if req.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.Method)
	}
	if want := "/accounts/acc-1/r2/buckets"; req.Path != want {
		t.Errorf("path = %q, want %q", req.Path, want)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
		t.Fatalf("body %q: %v", req.Body, err)
	}
	if body["name"] != "my-backups" {
		t.Errorf("body = %v, want the bucket name", body)
	}
	// No region or storage class: Cloudflare's defaults are what a backup
	// wants, and a region menu is a question with no good answer for the
	// person being asked.
	if _, ok := body["locationHint"]; ok {
		t.Error("a location hint was sent")
	}
	if _, ok := body["storageClass"]; ok {
		t.Error("a storage class was sent")
	}
}

// "Already there" is frequently good news on the setup path -- the caller can
// go on and use the bucket -- so it must be distinguishable.
func TestCreateBucketReportsAnExistingBucket(t *testing.T) {
	f := newFake(t)
	f.status = http.StatusBadRequest
	f.body = `{"success":false,"errors":[{"code":10004,"message":"The bucket you tried to create already exists"}],"result":null}`

	err := clientFor(f).CreateBucket(context.Background(), "acc-1", "taken-name")
	if !errors.Is(err, ErrBucketExists) {
		t.Fatalf("err = %v, want ErrBucketExists", err)
	}
}

func TestRejectedTokenIsItsOwnError(t *testing.T) {
	f := newFake(t)
	f.status = http.StatusUnauthorized
	f.body = `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}],"result":null}`

	_, err := clientFor(f).Accounts(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

// Cloudflare's own message is far more useful than a status code, so it has
// to survive into the error a person reads.
func TestCloudflareMessageSurvivesIntoTheError(t *testing.T) {
	f := newFake(t)
	f.status = http.StatusBadRequest
	f.body = `{"success":false,"errors":[{"code":7003,"message":"Could not route to /accounts"}],"result":null}`

	_, err := clientFor(f).Accounts(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "Could not route") {
		t.Errorf("err = %v, want Cloudflare's message", err)
	}
}

func TestValidateBucketName(t *testing.T) {
	valid := []string{"abc", "my-backups", "a-1-b", strings.Repeat("a", 63)}
	for _, name := range valid {
		if err := ValidateBucketName(name); err != nil {
			t.Errorf("ValidateBucketName(%q) = %v, want nil", name, err)
		}
	}
	invalid := map[string]string{
		"ab":                    "too short",
		strings.Repeat("a", 64): "too long",
		"My-Backups":            "uppercase",
		"my_backups":            "underscore",
		"my backups":            "space",
		"-leading":              "leading hyphen",
		"trailing-":             "trailing hyphen",
		"emoji-\U0001F600-name": "emoji",
	}
	for name, why := range invalid {
		if err := ValidateBucketName(name); err == nil {
			t.Errorf("ValidateBucketName(%q) = nil, want an error (%s)", name, why)
		}
	}
}

// A bad name should not cost a round trip; it should come back instantly.
func TestCreateBucketValidatesBeforeCalling(t *testing.T) {
	f := newFake(t)
	if err := clientFor(f).CreateBucket(context.Background(), "acc-1", "No Good"); err == nil {
		t.Fatal("want an error for an invalid name")
	}
	if len(f.got) != 0 {
		t.Errorf("made %d request(s) for a name that could not be valid", len(f.got))
	}
}

func TestCallsRequireAnAccountID(t *testing.T) {
	f := newFake(t)
	c := clientFor(f)
	if _, err := c.Buckets(context.Background(), ""); err == nil {
		t.Error("Buckets with no account ID: want an error")
	}
	if err := c.CreateBucket(context.Background(), "", "valid-name"); err == nil {
		t.Error("CreateBucket with no account ID: want an error")
	}
	if len(f.got) != 0 {
		t.Errorf("made %d request(s) with no account ID", len(f.got))
	}
}
