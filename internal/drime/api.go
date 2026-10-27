// Package drime stores backups in Drime (drime.cloud), a cloud drive with no
// S3 endpoint.
//
// Everything else in r2backup was built against S3's model: flat keys,
// prefix listing, server-side copy, metadata on each object. Drime is a
// folder tree addressed by numeric ids, with no per-file metadata, no
// modification times and no hashes. This package does the translating, so
// the rest of the program keeps one model and Client satisfies remote.Store:
//
//   - A key "machines/pc/Docs/current/a/b.txt" is the file b.txt in the folder
//     r2backup/machines/pc/Docs/current/a. Backups are an ordinary, browsable
//     folder tree in the Drime web app and can be downloaded from there with
//     no r2backup at all.
//   - Folder ids are looked up once and cached for the life of the Client.
//   - File dates, permissions and symlink targets go in one small manifest per
//     set (see manifest.go), written at the end of a run.
//   - Large files go up through Drime's own multipart endpoints, which are S3
//     multipart underneath, so an interrupted upload resumes from the parts
//     already sent exactly as it does on R2.
//
// Drime does not publish its API. The endpoints here are the ones its web app
// uses, as documented by rclone's Drime backend, which is the most widely used
// third-party client. If Drime changes them this package is what breaks, and
// it breaks loudly: every call checks its response.
package drime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// traceOn logs every request to stderr when R2BACKUP_DRIME_DEBUG is set: the
// method, the path (never the query, which on a part upload carries a
// signature, and never the token), the status and how long it took. It is how
// a slow or failing run against the real service is diagnosed.
var traceOn = os.Getenv("R2BACKUP_DRIME_DEBUG") != ""

func trace(req *http.Request, resp *http.Response, err error, attempt int, d time.Duration) {
	status := "error: " + fmt.Sprint(err)
	if resp != nil {
		status = resp.Status
	}
	host := ""
	if !strings.Contains(req.URL.Host, "drime") {
		host = req.URL.Host
	}
	fmt.Fprintf(os.Stderr, "%s drime %s %s%s -> %s in %s (attempt %d)\n",
		time.Now().Format("15:04:05.000"), req.Method, host, req.URL.Path, status, d.Round(time.Millisecond), attempt)
}

// DefaultBaseURL is Drime's web app, under which the API lives at api/v1.
const DefaultBaseURL = "https://app.drime.cloud/"

// api is the HTTP layer: auth, JSON, retries and error decoding. It knows
// nothing about keys or folders.
type api struct {
	base        string // with trailing slash
	token       string
	workspace   string
	hc          *http.Client
	maxAttempts int
}

// apiError is a non-2xx answer from Drime.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("drime: %s (HTTP %d)", e.Message, e.Status)
}

// isRetryable reports whether a status is worth asking again about. 429 is
// Drime's "slow down", and Cloudflare, which fronts it, answers 520-524 when
// the origin hiccups.
func isRetryable(status int) bool {
	switch {
	case status == http.StatusTooManyRequests, status == http.StatusRequestTimeout:
		return true
	case status >= 500 && status != http.StatusNotImplemented:
		return true
	}
	return false
}

// isNotFound reports whether err means the thing asked about is not there.
// Drime says so either with a 404 or, for ids it no longer has, with a 422
// whose message names the ids as invalid.
func isNotFound(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.Status == http.StatusNotFound {
		return true
	}
	m := strings.ToLower(ae.Message)
	return ae.Status == http.StatusUnprocessableEntity && strings.Contains(m, "invalid")
}

