package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// itemKind is what an object represents, decided from its metadata at
// download time -- see classify.
type itemKind uint8

const (
	kindFile itemKind = iota
	kindSymlink
	kindEmptyDir
)

// classify decides what kind of thing an object represents.
//
// A symlink is unambiguous: Metadata.Symlink is only ever set for one (see
// remote.Metadata). An empty directory marker is not so lucky. Object
// storage has no directories of its own, so backup writes one as a
// zero-byte object -- but remote.Metadata.ToS3 stores only Mode.Perm(),
// stripping every type bit before the object ever reaches the bucket, so
// the marker and a genuine zero-byte file arrive back here with identical
// metadata: same size, same absent Symlink, same bare permission bits. The
// one bit that does survive and still means something is the owner-execute
// bit: a directory is unusable without it (nothing can even list what's
// inside), and os.MkdirAll always sets it, while an empty *file* has no
// reason to carry it. That is a heuristic, not a certainty -- an empty,
// deliberately-executable placeholder file would be misread as a directory
// marker -- but it is the only signal this wire format leaves behind, and
// every empty directory this codebase itself ever writes satisfies it.
// classify decides what to reconstruct from an object.
//
// The object says what it is: backup stamps a "kind" into the metadata
// precisely so this is not a guess. The fallback below only applies to objects
// written before that field existed, and it is a guess -- an empty executable
// file (touch run.sh; chmod +x run.sh) is indistinguishable from a directory
// marker without it, and would come back as a directory. That is exactly why
// the explicit field was added.
func classify(meta remote.Metadata) itemKind {
	switch meta.Kind {
	case remote.KindSymlink:
		return kindSymlink
	case remote.KindEmptyDir:
		return kindEmptyDir
	case remote.KindFile:
		return kindFile
	}
	// No kind recorded: an object from before the field existed.
	if meta.Symlink != "" {
		return kindSymlink
	}
	if meta.Size == 0 && meta.Mode.Perm()&0o100 != 0 {
		return kindEmptyDir
	}
	return kindFile
}

// errVerifyMismatch is wrapped into the error returned for an item whose
// re-read from disk did not match what was written. errors.Is finds it, so
// callers (and this package's own worker loop) can tell a verify failure
// apart from every other reason a download might fail.
var errVerifyMismatch = errors.New("restore: downloaded content did not match on re-read")

// downloadResult is what a batch of workers accomplished.
type downloadResult struct {
	downloaded int
	bytes      int64
	verified   int
	mismatches []string
	failures   []Failure
}

// runWorkers drains items with a bounded pool of goroutines, feeding
// tracker as bytes land. It is the download-side twin of
// internal/engine's upload pool, sized down: restore has no adaptive
// concurrency or move phase to coordinate, just a fixed number of
// downloads in flight and a single kind of job.
func runWorkers(ctx context.Context, client Backend, items []planItem, workers int, verify bool, tracker *progress.Tracker) downloadResult {
	if workers <= 0 {
		workers = 16
	}
	if workers > len(items) {
		workers = len(items)
	}
	if workers < 1 {
		workers = 1
	}

	jobs := make(chan planItem, len(items))
	for _, it := range items {
		jobs <- it
	}
	close(jobs)

	type outcome struct {
		relPath  string
		bytes    int64
		verified bool
		err      error
	}
	results := make(chan outcome, len(items))

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				if ctx.Err() != nil {
					// The run is being cancelled and this item was never
					// started: leave it unreported rather than guessing at
					// success or failure, exactly like engine.worker does
					// for a job cancellation strands before it can even be
					// dispatched.
					continue
				}
				n, ok, err := processItem(ctx, client, it, verify, tracker)
				results <- outcome{relPath: it.relPath, bytes: n, verified: ok, err: err}
			}
		}()
	}
	wg.Wait()
	close(results)

	var res downloadResult
	for o := range results {
		if o.err != nil {
			res.failures = append(res.failures, Failure{Key: o.relPath, Err: o.err})
			if errors.Is(o.err, errVerifyMismatch) {
				res.mismatches = append(res.mismatches, o.relPath)
			}
			continue
		}
		res.downloaded++
		res.bytes += o.bytes
		if o.verified {
			res.verified++
		}
	}
	return res
}

// reporter is the subset of progress.Tracker a download needs, restated as
// an interface so a nil tracker never has to be nil-checked at every call
// site -- the same reason backup/adapters.go wraps *progress.Tracker
// rather than passing it around bare.
type reporter interface {
	AddBytes(n int64)
	Complete(bytes int64)
}

type nopReporter struct{}

func (nopReporter) AddBytes(int64) {}
func (nopReporter) Complete(int64) {}

func reporterFor(t *progress.Tracker) reporter {
	if t == nil {
		return nopReporter{}
	}
	return t
}

// processItem fetches one object and reconstructs it on disk. The object's
// body is read exactly once, streamed straight to its destination; nothing
// about kind is known until the metadata comes back with it, which is why
// listing never tries to classify ahead of time (see buildPlan's doc).
func processItem(ctx context.Context, client Backend, it planItem, verify bool, tracker *progress.Tracker) (bytesWritten int64, verified bool, err error) {
	obj, err := client.Get(ctx, it.key)
	if err != nil {
		return 0, false, fmt.Errorf("get %s: %w", it.relPath, err)
	}
	defer obj.Body.Close()

	rep := reporterFor(tracker)

	switch classify(obj.Metadata) {
	case kindSymlink:
		if err := restoreSymlink(it.localPath, obj.Metadata.Symlink); err != nil {
			return 0, false, err
		}
		rep.Complete(obj.Size)
		return obj.Size, false, nil

	case kindEmptyDir:
		if err := restoreEmptyDir(it.localPath, obj.Metadata); err != nil {
			return 0, false, err
		}
		rep.Complete(0)
		return 0, false, nil

	default:
		n, sum, err := restoreFile(it.localPath, obj, rep)
		if err != nil {
			return n, false, err
		}
		if !verify {
			return n, false, nil
		}
		ok, err := verifyFile(it.localPath, sum)
		if err != nil {
			return n, false, fmt.Errorf("verify %s: %w", it.relPath, err)
		}
		if !ok {
			return n, true, fmt.Errorf("%s: %w", it.relPath, errVerifyMismatch)
		}
		return n, true, nil
	}
}

