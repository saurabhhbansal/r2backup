// Package cfapi talks to Cloudflare's own API with a token from a browser
// sign-in.
//
// It exists to do three things during setup and then stop: find which
// accounts a person belongs to, list the R2 buckets in one, and make a bucket
// if there is not a suitable one already. That is the whole surface, and it is
// meant to stay that way.
//
// In particular this package never touches objects. Moving files is the S3
// API's job, signed with the R2 keys in internal/creds, and the OAuth token
// here could not do it anyway -- Cloudflare's object endpoints take a signed
// request, not a bearer token. The consent screen asks only for the two
// bucket-level R2 scopes for that reason: r2backup manages the container and
// never reads the contents with this credential.
package cfapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is Cloudflare's API root.
const DefaultBaseURL = "https://api.cloudflare.com/client/v4"

// Scopes are what r2backup asks for on the consent screen.
//
// The two bucket-item scopes Cloudflare also offers -- workers-r2-bucket-item
// read and write, which reach inside a bucket -- are deliberately absent. They
// would let this token read someone's backed-up files, which it has no reason
// to do and which the S3 keys handle instead. Asking for less makes the
// consent screen honest, and honest is the whole argument for doing this in a
// browser rather than asking for keys.
//
// memberships.read is how "which account?" gets answered without asking
// anyone to find their account ID in a dashboard.
var Scopes = []string{
	"workers-r2.read",
	"workers-r2.write",
	"memberships.read",
}

// Sentinel errors a caller can match with errors.Is.
var (
	// ErrUnauthorized means the token was rejected: expired, revoked, or
	// never carried the scope this call needs.
	ErrUnauthorized = errors.New("cfapi: Cloudflare rejected the sign-in")

	// ErrBucketExists means a bucket of that name is already there. On the
	// setup path this is frequently good news rather than a failure -- the
	// caller can go on and use it -- so it is worth telling apart.
	ErrBucketExists = errors.New("cfapi: a bucket with that name already exists")

	// ErrNoAccounts means the sign-in succeeded but the person belongs to
	// no Cloudflare account, which nothing downstream can proceed from.
	ErrNoAccounts = errors.New("cfapi: this Cloudflare login has no accounts")
)

// Client calls Cloudflare's API as the person who signed in.
type Client struct {
	token      string
	baseURL    string
	httpClient *http.Client
}

