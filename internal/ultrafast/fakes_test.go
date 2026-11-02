package ultrafast

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/relay"
)

// fakeCF is the part of Cloudflare's API Ultra fast mode uses.
type fakeCF struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	token     string
	accounts  []Account
	subdomain string
	scripts   map[string]fakeScript // account/name
	enabled   map[string]bool
	deleteErr int // status to answer deletes with, 0 for success
	calls     []string
	onUpload  func(name, secret string)
}

type fakeScript struct {
	module []byte
	secret string
	meta   map[string]any
}

func newFakeCF(t *testing.T) *fakeCF {
	f := &fakeCF{t: t, token: "cf-token", accounts: []Account{{ID: "acct1", Name: "Personal"}},
		scripts: map[string]fakeScript{}, enabled: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCF) answer(w http.ResponseWriter, status int, result any, errs ...cfError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": status < 300, "errors": errs, "result": result})
}

func (f *fakeCF) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		f.answer(w, http.StatusBadRequest, nil, cfError{Code: 10000, Message: "Authentication error"})
		return
	}
	p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/accounts":
		f.answer(w, 200, f.accounts)
	case len(p) == 4 && p[2] == "workers" && p[3] == "subdomain" && r.Method == http.MethodGet:
		if f.subdomain == "" {
			f.answer(w, 404, nil, cfError{Code: 10007, Message: "This account does not have a workers.dev subdomain"})
			return
		}
		f.answer(w, 200, map[string]string{"subdomain": f.subdomain})
	case len(p) == 4 && p[2] == "workers" && p[3] == "subdomain" && r.Method == http.MethodPut:
		var body struct{ Subdomain string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.subdomain = body.Subdomain
		f.answer(w, 200, map[string]string{"subdomain": body.Subdomain})
	case len(p) == 5 && p[3] == "scripts" && r.Method == http.MethodPut:
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			f.t.Errorf("script upload is not a multipart form: %v", err)
			f.answer(w, 400, nil)
			return
		}
		var meta map[string]any
		if err := json.Unmarshal([]byte(r.FormValue("metadata")), &meta); err != nil {
			f.t.Errorf("metadata: %v", err)
		}
		fh := r.MultipartForm.File[meta["main_module"].(string)]
		if len(fh) != 1 || fh[0].Header.Get("Content-Type") != "application/javascript+module" {
			f.t.Errorf("main module part missing or of the wrong type: %v", fh)
			f.answer(w, 400, nil)
			return
		}
		file, _ := fh[0].Open()
		module, _ := io.ReadAll(file)
		var secret string
		for _, b := range meta["bindings"].([]any) {
			b := b.(map[string]any)
			if b["type"] == "secret_text" && b["name"] == "RELAY_SECRET" {
				secret = b["text"].(string)
			}
		}
		f.scripts[p[1]+"/"+p[4]] = fakeScript{module: module, secret: secret, meta: meta}
		if f.onUpload != nil {
			f.onUpload(p[4], secret)
		}
		f.answer(w, 200, map[string]string{"id": p[4]})
	case len(p) == 6 && p[3] == "scripts" && p[5] == "subdomain" && r.Method == http.MethodPost:
		if _, ok := f.scripts[p[1]+"/"+p[4]]; !ok {
			f.answer(w, 404, nil, cfError{Code: 10007, Message: "script not found"})
			return
		}
		f.enabled[p[1]+"/"+p[4]] = true
		f.answer(w, 200, map[string]bool{"enabled": true})
	case len(p) == 5 && p[3] == "scripts" && r.Method == http.MethodDelete:
		if f.deleteErr != 0 {
			f.answer(w, f.deleteErr, nil, cfError{Code: 10013, Message: "something went wrong"})
			return
		}
		if _, ok := f.scripts[p[1]+"/"+p[4]]; !ok {
			f.answer(w, 404, nil, cfError{Code: 10007, Message: "script not found"})
			return
		}
		delete(f.scripts, p[1]+"/"+p[4])
		f.answer(w, 200, nil)
	default:
		f.t.Errorf("unexpected Cloudflare call %s %s", r.Method, r.URL.Path)
		f.answer(w, 404, nil)
	}
}

func (f *fakeCF) script(account, name string) (fakeScript, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.scripts[account+"/"+name]
	return s, ok
}

// fakeRelay does in Go what relay.js does in the Worker, so the Go side can
// be tested end to end: fetch a range from src, PUT it to dst.
type fakeRelay struct {
	srv     *httptest.Server
	secret  atomic.Value // string
	version atomic.Value // string
	copies  atomic.Int64
	status  atomic.Int64 // when set, every /copy answers this with a Cloudflare page
	failN   atomic.Int64 // this many /copy calls fail as the upload refusing
}

