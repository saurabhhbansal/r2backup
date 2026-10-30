package drime

import (
	"context"
	"path"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// --- Names -------------------------------------------------------------------

// Drime refuses a few names a filesystem allows: a backslash, a name with a
// leading or trailing space, control characters, and "." and "..". Each is
// swapped for a look-alike that Drime takes, the way rclone does it, so a
// file keeps a recognisable name in the web app and comes back exactly as it
// was. A name that already contains one of the look-alikes has it escaped
// with quoteRune first, so decoding can never mistake it for a substitution.
const quoteRune = '‛'

var encodeMap = map[rune]rune{
	'\\': '＼',
	0x7f: '␡',
}

func isSubstitute(r rune) bool {
	if r == '＼' || r == '␡' || r == '␠' || r == '．' || r == quoteRune {
		return true
	}
	return r >= 0x2400 && r <= 0x241f // control pictures
}

func encodeName(name string) string {
	switch name {
	case ".":
		return "．"
	case "..":
		return "．．"
	}
	var b strings.Builder
	n := utf8.RuneCountInString(name)
	i := 0
	for _, r := range name {
		switch {
		case isSubstitute(r):
			b.WriteRune(quoteRune)
			b.WriteRune(r)
		case r == ' ' && (i == 0 || i == n-1):
			b.WriteRune('␠')
		case r < 0x20:
			b.WriteRune(0x2400 + r)
		default:
			if m, ok := encodeMap[r]; ok {
				b.WriteRune(m)
			} else {
				b.WriteRune(r)
			}
		}
		i++
	}
	return b.String()
}

func decodeName(name string) string {
	switch name {
	case "．":
		return "."
	case "．．":
		return ".."
	}
	if !strings.ContainsFunc(name, isSubstitute) {
		return name
	}
	var b strings.Builder
	quoted := false
	for _, r := range name {
		if quoted {
			b.WriteRune(r)
			quoted = false
			continue
		}
		switch {
		case r == quoteRune:
			quoted = true
		case r == '＼':
			b.WriteRune('\\')
		case r == '␡':
			b.WriteRune(0x7f)
		case r == '␠':
			b.WriteRune(' ')
		case r >= 0x2400 && r <= 0x241f:
			b.WriteRune(r - 0x2400)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// --- Folder tree ---------------------------------------------------------------

// dir is one folder as last listed. Names in it are decoded.
type dir struct {
	id      string
	folders map[string]string // name -> id
	// files maps a name to every entry Drime holds by that name, newest
	// first. There is normally one; more means an earlier replace was
	// interrupted between the upload and the delete of the old copy.
	files map[string][]item
}

func (d *dir) newest(name string) (item, bool) {
	if fs := d.files[name]; len(fs) > 0 {
		return fs[0], true
	}
	return item{}, false
}

func (d *dir) addFile(name string, it item) {
	d.files[name] = append([]item{it}, d.files[name]...)
}

// tree caches the folder structure under the backup root, keyed by path
// relative to it ("" is the root itself). A folder is listed the first time
// anything inside it is asked about and then kept up to date by this
// process's own writes, so a backup run lists each folder at most once.
//
// One lock covers the cache, and folder creation happens under it: that is
// what stops two uploads into the same new folder from creating it twice --
// Drime allows two folders with one name side by side. Listings are the
// exception; see load.
type tree struct {
	api    *api
	rootID string

	mu   sync.Mutex
	dirs map[string]*dir
	// inflight marks folders being listed right now; see load.
	inflight map[string]chan struct{}
	// missing remembers paths found not to exist, so a restore asking about
	// a folder that is not there does not list its parent again per file.
	missing map[string]bool
}

func newTree(a *api, rootID string) *tree {
	return &tree{api: a, rootID: rootID, dirs: map[string]*dir{}, missing: map[string]bool{},
		inflight: map[string]chan struct{}{}}
}

// load lists folder id as path p, caching the result. Caller holds t.mu.
//
// The lock is let go for the listing itself, which is a network round trip
// of a second or two: holding it would make every upload into every other
// folder wait in line behind it. Two callers wanting the same folder share
// one listing -- the second waits for the first rather than asking again.
func (t *tree) load(ctx context.Context, p, id string) (*dir, error) {
	for {
		if d, ok := t.dirs[p]; ok {
			return d, nil
		}
		wait, busy := t.inflight[p]
		if !busy {
			break
		}
		t.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			t.mu.Lock()
			return nil, ctx.Err()
		}
		t.mu.Lock()
	}
	done := make(chan struct{})
	t.inflight[p] = done
	t.mu.Unlock()
	items, err := t.api.listFolder(ctx, id, false)
	t.mu.Lock()
	delete(t.inflight, p)
	close(done)
	if err != nil {
		return nil, err
	}
	d := &dir{id: id, folders: map[string]string{}, files: map[string][]item{}}
	for _, it := range items {
		name := decodeName(it.Name)
		if it.isFolder() {
			// Two folders with one name: keep the first, so every lookup
			// agrees on the same one.
			if _, dup := d.folders[name]; !dup {
				d.folders[name] = it.ID.String()
			}
			continue
		}
		d.files[name] = append(d.files[name], it)
	}
	for _, fs := range d.files {
		sort.SliceStable(fs, func(i, j int) bool { return fs[i].UpdatedAt.After(fs[j].UpdatedAt) })
	}
	t.dirs[p] = d
	return d, nil
}

// resolve returns the folder at p, creating any part of it that is missing
// when create is set. A missing folder with create unset is (nil, nil).
func (t *tree) resolve(ctx context.Context, p string, create bool) (*dir, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.resolveLocked(ctx, p, create)
}

func (t *tree) resolveLocked(ctx context.Context, p string, create bool) (*dir, error) {
	p = strings.Trim(p, "/")
	if d, ok := t.dirs[p]; ok {
		return d, nil
	}
	if !create && t.missing[p] {
		return nil, nil
	}
	if p == "" {
		return t.load(ctx, "", t.rootID)
	}
	parentPath, name := path.Split(p)
	parent, err := t.resolveLocked(ctx, parentPath, create)
	if err != nil || parent == nil {
		if parent == nil && err == nil {
			t.missing[p] = true
		}
		return nil, err
	}
	if id, ok := parent.folders[name]; ok {
		return t.load(ctx, p, id)
	}
	if !create {
		t.missing[p] = true
		return nil, nil
	}
	f, err := t.api.createFolder(ctx, parent.id, encodeName(name))
	if err != nil {
		return nil, err
	}
	id := f.ID.String()
	parent.folders[name] = id
	delete(t.missing, p)
	d := &dir{id: id, folders: map[string]string{}, files: map[string][]item{}}
	t.dirs[p] = d
	return d, nil
}

// lookup finds the newest file stored at key.
func (t *tree) lookup(ctx context.Context, key string) (item, bool, error) {
	dp, name := path.Split(key)
	d, err := t.resolve(ctx, dp, false)
	if err != nil || d == nil {
		return item{}, false, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	it, ok := d.newest(name)
	return it, ok, nil
}

// all returns every entry stored at key, newest first.
func (t *tree) all(ctx context.Context, key string) ([]item, error) {
	dp, name := path.Split(key)
	d, err := t.resolve(ctx, dp, false)
	if err != nil || d == nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]item(nil), d.files[name]...), nil
}

// put records that key now holds it, and only it.
func (t *tree) put(key string, it item) {
	dp, name := path.Split(key)
	t.mu.Lock()
	defer t.mu.Unlock()
	if d, ok := t.dirs[strings.Trim(dp, "/")]; ok {
		d.files[name] = nil
		d.addFile(name, it)
	}
}

// forgetIDs drops entries by id wherever they are cached.
func (t *tree) forgetIDs(ids map[string]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, d := range t.dirs {
		for name, fs := range d.files {
			kept := fs[:0]
			for _, it := range fs {
				if !ids[it.ID.String()] {
					kept = append(kept, it)
				}
			}
			if len(kept) == 0 {
				delete(d.files, name)
			} else {
				d.files[name] = kept
			}
		}
	}
}

// forgetDir drops a folder and everything under it from the cache, after it
// was deleted on the server.
func (t *tree) forgetDir(p string) {
	p = strings.Trim(p, "/")
	t.mu.Lock()
	defer t.mu.Unlock()
	for k := range t.dirs {
		if k == p || strings.HasPrefix(k, p+"/") {
			delete(t.dirs, k)
		}
	}
	parentPath, name := path.Split(p)
	if parent, ok := t.dirs[strings.Trim(parentPath, "/")]; ok {
		delete(parent.folders, name)
	}
	t.missing[p] = true
}

// fileEntry is one file found by walk.
type fileEntry struct {
	key string
	it  item
}

// walk returns every file under folder p, recursively, with keys relative to
// the backup root. Where duplicates share a name only the newest is
// reported, matching what lookup would return.
func (t *tree) walk(ctx context.Context, p string) ([]fileEntry, error) {
	p = strings.Trim(p, "/")
	d, err := t.resolve(ctx, p, false)
	if err != nil || d == nil {
		return nil, err
	}
	var out []fileEntry
	t.mu.Lock()
	names := make([]string, 0, len(d.files))
	for name := range d.files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		it, _ := d.newest(name)
		out = append(out, fileEntry{key: join(p, name), it: it})
	}
	sub := make([]string, 0, len(d.folders))
	for name := range d.folders {
		sub = append(sub, name)
	}
	t.mu.Unlock()
	sort.Strings(sub)
	for _, name := range sub {
		more, err := t.walk(ctx, join(p, name))
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
	}
	return out, nil
}

// folders lists the names of the folders directly inside p.
func (t *tree) folders(ctx context.Context, p string) ([]string, error) {
	d, err := t.resolve(ctx, p, false)
	if err != nil || d == nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(d.folders))
	for name := range d.folders {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func join(dirPath, name string) string {
	if dirPath == "" {
		return name
	}
	return dirPath + "/" + name
}
