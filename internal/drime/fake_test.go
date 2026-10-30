package drime

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDrime is an in-memory stand-in for the parts of Drime's API this
// package uses. It mimics the quirks that matter: uploads never overwrite,
// duplicates get a " (1)" suffix, listings page, and multipart uploads hand
// out part URLs that are plain PUTs answered with an ETag.
type fakeDrime struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	nextID  int
	entries map[string]*fakeEntry
	uploads map[string]*fakeUpload // by upload id

	token     string
	perPage   int         // forces paging when small
	failParts map[int]int // part number -> failures left before it succeeds
	calls     map[string]int
}

type fakeEntry struct {
	item
	data []byte
}

type fakeUpload struct {
	key   string
	parts map[int][]byte
	done  bool
}

func newFake(t *testing.T) *fakeDrime {
	f := &fakeDrime{
		t: t, entries: map[string]*fakeEntry{}, uploads: map[string]*fakeUpload{},
		token: "tok", failParts: map[int]int{}, calls: map[string]int{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDrime) id() string {
	f.nextID++
	return strconv.Itoa(f.nextID)
}

func (f *fakeDrime) add(parent, name, typ string, data []byte) *fakeEntry {
	e := &fakeEntry{item: item{
		ID: json.Number(f.id()), Name: name, Type: typ, FileSize: int64(len(data)),
		ParentID: json.Number(parent), UpdatedAt: time.Now().Add(time.Duration(f.nextID) * time.Millisecond),
	}, data: data}
	e.URL = "api/v1/file-entries/" + e.ID.String() + "/download"
	f.entries[e.ID.String()] = e
	return e
}

// freeName mimics Drime renaming a duplicate rather than replacing.
func (f *fakeDrime) freeName(parent, name string, dupRename bool) string {
	if !dupRename {
		return name
	}
	taken := func(n string) bool {
		for _, e := range f.entries {
			if e.ParentID.String() == parent && e.Name == n {
				return true
			}
		}
		return false
	}
	if !taken(name) {
		return name
	}
	for i := 1; ; i++ {
		if n := fmt.Sprintf("%s (%d)", name, i); !taken(n) {
			return n
		}
	}
}

func (f *fakeDrime) children(parent string) []*fakeEntry {
	var out []*fakeEntry
	for _, e := range f.entries {
		if e.ParentID.String() == parent {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := strconv.Atoi(out[i].ID.String())
		b, _ := strconv.Atoi(out[j].ID.String())
		return a < b
	})
	return out
}

func (f *fakeDrime) removeTree(id string) {
	for _, c := range f.children(id) {
		f.removeTree(c.ID.String())
	}
	delete(f.entries, id)
}

// path returns an entry's path from the drive root, for assertions.
func (f *fakeDrime) path(e *fakeEntry) string {
	p := e.Name
	for e.ParentID != "" {
		e = f.entries[e.ParentID.String()]
		if e == nil {
			return "?/" + p
		}
		p = e.Name + "/" + p
	}
	return p
}

// files returns every file by path.
func (f *fakeDrime) files() map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]byte{}
	for _, e := range f.entries {
		if e.Type != itemTypeFolder {
			out[f.path(e)] = e.data
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeDrime) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	f.calls[r.Method+" "+p]++

	// Presigned part URLs are not authenticated with the token.
	if strings.HasPrefix(p, "/part/") {
		f.servePart(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]string{"message": "Unauthenticated."})
		return
	}

	switch {
	case r.Method == http.MethodGet && p == "/api/v1/drive/file-entries":
		q := r.URL.Query()
		parent := q.Get("folderId")
		var all []item
		for _, e := range f.children(parent) {
			if q.Get("type") == itemTypeFolder && e.Type != itemTypeFolder {
				continue
			}
			all = append(all, e.item)
		}
		per := f.perPage
		if per == 0 {
			per, _ = strconv.Atoi(q.Get("perPage"))
		}
		page, _ := strconv.Atoi(q.Get("page"))
		last := max(1, (len(all)+per-1)/per)
		lo, hi := min((page-1)*per, len(all)), min(page*per, len(all))
		writeJSON(w, listing{CurrentPage: page, LastPage: last, Data: all[lo:hi]})

	case r.Method == http.MethodPost && p == "/api/v1/folders":
		var req struct {
			Name     string      `json:"name"`
			ParentID json.Number `json:"parentId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		e := f.add(req.ParentID.String(), req.Name, itemTypeFolder, nil)
		writeJSON(w, map[string]any{"folder": e.item})

	case r.Method == http.MethodPost && p == "/api/v1/uploads":
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		data, _ := io.ReadAll(file)
		parent := r.FormValue("parentId")
		e := f.add(parent, r.FormValue("relativePath"), "file", data)
		writeJSON(w, map[string]any{"status": "success", "fileEntry": e.item})

	case r.Method == http.MethodGet && strings.HasSuffix(p, "/download"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/api/v1/file-entries/"), "/download")
		e := f.entries[id]
		if e == nil {
			w.WriteHeader(404)
			writeJSON(w, map[string]string{"message": "Not found"})
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(e.data)))
		_, _ = w.Write(e.data)

	case r.Method == http.MethodPost && p == "/api/v1/file-entries/delete":
		var req struct {
			EntryIDs []string `json:"entryIds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, id := range req.EntryIDs {
			if f.entries[id] == nil {
				w.WriteHeader(422)
				writeJSON(w, map[string]string{"message": "The selected entry ids is invalid."})
				return
			}
		}
		for _, id := range req.EntryIDs {
			f.removeTree(id)
		}
		writeJSON(w, map[string]any{"status": "success"})

	case r.Method == http.MethodPost && p == "/api/v1/file-entries/move":
		var req struct {
			EntryIDs      []string `json:"entryIds"`
			DestinationID string   `json:"destinationId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, id := range req.EntryIDs {
			e := f.entries[id]
			if e == nil {
				w.WriteHeader(422)
				writeJSON(w, map[string]string{"message": "The selected entry ids is invalid."})
				return
			}
			e.Name = f.freeName(req.DestinationID, e.Name, true)
			e.ParentID = json.Number(req.DestinationID)
		}
		writeJSON(w, map[string]any{"status": "success"})

	case r.Method == http.MethodPost && p == "/api/v1/file-entries/duplicate":
		var req struct {
			EntryIDs      []string `json:"entryIds"`
			DestinationID string   `json:"destinationId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		src := f.entries[req.EntryIDs[0]]
		if src == nil {
			w.WriteHeader(422)
			writeJSON(w, map[string]string{"message": "The selected entry ids is invalid."})
			return
		}
		e := f.add(req.DestinationID, f.freeName(req.DestinationID, src.Name, true), src.Type, src.data)
		writeJSON(w, map[string]any{"entries": []item{e.item}})

	case r.Method == http.MethodPost && strings.HasPrefix(p, "/api/v1/file-entries/") && r.Header.Get("X-HTTP-Method-Override") == "PUT":
		id := strings.TrimPrefix(p, "/api/v1/file-entries/")
		var req struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		e := f.entries[id]
		if e == nil {
			w.WriteHeader(404)
			return
		}
		e.Name = req.Name
		writeJSON(w, map[string]any{"fileEntry": e.item})

	case r.Method == http.MethodPost && p == "/api/v1/s3/multipart/create":
		var req struct {
			Filename string `json:"filename"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		uid := "up" + f.id()
		key := "uploads/" + uid + "/" + req.Filename
		f.uploads[uid] = &fakeUpload{key: key, parts: map[int][]byte{}}
		writeJSON(w, map[string]string{"uploadId": uid, "key": key})

	case r.Method == http.MethodPost && p == "/api/v1/s3/multipart/batch-sign-part-urls":
		var req struct {
			UploadID    string `json:"uploadId"`
			PartNumbers []int  `json:"partNumbers"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if f.uploads[req.UploadID] == nil {
			w.WriteHeader(404)
			writeJSON(w, map[string]string{"message": "NoSuchUpload"})
			return
		}
		type u struct {
			URL        string `json:"url"`
			PartNumber int    `json:"partNumber"`
		}
		var urls []u
		for _, n := range req.PartNumbers {
			urls = append(urls, u{URL: fmt.Sprintf("%s/part/%s/%d?sig=x", f.srv.URL, req.UploadID, n), PartNumber: n})
		}
		writeJSON(w, map[string]any{"urls": urls})

	case r.Method == http.MethodPost && p == "/api/v1/s3/multipart/complete":
		var req struct {
			UploadID string `json:"uploadId"`
			Parts    []struct {
				ETag       string `json:"ETag"`
				PartNumber int    `json:"PartNumber"`
			} `json:"parts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		up := f.uploads[req.UploadID]
		if up == nil {
			w.WriteHeader(404)
			writeJSON(w, map[string]string{"message": "NoSuchUpload"})
			return
		}
		for i, part := range req.Parts {
			if part.PartNumber != i+1 || up.parts[part.PartNumber] == nil || part.ETag != etagFor(req.UploadID, part.PartNumber) {
				w.WriteHeader(400)
				writeJSON(w, map[string]string{"message": "InvalidPart"})
				return
			}
		}
		up.done = true
		writeJSON(w, map[string]string{"location": up.key})

	case r.Method == http.MethodPost && p == "/api/v1/s3/entries":
		var req struct {
			ClientName string      `json:"clientName"`
			Filename   string      `json:"filename"`
			ParentID   json.Number `json:"parentId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var up *fakeUpload
		var uid string
		for id, u := range f.uploads {
			if u.done && strings.HasSuffix(u.key, "/"+req.Filename) {
				up, uid = u, id
			}
		}
		if up == nil {
			w.WriteHeader(422)
			writeJSON(w, map[string]string{"message": "no such upload"})
			return
		}
		var data []byte
		for n := 1; n <= len(up.parts); n++ {
			data = append(data, up.parts[n]...)
		}
		delete(f.uploads, uid)
		e := f.add(req.ParentID.String(), req.ClientName, "file", data)
		writeJSON(w, map[string]any{"fileEntry": e.item})

	case r.Method == http.MethodPost && p == "/api/v1/s3/multipart/abort":
		var req struct {
			UploadID string `json:"uploadId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		delete(f.uploads, req.UploadID)
		w.WriteHeader(204)

	case r.Method == http.MethodGet && p == "/api/v1/user/space-usage":
		writeJSON(w, SpaceUsage{Used: 10, Available: 100})

	default:
		f.t.Errorf("fake drime: unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(404)
	}
}

func etagFor(uid string, n int) string { return fmt.Sprintf(`"%s-%d"`, uid, n) }

func (f *fakeDrime) servePart(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" {
		f.t.Errorf("part upload carried the API token to the storage URL")
	}
	rest := strings.Split(strings.TrimPrefix(r.URL.Path, "/part/"), "/")
	uid := rest[0]
	n, _ := strconv.Atoi(rest[1])
	data, _ := io.ReadAll(r.Body)
	if f.failParts[n] > 0 {
		f.failParts[n]--
		w.WriteHeader(http.StatusForbidden) // not retryable: the upload stops
		return
	}
	up := f.uploads[uid]
	if up == nil {
		w.WriteHeader(404)
		return
	}
	up.parts[n] = data
	w.Header().Set("ETag", etagFor(uid, n))
	w.WriteHeader(200)
}