// countingWriter feeds every write through to an underlying writer and
// reports the byte count to a reporter, which is how a single large file
// still moves the progress bar smoothly rather than jumping only when it
// finishes -- the same role progressReader plays on the upload side in
// internal/remote.
type countingWriter struct {
	w   io.Writer
	rep reporter
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 {
		c.rep.AddBytes(int64(n))
	}
	return n, err
}

// restoreFile streams obj's body to localPath and applies its recorded
// mode and mtime.
//
// It writes to a temporary file in the same directory first and renames
// into place only once the transfer, chmod, and chtimes have all
// succeeded. This is what keeps a killed or failed download from ever
// leaving a half-written file sitting at the real destination -- including
// the specific case of Overwrite replacing a file that was previously
// good: a failure here must leave the original untouched, not a truncated
// mix of old and new content.
func restoreFile(localPath string, obj *remote.Object, rep reporter) (int64, string, error) {
	dir := filepath.Dir(localPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, "", fmt.Errorf("create directory for %s: %w", localPath, err)
	}
	tmp, err := os.CreateTemp(dir, ".r2restore-*")
	if err != nil {
		return 0, "", fmt.Errorf("create temp file for %s: %w", localPath, err)
	}
	tmpPath := tmp.Name()
	placed := false
	defer func() {
		if !placed {
			os.Remove(tmpPath)
		}
	}()

	h := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(tmp, h), rep: rep}
	n, copyErr := io.Copy(cw, obj.Body)
	closeErr := tmp.Close()
	if copyErr != nil {
		return n, "", fmt.Errorf("download %s: %w", localPath, copyErr)
	}
	if closeErr != nil {
		return n, "", fmt.Errorf("close temp file for %s: %w", localPath, closeErr)
	}

	mode := obj.Metadata.Mode.Perm()
	if mode == 0 {
		mode = 0o644
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return n, "", fmt.Errorf("chmod %s: %w", localPath, err)
	}
	if !obj.Metadata.ModTime.IsZero() {
		if err := os.Chtimes(tmpPath, obj.Metadata.ModTime, obj.Metadata.ModTime); err != nil {
			return n, "", fmt.Errorf("set modtime on %s: %w", localPath, err)
		}
	}
	if err := os.Rename(tmpPath, localPath); err != nil {
		return n, "", fmt.Errorf("place %s: %w", localPath, err)
	}
	placed = true
	rep.Complete(0) // every byte was already reported via AddBytes above
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// restoreSymlink creates target's link, never a copy of what it points at.
//
// Following it instead -- copying the bytes at the target -- is exactly
// the bug this format exists to avoid: a pnpm node_modules is built almost
// entirely out of links into .pnpm/, and resolving them would expand a few
// megabytes into hundreds of duplicated copies. On Windows this can fail
// without Developer Mode or an elevated process; that is recorded as a
// per-object failure like any other and never aborts the run.
func restoreSymlink(localPath, target string) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", localPath, err)
	}
	// Overwrite already decided, during planning, whether this path was
	// allowed to be touched at all (see buildPlan) -- so by the time we
	// get here any existing entry, including a stale symlink from a prior
	// partial restore, is meant to be replaced. os.Symlink itself refuses
	// to create over one, so it is cleared first.
	_ = os.Remove(localPath)
	if err := os.Symlink(filepath.FromSlash(target), localPath); err != nil {
		return fmt.Errorf("create symlink %s -> %s: %w", localPath, target, err)
	}
	return nil
}

// restoreEmptyDir recreates a directory with no contents.
//
// Object storage cannot represent a directory except as a marker object,
// which is why this branch exists at all: without it, a folder that held
// nothing but other empty folders would simply disappear on restore.
func restoreEmptyDir(localPath string, meta remote.Metadata) error {
	mode := meta.Mode.Perm()
	if mode == 0 {
		mode = 0o755
	}
	if err := os.MkdirAll(localPath, mode); err != nil {
		return fmt.Errorf("create directory %s: %w", localPath, err)
	}
	// MkdirAll only applies mode (masked by umask) to directories it
	// actually creates; chmod afterward makes sure one that already
	// existed, or was masked down, still ends up with the mode the bucket
	// recorded.
	if err := os.Chmod(localPath, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", localPath, err)
	}
	if !meta.ModTime.IsZero() {
		if err := os.Chtimes(localPath, meta.ModTime, meta.ModTime); err != nil {
			return fmt.Errorf("set modtime on %s: %w", localPath, err)
		}
	}
	return nil
}

// verifyFile re-reads localPath from disk and reports whether its hash
// still matches wantSum, the digest computed while it was being written.
// Re-reading rather than trusting the in-memory hash is the point: it is
// what would catch a filesystem that silently truncated or corrupted the
// write on close.
func verifyFile(localPath, wantSum string) (bool, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return false, fmt.Errorf("reopen %s: %w", localPath, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, fmt.Errorf("re-read %s: %w", localPath, err)
	}
	return hex.EncodeToString(h.Sum(nil)) == wantSum, nil
}