// New builds a Client for a token from internal/oauth.
func New(token string) *Client {
	return &Client{
		token:   token,
		baseURL: DefaultBaseURL,
		// A timeout, because the default http.Client has none and setup is
		// a person sitting at a terminal waiting for it.
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// SetBaseURL points the client somewhere else. Tests use it; nothing else
// should.
func (c *Client) SetBaseURL(u string) { c.baseURL = strings.TrimRight(u, "/") }

// SetHTTPClient replaces the HTTP client. Tests use it.
func (c *Client) SetHTTPClient(h *http.Client) { c.httpClient = h }

// Account is one Cloudflare account the signed-in person belongs to.
type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Bucket is one R2 bucket.
type Bucket struct {
	Name         string `json:"name"`
	Location     string `json:"location"`
	StorageClass string `json:"storage_class"`
	CreationDate string `json:"creation_date"`
}

// envelope is the shape every Cloudflare API response shares.
type envelope struct {
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Accounts lists the accounts this login belongs to.
//
// Returning them all rather than picking one is the point: plenty of people
// have a personal account and a work one, and choosing for them is how
// backups end up in the wrong place. The caller asks when there is more than
// one and stays quiet when there is exactly one.
func (c *Client) Accounts(ctx context.Context) ([]Account, error) {
	var out []Account
	// per_page is capped at 50 by the API. Anyone with more accounts than
	// that has bigger problems than this list, but paging is cheap.
	page := 1
	for {
		var accounts []Account
		q := url.Values{
			"page":     {fmt.Sprint(page)},
			"per_page": {"50"},
		}
		if err := c.get(ctx, "/accounts?"+q.Encode(), &accounts); err != nil {
			return nil, err
		}
		out = append(out, accounts...)
		if len(accounts) < 50 {
			break
		}
		page++
		// A guard against an API that never says it is finished, rather
		// than against anything expected.
		if page > 20 {
			break
		}
	}
	if len(out) == 0 {
		return nil, ErrNoAccounts
	}
	return out, nil
}

// Buckets lists the R2 buckets in an account.
func (c *Client) Buckets(ctx context.Context, accountID string) ([]Bucket, error) {
	if strings.TrimSpace(accountID) == "" {
		return nil, errors.New("cfapi: no account ID")
	}
	// The result of this one is an object with a buckets array inside it,
	// not a bare array like /accounts.
	var result struct {
		Buckets []Bucket `json:"buckets"`
	}
	path := "/accounts/" + url.PathEscape(accountID) + "/r2/buckets?per_page=1000"
	if err := c.get(ctx, path, &result); err != nil {
		return nil, err
	}
	return result.Buckets, nil
}

// CreateBucket makes a new R2 bucket.
//
// No location hint and no storage class: Cloudflare's defaults are Standard
// in an automatically chosen region, which is what a backup wants, and
// offering someone a region menu during setup is a question with no good
// answer for the person being asked.
func (c *Client) CreateBucket(ctx context.Context, accountID, name string) error {
	if strings.TrimSpace(accountID) == "" {
		return errors.New("cfapi: no account ID")
	}
	if err := ValidateBucketName(name); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return fmt.Errorf("cfapi: encode the request: %w", err)
	}
	path := "/accounts/" + url.PathEscape(accountID) + "/r2/buckets"
	return c.post(ctx, path, body, nil)
}

// ValidateBucketName checks a name against R2's rules before spending a round
// trip on it, so a typo comes back instantly and in plain words rather than
// as an API error code.
func ValidateBucketName(name string) error {
	switch {
	case len(name) < 3:
		return errors.New("cfapi: a bucket name needs at least 3 characters")
	case len(name) > 63:
		return errors.New("cfapi: a bucket name can be at most 63 characters")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return fmt.Errorf("cfapi: a bucket name can only use lowercase letters, numbers and hyphens (%q is not allowed)", r)
		}
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return errors.New("cfapi: a bucket name cannot start or end with a hyphen")
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, into any) error {
	return c.do(ctx, http.MethodGet, path, nil, into)
}

func (c *Client) post(ctx context.Context, path string, body []byte, into any) error {
	return c.do(ctx, http.MethodPost, path, body, into)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, into any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("cfapi: build the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("cfapi: reach Cloudflare: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}

	var env envelope
	decodeErr := json.NewDecoder(resp.Body).Decode(&env)
	if !env.Success {
		// Cloudflare's own message is far more useful than the status code,
		// so it is preferred whenever there is one.
		if msg := firstMessage(env.Errors); msg != "" {
			if isBucketExists(env.Errors) {
				return ErrBucketExists
			}
			return fmt.Errorf("cfapi: Cloudflare said: %s", msg)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("cfapi: Cloudflare returned %s", resp.Status)
		}
		if decodeErr != nil {
			return fmt.Errorf("cfapi: could not read Cloudflare's response: %w", decodeErr)
		}
		return errors.New("cfapi: Cloudflare reported a failure with no reason given")
	}
	if into == nil || len(env.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Result, into); err != nil {
		return fmt.Errorf("cfapi: could not read Cloudflare's response: %w", err)
	}
	return nil
}

func firstMessage(errs []apiError) string {
	for _, e := range errs {
		if e.Message != "" {
			return e.Message
		}
	}
	return ""
}

// isBucketExists recognises the "already there" case by message rather than
// by code, because R2 has reported it under more than one code and the text
// has been the stable part. A misread here only costs a slightly less
// specific error, never a wrong action.
func isBucketExists(errs []apiError) bool {
	for _, e := range errs {
		m := strings.ToLower(e.Message)
		if strings.Contains(m, "already exists") || strings.Contains(m, "bucket name already") {
			return true
		}
	}
	return false
}
