package ultrafast

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// cloudflareAPI is Cloudflare's API root.
const cloudflareAPI = "https://api.cloudflare.com/client/v4"

// compatibilityDate pins the Workers runtime behaviour the relay was written
// and tested against.
const compatibilityDate = "2025-09-01"

// ErrBadToken is a Cloudflare API token that was refused, or that lacks the
// Workers Scripts: Edit permission.
var ErrBadToken = errors.New("Cloudflare did not accept the API token. It needs the permission Account > Workers Scripts > Edit")

// Account is a Cloudflare account the token can reach.
type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// cloudflare calls the few Cloudflare API endpoints Ultra fast mode needs,
// all of them covered by one permission, Workers Scripts: Edit.
type cloudflare struct {
	token string
	base  string
	hc    *http.Client
}

func newCloudflare(token, base string) *cloudflare {
	if base == "" {
		base = cloudflareAPI
	}
	return &cloudflare{token: strings.TrimSpace(token), base: strings.TrimRight(base, "/"), hc: &http.Client{Timeout: 60 * time.Second}}
}

type cfEnvelope struct {
	Success bool            `json:"success"`
	Errors  []cfError       `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// cfStatusError is an answer Cloudflare gave that was not a success.
type cfStatusError struct {
	Status int
	Codes  []int
	Msg    string
}

func (e *cfStatusError) Error() string {
	if e.Msg != "" {
		return "Cloudflare said: " + e.Msg
	}
	return fmt.Sprintf("Cloudflare answered %d", e.Status)
}

func (e *cfStatusError) has(code int) bool {
	for _, c := range e.Codes {
		if c == code {
			return true
		}
	}
	return false
}

func (c *cloudflare) call(ctx context.Context, method, path string, body io.Reader, contentType string, into any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach Cloudflare: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrBadToken
	}
	var env cfEnvelope
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&env)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !env.Success {
		se := &cfStatusError{Status: resp.StatusCode}
		for _, e := range env.Errors {
			se.Codes = append(se.Codes, e.Code)
			if se.Msg == "" {
				se.Msg = e.Message
			}
		}
		if se.has(10000) { // Authentication error
			return ErrBadToken
		}
		return se
	}
	if decodeErr != nil {
		return fmt.Errorf("could not read Cloudflare's answer: %w", decodeErr)
	}
	if into != nil && len(env.Result) > 0 && string(env.Result) != "null" {
		if err := json.Unmarshal(env.Result, into); err != nil {
			return fmt.Errorf("could not read Cloudflare's answer: %w", err)
		}
	}
	return nil
}

func accountPath(id string) string { return "/accounts/" + url.PathEscape(id) }

// accounts lists the accounts the token can reach.
func (c *cloudflare) accounts(ctx context.Context) ([]Account, error) {
	var out []Account
	for page := 1; page <= 20; page++ {
		var got []Account
		if err := c.call(ctx, http.MethodGet, fmt.Sprintf("/accounts?per_page=50&page=%d", page), nil, "", &got); err != nil {
			return nil, err
		}
		out = append(out, got...)
		if len(got) < 50 {
			break
		}
	}
	return out, nil
}

// subdomain is the account's workers.dev name, or "" when it has none yet.
func (c *cloudflare) subdomain(ctx context.Context, account string) (string, error) {
	var res struct {
		Subdomain string `json:"subdomain"`
	}
	err := c.call(ctx, http.MethodGet, accountPath(account)+"/workers/subdomain", nil, "", &res)
	var se *cfStatusError
	if errors.As(err, &se) && (se.Status == http.StatusNotFound || se.has(10007)) {
		return "", nil
	}
	return res.Subdomain, err
}

// createSubdomain gives the account a workers.dev name.
func (c *cloudflare) createSubdomain(ctx context.Context, account, name string) (string, error) {
	body, _ := json.Marshal(map[string]string{"subdomain": name})
	var res struct {
		Subdomain string `json:"subdomain"`
	}
	if err := c.call(ctx, http.MethodPut, accountPath(account)+"/workers/subdomain", bytes.NewReader(body), "application/json", &res); err != nil {
		return "", err
	}
	if res.Subdomain == "" {
		res.Subdomain = name
	}
	return res.Subdomain, nil
}

// uploadScript puts the relay in the account, with its secret bound as
// RELAY_SECRET. Uploading again replaces it.
func (c *cloudflare) uploadScript(ctx context.Context, account, name string, module []byte, secret string) error {
	meta, err := json.Marshal(map[string]any{
		"main_module":        "relay.js",
		"compatibility_date": compatibilityDate,
		"bindings": []map[string]string{
			{"type": "secret_text", "name": "RELAY_SECRET", "text": secret},
		},
	})
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("metadata", string(meta)); err != nil {
		return err
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="relay.js"; filename="relay.js"`)
	h.Set("Content-Type", "application/javascript+module")
	part, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	if _, err := part.Write(module); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.call(ctx, http.MethodPut, accountPath(account)+"/workers/scripts/"+url.PathEscape(name), &buf, w.FormDataContentType(), nil)
}

// enableWorkersDev serves the script at <name>.<subdomain>.workers.dev.
func (c *cloudflare) enableWorkersDev(ctx context.Context, account, name string) error {
	body := []byte(`{"enabled":true,"previews_enabled":false}`)
	return c.call(ctx, http.MethodPost, accountPath(account)+"/workers/scripts/"+url.PathEscape(name)+"/subdomain",
		bytes.NewReader(body), "application/json", nil)
}

// deleteScript removes the relay. One already gone is not an error.
func (c *cloudflare) deleteScript(ctx context.Context, account, name string) error {
	err := c.call(ctx, http.MethodDelete, accountPath(account)+"/workers/scripts/"+url.PathEscape(name)+"?force=true", nil, "", nil)
	var se *cfStatusError
	if errors.As(err, &se) && (se.Status == http.StatusNotFound || se.has(10007)) {
		return nil
	}
	return err
}
