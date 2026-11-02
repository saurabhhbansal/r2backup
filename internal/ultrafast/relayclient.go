package ultrafast

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/mirror"
)

// relayClient calls the relay Worker.
type relayClient struct {
	url    string
	secret string
	hc     *http.Client
}

func newRelayClient(url, secret string, hc *http.Client) *relayClient {
	if hc == nil {
		hc = &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			// The answer comes when the copy is done, so waiting for it is
			// waiting for one part to cross between two clouds.
			ResponseHeaderTimeout: 30 * time.Minute,
			MaxIdleConnsPerHost:   32,
		}}
	}
	return &relayClient{url: strings.TrimRight(url, "/"), secret: secret, hc: hc}
}

// copyRequest is the relay's /copy body.
type copyRequest struct {
	Src    copySource `json:"src"`
	Dst    copyDest   `json:"dst"`
	Length int64      `json:"length"`
}

type copySource struct {
	URL   string `json:"url"`
	Start *int64 `json:"start,omitempty"`
	End   *int64 `json:"end,omitempty"`
}

type copyDest struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

type relayAnswer struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Stage   string `json:"stage"`
	Status  int    `json:"status"`
	ETag    string `json:"etag"`
	Version string `json:"version"`
}

// stopped is an error that ends Ultra fast mode for the rest of a run.
func stopped(format string, a ...any) error {
	return fmt.Errorf("%w: %s", mirror.ErrRelayStopped, fmt.Sprintf(format, a...))
}

func (r *relayClient) do(ctx context.Context, method, path string, body any) (*relayAnswer, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.url+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach the Worker: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var ans relayAnswer
	jsonErr := json.Unmarshal(raw, &ans)

	// Cloudflare's own refusals are HTML pages naming an error code; the
	// relay itself always answers JSON.
	page := jsonErr != nil
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, stopped("the Worker refused this computer's password. Turn Ultra fast mode off and on again to set it up afresh")
	case resp.StatusCode == http.StatusTooManyRequests || page && bytes.Contains(raw, []byte("1027")):
		return nil, stopped("your Cloudflare account's free Worker allowance for today is used up (100,000 requests a day)")
	case page && (bytes.Contains(raw, []byte("1102")) || bytes.Contains(raw, []byte("exceeded resource limits"))):
		return nil, stopped("the Worker ran out of the time Cloudflare's free plan allows it")
	case page && resp.StatusCode == http.StatusNotFound:
		return nil, stopped("the Worker is no longer in your Cloudflare account. Turn Ultra fast mode off and on again")
	case page:
		return nil, fmt.Errorf("the Worker answered %s", resp.Status)
	}
	if !ans.OK {
		if ans.Error == "" {
			ans.Error = resp.Status
		}
		return &ans, fmt.Errorf("the Worker could not copy it: %s", ans.Error)
	}
	return &ans, nil
}

// health checks the Worker answers, and says which relay version it is.
func (r *relayClient) health(ctx context.Context) (string, error) {
	ans, err := r.do(ctx, http.MethodGet, "/health", nil)
	if err != nil {
		return "", err
	}
	return ans.Version, nil
}

// copy has the Worker copy one range, answering the upload's ETag.
func (r *relayClient) copy(ctx context.Context, req copyRequest) (string, error) {
	ans, err := r.do(ctx, http.MethodPost, "/copy", req)
	if err != nil {
		return "", err
	}
	return ans.ETag, nil
}