func (a *api) url(path string, q url.Values) string {
	u := a.base + "api/v1" + path
	if a.workspace != "" {
		if q == nil {
			q = url.Values{}
		}
		q.Set("workspaceId", a.workspace)
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// backoff is how long to wait before attempt n (1-based), honouring a
// Retry-After the server sent.
func backoff(n int, resp *http.Response) time.Duration {
	if resp != nil {
		if s := resp.Header.Get("Retry-After"); s != "" {
			if secs, err := strconv.Atoi(s); err == nil && secs >= 0 && secs <= 120 {
				return time.Duration(secs) * time.Second
			}
		}
	}
	d := time.Duration(1<<min(n-1, 5)) * time.Second // 1s, 2s, 4s ... 32s
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// request describes one call. newBody, when set, is called once per attempt,
// so a retry sends the whole body again rather than whatever a failed attempt
// left unread.
type request struct {
	method      string
	url         string
	newBody     func() (io.Reader, error)
	contentType string
	length      int64 // -1 when unknown
	headers     map[string]string
	noAuth      bool // presigned part URLs carry their own signature
	// rewind is called before every retry. A body that cannot be sent twice
	// leaves it nil and newBody returns an error on the second call.
	rewind func() error
}

// send performs r with retries and returns the response of the first attempt
// that is not worth retrying. A non-2xx response is turned into *apiError and
// its body closed; a 2xx response is the caller's to close.
func (a *api) send(ctx context.Context, r request) (*http.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= a.maxAttempts; attempt++ {
		if attempt > 1 && r.rewind != nil {
			if err := r.rewind(); err != nil {
				return nil, err
			}
		}
		var body io.Reader
		if r.newBody != nil {
			b, err := r.newBody()
			if err != nil {
				return nil, err
			}
			body = b
		}
		req, err := http.NewRequestWithContext(ctx, r.method, r.url, body)
		if err != nil {
			return nil, err
		}
		if r.length >= 0 && body != nil {
			req.ContentLength = r.length
		}
		if r.contentType != "" {
			req.Header.Set("Content-Type", r.contentType)
		}
		if !r.noAuth {
			req.Header.Set("Authorization", "Bearer "+a.token)
			req.Header.Set("Accept", "application/json")
		}
		for k, v := range r.headers {
			req.Header.Set(k, v)
		}

		began := time.Now()
		resp, err := a.hc.Do(req)
		if traceOn {
			trace(req, resp, err, attempt, time.Since(began))
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			if attempt < a.maxAttempts {
				if serr := sleepCtx(ctx, backoff(attempt, nil)); serr != nil {
					return nil, serr
				}
			}
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		apiErr := decodeError(resp)
		if !isRetryable(resp.StatusCode) || attempt == a.maxAttempts {
			return nil, apiErr
		}
		lastErr = apiErr
		if serr := sleepCtx(ctx, backoff(attempt, resp)); serr != nil {
			return nil, serr
		}
	}
	return nil, lastErr
}

// decodeError reads Drime's {"message": "..."} error body, falling back to
// the raw text, and closes the response.
func decodeError(resp *http.Response) error {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var body struct {
		Message string `json:"message"`
	}
	msg := ""
	if json.Unmarshal(raw, &body) == nil {
		msg = body.Message
	}
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return &apiError{Status: resp.StatusCode, Message: msg}
}

// call sends JSON in (if non-nil) and decodes JSON into out (if non-nil).
func (a *api) call(ctx context.Context, method, path string, q url.Values, in, out any, headers map[string]string) error {
	r := request{method: method, url: a.url(path, q), length: -1, headers: headers}
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return err
		}
		r.contentType = "application/json"
		r.length = int64(len(buf))
		r.newBody = func() (io.Reader, error) { return bytes.NewReader(buf), nil }
	}
	resp, err := a.send(ctx, r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("drime: %s %s: unexpected response: %w", method, path, err)
	}
	return nil
}

// --- Wire types ---------------------------------------------------------------

const itemTypeFolder = "folder"

// item is a file or folder as /drive/file-entries returns it. Only the fields
// this package reads are declared.
type item struct {
	ID        json.Number `json:"id"`
	Name      string      `json:"name"`
	Type      string      `json:"type"`
	FileSize  int64       `json:"file_size"`
	ParentID  json.Number `json:"parent_id"`
	Mime      string      `json:"mime"`
	URL       string      `json:"url"`
	UpdatedAt time.Time   `json:"updated_at"`
}

func (it item) isFolder() bool { return it.Type == itemTypeFolder }

type listing struct {
	CurrentPage int    `json:"current_page"`
	LastPage    int    `json:"last_page"`
	Data        []item `json:"data"`
}

// listChunk is how many entries one listing page asks for.
const listChunk = 1000

// listFolder returns every entry directly inside folderID ("" is the drive's
// root), following pages.
func (a *api) listFolder(ctx context.Context, folderID string, foldersOnly bool) ([]item, error) {
	var out []item
	for page := 1; ; {
		q := url.Values{}
		if folderID != "" {
			q.Set("folderId", folderID)
		}
		if foldersOnly {
			q.Set("type", itemTypeFolder)
		}
		q.Set("perPage", strconv.Itoa(listChunk))
		q.Set("page", strconv.Itoa(page))
		var res listing
		if err := a.call(ctx, http.MethodGet, "/drive/file-entries", q, nil, &res, nil); err != nil {
			return nil, fmt.Errorf("drime: list folder: %w", err)
		}
		// Drime stops paging past a fixed number of entries and serves its
		// last allowed page again instead of an error. Carrying on would loop
		// forever over the same page; stopping quietly would lose files.
		if res.CurrentPage != 0 && res.CurrentPage != page {
			return nil, errors.New("drime: a folder holds more entries than Drime will list; split it into subfolders")
		}
		for _, it := range res.Data {
			if foldersOnly && !it.isFolder() {
				continue
			}
			out = append(out, it)
		}
		if res.LastPage == 0 || res.CurrentPage >= res.LastPage {
			return out, nil
		}
		page = res.CurrentPage + 1
	}
}

func (a *api) createFolder(ctx context.Context, parentID, name string) (item, error) {
	req := struct {
		Name     string      `json:"name"`
		ParentID json.Number `json:"parentId,omitempty"`
	}{Name: name, ParentID: json.Number(parentID)}
	var res struct {
		Folder item `json:"folder"`
	}
	if err := a.call(ctx, http.MethodPost, "/folders", nil, req, &res, nil); err != nil {
		return item{}, fmt.Errorf("drime: create folder %q: %w", name, err)
	}
	if res.Folder.ID == "" {
		return item{}, fmt.Errorf("drime: create folder %q: no id in response", name)
	}
	return res.Folder, nil
}

// deleteEntries removes ids. forever skips Drime's own trash: r2backup keeps
// its own, and anything left in Drime's would count against the account's
// space twice.
func (a *api) deleteEntries(ctx context.Context, ids []string, forever bool) error {
	if len(ids) == 0 {
		return nil
	}
	req := struct {
		EntryIDs      []string `json:"entryIds"`
		DeleteForever bool     `json:"deleteForever"`
	}{ids, forever}
	var res struct {
		Errors map[string]string `json:"errors"`
	}
	if err := a.call(ctx, http.MethodPost, "/file-entries/delete", nil, req, &res, nil); err != nil {
		return err
	}
	for name, msg := range res.Errors {
		return fmt.Errorf("drime: delete %q: %s", name, msg)
	}
	return nil
}

// duplicate makes a server-side copy of id inside destID and returns it. Drime
// names the copy "name (1)" when destID already holds "name".
func (a *api) duplicate(ctx context.Context, id, destID string) (item, error) {
	req := struct {
		EntryIDs      []string `json:"entryIds"`
		DestinationID string   `json:"destinationId"`
	}{[]string{id}, destID}
	var res struct {
		Entries []item `json:"entries"`
	}
	if err := a.call(ctx, http.MethodPost, "/file-entries/duplicate", nil, req, &res, nil); err != nil {
		return item{}, err
	}
	if len(res.Entries) == 0 {
		return item{}, errors.New("drime: duplicate: no entry in response")
	}
	return res.Entries[0], nil
}

// move puts id inside destID, keeping its name.
func (a *api) move(ctx context.Context, id, destID string) error {
	req := struct {
		EntryIDs      []string `json:"entryIds"`
		DestinationID string   `json:"destinationId"`
	}{[]string{id}, destID}
	return a.call(ctx, http.MethodPost, "/file-entries/move", nil, req, nil, nil)
}

// rename changes an entry's name. Drime's origin answers a literal PUT here
// with a malformed response, so this is a POST with Laravel's method override,
// which the API routes to the same handler.
func (a *api) rename(ctx context.Context, id, name string) (item, error) {
	req := struct {
		Name string `json:"name"`
	}{name}
	var res struct {
		FileEntry item `json:"fileEntry"`
	}
	err := a.call(ctx, http.MethodPost, "/file-entries/"+id, nil, req, &res,
		map[string]string{"X-HTTP-Method-Override": "PUT"})
	if err != nil {
		return item{}, err
	}
	return res.FileEntry, nil
}

// SpaceUsage is how much of the account is used.
type SpaceUsage struct {
	Used      int64 `json:"used"`
	Available int64 `json:"available"`
}

func (a *api) spaceUsage(ctx context.Context) (SpaceUsage, error) {
	var res SpaceUsage
	if err := a.call(ctx, http.MethodGet, "/user/space-usage", nil, nil, &res, nil); err != nil {
		return SpaceUsage{}, err
	}
	return res, nil
}

// download opens an entry's content.
func (a *api) download(ctx context.Context, it item) (*http.Response, error) {
	u := it.URL
	if u == "" {
		u = "api/v1/file-entries/" + it.ID.String() + "/download"
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = a.base + strings.TrimPrefix(u, "/")
	}
	return a.send(ctx, request{method: http.MethodGet, url: u, length: -1})
}