func newFakeRelay(t *testing.T, secret string) *fakeRelay {
	r := &fakeRelay{}
	r.secret.Store(secret)
	r.version.Store(relay.Version)
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *fakeRelay) serve(w http.ResponseWriter, req *http.Request) {
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	if req.Header.Get("Authorization") != "Bearer "+r.secret.Load().(string) {
		reply(401, map[string]any{"ok": false, "error": "not allowed"})
		return
	}
	if req.URL.Path == "/health" {
		reply(200, map[string]any{"ok": true, "version": r.version.Load()})
		return
	}
	if s := r.status.Load(); s != 0 {
		w.WriteHeader(int(s))
		fmt.Fprint(w, "<html>error code: 1027</html>")
		return
	}
	if r.failN.Load() > 0 {
		r.failN.Add(-1)
		reply(502, map[string]any{"ok": false, "stage": "destination", "status": 500, "error": "the upload answered 500"})
		return
	}
	var cr copyRequest
	if err := json.NewDecoder(req.Body).Decode(&cr); err != nil {
		reply(400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	get, _ := http.NewRequest(http.MethodGet, cr.Src.URL, nil)
	if cr.Src.Start != nil {
		get.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", *cr.Src.Start, *cr.Src.End))
	}
	down, err := http.DefaultClient.Do(get)
	if err != nil {
		reply(502, map[string]any{"ok": false, "stage": "source", "error": err.Error()})
		return
	}
	defer down.Body.Close()
	if down.ContentLength != cr.Length {
		reply(502, map[string]any{"ok": false, "stage": "source", "error": "wrong length " + strconv.FormatInt(down.ContentLength, 10)})
		return
	}
	put, _ := http.NewRequest(http.MethodPut, cr.Dst.URL, down.Body)
	put.ContentLength = cr.Length
	for k, v := range cr.Dst.Headers {
		put.Header.Set(k, v)
	}
	up, err := http.DefaultClient.Do(put)
	if err != nil {
		reply(502, map[string]any{"ok": false, "stage": "destination", "error": err.Error()})
		return
	}
	defer up.Body.Close()
	if up.StatusCode != 200 {
		b, _ := io.ReadAll(up.Body)
		reply(502, map[string]any{"ok": false, "stage": "destination", "status": up.StatusCode, "error": string(b)})
		return
	}
	r.copies.Add(1)
	reply(200, map[string]any{"ok": true, "etag": up.Header.Get("ETag"), "bytes": cr.Length})
}

// linkSource is a source store whose objects are served by link, as
// Drime's are: a download link that takes Range and needs nothing else.
type linkSource struct {
	srv   *httptest.Server
	mu    sync.Mutex
	objs  map[string][]byte
	md    map[string]remote.Metadata
	links atomic.Int64
	gets  atomic.Int64
}

func newLinkSource(t *testing.T) *linkSource {
	s := &linkSource{objs: map[string][]byte{}, md: map[string]remote.Metadata{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		data, ok := s.objs[strings.TrimPrefix(r.URL.Path, "/k/")]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *linkSource) add(key string, data []byte, md remote.Metadata) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objs[key] = data
	s.md[key] = md
}

func (s *linkSource) DownloadLink(ctx context.Context, key string) (*remote.DownloadLink, error) {
	s.links.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objs[key]
	if !ok {
		return nil, remote.ErrNotFound
	}
	md := s.md[key]
	md.Size = int64(len(data))
	return &remote.DownloadLink{URL: s.srv.URL + "/k/" + key, Size: int64(len(data)), Metadata: md,
		ContentType: "application/octet-stream", Expires: time.Now().Add(30 * time.Minute)}, nil
}

func (s *linkSource) Get(ctx context.Context, key string) (*remote.Object, error) {
	s.gets.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objs[key]
	if !ok {
		return nil, remote.ErrNotFound
	}
	return &remote.Object{Body: io.NopCloser(bytes.NewReader(data)), Metadata: s.md[key], Size: int64(len(data))}, nil
}

func (s *linkSource) List(ctx context.Context, prefix string) ([]remote.ListEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []remote.ListEntry
	for k, v := range s.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, remote.ListEntry{Key: k, Size: int64(len(v)), ModTime: time.Unix(1, 0)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (s *linkSource) Put(context.Context, remote.PutInput) error               { panic("source is read only") }
func (s *linkSource) Head(context.Context, string) (*remote.HeadResult, error) { panic("not used") }
func (s *linkSource) ListPrefixes(context.Context, string) ([]string, error)   { panic("not used") }
func (s *linkSource) Copy(context.Context, string, string) error               { panic("not used") }
func (s *linkSource) Delete(context.Context, string) error                     { panic("not used") }
func (s *linkSource) DeleteBatch(context.Context, []string) error              { panic("not used") }
func (s *linkSource) DeletePrefix(context.Context, string) (remote.PrefixDeletion, error) {
	panic("not used")
}
func (s *linkSource) AbandonStaleUploads(context.Context) (int, error) { return 0, nil }
func (s *linkSource) ListMultipartUploads(context.Context, string) ([]remote.MultipartUpload, error) {
	return nil, nil
}
func (s *linkSource) AbortUpload(context.Context, string, string) error { return nil }
func (s *linkSource) Bucket() string                                    { return "drime" }

func storeAt(t *testing.T) *Store { return Open(filepath.Join(t.TempDir(), "ultrafast")) }
