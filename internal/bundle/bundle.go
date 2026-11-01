// Package bundle packs a backup's small files into archives before they go
// up, so a folder of 60,000 small files costs a few hundred write operations
// instead of 60,000.
//
// Storage priced by the request -- R2's and S3's Class A operations, B2's
// transactions -- charges the same to write a 2 KB file as a 2 GB one, and a
// monthly free allowance of a million writes goes in one backup of a large
// source tree. Bundling trades that for a little bookkeeping.
//
// # Layout
//
// Everything belonging to one backup lives under its key scope,
// machines/<machine>/<set>/. Bundling adds one folder there:
//
//	machines/pc/Docs/.bundles/index.json.gz      what is where
//	machines/pc/Docs/.bundles/packs/<id>.zip     the archives
//
// Each pack is an ordinary zip file (stored or deflated entries, named by
// their path in the backup), so a pack downloaded by hand opens with any
// unzip tool. The index maps every key of the backup to where its bytes are:
// a byte range inside a pack, or an object of its own (files over SmallMax go
// up directly -- one request each is nothing beside their size).
//
// # One changed file
//
// Packs are written once and never rewritten in place. A changed file goes
// into the next new pack and its index entry moves there; the old bytes
// become dead space in the old pack. Moving a file to trash, renaming it, or
// deleting it only edits the index. When a pack is mostly dead it is
// compacted -- its live entries copied into a new pack -- and deleted. So a
// backup where one file changed uploads one small pack and the index.
//
// # Durability
//
// A pack is only on the index once it has been uploaded, and the index is
// written by Flush. backup.Run flushes before it records anything as backed
// up, so a run that dies before the flush has recorded nothing and the next
// run sends those files again. A pack uploaded without its index entry is
// garbage that the daily sweep removes.
//
// # Turning it on and off
//
// Bundling is a property of the destination. The Store reads bundled backups
// whether or not it is bundling new writes: a backup with an index is always
// read through it, and one without (never bundled) passes straight through to
// the underlying store at no cost. Turning bundling off later sends new and
// changed files up directly while the rest stay where they are; turning it on
// over direct backups tracks the existing objects in the index and bundles
// from then on.
package bundle

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

const (
	// Dir is the folder under a backup's scope that bundling uses.
	Dir       = ".bundles/"
	indexName = Dir + "index.json.gz"
	packsDir  = Dir + "packs/"

	// DefaultPackSize is how full a pack gets before it is sent: at or
	// under the S3 client's multipart threshold, so a pack is one request.
	DefaultPackSize = 16 << 20
	// DefaultSmallMax is the largest file that goes into a pack. Anything
	// bigger is its own object.
	DefaultSmallMax = 4 << 20

	// compactBelow is the live fraction under which a pack is rewritten.
	compactBelow = 0.5
	// compactBudget bounds how many live bytes one Flush rewrites, so
	// tidying never costs more than a few packs' worth of requests.
	compactBudget = 4 * DefaultPackSize
	// sweepEvery is how often packs nothing points at are looked for.
	sweepEvery = 24 * time.Hour
	// orphanAge is how old an unreferenced pack must be before the sweep
	// deletes it, so a run still uploading elsewhere is not robbed.
	orphanAge = 6 * time.Hour
)

// Options configures a Store.
type Options struct {
	// Pack bundles new writes. Off, writes go up directly but backups
	// that were bundled before are still read (and kept consistent).
	Pack bool
	// PackSize and SmallMax override the defaults; tests use small ones.
	PackSize int64
	SmallMax int64
	// TempDir is where packs are assembled, and where whole packs are kept
	// briefly for a store that cannot read part of an object. Empty is the
	// system temporary directory.
	TempDir string
	// Now stands in for time.Now.
	Now func() time.Time
}

// Store is a remote.Store that bundles.
type Store struct {
	base remote.Store
	opts Options

	mu     sync.Mutex
	spaces map[string]*space

	cache *packCache

	// writes counts the write requests sent to the base store, for the
	// operation count a run reports.
	writes atomic.Int64
}

// BundledWrites reports how many write requests went to the base store since
// it was last asked, and whether this Store is bundling -- when it is not, a
// caller's own per-file count is the right one.
func (s *Store) BundledWrites() (int, bool) {
	return int(s.writes.Swap(0)), s.opts.Pack
}

// basePut is every write to the base store, counted.
func (s *Store) basePut(ctx context.Context, in remote.PutInput) error {
	s.writes.Add(1)
	return s.base.Put(ctx, in)
}

