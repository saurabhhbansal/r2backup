package restore

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// fakeObject is one object a fakeBackend holds: content plus the metadata
// Head/Get would report for it.
type fakeObject struct {
	body []byte
	meta remote.Metadata
}

// fakeBackend is an in-memory stand-in for Backend, so this package's own
// planning and download logic -- glob filtering, overwrite/skip, per-item
// failure handling, cancellation, classify -- can be exercised without a
// network or a real bucket. The MinIO-backed tests in restore_test.go
// cover the parts a fake cannot: real S3 list pagination, real metadata
// round-tripping, and the actual byte-for-byte round trip through
// internal/backup.
type fakeBackend struct {
	mu      sync.Mutex
	objects map[string]fakeObject

	// blockGet, when non-nil, is closed to release a Get call that would
	// otherwise hang forever -- used to prove cancellation stops a run
	// promptly rather than waiting for every in-flight download.
	blockGet chan struct{}

	getCalls int
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{objects: make(map[string]fakeObject)}
}

func (f *fakeBackend) put(key string, body []byte, meta remote.Metadata) {
	f.mu.Lock()
	defer f.mu.Unlock()
	meta.Size = int64(len(body))
	f.objects[key] = fakeObject{body: body, meta: meta}
}

func (f *fakeBackend) List(ctx context.Context, prefix string) ([]remote.ListEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []remote.ListEntry
	for k, o := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, remote.ListEntry{Key: k, Size: int64(len(o.body))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (f *fakeBackend) Get(ctx context.Context, key string) (*remote.Object, error) {
	f.mu.Lock()
	f.getCalls++
	block := f.blockGet
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f.mu.Lock()
	o, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("fake: get %q: %w", key, remote.ErrNotFound)
	}
	return &remote.Object{
		Body:     io.NopCloser(strings.NewReader(string(o.body))),
		Metadata: o.meta,
		Size:     int64(len(o.body)),
	}, nil
}
