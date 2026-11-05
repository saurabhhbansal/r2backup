package drime

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// ManifestName is the file, at the top of each set's folder, that holds what
// Drime cannot keep on the files themselves: modification times, permission
// bits, symlink targets and which zero-byte files stand for empty folders.
//
// One file per set rather than one per file, because Drime has nowhere to put
// per-file metadata short of the description field, which would cost a second
// request for every upload and show up as clutter in the web app. The cost of
// one file is that a run cut off before it flushes loses the dates of what
// it uploaded since the last flush; those files still restore, with Drime's
// upload time as their date. Flushing every few minutes during a long run
// keeps that window small.
const ManifestName = ".r2backup-manifest.json.gz"

// manifestVersion is bumped if the format ever changes incompatibly.
const manifestVersion = 1

// flushEvery bounds how much a run can lose to an interruption.
const (
	flushEveryChanges = 2000
	flushEveryTime    = 5 * time.Minute
)

// metaRecord is one file's entry. Short field names because a set can have
// tens of thousands of them and this file is downloaded whole.
type metaRecord struct {
	ModTime int64       `json:"m"`           // Unix nanoseconds
	Mode    fs.FileMode `json:"p"`           // permission bits
	Size    int64       `json:"z"`           // as backed up
	Symlink string      `json:"s,omitempty"` // link target
	Kind    remote.Kind `json:"k,omitempty"`
}

type manifestFile struct {
	Version int                   `json:"version"`
	Entries map[string]metaRecord `json:"entries"` // key relative to the set
}

// setScope splits a key into the set it belongs to and its path inside it.
// Every set lives at machines/<machine>/<set>/; a key outside that shape has
// no manifest and gets defaults.
func setScope(key string) (scope, rel string, ok bool) {
	parts := strings.SplitN(key, "/", 4)
	if len(parts) < 4 || parts[0] != "machines" || parts[3] == "" {
		return "", "", false
	}
	return strings.Join(parts[:3], "/"), parts[3], true
}

// manifest is one set's records, loaded on first use.
type manifest struct {
	scope   string
	entries map[string]metaRecord
	dirty   int
	since   time.Time
	// dropped is set when a flush let go of this copy so the next use reads
	// the manifest again. Guarded by manifests.mu.
	dropped bool
}

// manifests holds every set's manifest this Client has touched.
type manifests struct {
	c  *Client
	mu sync.Mutex
	// loading serialises first loads so two goroutines do not both download
	// the same manifest.
	loading sync.Mutex
	// flushing serialises writes, so two uploads crossing the flush
	// threshold together do not each write a copy of the same manifest.
	flushing sync.Mutex
	sets     map[string]*manifest
}

func newManifests(c *Client) *manifests {
	return &manifests{c: c, sets: map[string]*manifest{}}
}

func (ms *manifests) get(ctx context.Context, scope string) (*manifest, error) {
	ms.mu.Lock()
	m, ok := ms.sets[scope]
	ms.mu.Unlock()
	if ok {
		return m, nil
	}
	ms.loading.Lock()
	defer ms.loading.Unlock()
	ms.mu.Lock()
	m, ok = ms.sets[scope]
	ms.mu.Unlock()
	if ok {
		return m, nil
	}
	entries, err := ms.c.readManifest(ctx, scope)
	if err != nil {
		return nil, err
	}
	m = &manifest{scope: scope, entries: entries, since: time.Now()}
	ms.mu.Lock()
	ms.sets[scope] = m
	ms.mu.Unlock()
	return m, nil
}

// locked returns scope's manifest with ms.mu held, never a copy a flush has
// let go of. The caller unlocks.
func (ms *manifests) locked(ctx context.Context, scope string) (*manifest, error) {
	for {
		m, err := ms.get(ctx, scope)
		if err != nil {
			return nil, err
		}
		ms.mu.Lock()
		if !m.dropped {
			return m, nil
		}
		ms.mu.Unlock()
	}
}

// lookup returns key's metadata, or ok=false when nothing was recorded.
func (ms *manifests) lookup(ctx context.Context, key string) (remote.Metadata, bool, error) {
	scope, rel, ok := setScope(key)
	if !ok {
		return remote.Metadata{}, false, nil
	}
	m, err := ms.locked(ctx, scope)
	if err != nil {
		return remote.Metadata{}, false, err
	}
	r, found := m.entries[rel]
	ms.mu.Unlock()
	if !found {
		return remote.Metadata{}, false, nil
	}
	return remote.Metadata{
		ModTime: time.Unix(0, r.ModTime),
		Mode:    r.Mode,
		Size:    r.Size,
		Symlink: r.Symlink,
		Kind:    r.Kind,
	}, true, nil
}

// set records key's metadata. It reports whether the set has gathered
// enough unsaved changes that it should be flushed now.
func (ms *manifests) set(ctx context.Context, key string, md remote.Metadata) (flushScope string, err error) {
	scope, rel, ok := setScope(key)
	if !ok {
		return "", nil
	}
	m, err := ms.locked(ctx, scope)
	if err != nil {
		return "", err
	}
	defer ms.mu.Unlock()
	var mt int64
	if !md.ModTime.IsZero() {
		mt = md.ModTime.UnixNano()
	}
	m.entries[rel] = metaRecord{ModTime: mt, Mode: md.Mode.Perm(), Size: md.Size, Symlink: md.Symlink, Kind: md.Kind}
	return m.touch(), nil
}