// New wraps base.
func New(base remote.Store, opts Options) *Store {
	if opts.PackSize <= 0 {
		opts.PackSize = DefaultPackSize
	}
	if opts.SmallMax <= 0 {
		opts.SmallMax = DefaultSmallMax
	}
	if opts.SmallMax > opts.PackSize {
		opts.SmallMax = opts.PackSize
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Store{base: base, opts: opts, spaces: map[string]*space{}, cache: newPackCache(opts.TempDir, 4)}
}

// Base is the store underneath.
func (s *Store) Base() remote.Store { return s.base }

// Packing reports whether new writes are bundled.
func (s *Store) Packing() bool { return s.opts.Pack }

// Scope is the backup a key belongs to, "machines/<machine>/<set>/", or ""
// for a key outside any backup, which is never bundled.
func Scope(key string) string {
	if !strings.HasPrefix(key, "machines/") {
		return ""
	}
	parts := strings.SplitN(key, "/", 4)
	if len(parts) < 4 {
		return ""
	}
	return parts[0] + "/" + parts[1] + "/" + parts[2] + "/"
}

// internal reports whether key is one of bundling's own objects.
func internal(key string) bool {
	ns := Scope(key)
	return ns != "" && strings.HasPrefix(key[len(ns):], Dir)
}

// entry is where one key's bytes are.
type entry struct {
	// Pack is the pack holding the bytes; "" means an object of its own at
	// the key itself.
	Pack string `json:"p,omitempty"`
	// Off and Len are the stored bytes' place in the pack.
	Off int64 `json:"o,omitempty"`
	Len int64 `json:"n,omitempty"`
	// Size is the file's own size.
	Size int64 `json:"s"`
	// Method is the zip method: 0 stored, 8 deflated.
	Method uint16 `json:"m,omitempty"`
	CRC    uint32 `json:"c,omitempty"`
	// Written is when this key was last written, Unix nanoseconds. It is
	// what a listing reports as the object's time.
	Written int64 `json:"w"`
	// The file's own details, which a direct object carries itself.
	MTime int64  `json:"t,omitempty"`
	Mode  uint32 `json:"f,omitempty"`
	Kind  string `json:"k,omitempty"`
	Link  string `json:"l,omitempty"`
}

func (e entry) packed() bool { return e.Pack != "" }

func (e entry) metadata() remote.Metadata {
	md := remote.Metadata{Size: e.Size, Mode: fs.FileMode(e.Mode), Symlink: e.Link, Kind: remote.Kind(e.Kind)}
	if e.MTime != 0 {
		md.ModTime = time.Unix(0, e.MTime)
	}
	if md.Kind == "" {
		md.Kind = remote.KindFile
	}
	return md
}

// indexFile is the stored index.
type indexFile struct {
	Version int              `json:"version"`
	Entries map[string]entry `json:"entries"`
	// Packs are the sizes of the packs, by id.
	Packs map[string]int64 `json:"packs"`
	// SweptAt is when unreferenced packs were last looked for, Unix seconds.
	SweptAt int64 `json:"swept_at,omitempty"`
}

// space is one backup's state.
type space struct {
	ns string

	mu          sync.Mutex
	loaded      bool
	passthrough bool // no index, not packing: the base store does it all
	idx         indexFile
	dirty       bool
	open        *spool
	// staleDirect are objects of their own that a packed entry replaced,
	// deleted at the next Flush.
	staleDirect []string
}

func (s *Store) space(ns string) *space {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.spaces[ns]
	if !ok {
		sp = &space{ns: ns}
		s.spaces[ns] = sp
	}
	return sp
}

// load reads ns's index the first time it is needed. Called with sp.mu held.
func (s *Store) load(ctx context.Context, sp *space) error {
	if sp.loaded {
		return nil
	}
	obj, err := s.base.Get(ctx, sp.ns+indexName)
	switch {
	case err == nil:
		defer obj.Body.Close()
		zr, err := gzip.NewReader(obj.Body)
		if err != nil {
			return fmt.Errorf("bundle: read index of %s: %w", sp.ns, err)
		}
		var idx indexFile
		if err := json.NewDecoder(zr).Decode(&idx); err != nil {
			return fmt.Errorf("bundle: read index of %s: %w", sp.ns, err)
		}
		if idx.Version != 1 {
			return fmt.Errorf("bundle: the index of %s was written by a newer r2backup (version %d)", sp.ns, idx.Version)
		}
		if idx.Entries == nil {
			idx.Entries = map[string]entry{}
		}
		if idx.Packs == nil {
			idx.Packs = map[string]int64{}
		}
		sp.idx = idx
	case errors.Is(err, remote.ErrNotFound):
		if !s.opts.Pack {
			sp.passthrough = true
			break
		}
		// Bundling starts here, over whatever was backed up directly
		// before: those objects stay, and the index learns about them.
		sp.idx = indexFile{Version: 1, Entries: map[string]entry{}, Packs: map[string]int64{}}
		list, err := s.base.List(ctx, sp.ns)
		if err != nil {
			return fmt.Errorf("bundle: list %s: %w", sp.ns, err)
		}
		for _, o := range list {
			if internal(o.Key) {
				continue
			}
			sp.idx.Entries[o.Key[len(sp.ns):]] = entry{Size: o.Size, Written: o.ModTime.UnixNano()}
		}
		sp.dirty = true
	default:
		return fmt.Errorf("bundle: read index of %s: %w", sp.ns, err)
	}
	sp.loaded = true
	return nil
}

// lookup finds rel in sp, including entries in the pack still being filled.
// Called with sp.mu held.
func (sp *space) lookup(rel string) (entry, bool) {
	if sp.open != nil {
		if e, ok := sp.open.entries[rel]; ok {
			return e, true
		}
	}
	e, ok := sp.idx.Entries[rel]
	return e, ok
}

// set records rel. Called with sp.mu held.
func (sp *space) set(rel string, e entry) {
	if old, ok := sp.idx.Entries[rel]; ok && !old.packed() && e.packed() {
		sp.staleDirect = append(sp.staleDirect, sp.ns+rel)
	}
	sp.idx.Entries[rel] = e
	sp.dirty = true
}

// remove forgets rel, reporting whether it was an object of its own that the
// caller must delete from the base store. Called with sp.mu held.
func (sp *space) remove(rel string) (direct bool, known bool) {
	if sp.open != nil {
		delete(sp.open.entries, rel)
	}
	e, ok := sp.idx.Entries[rel]
	if !ok {
		return false, false
	}
	delete(sp.idx.Entries, rel)
	sp.dirty = true
	return !e.packed(), true
}

// enter returns ns's space loaded, locked, and whether it passes through.
func (s *Store) enter(ctx context.Context, key string) (sp *space, rel string, through bool, err error) {
	ns := Scope(key)
	if ns == "" || internal(key) {
		return nil, "", true, nil
	}
	sp = s.space(ns)
	sp.mu.Lock()
	if err := s.load(ctx, sp); err != nil {
		sp.mu.Unlock()
		return nil, "", false, err
	}
	if sp.passthrough {
		sp.mu.Unlock()
		return nil, "", true, nil
	}
	return sp, key[len(ns):], false, nil
}

func (s *Store) now() int64 { return s.opts.Now().UnixNano() }

func newID(now time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%016x-%s", now.UnixNano(), hex.EncodeToString(b[:]))
}

// --- writes -----------------------------------------------------------------

// Put writes one object: into the pack being filled when bundling and it is
// small, as an object of its own otherwise.
func (s *Store) Put(ctx context.Context, in remote.PutInput) error {
	sp, rel, through, err := s.enter(ctx, in.Key)
	if err != nil {
		return err
	}
	if through {
		return s.base.Put(ctx, in)
	}
	if !s.opts.Pack || in.Size > s.opts.SmallMax {
		sp.mu.Unlock()
		if err := s.basePut(ctx, in); err != nil {
			return err
		}
		sp.mu.Lock()
		defer sp.mu.Unlock()
		sp.remove(rel)
		sp.set(rel, entry{Size: in.Size, Written: s.now()})
		return nil
	}
	sp.mu.Unlock()

	// Read and compress outside the lock: other files are being added by
	// other workers meanwhile.
	data, err := io.ReadAll(io.LimitReader(in.Body, s.opts.SmallMax+1))
	if err != nil {
		return fmt.Errorf("bundle: read %q: %w", in.Key, err)
	}
	if int64(len(data)) != in.Size {
		return fmt.Errorf("bundle: %q is %d bytes, expected %d (did it change while being read?)", in.Key, len(data), in.Size)
	}
	stored, method, crc := compress(data)
	if in.Progress != nil {
		in.Progress(int64(len(data)))
	}
	md := in.Metadata
	e := entry{Size: int64(len(data)), Method: method, CRC: crc, Written: s.now(),
		Mode: uint32(md.Mode), Kind: string(md.Kind), Link: md.Symlink}
	if !md.ModTime.IsZero() {
		e.MTime = md.ModTime.UnixNano()
	}
	if e.Kind == string(remote.KindFile) {
		e.Kind = ""
	}

	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.open == nil {
		if sp.open, err = newSpool(s.opts.TempDir, newID(s.opts.Now())); err != nil {
			return err
		}
	}
	if err := sp.open.add(rel, &e, stored, s.opts.Now()); err != nil {
		return err
	}
	if sp.open.size() >= s.opts.PackSize {
		return s.seal(ctx, sp)
	}
	return nil
}

// seal finishes the pack being filled, sends it, and puts its entries on the
// index. Called with sp.mu held.
func (s *Store) seal(ctx context.Context, sp *space) error {
	sp2 := sp.open
	if sp2 == nil {
		return nil
	}
	sp.open = nil
	defer sp2.discard()
	if len(sp2.entries) == 0 {
		return nil
	}
	size, err := sp2.finish()
	if err != nil {
		return err
	}
	f := sp2.file
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	key := sp.ns + packsDir + sp2.id + ".zip"
	err = s.basePut(ctx, remote.PutInput{
		Key: key, Body: f, Size: size, ContentType: "application/zip",
		Metadata: remote.Metadata{ModTime: s.opts.Now(), Size: size, Mode: 0o644, Kind: remote.KindFile},
	})
	if err != nil {
		return fmt.Errorf("bundle: send pack: %w", err)
	}
	sp.idx.Packs[sp2.id] = size
	for rel, e := range sp2.entries {
		sp.set(rel, e)
	}
	sp.dirty = true
	return nil
}

// Copy gives dstKey srcKey's content. Inside one bundled backup that is an
// index edit; anything else is copied.
func (s *Store) Copy(ctx context.Context, srcKey, dstKey string) error {
	return s.copyOrMove(ctx, srcKey, dstKey, false)
}

// Move is Copy without the source afterwards. Inside one bundled backup it
// is an index edit, which is how moving files to trash costs nothing.
func (s *Store) Move(ctx context.Context, srcKey, dstKey string) error {
	return s.copyOrMove(ctx, srcKey, dstKey, true)
}

func (s *Store) copyOrMove(ctx context.Context, srcKey, dstKey string, move bool) error {
	baseMove := func() error {
		if mv, ok := s.base.(remote.Mover); ok && move {
			return mv.Move(ctx, srcKey, dstKey)
		}
		s.writes.Add(1)
		if err := s.base.Copy(ctx, srcKey, dstKey); err != nil {
			return err
		}
		if move {
			return s.base.Delete(ctx, srcKey)
		}
		return nil
	}
	ns := Scope(srcKey)
	if ns == "" || ns != Scope(dstKey) || internal(srcKey) || internal(dstKey) {
		return s.crossCopy(ctx, srcKey, dstKey, move)
	}
	sp, srel, through, err := s.enter(ctx, srcKey)
	if err != nil {
		return err
	}
	if through {
		return baseMove()
	}
	drel := dstKey[len(ns):]
	e, ok := sp.lookup(srel)
	if !ok {
		sp.mu.Unlock()
		return fmt.Errorf("bundle: copy %q: %w", srcKey, remote.ErrNotFound)
	}
	if e.packed() {
		defer sp.mu.Unlock()
		if sp.open != nil {
			if _, inOpen := sp.open.entries[srel]; inOpen {
				// Still being filled: the copy goes in the same pack.
				sp.open.entries[drel] = withWritten(e, s.now())
				if move {
					sp.remove(srel)
				}
				return nil
			}
		}
		if old, ok := sp.idx.Entries[drel]; ok && !old.packed() {
			sp.staleDirect = append(sp.staleDirect, dstKey)
		}
		sp.idx.Entries[drel] = withWritten(e, s.now())
		sp.dirty = true
		if move {
			delete(sp.idx.Entries, srel)
		}
		return nil
	}
	sp.mu.Unlock()
	if err := baseMove(); err != nil {
		return err
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.idx.Entries[drel] = withWritten(e, s.now())
	if move {
		delete(sp.idx.Entries, srel)
	}
	sp.dirty = true
	return nil
}

func withWritten(e entry, w int64) entry {
	e.Written = w
	return e
}

// crossCopy copies between backups, through this machine.
func (s *Store) crossCopy(ctx context.Context, srcKey, dstKey string, move bool) error {
	if Scope(srcKey) == "" && Scope(dstKey) == "" {
		if mv, ok := s.base.(remote.Mover); ok && move {
			return mv.Move(ctx, srcKey, dstKey)
		}
		if err := s.base.Copy(ctx, srcKey, dstKey); err != nil {
			return err
		}
		if move {
			return s.base.Delete(ctx, srcKey)
		}
		return nil
	}
	obj, err := s.Get(ctx, srcKey)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(obj.Body)
	obj.Body.Close()
	if err != nil {
		return err
	}
	if err := s.Put(ctx, remote.PutInput{Key: dstKey, Body: bytes.NewReader(data), Size: int64(len(data)), Metadata: obj.Metadata}); err != nil {
		return err
	}
	if move {
		return s.Delete(ctx, srcKey)
	}
	return nil
}

// Delete removes one key. A missing key is not an error.
func (s *Store) Delete(ctx context.Context, key string) error {
	return s.DeleteBatch(ctx, []string{key})
}

// DeleteBatch removes keys. Bundled ones are index edits; objects of their
// own are deleted from the base store.
func (s *Store) DeleteBatch(ctx context.Context, keys []string) error {
	var direct []string
	for _, k := range keys {
		sp, rel, through, err := s.enter(ctx, k)
		if err != nil {
			return err
		}
		if through {
			direct = append(direct, k)
			continue
		}
		isDirect, known := sp.remove(rel)
		sp.mu.Unlock()
		if isDirect || !known {
			direct = append(direct, k)
		}
	}
	if len(direct) == 0 {
		return nil
	}
	return s.base.DeleteBatch(ctx, direct)
}

// DeletePrefix removes everything under prefix. A prefix that takes in a
// whole backup removes its packs and index too.
func (s *Store) DeletePrefix(ctx context.Context, prefix string) (remote.PrefixDeletion, error) {
	ns := Scope(prefix)
	if ns == "" || len(prefix) <= len(ns) {
		// A whole backup or more. Count what a person would count -- their
		// files -- then let the base store delete every object.
		var out remote.PrefixDeletion
		list, err := s.List(ctx, prefix)
		if err != nil {
			return out, err
		}
		for _, o := range list {
			out.Objects++
			out.Bytes += o.Size
		}
		if _, err := s.base.DeletePrefix(ctx, prefix); err != nil {
			return remote.PrefixDeletion{}, err
		}
		s.mu.Lock()
		for k, sp := range s.spaces {
			if strings.HasPrefix(k, prefix) {
				sp.mu.Lock()
				if sp.open != nil {
					sp.open.discard()
				}
				sp.mu.Unlock()
				delete(s.spaces, k)
			}
		}
		s.mu.Unlock()
		return out, nil
	}
	list, err := s.List(ctx, prefix)
	if err != nil {
		return remote.PrefixDeletion{}, err
	}
	var out remote.PrefixDeletion
	keys := make([]string, 0, len(list))
	for _, o := range list {
		keys = append(keys, o.Key)
		out.Objects++
		out.Bytes += o.Size
	}
	if err := s.DeleteBatch(ctx, keys); err != nil {
		return remote.PrefixDeletion{}, err
	}
	return out, nil
}

// --- reads ------------------------------------------------------------------

// Get streams one object.
func (s *Store) Get(ctx context.Context, key string) (*remote.Object, error) {
	sp, rel, through, err := s.enter(ctx, key)
	if err != nil {
		return nil, err
	}
	if through {
		return s.base.Get(ctx, key)
	}
	e, ok := sp.lookup(rel)
	var open *spool
	if ok && sp.open != nil {
		if _, inOpen := sp.open.entries[rel]; inOpen {
			open = sp.open
		}
	}
	var stored []byte
	if open != nil {
		stored, err = open.read(e.Off, e.Len)
	}
	sp.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bundle: get %q: %w", key, remote.ErrNotFound)
	}
	if !e.packed() {
		return s.base.Get(ctx, key)
	}
	var body io.ReadCloser
	if stored != nil {
		body = io.NopCloser(bytes.NewReader(stored))
	} else if body, err = s.readRange(ctx, sp.ns+packsDir+e.Pack+".zip", e.Off, e.Len); err != nil {
		return nil, fmt.Errorf("bundle: get %q: %w", key, err)
	}
	return &remote.Object{Body: decoder(body, e), Metadata: e.metadata(), Size: e.Size}, nil
}

// readRange reads part of a pack: with a range request where the base store
// has them, otherwise from a whole copy of the pack kept for a little while.
func (s *Store) readRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	if rr, ok := s.base.(remote.RangeReader); ok {
		return rr.GetRange(ctx, key, off, n)
	}
	return s.cache.read(ctx, s.base, key, off, n)
}

