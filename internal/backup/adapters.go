package backup

import (
	"context"
	"io"
	"strings"

	"github.com/saurabhhbansal/r2backup/internal/engine"
	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/scan"
)

// The engine depends on interfaces it declares itself rather than on the R2
// client or the progress tracker directly. These adapters are the only place
// the two are joined, which is what lets the engine be tested with fakes and
// the client be tested against a real server, neither needing the other.

// uploader bridges the R2 client to engine.Uploader.
type uploader struct {
	client *remote.Client
	prefix string // "<machine>/<set>/current"
}

func (u uploader) key(k string) string { return u.prefix + "/" + k }

func (u uploader) Put(ctx context.Context, key string, r io.Reader, size int64, meta map[string]string, onBytes func(int64)) (string, error) {
	md, err := remote.MetadataFromS3(meta)
	if err != nil {
		return "", err
	}
	md.Size = size
	in := remote.PutInput{
		Key:      u.key(key),
		Body:     r,
		Size:     size,
		Metadata: md,
		Progress: onBytes,
	}
	if err := u.client.Put(ctx, in); err != nil {
		return "", err
	}
	// The index does not need an etag -- change detection is size and mtime,
	// deliberately, because hashing 60,000 files to find three changes is the
	// cost this design exists to avoid. Fetching one here would mean an extra
	// HEAD per object, which is a Class B operation per file for no benefit.
	return "", nil
}

func (u uploader) Copy(ctx context.Context, from, to string) error {
	return u.client.Copy(ctx, u.key(from), u.key(to))
}

func (u uploader) DeleteMany(ctx context.Context, keys []string) error {
	full := make([]string, len(keys))
	for i, k := range keys {
		full[i] = u.key(k)
	}
	return u.client.DeleteBatch(ctx, full)
}

// reporter bridges the progress tracker to engine.Reporter.
type reporter struct{ t *progress.Tracker }

func (r reporter) AddBytes(n int64)        { r.t.AddBytes(n) }
func (r reporter) CompleteFile(size int64) { r.t.Complete(0) }

// metadataFor builds the per-object metadata that makes the bucket
// self-describing, so a restore needs no manifest.
func metadataFor(e scan.Entry) map[string]string {
	return remote.Metadata{
		ModTime: e.ModTime,
		Mode:    e.Mode,
		Size:    e.Size,
		Symlink: e.Target,
	}.ToS3()
}

// isThrottle recognises the shapes R2 uses to say "slow down", so the engine
// backs off instead of hammering a bucket that is already refusing work.
func isThrottle(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, marker := range []string{"slowdown", "slow down", "503", "toomanyrequests", "too many requests", "429", "serviceunavailable"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

var _ engine.Uploader = uploader{}
var _ engine.Reporter = reporter{}
