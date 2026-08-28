// Package scan walks a folder and produces the exact list of things that need
// backing up, before a single byte is transferred.
//
// Doing the whole walk up front is what lets the progress bar be honest: the
// total is known before the bar appears and never moves afterwards. The tool
// this replaces interleaved discovery with transfer, so its denominator kept
// changing and its estimate was worthless.
package scan

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Kind distinguishes the three things a backup has to represent. Object storage
// only really stores files, so the other two need explicit handling or they are
// lost silently.
type Kind uint8

const (
	KindFile Kind = iota
	// KindSymlink is stored as its target string, never followed. Following
	// symlinks would expand a pnpm node_modules -- which is built almost
	// entirely out of links into .pnpm/ -- into hundreds of copies of the
	// same bytes.
	KindSymlink
	// KindEmptyDir needs a marker object because object storage has no
	// directories; without one, an empty folder simply disappears on restore.
	KindEmptyDir
)

func (k Kind) String() string {
	switch k {
	case KindFile:
		return "file"
	case KindSymlink:
		return "symlink"
	case KindEmptyDir:
		return "empty-dir"
	}
	return "unknown"
}

// Entry is one thing found on disk.
type Entry struct {
	Key     string      // relative, forward-slashed, NFC. See Key.
	Size    int64       // bytes; 0 for symlinks and empty dirs
	ModTime time.Time   // restored onto the file
	Mode    fs.FileMode // permission bits, applied on Unix, ignored on Windows
	Kind    Kind
	Target  string // symlink target, when Kind is KindSymlink
}

// Problem is something we could not read. Problems never abort a run: a locked
// Outlook .pst or a permission-denied directory must not stop the other 60,000
// files from being backed up. They are collected, counted, and reported.
type Problem struct {
	Path string
	Err  error
}

// Result is the complete picture of the folder.
type Result struct {
	Entries  []Entry
	Problems []Problem

	Files    int64 // count of KindFile entries
	Bytes    int64 // total size of KindFile entries
	Symlinks int64
	Dirs     int64 // directories visited
}

// Options controls a walk.
type Options struct {
	// Root is the folder to scan. It is made absolute before use.
	Root string
	// Skip reports whether a relative key should be left out entirely. A
	// directory that is skipped is not descended into. Nil includes
	// everything, which is the default the picker starts from.
	Skip func(key string, isDir bool) bool
	// Progress, if set, is called periodically with the number of entries
	// found so far, so the caller can render "Scanning... 32,104 files".
	Progress func(found int64)
}

// ErrRootMissing means the folder we were asked to scan is not there.
//
// This is deliberately its own error and callers must treat it as fatal to the
// run. It is never correct to read a missing root as "every file was deleted":
// that turns a renamed folder, an unplugged drive or an unmounted share into
// the deletion of an entire backup.
var ErrRootMissing = errors.New("set root does not exist")

// Walk scans Root and returns everything under it.
func Walk(ctx context.Context, opts Options) (*Result, error) {
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrRootMissing
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("set root is not a directory: " + root)
	}

	res := &Result{}
	var found int64
	report := func() {
		if opts.Progress != nil {
			opts.Progress(found)
		}
	}

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			// Unreadable directory or file: record and carry on. Returning
			// the error here would abandon the rest of the tree.
			res.Problems = append(res.Problems, Problem{Path: path, Err: err})
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		key, kerr := Key(root, path)
		if kerr != nil {
			res.Problems = append(res.Problems, Problem{Path: path, Err: kerr})
			return nil
		}
		if key == "" {
			res.Dirs++
			return nil // the root itself
		}

		if opts.Skip != nil && opts.Skip(key, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, terr := os.Readlink(path)
			if terr != nil {
				res.Problems = append(res.Problems, Problem{Path: path, Err: terr})
				return nil
			}
			li, lerr := d.Info()
			if lerr != nil {
				res.Problems = append(res.Problems, Problem{Path: path, Err: lerr})
				return nil
			}
			res.Entries = append(res.Entries, Entry{
				Key:     key,
				ModTime: li.ModTime(),
				Mode:    li.Mode(),
				Kind:    KindSymlink,
				Target:  filepath.ToSlash(target),
			})
			res.Symlinks++
			found++

		case d.IsDir():
			res.Dirs++
			empty, eerr := isEmptyDir(path)
			if eerr != nil {
				res.Problems = append(res.Problems, Problem{Path: path, Err: eerr})
				return nil
			}
			if empty {
				di, ierr := d.Info()
				if ierr != nil {
					res.Problems = append(res.Problems, Problem{Path: path, Err: ierr})
					return nil
				}
				res.Entries = append(res.Entries, Entry{
					Key:     key,
					ModTime: di.ModTime(),
					Mode:    di.Mode(),
					Kind:    KindEmptyDir,
				})
				found++
			}

		case d.Type().IsRegular():
			fi, ierr := d.Info()
			if ierr != nil {
				res.Problems = append(res.Problems, Problem{Path: path, Err: ierr})
				return nil
			}
			res.Entries = append(res.Entries, Entry{
				Key:     key,
				Size:    fi.Size(),
				ModTime: fi.ModTime(),
				Mode:    fi.Mode(),
				Kind:    KindFile,
			})
			res.Files++
			res.Bytes += fi.Size()
			found++

		default:
			// Sockets, devices, fifos. Nothing a backup can meaningfully
			// carry, but they are reported rather than dropped in silence.
			res.Problems = append(res.Problems, Problem{
				Path: path,
				Err:  errors.New("unsupported file type: " + d.Type().String()),
			})
		}

		if found%2048 == 0 {
			report()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	report()

	// Deterministic order, so two scans of the same tree produce the same
	// plan and tests can compare results directly.
	sort.Slice(res.Entries, func(i, j int) bool { return res.Entries[i].Key < res.Entries[j].Key })
	sort.Slice(res.Problems, func(i, j int) bool { return res.Problems[i].Path < res.Problems[j].Path })
	return res, nil
}

func isEmptyDir(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return len(names) == 0, nil
}