// Head describes one object.
func (s *Store) Head(ctx context.Context, key string) (*remote.HeadResult, error) {
	sp, rel, through, err := s.enter(ctx, key)
	if err != nil {
		return nil, err
	}
	if through {
		return s.base.Head(ctx, key)
	}
	e, ok := sp.lookup(rel)
	sp.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("bundle: head %q: %w", key, remote.ErrNotFound)
	}
	if !e.packed() {
		return s.base.Head(ctx, key)
	}
	return &remote.HeadResult{Size: e.Size, LastModified: time.Unix(0, e.Written), Metadata: e.metadata()}, nil
}

// List returns every object under prefix, bundled ones as if they were
// objects of their own, and never bundling's own files.
func (s *Store) List(ctx context.Context, prefix string) ([]remote.ListEntry, error) {
	if ns := Scope(prefix); ns != "" {
		sp := s.space(ns)
		sp.mu.Lock()
		if err := s.load(ctx, sp); err != nil {
			sp.mu.Unlock()
			return nil, err
		}
		if !sp.passthrough {
			out := sp.list(prefix)
			sp.mu.Unlock()
			return out, nil
		}
		sp.mu.Unlock()
		return filterInternal(s.base.List(ctx, prefix))
	}
	// Wider than one backup: the base store's listing, with each bundled
	// backup's contents read from its index instead.
	raw, err := s.base.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	bundled := map[string]bool{}
	for _, o := range raw {
		if ns := Scope(o.Key); ns != "" && o.Key == ns+indexName {
			bundled[ns] = true
		}
	}
	var out []remote.ListEntry
	for _, o := range raw {
		if ns := Scope(o.Key); ns != "" && (bundled[ns] || internal(o.Key)) {
			continue
		}
		out = append(out, o)
	}
	nss := make([]string, 0, len(bundled))
	for ns := range bundled {
		nss = append(nss, ns)
	}
	sort.Strings(nss)
	for _, ns := range nss {
		sp := s.space(ns)
		sp.mu.Lock()
		if err := s.load(ctx, sp); err != nil {
			sp.mu.Unlock()
			return nil, err
		}
		out = append(out, sp.list(ns)...)
		sp.mu.Unlock()
	}
	return out, nil
}

