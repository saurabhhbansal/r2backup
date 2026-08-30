package backup

import (
	"context"
	"io"
	"strings"

	"github.com/saurabhhbansal/r2backup/internal/engine"
	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/scan"
	"github.com/saurabhhbansal/r2backup/internal/trash"
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
	kind := remote.KindFile
	switch e.Kind {
	case scan.KindSymlink:
		kind = remote.KindSymlink
	case scan.KindEmptyDir:
		kind = remote.KindEmptyDir
	}
	return remote.Metadata{
		ModTime: e.ModTime,
		Mode:    e.Mode,
		Size:    e.Size,
		Symlink: e.Target,
		Kind:    kind,
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

// trashAdapter narrows the trash package to the single call a run makes, so
// backup does not carry retention policy around with it.
type trashAdapter struct {
	t             *trash.Trash
	retentionDays int
}

func (a trashAdapter) Move(ctx context.Context, prefix string, keys []string) error {
	_, err := a.t.Move(ctx, prefix, keys, a.retentionDays)
	return err
}

func (a trashAdapter) Prune(ctx context.Context, prefix string) (Pruned, error) {
	res, err := a.t.Prune(ctx, prefix, a.retentionDays)
	if err != nil {
		return Pruned{}, err
	}
	return Pruned{Dates: res.DatesPruned, Keys: res.KeysDeleted, Ops: res.ClassAOps}, nil
}

// NewTrash builds the Trash a run should use, or nil when the set keeps no
// history. A set that is pure build output does not need thirty days of it.
func NewTrash(client *remote.Client, retentionDays int) Trash {
	if retentionDays <= 0 {
		return nil
	}
	return trashAdapter{t: trash.New(client, trash.Clock{}), retentionDays: retentionDays}
}

// resumeStore joins the index's unfinished-upload bucket to what the R2
// client needs from one.
//
// The two types are deliberately separate -- index.PendingUpload is an
// on-disk format and remote.Upload is a wire concern -- so this is the one
// place they meet, and a change to either fails to compile here rather than
// quietly changing what is written to disk. The same reasoning as the
// uploader and reporter adapters above.
type resumeStore struct{ db *index.DB }

func (r resumeStore) Resumable(key string) (remote.Upload, bool, error) {
	u, ok, err := r.db.PendingUploadFor(key)
	if err != nil || !ok {
		return remote.Upload{}, false, err
	}
	return remote.Upload{
		Key: u.Key, UploadID: u.UploadID, PartSize: u.PartSize,
		Size: u.Size, ModTime: u.ModTime, StartedAt: u.StartedAt,
		Parts: toRemoteParts(u.Parts),
	}, true, nil
}

func (r resumeStore) SaveResumable(u remote.Upload) error {
	return r.db.SavePendingUpload(index.PendingUpload{
		Key: u.Key, UploadID: u.UploadID, PartSize: u.PartSize,
		Size: u.Size, ModTime: u.ModTime, StartedAt: u.StartedAt,
		Parts: toIndexParts(u.Parts),
	})
}

func (r resumeStore) ForgetResumable(key string) error {
	return r.db.ForgetPendingUpload(key)
}

func (r resumeStore) AllResumable() ([]remote.Upload, error) {
	all, err := r.db.AllPendingUploads()
	if err != nil {
		return nil, err
	}
	out := make([]remote.Upload, 0, len(all))
	for _, u := range all {
		out = append(out, remote.Upload{
			Key: u.Key, UploadID: u.UploadID, PartSize: u.PartSize,
			Size: u.Size, ModTime: u.ModTime, StartedAt: u.StartedAt,
			Parts: toRemoteParts(u.Parts),
		})
	}
	return out, nil
}

func toRemoteParts(in []index.PendingPart) []remote.PartRecord {
	out := make([]remote.PartRecord, 0, len(in))
	for _, p := range in {
		out = append(out, remote.PartRecord{Number: p.Number, ETag: p.ETag, Size: p.Size})
	}
	return out
}

func toIndexParts(in []remote.PartRecord) []index.PendingPart {
	out := make([]index.PendingPart, 0, len(in))
	for _, p := range in {
		out = append(out, index.PendingPart{Number: p.Number, ETag: p.ETag, Size: p.Size})
	}
	return out
}

// ResumeStoreFor is how a caller attaches the index to a client so large
// uploads can be picked up where they stopped.
func ResumeStoreFor(db *index.DB) remote.ResumeStore { return resumeStore{db: db} }