// remove drops keys' records.
func (ms *manifests) remove(ctx context.Context, keys []string) error {
	for _, key := range keys {
		scope, rel, ok := setScope(key)
		if !ok {
			continue
		}
		m, err := ms.locked(ctx, scope)
		if err != nil {
			return err
		}
		if _, had := m.entries[rel]; had {
			delete(m.entries, rel)
			m.touch()
		}
		ms.mu.Unlock()
	}
	return nil
}

// touch marks a change and returns the scope when it is time to flush.
// Caller holds ms.mu.
func (m *manifest) touch() string {
	if m.dirty == 0 {
		m.since = time.Now()
	}
	m.dirty++
	if m.dirty >= flushEveryChanges || time.Since(m.since) >= flushEveryTime {
		return m.scope
	}
	return ""
}

// forgetUnder reflects the deletion of everything under prefix. A whole set
// (or more) going takes its manifest with it, so the cached copy is dropped
// unsaved; a folder inside a set going takes only those records.
func (ms *manifests) forgetUnder(prefix string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	prefix = strings.TrimSuffix(prefix, "/")
	for scope, m := range ms.sets {
		switch {
		case scope == prefix || strings.HasPrefix(scope, prefix+"/"):
			m.dropped = true
			delete(ms.sets, scope)
		case strings.HasPrefix(prefix, scope+"/"):
			sub := strings.TrimPrefix(prefix, scope+"/") + "/"
			for rel := range m.entries {
				if strings.HasPrefix(rel, sub) {
					delete(m.entries, rel)
					m.touch()
				}
			}
		}
	}
}

// pending reports whether any manifest has unsaved changes.
func (ms *manifests) pending() bool {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	for _, m := range ms.sets {
		if m.dirty > 0 {
			return true
		}
	}
	return false
}

// flush writes every manifest with unsaved changes, or only scope's when
// scope is set. Flushing every set also lets go of the clean ones.
func (ms *manifests) flush(ctx context.Context, scope string) error {
	return ms.write(ctx, scope, scope == "")
}

// write saves every manifest with unsaved changes (only scope's when scope is
// set), and with forget lets go of the clean ones afterwards.
func (ms *manifests) write(ctx context.Context, scope string, forget bool) error {
	ms.flushing.Lock()
	defer ms.flushing.Unlock()
	return ms.writeLocked(ctx, scope, forget)
}

// flushSoon is the flush an upload asks for when its set has gathered
// enough changes. If a save is already under way it does nothing: that
// save, or the next checkpoint, takes these changes too. Waiting for it
// instead would stall the upload, and every other upload crossing the
// threshold meanwhile, only to write the same manifest again and again.
func (ms *manifests) flushSoon(ctx context.Context, scope string) error {
	if !ms.flushing.TryLock() {
		return nil
	}
	defer ms.flushing.Unlock()
	return ms.writeLocked(ctx, scope, false)
}

// writeLocked is write with ms.flushing held.
func (ms *manifests) writeLocked(ctx context.Context, scope string, forget bool) error {
	ms.mu.Lock()
	type job struct {
		scope string
		data  []byte
		m     *manifest
		n     int
	}
	var jobs []job
	for s, m := range ms.sets {
		if m.dirty == 0 || (scope != "" && s != scope) {
			continue
		}
		data, err := encodeManifest(m.entries)
		if err != nil {
			ms.mu.Unlock()
			return err
		}
		jobs = append(jobs, job{scope: s, data: data, m: m, n: m.dirty})
	}
	ms.mu.Unlock()

	var firstErr error
	for _, j := range jobs {
		if err := ms.c.writeManifest(ctx, j.scope, j.data); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("drime: save details for %s: %w", j.scope, err)
			}
			continue
		}
		ms.mu.Lock()
		// Changes made while this was uploading stay counted, so they are
		// picked up by the next flush rather than lost.
		j.m.dirty -= j.n
		if j.m.dirty < 0 {
			j.m.dirty = 0
		}
		j.m.since = time.Now()
		ms.mu.Unlock()
	}
	if forget && firstErr == nil {
		ms.forgetClean()
	}
	return firstErr
}

// forgetClean lets go of every manifest with nothing left to save, so the
// next use reads it again. A long-lived process (the tray) keeps one Client
// for its whole life while scheduled backups run as processes of their own;
// without this it would keep the copy it first read, and its next save would
// write that old copy over the details the other process recorded.
func (ms *manifests) forgetClean() {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	for s, m := range ms.sets {
		if m.dirty == 0 {
			m.dropped = true
			delete(ms.sets, s)
		}
	}
}

func encodeManifest(entries map[string]metaRecord) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(manifestFile{Version: manifestVersion, Entries: entries}); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeManifest(r io.Reader) (map[string]metaRecord, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var f manifestFile
	if err := json.NewDecoder(zr).Decode(&f); err != nil {
		return nil, err
	}
	if f.Version > manifestVersion {
		return nil, fmt.Errorf("written by a newer r2backup (format %d); update with: r2b update", f.Version)
	}
	if f.Entries == nil {
		f.Entries = map[string]metaRecord{}
	}
	return f.Entries, nil
}