// list is sp's keys under prefix. Called with sp.mu held.
func (sp *space) list(prefix string) []remote.ListEntry {
	seen := map[string]entry{}
	for rel, e := range sp.idx.Entries {
		seen[rel] = e
	}
	if sp.open != nil {
		for rel, e := range sp.open.entries {
			seen[rel] = e
		}
	}
	out := make([]remote.ListEntry, 0, len(seen))
	for rel, e := range seen {
		key := sp.ns + rel
		if strings.HasPrefix(key, prefix) {
			out = append(out, remote.ListEntry{Key: key, Size: e.Size, ModTime: time.Unix(0, e.Written)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func filterInternal(list []remote.ListEntry, err error) ([]remote.ListEntry, error) {
	if err != nil {
		return nil, err
	}
	out := list[:0]
	for _, o := range list {
		if !internal(o.Key) {
			out = append(out, o)
		}
	}
	return out, nil
}

// ListPrefixes returns the immediate child folders under prefix.
func (s *Store) ListPrefixes(ctx context.Context, prefix string) ([]string, error) {
	if Scope(prefix) == "" {
		names, err := s.base.ListPrefixes(ctx, prefix)
		if err != nil {
			return nil, err
		}
		return names, nil
	}
	list, err := s.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, o := range list {
		rest := o.Key[len(prefix):]
		i := strings.Index(rest, "/")
		if i <= 0 {
			continue
		}
		if name := rest[:i]; !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// --- flushing ---------------------------------------------------------------

// Flush sends every pack being filled, deletes what was replaced, tidies
// mostly-empty packs, and writes each changed index. Nothing written since
// the last Flush is on record until it returns.
func (s *Store) Flush(ctx context.Context) error {
	s.mu.Lock()
	spaces := make([]*space, 0, len(s.spaces))
	for _, sp := range s.spaces {
		spaces = append(spaces, sp)
	}
	s.mu.Unlock()
	sort.Slice(spaces, func(i, j int) bool { return spaces[i].ns < spaces[j].ns })

	var first error
	for _, sp := range spaces {
		if err := s.flushSpace(ctx, sp); err != nil && first == nil {
			first = err
		}
	}
	if err := remote.FlushStore(ctx, s.base); err != nil && first == nil {
		first = err
	}
	return first
}

func (s *Store) flushSpace(ctx context.Context, sp *space) error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if !sp.loaded || sp.passthrough {
		return nil
	}
	if err := s.seal(ctx, sp); err != nil {
		return err
	}
	if !sp.dirty && len(sp.staleDirect) == 0 {
		return nil
	}
	if err := s.compact(ctx, sp); err != nil {
		return err
	}
	dead := sp.deadPacks()
	s.sweep(ctx, sp)
	// The index goes first: once it no longer points at a pack, deleting
	// the pack is safe whenever it happens, and a failed delete leaves only
	// garbage for the sweep.
	if err := s.writeIndex(ctx, sp); err != nil {
		return err
	}
	var gone []string
	for _, id := range dead {
		gone = append(gone, sp.ns+packsDir+id+".zip")
	}
	gone = append(gone, sp.staleDirect...)
	sp.staleDirect = nil
	if len(gone) > 0 {
		if err := s.base.DeleteBatch(ctx, gone); err != nil {
			return fmt.Errorf("bundle: delete replaced files: %w", err)
		}
	}
	return nil
}

// deadPacks drops packs no entry points at from the index and returns them.
func (sp *space) deadPacks() []string {
	live := sp.liveBytes()
	var dead []string
	for id := range sp.idx.Packs {
		if live[id] == 0 {
			dead = append(dead, id)
			delete(sp.idx.Packs, id)
			sp.dirty = true
		}
	}
	sort.Strings(dead)
	return dead
}

func (sp *space) liveBytes() map[string]int64 {
	live := map[string]int64{}
	// The same bytes can be on the index twice (a file and its copy in
	// trash); they count once.
	type span struct {
		pack     string
		off, len int64
	}
	seen := map[span]bool{}
	for _, e := range sp.idx.Entries {
		if !e.packed() {
			continue
		}
		k := span{e.Pack, e.Off, e.Len}
		if !seen[k] {
			seen[k] = true
			live[e.Pack] += e.Len
		}
	}
	return live
}

// compact rewrites packs that are mostly dead space into a new one, up to
// compactBudget live bytes per Flush. Called with sp.mu held.
func (s *Store) compact(ctx context.Context, sp *space) error {
	live := sp.liveBytes()
	var victims []string
	var budget int64
	for id, size := range sp.idx.Packs {
		l := live[id]
		if l == 0 || size == 0 || float64(l)/float64(size) >= compactBelow || size-l < s.opts.PackSize/4 {
			continue
		}
		if budget+l > compactBudget {
			continue
		}
		budget += l
		victims = append(victims, id)
	}
	if len(victims) < 1 {
		return nil
	}
	// One sparse pack on its own is not worth a rewrite unless it is
	// nearly empty: rewriting 16 MB to save 8 MB is not tidying.
	if len(victims) == 1 && float64(live[victims[0]])/float64(sp.idx.Packs[victims[0]]) > 0.25 {
		return nil
	}
	sort.Strings(victims)
	isVictim := map[string]bool{}
	for _, id := range victims {
		isVictim[id] = true
	}
	// Every entry in a victim pack, grouped by span so a file and its copy
	// in trash are rewritten once.
	type span struct {
		pack     string
		off, len int64
	}
	byspan := map[span][]string{}
	for rel, e := range sp.idx.Entries {
		if e.packed() && isVictim[e.Pack] {
			k := span{e.Pack, e.Off, e.Len}
			byspan[k] = append(byspan[k], rel)
		}
	}
	spans := make([]span, 0, len(byspan))
	for k := range byspan {
		spans = append(spans, k)
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].pack != spans[j].pack {
			return spans[i].pack < spans[j].pack
		}
		return spans[i].off < spans[j].off
	})

	out, err := newSpool(s.opts.TempDir, newID(s.opts.Now()))
	if err != nil {
		return err
	}
	defer out.discard()
	moved := map[string]entry{}
	for _, k := range spans {
		rc, err := s.readRange(ctx, sp.ns+packsDir+k.pack+".zip", k.off, k.len)
		if err != nil {
			return fmt.Errorf("bundle: tidy %s: %w", sp.ns, err)
		}
		stored, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("bundle: tidy %s: %w", sp.ns, err)
		}
		rels := byspan[k]
		sort.Strings(rels)
		e := sp.idx.Entries[rels[0]]
		if err := out.add(rels[0], &e, stored, s.opts.Now()); err != nil {
			return err
		}
		for _, rel := range rels {
			ne := sp.idx.Entries[rel]
			ne.Pack, ne.Off, ne.Len = e.Pack, e.Off, e.Len
			moved[rel] = ne
		}
	}
	size, err := out.finish()
	if err != nil {
		return err
	}
	if _, err := out.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	err = s.basePut(ctx, remote.PutInput{
		Key: sp.ns + packsDir + out.id + ".zip", Body: out.file, Size: size, ContentType: "application/zip",
		Metadata: remote.Metadata{ModTime: s.opts.Now(), Size: size, Mode: 0o644, Kind: remote.KindFile},
	})
	if err != nil {
		return fmt.Errorf("bundle: tidy %s: %w", sp.ns, err)
	}
	sp.idx.Packs[out.id] = size
	for rel, e := range moved {
		sp.idx.Entries[rel] = e
	}
	sp.dirty = true
	return nil
}

func (s *Store) writeIndex(ctx context.Context, sp *space) error {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(sp.idx); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	err := s.basePut(ctx, remote.PutInput{
		Key: sp.ns + indexName, Body: bytes.NewReader(buf.Bytes()), Size: int64(buf.Len()),
		ContentType: "application/gzip",
		Metadata:    remote.Metadata{ModTime: s.opts.Now(), Size: int64(buf.Len()), Mode: 0o644, Kind: remote.KindFile},
	})
	if err != nil {
		return fmt.Errorf("bundle: save index of %s: %w", sp.ns, err)
	}
	sp.dirty = false
	return nil
}

// sweep deletes packs nothing points at -- left by a run that died between
// sending a pack and saving the index -- once a day, just before the index is
// saved. Failing is fine: it is tidying, tried again next time. Called with
// sp.mu held.
func (s *Store) sweep(ctx context.Context, sp *space) {
	now := s.opts.Now()
	if time.Unix(sp.idx.SweptAt, 0).Add(sweepEvery).After(now) {
		return
	}
	list, err := s.base.List(ctx, sp.ns+packsDir)
	if err != nil {
		return
	}
	var orphans []string
	for _, o := range list {
		id := strings.TrimSuffix(path.Base(o.Key), ".zip")
		if _, ok := sp.idx.Packs[id]; ok || now.Sub(o.ModTime) < orphanAge {
			continue
		}
		orphans = append(orphans, o.Key)
	}
	if len(orphans) > 0 {
		if err := s.base.DeleteBatch(ctx, orphans); err != nil {
			return
		}
	}
	sp.idx.SweptAt = now.Unix()
	sp.dirty = true
}

// --- the rest is the base store's --------------------------------------------

func (s *Store) AbandonStaleUploads(ctx context.Context) (int, error) {
	return s.base.AbandonStaleUploads(ctx)
}

func (s *Store) ListMultipartUploads(ctx context.Context, prefix string) ([]remote.MultipartUpload, error) {
	return s.base.ListMultipartUploads(ctx, prefix)
}

func (s *Store) AbortUpload(ctx context.Context, key, uploadID string) error {
	return s.base.AbortUpload(ctx, key, uploadID)
}

func (s *Store) Bucket() string { return s.base.Bucket() }

var (
	_ remote.Store   = (*Store)(nil)
	_ remote.Mover   = (*Store)(nil)
	_ remote.Flusher = (*Store)(nil)
)

// --- compression --------------------------------------------------------------

// compress deflates data when that saves at least a tenth, and stores it as
// it is otherwise -- photos and archives do not shrink, and inflating them on
// every restore would be work for nothing.
func compress(data []byte) (stored []byte, method uint16, crc uint32) {
	crc = crc32IEEE(data)
	if len(data) < 64 {
		return data, 0, crc
	}
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
	_, _ = w.Write(data)
	_ = w.Close()
	if buf.Len() < len(data)*9/10 {
		return buf.Bytes(), 8, crc
	}
	return data, 0, crc
}

// decoder inflates if needed and checks the file came back whole.
func decoder(body io.ReadCloser, e entry) io.ReadCloser {
	var r io.Reader = body
	if e.Method == 8 {
		r = flate.NewReader(body)
	}
	return &checked{r: r, c: body, want: e.CRC, size: e.Size, h: newCRC()}
}

// cleanupTemp removes a file, ignoring a file already gone.
func cleanupTemp(name string) {
	if name != "" {
		_ = os.Remove(name)
	}
}
