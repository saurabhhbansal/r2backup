// Package fixtures builds synthetic folder trees for tests, and compares two
// trees byte for byte.
//
// Every awkward case in here is one that has broken a real backup tool: a name
// spelled NFD on macOS and NFC everywhere else, a path over Windows' 260
// character limit, a symlink farm of the shape pnpm builds, a directory with
// nothing in it. A backup that has not been restored has not been tested, so
// Compare exists to make the restore side of every test assert on actual bytes
// rather than on a success return.
package fixtures

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Names that have to survive a round trip. Kept as exported vars so tests can
// assert on the specific ones they care about.
var (
	// The same filename precomposed and decomposed. macOS returns the second
	// for a file created as the first, which made a predecessor re-upload the
	// file on every single run.
	NameNFC = "résumé.txt"
	NameNFD = "résumé.txt"

	// Characters that must survive being turned into an object key.
	AwkwardNames = []string{
		"a file with spaces.txt",
		"hash#in#name.txt",
		"question?mark.txt",
		"percent%20encoded.txt",
		"emoji-🎉-party.txt",
		"plus+and&ampersand.txt",
		"quote'single.txt",
		"bracket[square].txt",
		"dot.in.the.middle.tar.gz",
	}

	// Windows refuses these outright. Restoring onto Windows must handle them
	// rather than crashing partway through.
	WindowsReservedNames = []string{"CON", "PRN", "AUX", "NUL", "COM1", "LPT1"}
)

// Spec describes the tree to build. The zero value builds nothing, so every
// test opts explicitly into the cases it wants and reads as documentation of
// what it is covering.
type Spec struct {
	// SmallFiles is the count of ordinary small files, spread over
	// subdirectories. This is the request-bound case: 60,000 of these is the
	// shape of a real node_modules.
	SmallFiles int
	// SmallFileSize is the size of each; 0 means a varied 1-4KB.
	SmallFileSize int
	// LargeFileSize, when non-zero, writes one file of this size. Use it to
	// exercise multipart upload without holding the bytes in memory.
	LargeFileSize int64
	// ZeroByteFiles is how many empty files to create.
	ZeroByteFiles int
	// EmptyDirs is how many directories with no contents to create.
	EmptyDirs int
	// UnicodeNames writes NameNFC and NameNFD.
	UnicodeNames bool
	// AwkwardNames writes every entry in AwkwardNames.
	AwkwardNames bool
	// DeepPath writes a file at a path longer than 260 characters.
	DeepPath bool
	// ReservedNames writes the Windows reserved names. Ignored on Windows,
	// where they cannot be created at all.
	ReservedNames bool
	// Symlinks builds a pnpm-shaped link farm: real packages under .store,
	// and links to them from node_modules. Ignored on Windows without
	// symlink privilege.
	Symlinks bool
	// CaseOnlyPair writes Foo.txt and foo.txt, which are the same file on
	// Windows and macOS and two files on Linux.
	CaseOnlyPair bool
	// Seed makes content deterministic across runs.
	Seed int64
}

// Manifest is what Build actually created.
type Manifest struct {
	Files    []string // relative, slash-separated
	Dirs     []string
	Symlinks []string
	Bytes    int64
	Skipped  []string // cases the platform could not represent
}

