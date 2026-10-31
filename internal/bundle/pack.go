package bundle

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"sync"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// spool is a pack being filled, on disk until it is sent.
type spool struct {
	id      string
	file    *os.File
	count   *countingWriter
	zw      *zip.Writer
	entries map[string]entry
	closed  bool
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func newSpool(dir, id string) (*spool, error) {
	f, err := os.CreateTemp(dir, "r2b-pack-*.zip")
	if err != nil {
		return nil, fmt.Errorf("bundle: start a pack: %w", err)
	}
	cw := &countingWriter{w: f}
	return &spool{id: id, file: f, count: cw, zw: zip.NewWriter(cw), entries: map[string]entry{}}, nil
}

// add writes stored (already compressed by e.Method) as rel, and fills in
// where it went.
func (s *spool) add(rel string, e *entry, stored []byte, now time.Time) error {
	mod := now
	if e.MTime != 0 {
		mod = time.Unix(0, e.MTime)
	}
	fh := &zip.FileHeader{
		Name:               rel,
		Method:             e.Method,
		CRC32:              e.CRC,
		CompressedSize64:   uint64(len(stored)),
		UncompressedSize64: uint64(e.Size),
		Modified:           mod,
	}
	if e.Mode != 0 {
		fh.SetMode(os.FileMode(e.Mode))
	}
	w, err := s.zw.CreateRaw(fh)
	if err != nil {
		return fmt.Errorf("bundle: add %q to a pack: %w", rel, err)
	}
	// The zip writer buffers; flushing is what makes the count the true
	// offset of the bytes about to be written.
	if err := s.zw.Flush(); err != nil {
		return err
	}
	off := s.count.n
	if _, err := w.Write(stored); err != nil {
		return err
	}
	if err := s.zw.Flush(); err != nil {
		return err
	}
	if got := s.count.n - off; got != int64(len(stored)) {
		return fmt.Errorf("bundle: pack entry %q is %d bytes, wrote %d", rel, len(stored), got)
	}
	e.Pack, e.Off, e.Len = s.id, off, int64(len(stored))
	s.entries[rel] = *e
	return nil
}

func (s *spool) size() int64 { return s.count.n }

// read returns stored bytes from the pack still being filled.
func (s *spool) read(off, n int64) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := s.file.ReadAt(buf, off); err != nil {
		return nil, fmt.Errorf("bundle: read from the pack being filled: %w", err)
	}
	return buf, nil
}

// finish writes the zip's directory and returns the pack's size.
func (s *spool) finish() (int64, error) {
	if err := s.zw.Close(); err != nil {
		return 0, err
	}
	s.closed = true
	return s.count.n, nil
}

func (s *spool) discard() {
	if s.file == nil {
		return
	}
	name := s.file.Name()
	s.file.Close()
	cleanupTemp(name)
	s.file = nil
}

// --- reading back -------------------------------------------------------------

func newCRC() hash.Hash32 { return crc32.NewIEEE() }

func crc32IEEE(b []byte) uint32 { return crc32.ChecksumIEEE(b) }

// errCorrupt is a file that did not come back as it went in.
var errCorrupt = errors.New("bundle: a file came back damaged (checksum mismatch)")

// checked passes a file through and fails at the end if it is not the size
// and checksum it was stored with.
type checked struct {
	r    io.Reader
	c    io.Closer
	h    hash.Hash32
	want uint32
	size int64
	n    int64
}

func (c *checked) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.h.Write(p[:n])
		c.n += int64(n)
	}
	if err == io.EOF {
		if c.n != c.size || c.h.Sum32() != c.want {
			return n, errCorrupt
		}
	}
	return n, err
}

func (c *checked) Close() error { return c.c.Close() }

// packCache keeps a few whole packs on disk for a store that cannot read
// part of an object, so restoring a hundred files from one pack downloads it
// once, not a hundred times.
type packCache struct {
	dir   string
	limit int

	mu    sync.Mutex
	files map[string]string // key -> temp file
	order []string
}

func newPackCache(dir string, limit int) *packCache {
	return &packCache{dir: dir, limit: limit, files: map[string]string{}}
}

func (c *packCache) read(ctx context.Context, base remote.Store, key string, off, n int64) (io.ReadCloser, error) {
	name, err := c.fetch(ctx, base, key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.NewSectionReader(f, off, n), f}, nil
}

func (c *packCache) fetch(ctx context.Context, base remote.Store, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if name, ok := c.files[key]; ok {
		return name, nil
	}
	obj, err := base.Get(ctx, key)
	if err != nil {
		return "", err
	}
	defer obj.Body.Close()
	f, err := os.CreateTemp(c.dir, "r2b-packcache-*.zip")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, obj.Body); err != nil {
		f.Close()
		cleanupTemp(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		cleanupTemp(f.Name())
		return "", err
	}
	c.files[key] = f.Name()
	c.order = append(c.order, key)
	for len(c.order) > c.limit {
		old := c.order[0]
		c.order = c.order[1:]
		cleanupTemp(c.files[old])
		delete(c.files, old)
	}
	return f.Name(), nil
}

// Close removes the cached packs, and any pack still being filled -- which
// is only there when Flush was never called, so nothing points at it.
func (s *Store) Close() {
	s.mu.Lock()
	for _, sp := range s.spaces {
		sp.mu.Lock()
		if sp.open != nil {
			sp.open.discard()
			sp.open = nil
		}
		sp.mu.Unlock()
	}
	s.mu.Unlock()
	s.cache.mu.Lock()
	defer s.cache.mu.Unlock()
	for k, name := range s.cache.files {
		cleanupTemp(name)
		delete(s.cache.files, k)
	}
	s.cache.order = nil
}