// Build materialises spec under root, which must already exist.
func Build(root string, spec Spec) (*Manifest, error) {
	seed := spec.Seed
	if seed == 0 {
		seed = 1
	}
	rng := rand.New(rand.NewSource(seed))
	m := &Manifest{}

	writeFile := func(rel string, size int) error {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		buf := make([]byte, size)
		rng.Read(buf)
		if err := os.WriteFile(abs, buf, 0o644); err != nil {
			return err
		}
		m.Files = append(m.Files, rel)
		m.Bytes += int64(size)
		return nil
	}

	for i := 0; i < spec.SmallFiles; i++ {
		size := spec.SmallFileSize
		if size == 0 {
			size = 1024 + rng.Intn(3072)
		}
		// Fan out so no single directory holds all of them; a flat directory
		// of 60,000 entries is its own unrelated performance problem.
		rel := fmt.Sprintf("pkg/%02d/%02d/file%06d.dat", i%32, (i/32)%32, i)
		if err := writeFile(rel, size); err != nil {
			return nil, err
		}
	}

	for i := 0; i < spec.ZeroByteFiles; i++ {
		if err := writeFile(fmt.Sprintf("empty/zero%03d.txt", i), 0); err != nil {
			return nil, err
		}
	}

	for i := 0; i < spec.EmptyDirs; i++ {
		rel := fmt.Sprintf("hollow/dir%03d", i)
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
			return nil, err
		}
		m.Dirs = append(m.Dirs, rel)
	}

	if spec.LargeFileSize > 0 {
		rel := "big/payload.bin"
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return nil, err
		}
		f, err := os.Create(abs)
		if err != nil {
			return nil, err
		}
		// Streamed, so a 200MB fixture does not need 200MB of RAM.
		if _, err := io.CopyN(f, rng, spec.LargeFileSize); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		m.Files = append(m.Files, rel)
		m.Bytes += spec.LargeFileSize
	}

	if spec.UnicodeNames {
		before := len(m.Files)
		for _, n := range []string{NameNFC, NameNFD} {
			if err := writeFile("unicode/"+n, 64); err != nil {
				m.Skipped = append(m.Skipped, "unicode/"+n)
			}
		}
		// Whether these are one file or two is decided by the filesystem, not
		// by us. ext4 stores the bytes it was given and holds both; APFS
		// normalises on the way in, so the second write silently lands on the
		// first and the directory holds one.
		//
		// Detecting that by counting what is actually on disk matters more
		// than it looks: the write SUCCEEDS on APFS, so an error check alone
		// reports two files where there is one, and every test downstream then
		// expects a collision that cannot happen there.
		if sameFileCount(root, "unicode") == 1 {
			m.Files = m.Files[:before+1]
			m.Skipped = append(m.Skipped,
				"unicode/"+NameNFD+" (this filesystem normalises names, so NFC and NFD are one file)")
		}
	}

	if spec.AwkwardNames {
		for _, n := range AwkwardNames {
			if err := writeFile("awkward/"+n, 32); err != nil {
				m.Skipped = append(m.Skipped, "awkward/"+n)
			}
		}
	}

	if spec.DeepPath {
		// Comfortably past Windows' 260-character MAX_PATH.
		deep := "deep"
		for i := 0; i < 12; i++ {
			deep += "/" + strings.Repeat(fmt.Sprintf("level%02d", i), 3)
		}
		rel := deep + "/leaf.txt"
		if err := writeFile(rel, 16); err != nil {
			m.Skipped = append(m.Skipped, rel)
		}
	}

	if spec.ReservedNames && runtime.GOOS != "windows" {
		for _, n := range WindowsReservedNames {
			if err := writeFile("reserved/"+n+".txt", 16); err != nil {
				m.Skipped = append(m.Skipped, "reserved/"+n)
			}
		}
	} else if spec.ReservedNames {
		m.Skipped = append(m.Skipped, "reserved/* (cannot be created on Windows)")
	}

	if spec.CaseOnlyPair {
		if err := writeFile("case/Foo.txt", 8); err != nil {
			return nil, err
		}
		// On Windows and macOS this overwrites the first rather than creating
		// a second file. Detect that instead of asserting a count.
		before := len(m.Files)
		if err := writeFile("case/foo.txt", 9); err != nil {
			m.Skipped = append(m.Skipped, "case/foo.txt")
		} else if sameFileCount(root, "case") == 1 {
			m.Files = m.Files[:before]
			m.Skipped = append(m.Skipped, "case/foo.txt (filesystem is case-insensitive)")
		}
	}

	if spec.Symlinks {
		if err := buildLinkFarm(root, m, rng); err != nil {
			m.Skipped = append(m.Skipped, "symlinks (not permitted on this platform)")
		}
	}

	sort.Strings(m.Files)
	sort.Strings(m.Dirs)
	sort.Strings(m.Symlinks)
	return m, nil
}

// buildLinkFarm reproduces the shape pnpm creates: one real copy of each
// package under a store, and symlinks into it from node_modules. Following
// these links instead of storing them turns a few megabytes into gigabytes.
func buildLinkFarm(root string, m *Manifest, rng *rand.Rand) error {
	for i := 0; i < 8; i++ {
		pkg := fmt.Sprintf("node_modules/.store/pkg%02d", i)
		abs := filepath.Join(root, filepath.FromSlash(pkg))
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return err
		}
		buf := make([]byte, 512)
		rng.Read(buf)
		rel := pkg + "/index.js"
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), buf, 0o644); err != nil {
			return err
		}
		m.Files = append(m.Files, rel)
		m.Bytes += 512

		link := fmt.Sprintf("node_modules/pkg%02d", i)
		linkAbs := filepath.Join(root, filepath.FromSlash(link))
		if err := os.MkdirAll(filepath.Dir(linkAbs), 0o755); err != nil {
			return err
		}
		target := filepath.Join("..", ".store", fmt.Sprintf("pkg%02d", i))
		if err := os.Symlink(target, linkAbs); err != nil {
			return err
		}
		m.Symlinks = append(m.Symlinks, link)
	}
	return nil
}

func sameFileCount(root, sub string) int {
	entries, err := os.ReadDir(filepath.Join(root, sub))
	if err != nil {
		return -1
	}
	return len(entries)
}

// Difference is one way two trees disagree.
type Difference struct {
	Path   string
	Reason string
}

func (d Difference) String() string { return d.Path + ": " + d.Reason }

// Compare walks two trees and reports every way they differ: missing files,
// extra files, differing contents, differing symlink targets, and modification
// times more than tolerance apart.
//
// This is what makes a restore test meaningful. Asserting that restore returned
// nil proves nothing; asserting that every byte came back proves the product.
func Compare(want, got string, tolerance time.Duration) ([]Difference, error) {
	wantMap, err := index(want)
	if err != nil {
		return nil, fmt.Errorf("index %q: %w", want, err)
	}
	gotMap, err := index(got)
	if err != nil {
		return nil, fmt.Errorf("index %q: %w", got, err)
	}

	var diffs []Difference
	for rel, w := range wantMap {
		g, ok := gotMap[rel]
		if !ok {
			diffs = append(diffs, Difference{rel, "missing from the restored tree"})
			continue
		}
		if w.kind != g.kind {
			diffs = append(diffs, Difference{rel, fmt.Sprintf("kind %s became %s", w.kind, g.kind)})
			continue
		}
		switch w.kind {
		case "symlink":
			if w.target != g.target {
				diffs = append(diffs, Difference{rel, fmt.Sprintf("target %q became %q", w.target, g.target)})
			}
		case "file":
			if w.size != g.size {
				diffs = append(diffs, Difference{rel, fmt.Sprintf("size %d became %d", w.size, g.size)})
				continue
			}
			if w.sum != g.sum {
				diffs = append(diffs, Difference{rel, "contents differ"})
				continue
			}
			if d := w.mod.Sub(g.mod); d > tolerance || d < -tolerance {
				diffs = append(diffs, Difference{rel, fmt.Sprintf("mtime differs by %s", d)})
			}
		}
	}
	for rel := range gotMap {
		if _, ok := wantMap[rel]; !ok {
			diffs = append(diffs, Difference{rel, "unexpected: not in the original tree"})
		}
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Path < diffs[j].Path })
	return diffs, nil
}

type node struct {
	kind   string
	size   int64
	sum    string
	mod    time.Time
	target string
}

func index(root string) (map[string]node, error) {
	out := map[string]node{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			out[rel] = node{kind: "symlink", target: filepath.ToSlash(target), mod: info.ModTime()}
		case d.IsDir():
			entries, err := os.ReadDir(path)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				out[rel] = node{kind: "empty-dir", mod: info.ModTime()}
			}
		default:
			sum, err := checksum(path)
			if err != nil {
				return err
			}
			out[rel] = node{kind: "file", size: info.Size(), sum: sum, mod: info.ModTime()}
		}
		return nil
	})
	return out, err
}

func checksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Mutate applies a realistic set of changes to a tree: edit some files, delete
// some, add some. Returns what it did, so a test can assert the next backup
// moves exactly that much and no more.
func Mutate(root string, edit, del, add int, seed int64) (edited, deleted, added []string, err error) {
	rng := rand.New(rand.NewSource(seed))
	var files []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}
	sort.Strings(files)
	rng.Shuffle(len(files), func(i, j int) { files[i], files[j] = files[j], files[i] })

	for i := 0; i < edit && i < len(files); i++ {
		rel := files[i]
		abs := filepath.Join(root, filepath.FromSlash(rel))
		buf := make([]byte, 256)
		rng.Read(buf)
		if err := os.WriteFile(abs, buf, 0o644); err != nil {
			return nil, nil, nil, err
		}
		edited = append(edited, rel)
	}
	for i := edit; i < edit+del && i < len(files); i++ {
		rel := files[i]
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			return nil, nil, nil, err
		}
		deleted = append(deleted, rel)
	}
	for i := 0; i < add; i++ {
		rel := fmt.Sprintf("added/new%04d.dat", i)
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return nil, nil, nil, err
		}
		buf := make([]byte, 128)
		rng.Read(buf)
		if err := os.WriteFile(abs, buf, 0o644); err != nil {
			return nil, nil, nil, err
		}
		added = append(added, rel)
	}
	sort.Strings(edited)
	sort.Strings(deleted)
	sort.Strings(added)
	return edited, deleted, added, nil
}

var _ = bytes.Equal // kept for future byte-level helpers
